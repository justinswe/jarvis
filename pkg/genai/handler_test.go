package genai

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	googlegenai "google.golang.org/genai"
)

func TestSanitizeText(t *testing.T) {
	t.Run("basic text unchanged", func(t *testing.T) {
		assert.Equal(t, "hello world", sanitizeText("hello world"))
	})

	t.Run("trims and keeps tabs newlines", func(t *testing.T) {
		input := "  hello\tworld\n"
		assert.Equal(t, "hello\tworld\n", sanitizeText(input))
	})

	t.Run("quotes remain unescaped", func(t *testing.T) {
		assert.Equal(t, `say "hi"`, sanitizeText(`say "hi"`))
	})

	t.Run("control characters removed", func(t *testing.T) {
		assert.Equal(t, "hithere", sanitizeText("hi\x07there"))
	})

	t.Run("empty after trimming returns empty", func(t *testing.T) {
		assert.Equal(t, "", sanitizeText("   "))
	})
}

func TestComposeSystemPrompt(t *testing.T) {
	t.Run("falls back to default and appends context contract", func(t *testing.T) {
		composed := composeSystemPrompt("")
		assert.Contains(t, composed, "helpful assistant named Jarvis")
		assert.Contains(t, composed, "Context Handling Contract:")
	})

	t.Run("normalizes escaped newlines from flags", func(t *testing.T) {
		composed := composeSystemPrompt("First line\\nSecond line")
		assert.Contains(t, composed, "First line\nSecond line")
		assert.Contains(t, composed, "Context Handling Contract:")
	})

	t.Run("does not duplicate context contract", func(t *testing.T) {
		raw := "Custom instruction.\n\nContext Handling Contract:\n- already present."
		composed := composeSystemPrompt(raw)
		assert.Equal(t, 1, strings.Count(composed, "Context Handling Contract:"))
	})
}

func TestResolveOptions(t *testing.T) {
	handler := &Handler{
		cfg: Config{
			WebGroundingTimeout:                 7 * time.Second,
			WebGroundingAttemptTimeout:          3 * time.Second,
			WebGroundingAddendumMaxOutputTokens: 256,
			WebGroundingAPIVersion:              "v1",
			WebGroundingMaxResults:              6,
			WebGroundingBudgetRatio:             0.30,
			WebGroundingRequireCitations:        true,
		},
	}

	t.Run("uses defaults when options unset", func(t *testing.T) {
		resolved := handler.resolveOptions(GenerateOptions{UseWebGrounding: true})
		assert.True(t, resolved.UseWebGrounding)
		assert.Equal(t, float32(0.30), resolved.GroundingBudgetRatio)
		assert.Equal(t, 7*time.Second, resolved.WebGroundingTimeout)
		assert.Equal(t, 3*time.Second, resolved.WebGroundingAttemptTimeout)
		assert.Equal(t, 256, resolved.WebGroundingAddendumMaxOutputTokens)
		assert.Equal(t, "v1", resolved.WebGroundingAPIVersion)
		assert.Equal(t, 6, resolved.WebGroundingMaxResults)
		assert.True(t, resolved.RequireCitations)
	})

	t.Run("overrides with request options", func(t *testing.T) {
		resolved := handler.resolveOptions(GenerateOptions{
			UseWebGrounding:                     true,
			GroundingBudgetRatio:                0.25,
			WebGroundingTimeout:                 4 * time.Second,
			WebGroundingAttemptTimeout:          2 * time.Second,
			WebGroundingAddendumMaxOutputTokens: 128,
			WebGroundingAPIVersion:              "v1beta1",
			WebGroundingMaxResults:              4,
			RequireCitations:                    true,
		})
		assert.Equal(t, float32(0.25), resolved.GroundingBudgetRatio)
		assert.Equal(t, 4*time.Second, resolved.WebGroundingTimeout)
		assert.Equal(t, 2*time.Second, resolved.WebGroundingAttemptTimeout)
		assert.Equal(t, 128, resolved.WebGroundingAddendumMaxOutputTokens)
		assert.Equal(t, "v1beta1", resolved.WebGroundingAPIVersion)
		assert.Equal(t, 4, resolved.WebGroundingMaxResults)
		assert.True(t, resolved.RequireCitations)
	})

	t.Run("falls back to new defaults when config and options unset", func(t *testing.T) {
		resolved := (&Handler{}).resolveOptions(GenerateOptions{UseWebGrounding: true})
		assert.Equal(t, DefaultWebGroundingTimeout, resolved.WebGroundingTimeout)
		assert.Equal(t, DefaultWebGroundingAttemptTimeout, resolved.WebGroundingAttemptTimeout)
		assert.Equal(t, DefaultWebGroundingAddendumMaxOutputTokens, resolved.WebGroundingAddendumMaxOutputTokens)
		assert.Equal(t, DefaultWebGroundingAPIVersion, resolved.WebGroundingAPIVersion)
		assert.Equal(t, DefaultWebGroundingRetryMaxAttempts, 2)
	})
}

func TestComputeOutputBudgets(t *testing.T) {
	t.Run("applies thirty percent grounded budget", func(t *testing.T) {
		base, grounded := computeOutputBudgets(1000, 0.30)
		assert.Equal(t, 700, base)
		assert.Equal(t, 300, grounded)
	})

	t.Run("clamps invalid ratio to defaults", func(t *testing.T) {
		base, grounded := computeOutputBudgets(1000, 1.2)
		assert.Equal(t, 750, base)
		assert.Equal(t, 250, grounded)
	})
}

func TestGroundedAddendumBudgetCap(t *testing.T) {
	_, grounded := computeOutputBudgets(6144, 0.25)
	assert.Equal(t, 1536, grounded)
	assert.Equal(t, 512, minPositive(grounded, DefaultWebGroundingAddendumMaxOutputTokens))
}

func TestBuildGenerateContentConfigHTTPOptions(t *testing.T) {
	handler := &Handler{
		model: "google/gemini-3.5-flash",
		cfg: Config{
			SystemPrompt:    "system",
			MaxOutputTokens: 1024,
			Temperature:     0.8,
		},
	}

	groundedCfg, err := handler.buildGenerateContentConfig(512, true, "extra", "v1", 20*time.Second)
	assert.NoError(t, err)
	assert.NotNil(t, groundedCfg.HTTPOptions)
	assert.Equal(t, "v1", groundedCfg.HTTPOptions.APIVersion)
	assert.NotNil(t, groundedCfg.HTTPOptions.Timeout)
	assert.Equal(t, 20*time.Second, *groundedCfg.HTTPOptions.Timeout)
	assert.Len(t, groundedCfg.Tools, 1)
	assert.NotNil(t, groundedCfg.Tools[0].GoogleSearch)
	assert.Equal(t, int32(512), groundedCfg.MaxOutputTokens)

	standardCfg, err := handler.buildGenerateContentConfig(512, false, "", "v1", 20*time.Second)
	assert.NoError(t, err)
	assert.Nil(t, standardCfg.HTTPOptions)
	assert.Empty(t, standardCfg.Tools)
}

func TestExtractWebSources(t *testing.T) {
	resp := &googlegenai.GenerateContentResponse{
		Candidates: []*googlegenai.Candidate{
			{
				GroundingMetadata: &googlegenai.GroundingMetadata{
					GroundingChunks: []*googlegenai.GroundingChunk{
						{Web: &googlegenai.GroundingChunkWeb{Title: "Lavndr", URI: "https://lavndr.com"}},
						{Web: &googlegenai.GroundingChunkWeb{Title: "Duplicate", URI: "https://lavndr.com"}},
						{Web: &googlegenai.GroundingChunkWeb{Title: "", URI: "https://docs.example.com"}},
					},
				},
				CitationMetadata: &googlegenai.CitationMetadata{
					Citations: []*googlegenai.Citation{
						{Title: "News", URI: "https://news.example.com"},
					},
				},
			},
		},
	}

	t.Run("extracts and deduplicates sources", func(t *testing.T) {
		sources := extractWebSources(resp, 5)
		assert.Len(t, sources, 3)
		assert.Equal(t, "https://lavndr.com", sources[0].URL)
		assert.Equal(t, "https://docs.example.com", sources[1].URL)
		assert.Equal(t, "docs.example.com", sources[1].Title)
		assert.Equal(t, "https://news.example.com", sources[2].URL)
	})
}

func TestValidateCitationIndices(t *testing.T) {
	t.Run("accepts valid inline citations", func(t *testing.T) {
		err := validateCitationIndices("Here is a fact [1] and another [2].", 2)
		assert.NoError(t, err)
	})

	t.Run("rejects missing citations", func(t *testing.T) {
		err := validateCitationIndices("No citation markers here.", 2)
		assert.Error(t, err)
	})

	t.Run("rejects out-of-range citations", func(t *testing.T) {
		err := validateCitationIndices("Invalid citation [3].", 2)
		assert.Error(t, err)
	})
}

func TestIsRetryableGroundingError(t *testing.T) {
	t.Run("treats resource exhausted as retryable", func(t *testing.T) {
		err := errors.New("Error 429, Status: RESOURCE_EXHAUSTED")
		assert.True(t, isRetryableGroundingError(err))
	})

	t.Run("treats too many requests as retryable", func(t *testing.T) {
		err := errors.New("too many requests from upstream")
		assert.True(t, isRetryableGroundingError(err))
	})

	t.Run("treats deadline exceeded as retryable", func(t *testing.T) {
		err := errors.New("Error 504, Status: DEADLINE_EXCEEDED")
		assert.True(t, isRetryableGroundingError(err))
	})

	t.Run("does not retry context cancellation", func(t *testing.T) {
		assert.False(t, isRetryableGroundingError(context.Canceled))
		assert.False(t, isRetryableGroundingError(context.DeadlineExceeded))
	})

	t.Run("treats invalid argument as non-retryable", func(t *testing.T) {
		err := errors.New("Status: INVALID_ARGUMENT")
		assert.False(t, isRetryableGroundingError(err))
	})
}

func TestComputeRetryDelay(t *testing.T) {
	t.Run("applies exponential backoff and cap", func(t *testing.T) {
		delay1 := computeRetryDelay(1, 300*time.Millisecond, 3*time.Second, 0, 0.5)
		delay2 := computeRetryDelay(2, 300*time.Millisecond, 3*time.Second, 0, 0.5)
		delay3 := computeRetryDelay(3, 300*time.Millisecond, 3*time.Second, 0, 0.5)
		delay4 := computeRetryDelay(4, 300*time.Millisecond, 1*time.Second, 0, 0.5)

		assert.Equal(t, 300*time.Millisecond, delay1)
		assert.Equal(t, 600*time.Millisecond, delay2)
		assert.Equal(t, 1200*time.Millisecond, delay3)
		assert.Equal(t, 1*time.Second, delay4)
	})

	t.Run("applies jitter bounds", func(t *testing.T) {
		low := computeRetryDelay(1, 1*time.Second, 10*time.Second, 0.2, 0.0)
		high := computeRetryDelay(1, 1*time.Second, 10*time.Second, 0.2, 1.0)

		assert.Equal(t, 800*time.Millisecond, low)
		assert.Equal(t, 1200*time.Millisecond, high)
	})
}

func TestCallWithWebGroundingRetry(t *testing.T) {
	t.Run("retries on retryable error and succeeds", func(t *testing.T) {
		attempts := 0
		sleepCalls := 0
		handler := &Handler{
			cfg: Config{
				WebGroundingRetryMaxAttempts: 3,
				WebGroundingRetryBaseDelay:   100 * time.Millisecond,
				WebGroundingRetryMaxDelay:    500 * time.Millisecond,
				WebGroundingRetryJitter:      0,
			},
			sleepFn: func(_ context.Context, _ time.Duration) error {
				sleepCalls++
				return nil
			},
			jitterFn: func() float64 { return 0.5 },
		}

		resp, err := handler.callWithWebGroundingRetry(context.Background(), 0, func(_ context.Context) (*googlegenai.GenerateContentResponse, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("Error 429, Status: RESOURCE_EXHAUSTED")
			}
			return &googlegenai.GenerateContentResponse{}, nil
		})

		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, 2, attempts)
		assert.Equal(t, 1, sleepCalls)
	})

	t.Run("retries on 504 deadline exceeded and succeeds", func(t *testing.T) {
		attempts := 0
		sleepCalls := 0
		handler := &Handler{
			cfg: Config{
				WebGroundingRetryMaxAttempts: 3,
				WebGroundingRetryBaseDelay:   100 * time.Millisecond,
				WebGroundingRetryMaxDelay:    500 * time.Millisecond,
				WebGroundingRetryJitter:      0,
			},
			sleepFn: func(_ context.Context, _ time.Duration) error {
				sleepCalls++
				return nil
			},
			jitterFn: func() float64 { return 0.5 },
		}

		resp, err := handler.callWithWebGroundingRetry(context.Background(), 0, func(_ context.Context) (*googlegenai.GenerateContentResponse, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("Error 504, Message: Deadline expired before operation could complete., Status: DEADLINE_EXCEEDED")
			}
			return &googlegenai.GenerateContentResponse{}, nil
		})

		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, 2, attempts)
		assert.Equal(t, 1, sleepCalls)
	})

	t.Run("does not retry non-retryable errors", func(t *testing.T) {
		attempts := 0
		sleepCalls := 0
		handler := &Handler{
			cfg: Config{
				WebGroundingRetryMaxAttempts: 3,
			},
			sleepFn: func(_ context.Context, _ time.Duration) error {
				sleepCalls++
				return nil
			},
			jitterFn: func() float64 { return 0.5 },
		}

		_, err := handler.callWithWebGroundingRetry(context.Background(), 0, func(_ context.Context) (*googlegenai.GenerateContentResponse, error) {
			attempts++
			return nil, errors.New("Status: INVALID_ARGUMENT")
		})

		assert.Error(t, err)
		assert.Equal(t, 1, attempts)
		assert.Equal(t, 0, sleepCalls)
	})

	t.Run("returns last error when retries exhausted", func(t *testing.T) {
		attempts := 0
		sleepCalls := 0
		handler := &Handler{
			cfg: Config{
				WebGroundingRetryMaxAttempts: 2,
				WebGroundingRetryBaseDelay:   100 * time.Millisecond,
				WebGroundingRetryMaxDelay:    100 * time.Millisecond,
				WebGroundingRetryJitter:      0,
			},
			sleepFn: func(_ context.Context, _ time.Duration) error {
				sleepCalls++
				return nil
			},
			jitterFn: func() float64 { return 0.5 },
		}

		_, err := handler.callWithWebGroundingRetry(context.Background(), 0, func(_ context.Context) (*googlegenai.GenerateContentResponse, error) {
			attempts++
			return nil, errors.New("Error 429, Message: Resource exhausted")
		})

		assert.Error(t, err)
		assert.Equal(t, 2, attempts)
		assert.Equal(t, 1, sleepCalls)
	})

	t.Run("stops retrying when context is canceled during backoff", func(t *testing.T) {
		attempts := 0
		ctx, cancel := context.WithCancel(context.Background())
		handler := &Handler{
			cfg: Config{
				WebGroundingRetryMaxAttempts: 3,
				WebGroundingRetryBaseDelay:   100 * time.Millisecond,
				WebGroundingRetryMaxDelay:    100 * time.Millisecond,
				WebGroundingRetryJitter:      0,
			},
			sleepFn: func(ctx context.Context, _ time.Duration) error {
				cancel()
				<-ctx.Done()
				return ctx.Err()
			},
			jitterFn: func() float64 { return 0.5 },
		}

		_, err := handler.callWithWebGroundingRetry(ctx, 0, func(_ context.Context) (*googlegenai.GenerateContentResponse, error) {
			attempts++
			return nil, errors.New("Error 429, Status: RESOURCE_EXHAUSTED")
		})

		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, attempts)
	})

	t.Run("retries per-attempt timeout when parent has budget", func(t *testing.T) {
		attempts := 0
		handler := &Handler{
			cfg: Config{
				WebGroundingRetryMaxAttempts: 2,
				WebGroundingRetryBaseDelay:   time.Millisecond,
				WebGroundingRetryMaxDelay:    time.Millisecond,
				WebGroundingRetryJitter:      0,
			},
			sleepFn:  func(_ context.Context, _ time.Duration) error { return nil },
			jitterFn: func() float64 { return 0.5 },
		}

		resp, err := handler.callWithWebGroundingRetry(context.Background(), time.Millisecond, func(ctx context.Context) (*googlegenai.GenerateContentResponse, error) {
			attempts++
			if attempts == 1 {
				<-ctx.Done()
				return nil, errors.New("Error 499, Status: CANCELLED")
			}
			return &googlegenai.GenerateContentResponse{}, nil
		})

		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, 2, attempts)
	})

	t.Run("does not retry when parent context is canceled", func(t *testing.T) {
		attempts := 0
		handler := &Handler{
			cfg: Config{
				WebGroundingRetryMaxAttempts: 2,
				WebGroundingRetryBaseDelay:   time.Millisecond,
				WebGroundingRetryMaxDelay:    time.Millisecond,
				WebGroundingRetryJitter:      0,
			},
			sleepFn:  func(_ context.Context, _ time.Duration) error { return nil },
			jitterFn: func() float64 { return 0.5 },
		}
		ctx, cancel := context.WithCancel(context.Background())

		_, err := handler.callWithWebGroundingRetry(ctx, time.Second, func(_ context.Context) (*googlegenai.GenerateContentResponse, error) {
			attempts++
			cancel()
			return nil, errors.New("Error 499, Status: CANCELLED")
		})

		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, attempts)
	})
}
