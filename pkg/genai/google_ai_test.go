package genai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/justinswe/jarvis/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type googleAITransportFunc func(*http.Request) (*http.Response, error)

func (f googleAITransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGoogleAICredentialsAreRequiredOnlyWhenSelected(t *testing.T) {
	providers := map[llm.Provider]struct{}{llm.ProviderGoogleAI: {}}
	assert.ErrorContains(t, validateProviderCredentials(Config{}, providers), "google-ai-api-key")
	require.NoError(t, validateProviderCredentials(Config{GoogleAIAPIKey: "key"}, providers))

	unselected := map[llm.Provider]struct{}{llm.ProviderOpenRouter: {}}
	require.NoError(t, validateProviderCredentials(Config{
		GoogleAIAPIKey: "ignored", OpenRouterAPIKey: "selected",
	}, unselected))
}

func TestGoogleAIProfilesShareOneConfiguredHostWithoutVertexConfiguration(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: googleAITransportFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		assert.Equal(t, "key", request.Header.Get("x-goog-api-key"))
		assert.Empty(t, request.Header.Get("Authorization"))
		assert.NotContains(t, request.URL.Path, "projects/")
		body := `{"totalTokens":1}`
		if request.Method == http.MethodGet {
			body = `{
				"name":"models/gemini-3.1-flash-lite",
				"supportedGenerationMethods":["generateContent","countTokens"]
			}`
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}
	handler, err := New(context.Background(), Config{
		GoogleAIAPIKey: "key", GoogleAIHTTPClient: client,
		ModelProfiles: []string{
			"primary=google-ai:gemini-3.1-flash-lite",
			"fallback=google-ai:gemini-3.1-flash-lite",
		},
		PrimaryModelProfile: "primary", FallbackModelProfile: "fallback",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handler.Close()) })
	primary, ok := handler.Registry().Host("primary")
	require.True(t, ok)
	fallback, ok := handler.Registry().Host("fallback")
	require.True(t, ok)
	assert.Equal(t, primary, fallback)
	assert.Equal(t, 3, requests, "one deduplicated model lookup plus tool and image probes")

	profiles := handler.Registry().Profiles()
	require.Len(t, profiles, 2)
	assert.Equal(t, llm.ProviderGoogleAI, profiles[0].Provider)
	assert.True(t, profiles[0].ToolsEnabled())
}

func TestGoogleAIProfileParsingRemainsProviderNeutral(t *testing.T) {
	profiles, selection, err := modelProfileConfiguration(Config{
		ModelProfiles: []string{
			"primary=google-ai:gemini-3.1-flash-lite",
			"fallback=vertex:gemini-2.5-flash",
		},
		PrimaryModelProfile: "primary", FallbackModelProfile: "fallback",
	})
	require.NoError(t, err)
	assert.Equal(t, llm.Selection{Primary: "primary", Fallback: "fallback"}, selection)
	assert.Equal(t, llm.ProviderGoogleAI, profiles[0].Provider)
	assert.Equal(t, "gemini-3.1-flash-lite", profiles[0].ModelID)
}
