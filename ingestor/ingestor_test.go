package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngestorConfigDefaults(t *testing.T) {
	command := newRootCommand()
	port, err := command.Flags().GetString("port")
	require.NoError(t, err)
	natsURL, err := command.Flags().GetString("nats-url")
	require.NoError(t, err)
	subject, err := command.Flags().GetString("nats-subject")
	require.NoError(t, err)
	timeout, err := command.Flags().GetDuration("publish-timeout")
	require.NoError(t, err)
	assert.Equal(t, "8080", port)
	assert.Equal(t, "nats://127.0.0.1:4222", natsURL)
	assert.Equal(t, "jarvis.discord.v1.messages", subject)
	assert.Equal(t, 5*time.Second, timeout)
}
