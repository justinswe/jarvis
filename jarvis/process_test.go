package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestChildLogLevelsLinesByTag(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantLevel   zapcore.Level
		wantMessage string
	}{
		{
			name:        "info",
			line:        `[16] 2026/08/12 16:31:27.425364 [INF] Server is ready`,
			wantLevel:   zapcore.InfoLevel,
			wantMessage: "Server is ready",
		},
		{
			name:        "warn",
			line:        `[16] 2026/08/12 16:31:27.425364 [WRN] Filestore is at 90%`,
			wantLevel:   zapcore.WarnLevel,
			wantMessage: "Filestore is at 90%",
		},
		{
			name:        "error",
			line:        `[16] 2026/08/12 16:31:27.425364 [ERR] Stream create failed`,
			wantLevel:   zapcore.ErrorLevel,
			wantMessage: "Stream create failed",
		},
		{
			name:        "fatal is an error, not a crash",
			line:        `[16] 2026/08/12 16:31:27.425364 [FTL] Could not bind 4222`,
			wantLevel:   zapcore.ErrorLevel,
			wantMessage: "Could not bind 4222",
		},
		{
			// A panic or a start-up failure carries no tag and must not be filed as info.
			name:        "untagged",
			line:        "panic: runtime error: invalid memory address",
			wantLevel:   zapcore.ErrorLevel,
			wantMessage: "panic: runtime error: invalid memory address",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs := captureLogs(t)
			writer := &childLog{name: "nats-server"}

			_, err := writer.Write([]byte(test.line + "\n"))
			require.NoError(t, err)

			entries := logs.All()
			require.Len(t, entries, 1)
			assert.Equal(t, test.wantLevel, entries[0].Level)
			assert.Equal(t, test.wantMessage, entries[0].Message)
			assert.Equal(t, "nats-server", entries[0].ContextMap()["child"])
		})
	}
}

func TestChildLogBuffersPartialLines(t *testing.T) {
	logs := captureLogs(t)
	writer := &childLog{name: "nats-server"}

	// os/exec copies through a 32KiB buffer, so a line can arrive in pieces and
	// several lines can arrive at once.
	_, err := writer.Write([]byte("[16] 2026/08/12 16:31:27.425364 [INF] Server"))
	require.NoError(t, err)
	assert.Empty(t, logs.All(), "an unterminated line must not be emitted")

	_, err = writer.Write([]byte(" is ready\n[16] 2026/08/12 16:31:28.000000 [WRN] Slow consumer\n"))
	require.NoError(t, err)

	entries := logs.All()
	require.Len(t, entries, 2)
	assert.Equal(t, zapcore.InfoLevel, entries[0].Level)
	assert.Equal(t, "Server is ready", entries[0].Message)
	assert.Equal(t, zapcore.WarnLevel, entries[1].Level)
	assert.Equal(t, "Slow consumer", entries[1].Message)
}

func captureLogs(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	undo := zap.ReplaceGlobals(zap.New(core))
	t.Cleanup(undo)
	return logs
}
