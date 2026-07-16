package discord

import (
	"context"
	"testing"
	"time"

	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeContextReturnsVersionAndUTCClock(t *testing.T) {
	tool := runtimeContextTool{
		version: " v0.6.0 ",
		now: func() time.Time {
			return time.Date(2026, time.July, 16, 18, 30, 45, 0, time.UTC)
		},
	}

	result, err := tool.Execute(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, runtimeContextResponse{
		AssistantName: "Jarvis",
		Version:       "v0.6.0",
		Timezone:      "UTC",
		CurrentTime:   "2026-07-16T18:30:45Z",
		CurrentDate:   "2026-07-16",
		Weekday:       "Thursday",
	}, result)
}

func TestRuntimeContextConvertsToRequestedIANATimezone(t *testing.T) {
	tool := runtimeContextTool{now: func() time.Time {
		return time.Date(2026, time.July, 16, 2, 30, 0, 0, time.UTC)
	}}

	result, err := tool.Execute(context.Background(), map[string]any{"timezone": "America/Los_Angeles"})
	require.NoError(t, err)
	response := result.(runtimeContextResponse)
	assert.Equal(t, "America/Los_Angeles", response.Timezone)
	assert.Equal(t, "2026-07-15T19:30:00-07:00", response.CurrentTime)
	assert.Equal(t, "2026-07-15", response.CurrentDate)
	assert.Equal(t, "Wednesday", response.Weekday)
}

func TestRuntimeContextRejectsInvalidTimezone(t *testing.T) {
	tool := runtimeContextTool{}
	for _, args := range []map[string]any{{"timezone": 1}, {"timezone": "not/a-zone"}} {
		_, err := tool.Execute(context.Background(), args)
		var executionErr *genai.ExecutionError
		require.ErrorAs(t, err, &executionErr)
		assert.Equal(t, "invalid_timezone", executionErr.Code)
		assert.NotContains(t, executionErr.Message, "not/a-zone")
	}
}

func TestRuntimeContextDeclarationMakesTimezoneOptional(t *testing.T) {
	declaration := (runtimeContextTool{}).Declaration()
	assert.Equal(t, runtimeContextToolName, declaration.Name)
	assert.Contains(t, declaration.Description, "Do not call or mention its output in unrelated answers")
	require.Contains(t, declaration.Parameters.Properties, "timezone")
	assert.Empty(t, declaration.Parameters.Required)
}
