package discordv1

import (
	"context"
	"time"

	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// The transport contract the ingestor and the worker share. It lives beside the
// protobuf definition because it is the same wire agreement: the message shape and
// the place that message is carried. Neither service may redeclare these.
const (
	// StreamName is the JetStream stream holding normalized Discord events.
	StreamName = "JARVIS_DISCORD"
	// Subject is the subject normalized Discord events are published to.
	Subject = "jarvis.discord.v1.messages"
	// DurableName is the durable consumer shared by every worker replica.
	DurableName = "jarvis-worker"
	// MaxMessageAge bounds how long an undelivered message stays in the stream.
	MaxMessageAge = time.Hour
)

// EnsureStream provisions the stream carrying normalized Discord events.
//
// Both services call this. The worker needs it before it can consume, and the
// ingestor needs it because it may reach the bus first: publishing to a subject no
// stream is bound to fails, and in a split deployment nothing else would have created
// it. The call is idempotent, so whichever service arrives first wins harmlessly.
//
// It is also authoritative, not merely creative: CreateOrUpdateStream applies the
// configuration below every time either service starts, so an operator's out-of-band
// change to retention or age is reverted on the next restart. Change it here.
func EnsureStream(ctx context.Context, js jetstream.JetStream, stream, subject string) error {
	if js == nil {
		return errors.New("JetStream context is required")
	}
	if stream == "" {
		return errors.New("stream name is required")
	}
	if subject == "" {
		return errors.New("subject is required")
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      stream,
		Subjects:  []string{subject},
		Retention: jetstream.WorkQueuePolicy,
		MaxAge:    MaxMessageAge,
	}); err != nil {
		return errors.Wrap(err, "provision JetStream stream")
	}
	return nil
}

// Connect opens a NATS connection that reconnects for the process lifetime.
func Connect(url, name string) (*nats.Conn, error) {
	if url == "" {
		return nil, errors.New("NATS URL is required")
	}
	connection, err := nats.Connect(url,
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			app.L().Warn("Disconnected from NATS", zap.Error(err))
		}),
		nats.ReconnectHandler(func(conn *nats.Conn) {
			app.L().Info("Reconnected to NATS", zap.String("url", conn.ConnectedUrl()))
		}),
	)
	if err != nil {
		return nil, errors.Wrap(err, "connect to NATS")
	}
	return connection, nil
}

// Drain flushes pending NATS traffic and closes the connection.
func Drain(connection *nats.Conn) {
	if connection == nil {
		return
	}
	if err := connection.Drain(); err != nil {
		app.L().Warn("Draining the NATS connection failed", zap.Error(err))
	}
}
