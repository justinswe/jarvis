package genai

import (
	"context"
	"testing"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stallingHost blocks until its context is done, standing in for an upstream that
// accepts a request and then never answers.
type stallingHost struct {
	budgets []time.Duration
}

func (h *stallingHost) Generate(ctx context.Context, _ llm.Request) (llm.Response, error) {
	h.budgets = append(h.budgets, remaining(ctx))
	<-ctx.Done()
	return llm.Response{}, &llm.Error{
		Kind: llm.ErrorTimeout, Provider: llm.ProviderGoogleAI, ErrorType: "transport_timeout",
		Scope: "transport", Err: ctx.Err(),
	}
}

// budgetedHost answers immediately and records how much time each call was given.
type budgetedHost struct {
	text    string
	budgets []time.Duration
}

func (h *budgetedHost) Generate(ctx context.Context, _ llm.Request) (llm.Response, error) {
	h.budgets = append(h.budgets, remaining(ctx))
	return neutralText(h.text), nil
}

// remaining reports a context's time budget, or -1 when it has no deadline.
func remaining(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return -1
	}
	return time.Until(deadline)
}

func budgetedHandler(t *testing.T, hosts map[string]llm.Host, selection llm.Selection, attempt, reserve time.Duration) *Handler {
	t.Helper()
	profiles := make([]llm.Profile, 0, len(hosts))
	for name := range hosts {
		profiles = append(profiles, llm.Profile{Name: name, Provider: llm.ProviderGoogleAI, ModelID: "model"})
	}
	handler := neutralHandler(t, profiles, hosts, selection)
	handler.cfg.AttemptTimeout = attempt
	handler.cfg.FallbackReserve = reserve
	return handler
}

func budgetedRequest(fallback string) GenerateRequest {
	return GenerateRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
		Config:   &RequestConfig{MaxOutputTokens: 256, PrimaryModelProfile: "primary", FallbackModelProfile: fallback},
	}
}

// TestFallbackKeepsBudgetWhenPrimaryStalls is the regression guard for the production
// failure: one shared deadline let a stalled primary consume the whole request, so the
// fallback was invoked with an already-expired context and died in 0ms every time.
//
// The attempt timeout deliberately exceeds the request budget here. That is what
// production looked like — the Google adapter is handed no HTTP client, so nothing
// bounded a call below the message timeout — and it isolates the reserve as the only
// thing that can keep the fallback alive.
func TestFallbackKeepsBudgetWhenPrimaryStalls(t *testing.T) {
	primary := &stallingHost{}
	fallback := &budgetedHost{text: "fallback answered"}
	handler := budgetedHandler(t,
		map[string]llm.Host{"primary": primary, "fallback": fallback},
		llm.Selection{Primary: "primary", Fallback: "fallback"},
		10*time.Second, 100*time.Millisecond,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	got, err := handler.Generate(ctx, budgetedRequest("fallback"))

	require.NoError(t, err, "the fallback must be able to rescue a stalled primary")
	assert.Equal(t, "fallback answered", got.Text)
	require.Len(t, fallback.budgets, 1, "the fallback must actually be called")
	assert.Greater(t, fallback.budgets[0], time.Duration(0),
		"the fallback must receive a live context, not one the primary already exhausted")
	require.NotEmpty(t, primary.budgets)
	assert.Less(t, primary.budgets[0], 300*time.Millisecond,
		"the primary must be held to a shortened deadline so the reserve survives")
}

// TestAttemptTimeoutBoundsOneCall pins that no single provider call can spend the whole
// request budget, even when nothing else would stop it.
func TestAttemptTimeoutBoundsOneCall(t *testing.T) {
	primary := &budgetedHost{text: "answer"}
	handler := budgetedHandler(t,
		map[string]llm.Host{"primary": primary},
		llm.Selection{Primary: "primary"},
		50*time.Millisecond, 100*time.Millisecond,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := handler.Generate(ctx, budgetedRequest(""))

	require.NoError(t, err)
	require.NotEmpty(t, primary.budgets)
	assert.LessOrEqual(t, primary.budgets[0], 50*time.Millisecond,
		"one call must be capped at the attempt timeout, not the message timeout")
	assert.Greater(t, remaining(ctx), time.Second,
		"capping the call must not consume the parent budget")
}

// TestReservedContextDegradesSafely covers the cases where dividing the budget would do
// more harm than good: no fallback to save it for, no deadline to divide, and a budget
// too small to divide without starving the primary.
func TestReservedContextDegradesSafely(t *testing.T) {
	deadline := time.Now().Add(300 * time.Millisecond)

	t.Run("no fallback keeps the full budget", func(t *testing.T) {
		parent, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		ctx, release := reservedContext(parent, 100*time.Millisecond, false)
		defer release()
		assert.Equal(t, parent, ctx)
	})

	t.Run("no deadline stays unbounded", func(t *testing.T) {
		ctx, release := reservedContext(context.Background(), 100*time.Millisecond, true)
		defer release()
		_, ok := ctx.Deadline()
		assert.False(t, ok)
	})

	t.Run("oversized reserve is clamped to a third", func(t *testing.T) {
		parent, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		ctx, release := reservedContext(parent, time.Hour, true)
		defer release()
		assert.Greater(t, remaining(ctx), 150*time.Millisecond,
			"the primary must keep the majority of a budget it has to share")
	})
}
