package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/justinswe/jarvis/internal/ingestor"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

func runIngestor(parent context.Context, cfg ingestorConfig) error {
	if cfg.discordBotToken == "" {
		return errors.New("discord bot token is required")
	}
	if cfg.workerURL == "" {
		return errors.New("worker URL is required")
	}
	if cfg.workerRequestTimeout <= 0 {
		return errors.New("worker request timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	publisher, err := ingestor.NewHTTPPublisher(cfg.workerURL, &http.Client{Timeout: cfg.workerRequestTimeout})
	if err != nil {
		return errors.Wrap(err, "initialize worker publisher")
	}
	gateway, err := ingestor.New(cfg.discordBotToken, publisher)
	if err != nil {
		return errors.Wrap(err, "initialize Discord ingestor")
	}
	app.L().Info("Starting ingestor", zap.String("port", cfg.port), zap.String("worker_url", cfg.workerURL))
	return serve(ctx, cfg.port, gateway)
}
