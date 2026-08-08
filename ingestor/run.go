package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/justinswe/jarvis/ingestor/pkg/gateway"
	"github.com/justinswe/jarvis/ingestor/pkg/publisher"
	"github.com/justinswe/jarvis/ingestor/pkg/server"
	"github.com/justinswe/jarvis/mq"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

func runIngestor(parent context.Context, cfg ingestorConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	queue := cfg.queueConfig()
	publishing, err := mq.OpenPublisher(ctx, queue)
	if err != nil {
		return err
	}
	defer publishing.Close()

	messages, err := publisher.New(publishing, cfg.publishTimeout)
	if err != nil {
		return errors.Wrap(err, "initialize worker publisher")
	}
	service, err := gateway.New(cfg.discordBotToken, messages)
	if err != nil {
		return errors.Wrap(err, "initialize Discord ingestor")
	}
	app.L().Info("Starting ingestor",
		zap.String("port", cfg.port),
		zap.String("mq_driver", cfg.mqDriver),
		zap.String("topic", queue.Topic),
	)
	return server.Serve(ctx, cfg.port, service)
}
