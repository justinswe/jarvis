package main

import (
	"time"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

type ingestorConfig struct {
	port, discordBotToken, natsURL string
	natsStream, natsSubject        string
	publishTimeout                 time.Duration
}

func newRootCommand() *cobra.Command {
	cfg := ingestorConfig{
		port:           "8080",
		natsURL:        nats.DefaultURL,
		natsStream:     discordv1.StreamName,
		natsSubject:    discordv1.Subject,
		publishTimeout: 5 * time.Second,
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
	flags.StringVar(&cfg.natsURL, "nats-url", cfg.natsURL, "NATS server URL")
	flags.StringVar(&cfg.natsStream, "nats-stream", cfg.natsStream, "JetStream stream holding normalized Discord events")
	flags.StringVar(&cfg.natsSubject, "nats-subject", cfg.natsSubject, "Subject normalized Discord events are published to")
	flags.DurationVar(&cfg.publishTimeout, "publish-timeout", cfg.publishTimeout, "Maximum time to wait for the broker to store one message")
	return command
}
