package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/ingestor/pkg/gateway"
	"github.com/justinswe/jarvis/ingestor/pkg/publisher"
	"github.com/justinswe/jarvis/ingestor/pkg/server"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

func runIngestor(parent context.Context, cfg ingestorConfig) error {
	if cfg.discordBotToken == "" {
		return errors.New("discord bot token is required")
	}
	if cfg.natsURL == "" {
		return errors.New("NATS URL is required")
	}
	if cfg.natsSubject == "" {
		return errors.New("NATS subject is required")
	}
	if cfg.natsStream == "" {
		return errors.New("NATS stream is required")
	}
	if cfg.publishTimeout <= 0 {
		return errors.New("publish timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	connection, err := discordv1.Connect(cfg.natsURL, "jarvis-ingestor")
	if err != nil {
		return err
	}
	defer discordv1.Drain(connection)
	stream, err := jetstream.New(connection)
	if err != nil {
		return errors.Wrap(err, "initialize JetStream")
	}
	// The ingestor may reach the bus before any worker has: publishing to a subject
	// no stream is bound to fails, so provision it here rather than dropping every
	// message until a worker happens to start.
	if err := discordv1.EnsureStream(ctx, stream, cfg.natsStream, cfg.natsSubject); err != nil {
		return err
	}

	messages, err := publisher.New(stream, cfg.natsSubject, cfg.publishTimeout)
	if err != nil {
		return errors.Wrap(err, "initialize worker publisher")
	}
	service, err := gateway.New(cfg.discordBotToken, messages)
	if err != nil {
		return errors.Wrap(err, "initialize Discord ingestor")
	}
	app.L().Info("Starting ingestor",
		zap.String("port", cfg.port),
		zap.String("nats_url", cfg.natsURL),
		zap.String("stream", cfg.natsStream),
		zap.String("subject", cfg.natsSubject),
	)
	return server.Serve(ctx, cfg.port, service)
}
