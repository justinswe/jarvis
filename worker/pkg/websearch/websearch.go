// Package websearch provides normalized public-web search results.
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxQueryRunes    = 500
	maxTitleRunes    = 200
	maxSnippetRunes  = 1000
	maxResponseBytes = 1 << 20
	maxResults       = 3
)

var markdownURLComponentEscaper = strings.NewReplacer(
	"(", "%28",
	")", "%29",
	"[", "%5B",
	"]", "%5D",
)

// Provider identifies a supported web-search service.
type Provider string

const (
	ProviderSerper    Provider = "serper"
	ProviderFirecrawl Provider = "firecrawl"
	ProviderTavily    Provider = "tavily"
)

// Config configures one provider client.
type Config struct {
	Provider   Provider
	APIKey     string
	HTTPClient *http.Client
}

// Result is a provider-neutral search result.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Domain  string `json:"domain"`
	Snippet string `json:"snippet"`
}

// Diagnostics records provider-neutral response validation details.
type Diagnostics struct {
	ReturnedResults       int
	AcceptedResults       int
	MissingURLResults     int
	InvalidURLResults     int
	DuplicateURLResults   int
	MissingSnippetResults int
	ResponseBodyBytes     int
	HTTPStatus            int
	RetryAfter            time.Duration
	Latency               time.Duration
	ParserOutcome         string
}

// Response contains normalized results and validation diagnostics.
type Response struct {
	Results     []Result
	Diagnostics Diagnostics
}

// ErrorKind classifies a web-search failure without provider messages.
type ErrorKind string

const (
	ErrorInvalidInput      ErrorKind = "invalid_input"
	ErrorAuthentication    ErrorKind = "authentication"
	ErrorQuota             ErrorKind = "quota"
	ErrorRateLimit         ErrorKind = "rate_limit"
	ErrorTimeout           ErrorKind = "timeout"
	ErrorMalformedResponse ErrorKind = "malformed_response"
	ErrorTransport         ErrorKind = "transport"
	ErrorService           ErrorKind = "service"
)

// Error is a typed, credential-safe provider failure.
type Error struct {
	Kind       ErrorKind
	Provider   Provider
	StatusCode int
	RetryAfter time.Duration
	cause      error
}

// Error returns a provider-neutral message.
func (e *Error) Error() string {
	if e == nil {
		return "web search failed"
	}
	return "web search " + string(e.Kind)
}

// Unwrap exposes only safe sentinel causes such as context deadlines.
func (e *Error) Unwrap() error { return e.cause }

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client searches through one configured provider.
type Client struct {
	provider Provider
	apiKey   string
	doer     httpDoer
}

// New creates a provider client.
func New(config Config) (*Client, error) {
	if !SupportedProvider(config.Provider) {
		return nil, newError(ErrorInvalidInput, config.Provider, 0, 0, nil)
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, newError(ErrorInvalidInput, config.Provider, 0, 0, nil)
	}
	doer := httpDoer(http.DefaultClient)
	if config.HTTPClient != nil {
		doer = config.HTTPClient
	}
	return &Client{provider: config.Provider, apiKey: strings.TrimSpace(config.APIKey), doer: doer}, nil
}

// SupportedProvider reports whether provider has an adapter.
func SupportedProvider(provider Provider) bool {
	switch provider {
	case ProviderSerper, ProviderFirecrawl, ProviderTavily:
		return true
	default:
		return false
	}
}

// Provider returns the configured provider.
func (c *Client) Provider() Provider { return c.provider }

// Search returns at most three normalized results in provider order.
func (c *Client) Search(ctx context.Context, query string) (response Response, resultErr error) {
	query = strings.TrimSpace(query)
	if query == "" || utf8.RuneCountInString(query) > maxQueryRunes {
		return Response{}, newError(ErrorInvalidInput, c.provider, 0, 0, nil)
	}
	request, err := c.request(ctx, query)
	if err != nil {
		return Response{}, newError(ErrorInvalidInput, c.provider, 0, 0, nil)
	}

	started := time.Now()
	defer func() { response.Diagnostics.Latency = time.Since(started) }()
	httpResponse, err := c.doer.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return Response{Diagnostics: Diagnostics{ParserOutcome: "not_attempted"}}, context.Canceled
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return Response{Diagnostics: Diagnostics{ParserOutcome: "not_attempted"}}, newError(ErrorTimeout, c.provider, 0, 0, context.DeadlineExceeded)
		}
		return Response{Diagnostics: Diagnostics{ParserOutcome: "not_attempted"}}, newError(ErrorTransport, c.provider, 0, 0, nil)
	}
	defer httpResponse.Body.Close()

	diagnostics := Diagnostics{
		HTTPStatus:    httpResponse.StatusCode,
		RetryAfter:    parseRetryAfter(httpResponse.Header.Get("Retry-After"), time.Now()),
		ParserOutcome: "not_attempted",
	}
	body, readErr := io.ReadAll(io.LimitReader(httpResponse.Body, maxResponseBytes+1))
	diagnostics.ResponseBodyBytes = len(body)
	if readErr != nil {
		diagnostics.ParserOutcome = "read_failed"
		return Response{Diagnostics: diagnostics}, newError(ErrorTransport, c.provider, httpResponse.StatusCode, diagnostics.RetryAfter, nil)
	}
	if len(body) > maxResponseBytes {
		diagnostics.ParserOutcome = "response_too_large"
		return Response{Diagnostics: diagnostics}, newError(ErrorMalformedResponse, c.provider, httpResponse.StatusCode, diagnostics.RetryAfter, nil)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return Response{Diagnostics: diagnostics}, httpStatusError(c.provider, httpResponse.StatusCode, diagnostics.RetryAfter)
	}

	rawResults, decodeErr := decodeResults(c.provider, body)
	if decodeErr != nil {
		diagnostics.ParserOutcome = "malformed"
		return Response{Diagnostics: diagnostics}, newError(ErrorMalformedResponse, c.provider, httpResponse.StatusCode, diagnostics.RetryAfter, nil)
	}
	results := normalizeResults(rawResults, &diagnostics)
	diagnostics.ParserOutcome = "ok"
	return Response{Results: results, Diagnostics: diagnostics}, nil
}

func (c *Client) request(ctx context.Context, query string) (*http.Request, error) {
	endpoint, payload := "", any(nil)
	switch c.provider {
	case ProviderSerper:
		endpoint = "https://google.serper.dev/search"
		payload = struct {
			Query string `json:"q"`
			Count int    `json:"num"`
		}{Query: query, Count: maxResults}
	case ProviderFirecrawl:
		endpoint = "https://api.firecrawl.dev/v2/search"
		payload = struct {
			Query   string   `json:"query"`
			Limit   int      `json:"limit"`
			Sources []string `json:"sources"`
		}{Query: query, Limit: maxResults, Sources: []string{"web"}}
	case ProviderTavily:
		endpoint = "https://api.tavily.com/search"
		payload = struct {
			Query             string `json:"query"`
			SearchDepth       string `json:"search_depth"`
			MaxResults        int    `json:"max_results"`
			IncludeAnswer     bool   `json:"include_answer"`
			IncludeRawContent bool   `json:"include_raw_content"`
			IncludeImages     bool   `json:"include_images"`
			AutoParameters    bool   `json:"auto_parameters"`
		}{Query: query, SearchDepth: "basic", MaxResults: maxResults}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.provider == ProviderSerper {
		request.Header.Set("X-API-KEY", c.apiKey)
	} else {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return request, nil
}

type rawResult struct {
	title   string
	url     string
	snippet string
}

func decodeResults(provider Provider, body []byte) ([]rawResult, error) {
	switch provider {
	case ProviderSerper:
		var response struct {
			Organic *[]struct {
				Title   string `json:"title"`
				Link    string `json:"link"`
				Snippet string `json:"snippet"`
			} `json:"organic"`
		}
		if err := decodeJSON(body, &response); err != nil || response.Organic == nil {
			return nil, errMalformedPayload
		}
		results := make([]rawResult, 0, len(*response.Organic))
		for _, item := range *response.Organic {
			results = append(results, rawResult{title: item.Title, url: item.Link, snippet: item.Snippet})
		}
		return results, nil
	case ProviderFirecrawl:
		var response struct {
			Success bool `json:"success"`
			Data    *struct {
				Web *[]struct {
					Title       string `json:"title"`
					URL         string `json:"url"`
					Description string `json:"description"`
				} `json:"web"`
			} `json:"data"`
		}
		if err := decodeJSON(body, &response); err != nil || !response.Success || response.Data == nil || response.Data.Web == nil {
			return nil, errMalformedPayload
		}
		results := make([]rawResult, 0, len(*response.Data.Web))
		for _, item := range *response.Data.Web {
			results = append(results, rawResult{title: item.Title, url: item.URL, snippet: item.Description})
		}
		return results, nil
	case ProviderTavily:
		var response struct {
			Results *[]struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}
		if err := decodeJSON(body, &response); err != nil || response.Results == nil {
			return nil, errMalformedPayload
		}
		results := make([]rawResult, 0, len(*response.Results))
		for _, item := range *response.Results {
			results = append(results, rawResult{title: item.Title, url: item.URL, snippet: item.Content})
		}
		return results, nil
	default:
		return nil, errMalformedPayload
	}
}

type payloadError struct{}

func (payloadError) Error() string { return "malformed provider payload" }

var errMalformedPayload error = payloadError{}

func decodeJSON(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errMalformedPayload
	}
	return nil
}

func normalizeResults(raw []rawResult, diagnostics *Diagnostics) []Result {
	diagnostics.ReturnedResults = len(raw)
	seen := make(map[string]struct{}, len(raw))
	results := make([]Result, 0, min(len(raw), maxResults))
	for _, item := range raw {
		if strings.TrimSpace(item.url) == "" {
			diagnostics.MissingURLResults++
			continue
		}
		normalizedURL, domain, ok := normalizeURL(item.url)
		if !ok {
			diagnostics.InvalidURLResults++
			continue
		}
		snippet := strings.TrimSpace(item.snippet)
		if snippet == "" {
			diagnostics.MissingSnippetResults++
			continue
		}
		if _, duplicate := seen[normalizedURL]; duplicate {
			diagnostics.DuplicateURLResults++
			continue
		}
		seen[normalizedURL] = struct{}{}
		if len(results) == maxResults {
			continue
		}
		title := strings.TrimSpace(item.title)
		if title == "" {
			title = domain
		}
		results = append(results, Result{
			Title: truncateRunes(title, maxTitleRunes), URL: normalizedURL, Domain: domain,
			Snippet: truncateRunes(snippet, maxSnippetRunes),
		})
	}
	diagnostics.AcceptedResults = len(results)
	return results
}

func normalizeURL(raw string) (string, string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.User != nil || parsed.Host == "" {
		return "", "", false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", false
	}
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	domain := strings.ToLower(parsed.Hostname())
	if domain == "" {
		return "", "", false
	}
	parsed.RawPath = markdownURLComponentEscaper.Replace(parsed.EscapedPath())
	parsed.RawQuery = markdownURLComponentEscaper.Replace(parsed.RawQuery)
	return parsed.String(), domain, true
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func httpStatusError(provider Provider, status int, retryAfter time.Duration) error {
	kind := ErrorService
	switch {
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		kind = ErrorInvalidInput
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = ErrorAuthentication
	case status == http.StatusPaymentRequired || status == 432 || status == 433:
		kind = ErrorQuota
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		kind = ErrorTimeout
	case status == http.StatusTooManyRequests:
		kind = ErrorRateLimit
	}
	return newError(kind, provider, status, retryAfter, nil)
}

func newError(kind ErrorKind, provider Provider, status int, retryAfter time.Duration, cause error) *Error {
	return &Error{Kind: kind, Provider: provider, StatusCode: status, RetryAfter: retryAfter, cause: cause}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
