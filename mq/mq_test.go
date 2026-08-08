package mq

import (
	"testing"
	"time"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func natsConfig() Config {
	return Config{
		Driver: DriverNATS, ClientName: "test",
		URL: "nats://127.0.0.1:4222", Stream: discordv1.StreamName,
		Topic: discordv1.Subject, Consumer: discordv1.DurableName,
		AckWait: time.Second, MaxProcessingTime: time.Minute, DrainTimeout: time.Second,
		MaxDeliver: 5, MaxInFlight: 4,
	}
}

func pubSubConfig() Config {
	return Config{
		Driver: DriverPubSub, ClientName: "test",
		ProjectID: "project", Topic: discordv1.TopicName, Consumer: discordv1.SubscriptionName,
		AckWait: 30 * time.Second, MaxProcessingTime: time.Minute, DrainTimeout: time.Second,
		MaxDeliver: 5, MaxInFlight: 4,
	}
}

func TestDriverValid(t *testing.T) {
	assert.True(t, DriverNATS.Valid())
	assert.True(t, DriverPubSub.Valid())
	assert.False(t, Driver("").Valid())
	assert.False(t, Driver("kafka").Valid())
}

func TestSubscriberValidation(t *testing.T) {
	for _, valid := range []Config{natsConfig(), pubSubConfig()} {
		require.NoError(t, valid.validateSubscriber(), valid.Driver)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"driver", func(c *Config) { c.Driver = "kafka" }},
		{"topic", func(c *Config) { c.Topic = "" }},
		{"consumer", func(c *Config) { c.Consumer = "" }},
		{"ack wait", func(c *Config) { c.AckWait = 0 }},
		{"max processing time", func(c *Config) { c.MaxProcessingTime = 0 }},
		{"drain timeout", func(c *Config) { c.DrainTimeout = 0 }},
		{"max deliver", func(c *Config) { c.MaxDeliver = 0 }},
		{"max in flight", func(c *Config) { c.MaxInFlight = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := natsConfig()
			test.mutate(&cfg)
			assert.Error(t, cfg.validateSubscriber())
		})
	}
}

// TestPubSubRejectsADeliveryCapItCannotExpress covers a limit that exists on one broker
// and not the other: Pub/Sub will not accept a dead-letter policy below five attempts, so
// a configuration that is fine on JetStream must fail at startup here rather than leave
// the subscription retrying forever.
func TestPubSubRejectsADeliveryPolicyItCannotExpress(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"one attempt", func(c *Config) { c.MaxDeliver = 1 }},
		{"below the attempt floor", func(c *Config) { c.MaxDeliver = 4 }},
		{"above the attempt ceiling", func(c *Config) { c.MaxDeliver = 101 }},
		{"below the ack deadline floor", func(c *Config) { c.AckWait = 2 * time.Second }},
		{"above the ack deadline ceiling", func(c *Config) { c.AckWait = 11 * time.Minute }},
	} {
		t.Run(test.name, func(t *testing.T) {
			onPubSub := pubSubConfig()
			test.mutate(&onPubSub)
			assert.Error(t, onPubSub.validateSubscriber())

			onNATS := natsConfig()
			test.mutate(&onNATS)
			assert.NoError(t, onNATS.validateSubscriber(), "JetStream has no such limits")
		})
	}
}

// TestValidationRejectsFieldsTheOtherDriverNeeds is what stops a half-configured switch
// between brokers from failing at the first publish instead of at startup.
func TestValidationRejectsFieldsTheOtherDriverNeeds(t *testing.T) {
	for _, test := range []struct {
		name   string
		cfg    Config
		mutate func(*Config)
	}{
		{"NATS URL", natsConfig(), func(c *Config) { c.URL = "" }},
		{"NATS stream", natsConfig(), func(c *Config) { c.Stream = "" }},
		{"Pub/Sub project", pubSubConfig(), func(c *Config) { c.ProjectID = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := test.cfg
			test.mutate(&cfg)
			assert.Error(t, cfg.validateSubscriber())
		})
	}
}

// TestPublisherValidationIgnoresConsumerSettings keeps the ingestor from having to carry
// delivery policy it never uses.
func TestPublisherValidationIgnoresConsumerSettings(t *testing.T) {
	for _, valid := range []Config{natsConfig(), pubSubConfig()} {
		cfg := valid
		cfg.Consumer, cfg.AckWait, cfg.MaxDeliver, cfg.MaxInFlight = "", 0, 0, 0
		cfg.MaxProcessingTime, cfg.DrainTimeout = 0, 0
		assert.NoError(t, cfg.validatePublisher(), cfg.Driver)
	}
}
