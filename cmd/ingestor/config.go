package main

import (
	"time"

	"github.com/spf13/cobra"
)

const shutdownTimeout = 5 * time.Second

type ingestorConfig struct {
	port, discordBotToken, workerURL string
	workerRequestTimeout             time.Duration
}

func newRootCommand() *cobra.Command {
	cfg := ingestorConfig{
		port:                 "8080",
		workerRequestTimeout: 75 * time.Second,
	}
	command := &cobra.Command{
		Use:   "ingestor",
		Short: "Starts the Jarvis Discord Gateway ingestor",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runIngestor(cmd.Context(), cfg)
		},
	}
	flags := command.Flags()
	flags.StringVar(&cfg.port, "port", cfg.port, "HTTP health server port")
	flags.StringVar(&cfg.discordBotToken, "discord-bot-token", cfg.discordBotToken, "Discord bot token")
	flags.StringVar(&cfg.workerURL, "worker-url", cfg.workerURL, "Worker HTTP processing endpoint")
	flags.DurationVar(&cfg.workerRequestTimeout, "worker-request-timeout", cfg.workerRequestTimeout, "Maximum time to wait for worker processing")
	return command
}
