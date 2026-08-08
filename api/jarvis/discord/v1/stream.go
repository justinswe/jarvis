package discordv1

import "time"

// The transport contract the ingestor and the worker share. It lives beside the protobuf
// definition because it is the same wire agreement: the message shape and the place that
// message is carried. Neither service may redeclare these.
//
// Only the names live here. How a broker is connected to belongs to the mq package —
// this one is the contract, not the transport.
const (
	// StreamName is the JetStream stream holding normalized Discord events.
	StreamName = "JARVIS_DISCORD"
	// Subject is the NATS subject normalized Discord events are published to.
	Subject = "jarvis.discord.v1.messages"
	// DurableName is the durable NATS consumer shared by every worker replica.
	DurableName = "jarvis-worker"

	// TopicName is the Pub/Sub topic normalized Discord events are published to.
	// Pub/Sub names may not contain the dots a NATS subject uses.
	TopicName = "jarvis-discord-v1-messages"
	// SubscriptionName is the Pub/Sub subscription shared by every worker replica.
	SubscriptionName = "jarvis-worker"

	// MaxMessageAge bounds how long an undelivered message stays in the stream.
	MaxMessageAge = time.Hour
)
