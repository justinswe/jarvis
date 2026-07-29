package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testClient(t *testing.T, provider Provider, responseBody string) (*Client, *http.Request) {
	t.Helper()
	var captured *http.Request
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		captured = request
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})}
	client, err := New(Config{Provider: provider, APIKey: "secret", HTTPClient: httpClient})
	require.NoError(t, err)
	return client, captured
}

func TestProviderAdaptersShareNormalizedContract(t *testing.T) {
	fixtures := map[Provider]string{
		ProviderSerper:    `{"organic":[{"title":"First","link":"HTTPS://EXAMPLE.COM/a#fragment","snippet":"one"},{"title":"Second","link":"http://second.example/b","snippet":"two"}]}`,
		ProviderFirecrawl: `{"success":true,"data":{"web":[{"title":"First","url":"HTTPS://EXAMPLE.COM/a#fragment","description":"one"},{"title":"Second","url":"http://second.example/b","description":"two"}]}}`,
		ProviderTavily:    `{"results":[{"title":"First","url":"HTTPS://EXAMPLE.COM/a#fragment","content":"one"},{"title":"Second","url":"http://second.example/b","content":"two"}]}`,
	}
	want := Response{Results: []Result{
		{Title: "First", URL: "https://example.com/a", Domain: "example.com", Snippet: "one"},
		{Title: "Second", URL: "http://second.example/b", Domain: "second.example", Snippet: "two"},
	}}
	for provider, fixture := range fixtures {
		t.Run(string(provider), func(t *testing.T) {
			client, _ := testClient(t, provider, fixture)
			got, err := client.Search(context.Background(), "same query")
			require.NoError(t, err)
			assert.Equal(t, want.Results, got.Results)
			assert.Equal(t, 2, got.Diagnostics.ReturnedResults)
			assert.Equal(t, 2, got.Diagnostics.AcceptedResults)
			assert.Equal(t, "ok", got.Diagnostics.ParserOutcome)
		})
	}
}

func TestProviderRequestsUseExactWireContracts(t *testing.T) {
	tests := []struct {
		provider Provider
		endpoint string
		header   string
		value    string
		body     map[string]any
		fixture  string
	}{
		{provider: ProviderSerper, endpoint: "https://google.serper.dev/search", header: "X-API-KEY", value: "secret", body: map[string]any{"q": "query", "num": float64(3)}, fixture: `{"organic":[]}`},
		{provider: ProviderFirecrawl, endpoint: "https://api.firecrawl.dev/v2/search", header: "Authorization", value: "Bearer secret", body: map[string]any{"query": "query", "limit": float64(3), "sources": []any{"web"}}, fixture: `{"success":true,"data":{"web":[]}}`},
		{provider: ProviderTavily, endpoint: "https://api.tavily.com/search", header: "Authorization", value: "Bearer secret", body: map[string]any{
			"query": "query", "search_depth": "basic", "max_results": float64(3), "include_answer": false,
			"include_raw_content": false, "include_images": false, "auto_parameters": false,
		}, fixture: `{"results":[]}`},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			var captured *http.Request
			var body map[string]any
			httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				captured = request
				require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.fixture))}, nil
			})}
			client, err := New(Config{Provider: test.provider, APIKey: " secret ", HTTPClient: httpClient})
			require.NoError(t, err)
			_, err = client.Search(context.Background(), " query ")
			require.NoError(t, err)
			assert.Equal(t, http.MethodPost, captured.Method)
			assert.Equal(t, test.endpoint, captured.URL.String())
			assert.Equal(t, "application/json", captured.Header.Get("Content-Type"))
			assert.Equal(t, test.value, captured.Header.Get(test.header))
			assert.Equal(t, test.body, body)
			if test.provider == ProviderFirecrawl {
				assert.NotContains(t, body, "scrapeOptions")
			}
		})
	}
}

func TestNormalizationRejectsInvalidDuplicatesAndMissingSnippets(t *testing.T) {
	fixture := `{"organic":[
		{"title":"","link":"HTTPS://EXAMPLE.COM/path#one","snippet":" kept "},
		{"title":"duplicate","link":"https://example.com/path#two","snippet":"duplicate"},
		{"title":"missing","snippet":"missing url"},
		{"title":"bad","link":"ftp://example.com/a","snippet":"bad"},
		{"title":"userinfo","link":"https://user@example.com/a","snippet":"bad"},
		{"title":"empty","link":"https://empty.example/a","snippet":"  "},
		{"title":"second","link":"http://SECOND.EXAMPLE/b#fragment","snippet":"two"}
	]}`
	client, _ := testClient(t, ProviderSerper, fixture)
	got, err := client.Search(context.Background(), "query")
	require.NoError(t, err)
	assert.Equal(t, []Result{
		{Title: "example.com", URL: "https://example.com/path", Domain: "example.com", Snippet: "kept"},
		{Title: "second", URL: "http://second.example/b", Domain: "second.example", Snippet: "two"},
	}, got.Results)
	assert.Equal(t, 7, got.Diagnostics.ReturnedResults)
	assert.Equal(t, 1, got.Diagnostics.DuplicateURLResults)
	assert.Equal(t, 1, got.Diagnostics.MissingURLResults)
	assert.Equal(t, 2, got.Diagnostics.InvalidURLResults)
	assert.Equal(t, 1, got.Diagnostics.MissingSnippetResults)
}

func TestNormalizationEscapesMarkdownLinkDestinations(t *testing.T) {
	client, _ := testClient(t, ProviderSerper, `{"organic":[{
		"title":"source","link":"https://evil.example/a)[google](https://phish.example","snippet":"summary"
	}]}`)
	got, err := client.Search(context.Background(), "query")
	require.NoError(t, err)
	require.Len(t, got.Results, 1)
	assert.Equal(t, "https://evil.example/a%29%5Bgoogle%5D%28https://phish.example", got.Results[0].URL)
	assert.NotContains(t, got.Results[0].URL, ")")
	assert.NotContains(t, got.Results[0].URL, "(")
}

func TestNormalizationPreservesOrderingAndBoundsTextAndResults(t *testing.T) {
	longTitle := strings.Repeat("界", maxTitleRunes+1)
	longSnippet := strings.Repeat("🙂", maxSnippetRunes+1)
	fixture := `{"results":[` +
		`{"title":` + quoted(t, longTitle) + `,"url":"https://one.example","content":` + quoted(t, longSnippet) + `},` +
		`{"title":"two","url":"https://two.example","content":"two"},` +
		`{"title":"three","url":"https://three.example","content":"three"},` +
		`{"title":"four","url":"https://four.example","content":"four"}` +
		`]}`
	client, _ := testClient(t, ProviderTavily, fixture)
	got, err := client.Search(context.Background(), "query")
	require.NoError(t, err)
	require.Len(t, got.Results, 3)
	assert.Equal(t, []string{"one.example", "two.example", "three.example"}, []string{got.Results[0].Domain, got.Results[1].Domain, got.Results[2].Domain})
	assert.Len(t, []rune(got.Results[0].Title), maxTitleRunes)
	assert.Len(t, []rune(got.Results[0].Snippet), maxSnippetRunes)
}

func TestSearchRejectsInvalidInputAndConfiguration(t *testing.T) {
	for _, config := range []Config{{Provider: "unknown", APIKey: "secret"}, {Provider: ProviderSerper}} {
		_, err := New(config)
		assertErrorKind(t, err, ErrorInvalidInput)
	}
	client, err := New(Config{Provider: ProviderSerper, APIKey: "secret"})
	require.NoError(t, err)
	for _, query := range []string{" ", strings.Repeat("x", maxQueryRunes+1)} {
		_, err := client.Search(context.Background(), query)
		assertErrorKind(t, err, ErrorInvalidInput)
	}
}

func TestSearchClassifiesEveryHTTPErrorClassAndRetryAfter(t *testing.T) {
	tests := []struct {
		status int
		kind   ErrorKind
	}{
		{http.StatusBadRequest, ErrorInvalidInput},
		{http.StatusUnprocessableEntity, ErrorInvalidInput},
		{http.StatusUnauthorized, ErrorAuthentication},
		{http.StatusForbidden, ErrorAuthentication},
		{http.StatusPaymentRequired, ErrorQuota},
		{432, ErrorQuota},
		{433, ErrorQuota},
		{http.StatusRequestTimeout, ErrorTimeout},
		{http.StatusGatewayTimeout, ErrorTimeout},
		{http.StatusTooManyRequests, ErrorRateLimit},
		{http.StatusInternalServerError, ErrorService},
		{http.StatusConflict, ErrorService},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.status,
					Header:     http.Header{"Retry-After": []string{"7"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":"secret provider detail"}`)),
				}, nil
			})}
			client, err := New(Config{Provider: ProviderSerper, APIKey: "secret", HTTPClient: httpClient})
			require.NoError(t, err)
			response, err := client.Search(context.Background(), "query")
			assertErrorKind(t, err, test.kind)
			assert.Equal(t, test.status, response.Diagnostics.HTTPStatus)
			assert.Equal(t, 7*time.Second, response.Diagnostics.RetryAfter)
			assert.Equal(t, "not_attempted", response.Diagnostics.ParserOutcome)
			assert.NotContains(t, err.Error(), "provider detail")
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestSearchRejectsMalformedAndOversizedResponses(t *testing.T) {
	for _, fixture := range []string{`not json`, `{}`, `{"organic":null}`, `{"organic":[]} trailing`} {
		client, _ := testClient(t, ProviderSerper, fixture)
		response, err := client.Search(context.Background(), "query")
		assertErrorKind(t, err, ErrorMalformedResponse)
		assert.Equal(t, "malformed", response.Diagnostics.ParserOutcome)
	}
	client, _ := testClient(t, ProviderSerper, strings.Repeat("x", maxResponseBytes+1))
	response, err := client.Search(context.Background(), "query")
	assertErrorKind(t, err, ErrorMalformedResponse)
	assert.Equal(t, maxResponseBytes+1, response.Diagnostics.ResponseBodyBytes)
	assert.Equal(t, "response_too_large", response.Diagnostics.ParserOutcome)
}

func TestSearchCancellationTimeoutAndTransportAreSafe(t *testing.T) {
	client, err := New(Config{Provider: ProviderSerper, APIKey: "top-secret", HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}})
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Search(canceled, "query")
	assert.ErrorIs(t, err, context.Canceled)

	deadline, cancelDeadline := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelDeadline()
	_, err = client.Search(deadline, "query")
	assertErrorKind(t, err, ErrorTimeout)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	client.doer = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("Post https://example.test/?key=top-secret: failed")
	})}
	response, err := client.Search(context.Background(), "query")
	assertErrorKind(t, err, ErrorTransport)
	assert.Equal(t, "not_attempted", response.Diagnostics.ParserOutcome)
	assert.NotContains(t, err.Error(), "top-secret")
	assert.NotContains(t, err.Error(), "https://")
}

func TestRetryAfterParsesHTTPDate(t *testing.T) {
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, 5*time.Second, parseRetryAfter(now.Add(5*time.Second).Format(http.TimeFormat), now))
	assert.Zero(t, parseRetryAfter("invalid", now))
	assert.Zero(t, parseRetryAfter("-1", now))
}

func quoted(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func assertErrorKind(t *testing.T, err error, kind ErrorKind) {
	t.Helper()
	var searchErr *Error
	require.ErrorAs(t, err, &searchErr)
	assert.Equal(t, kind, searchErr.Kind)
}
