package main

import (
	"time"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/mq"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

type ingestorConfig struct {
	port, discordBotToken        string
	mqDriver                     string
	natsURL                      string
	natsStream, natsSubject      string
	pubsubProjectID, pubsubTopic string
	publishTimeout               time.Duration
}

func newRootCommand() *cobra.Command {
	cfg := ingestorConfig{
		port:           "8080",
		mqDriver:       string(mq.DriverNATS),
		natsURL:        nats.DefaultURL,
		natsStream:     discordv1.StreamName,
		natsSubject:    discordv1.Subject,
		pubsubTopic:    discordv1.TopicName,
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
	flags.StringVar(&cfg.mqDriver, "mq-driver", cfg.mqDriver, "Message queue broker: nats or pubsub")
	flags.StringVar(&cfg.natsURL, "nats-url", cfg.natsURL, "NATS server URL")
	flags.StringVar(&cfg.natsStream, "nats-stream", cfg.natsStream, "JetStream stream holding normalized Discord events")
	flags.StringVar(&cfg.natsSubject, "nats-subject", cfg.natsSubject, "Subject normalized Discord events are published to")
	flags.StringVar(&cfg.pubsubProjectID, "pubsub-project-id", cfg.pubsubProjectID, "GCP project owning the Pub/Sub topic")
	flags.StringVar(&cfg.pubsubTopic, "pubsub-topic", cfg.pubsubTopic, "Pub/Sub topic normalized Discord events are published to")
	flags.DurationVar(&cfg.publishTimeout, "publish-timeout", cfg.publishTimeout, "Maximum time to wait for the broker to store one message")
	return command
}

// validate reports whether the ingestor can start.
func (cfg ingestorConfig) validate() error {
	if cfg.discordBotToken == "" {
		return errors.New("discord bot token is required")
	}
	if cfg.port == "" {
		return errors.New("port is required")
	}
	if cfg.publishTimeout <= 0 {
		return errors.New("publish timeout must be positive")
	}
	if !mq.Driver(cfg.mqDriver).Valid() {
		return errors.Errorf("unsupported message queue driver %q", cfg.mqDriver)
	}
	return nil
}

// queueConfig maps the operator's flags onto the broker the driver selects.
func (cfg ingestorConfig) queueConfig() mq.Config {
	queue := mq.Config{
		Driver:     mq.Driver(cfg.mqDriver),
		ClientName: "jarvis-ingestor",
	}
	if queue.Driver == mq.DriverPubSub {
		queue.ProjectID, queue.Topic = cfg.pubsubProjectID, cfg.pubsubTopic
		return queue
	}
	queue.URL, queue.Stream, queue.Topic = cfg.natsURL, cfg.natsStream, cfg.natsSubject
	return queue
}
