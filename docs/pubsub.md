# GCP Pub/Sub transport

Jarvis carries normalized Discord events over one of two brokers, selected by `MQ_DRIVER`.
NATS JetStream is the default and runs the combined image, local development, and any single-site
deployment that must keep working without the internet. GCP Pub/Sub is the shared bus for a
multi-site deployment: a Pub/Sub topic is global, so there is no regional bus to fail over between
and the queue survives the loss of a whole site.

The abstraction lives in the `mq` package. Both the ingestor and the worker are written against
delivery semantics rather than against either client, so switching brokers is configuration.

## Runtime configuration

Every flag is also available through the app package's uppercase environment-variable mapping.

| Flag | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `--mq-driver` | `MQ_DRIVER` | `nats` | Broker to use: `nats` or `pubsub`. |
| `--pubsub-project-id` | `PUBSUB_PROJECT_ID` | empty | GCP project owning the topic and subscription. Required when the driver is `pubsub`. |
| `--pubsub-topic` | `PUBSUB_TOPIC` | `jarvis-discord-v1-messages` | Topic normalized Discord events are published to. |
| `--pubsub-subscription` | `PUBSUB_SUBSCRIPTION` | `jarvis-worker` | Subscription shared by every worker replica. Worker only. |

The delivery policy below is broker-neutral and applies to both drivers.

| Flag | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `--mq-ack-wait` | `MQ_ACK_WAIT` | `30s` | How long a held message may go without progress before redelivery, and how long a failed message waits before its retry. |
| `--mq-max-processing-time` | `MQ_MAX_PROCESSING_TIME` | `10m` | How long one message may be held in total. Set it above the largest `message_timeout_seconds` any server can configure. |
| `--mq-drain-timeout` | `MQ_DRAIN_TIMEOUT` | `20s` | How long shutdown lets in-flight messages finish before cancelling them. Must fit the platform's grace period. |
| `--mq-max-deliver` | `MQ_MAX_DELIVER` | `5` | Maximum delivery attempts per message. |
| `--mq-max-in-flight` | `MQ_MAX_IN_FLIGHT` | `4` | Maximum messages held un-acknowledged at once, per replica. |

These replace the former `NATS_ACK_WAIT`, `NATS_MAX_DELIVER`, `NATS_MAX_ACK_PENDING`, and
`NATS_DRAIN_TIMEOUT`. `NATS_URL`, `NATS_STREAM`, `NATS_SUBJECT`, and `NATS_DURABLE` are unchanged —
they name a NATS deployment specifically, where the settings above are policy either broker honors.

### Limits Pub/Sub imposes and NATS does not

Pub/Sub rejects a subscription outside these ranges, so Jarvis validates them at startup rather than
letting the first message fail:

- `--mq-max-deliver` must be between **5 and 100**. JetStream has no floor. This is why the default
  is 5 rather than the 3 it used to be — one value that is legal on both brokers.
- `--mq-ack-wait` must be between **10s and 600s**. JetStream has no floor.

## Provisioning

**Jarvis does not create Pub/Sub resources.** It verifies they exist and fails fast, naming the
command that fixes it. This is deliberate and differs from the NATS driver, which creates and
reasserts its stream on every start:

- creating topics needs administrative IAM the running service should not hold, and
- a mistyped subscription name would otherwise silently create a second subscription nobody
  publishes to — an outage that looks exactly like an empty queue.

```sh
PROJECT=justin-dev-00

gcloud pubsub topics create jarvis-discord-v1-messages --project="${PROJECT}"
gcloud pubsub topics create jarvis-discord-v1-messages-dead-letter --project="${PROJECT}"

gcloud pubsub subscriptions create jarvis-worker \
  --project="${PROJECT}" \
  --topic=jarvis-discord-v1-messages \
  --ack-deadline=30 \
  --message-retention-duration=1h \
  --min-retry-delay=30s \
  --max-retry-delay=30s \
  --dead-letter-topic=jarvis-discord-v1-messages-dead-letter \
  --max-delivery-attempts=5
```

The subscription is the source of truth for retry and dead-lettering. Unlike JetStream, where the
worker asserts the consumer's configuration on every start, nothing in Jarvis can correct a
subscription that drifts from these values — keep them matched to the flags by hand.

| Subscription setting | Matching flag |
| --- | --- |
| `--ack-deadline` | `--mq-ack-wait` |
| `--min-retry-delay` / `--max-retry-delay` | `--mq-ack-wait` |
| `--max-delivery-attempts` | `--mq-max-deliver` |
| `--message-retention-duration` | fixed at one hour, matching `discordv1.MaxMessageAge` |

### IAM

| Principal | Role |
| --- | --- |
| Ingestor | `roles/pubsub.publisher` on the topic |
| Worker | `roles/pubsub.subscriber` on the subscription |

Dead-lettering additionally needs the Pub/Sub service agent to hold `roles/pubsub.publisher` on the
dead-letter topic and `roles/pubsub.subscriber` on the subscription.

### Authentication

| Site | How Pub/Sub is reached |
| --- | --- |
| GCP (Cloud Run, GCE) | The attached service account, through Application Default Credentials. Nothing to configure. |
| On-premises Kubernetes | A workload identity pool federating the cluster's projected service-account tokens. Mount the external-account credential configuration and point `GOOGLE_APPLICATION_CREDENTIALS` at it; ADC does the rest. No Jarvis configuration and no exported keys. |

## How the drivers differ

The `mq` package exposes only what both brokers can honor. Everything below is absorbed by a driver
rather than surfaced to the ingestor or the worker.

| `mq` | NATS JetStream | GCP Pub/Sub |
| --- | --- | --- |
| `Publish(dedupeID)` | `WithMsgID`; the broker drops a duplicate inside the stream's duplicate window | an attribute only — **Pub/Sub has no publisher-side deduplication** |
| `Ack` | `Ack` | `Ack` |
| `Nak` | `NakWithDelay(MQ_ACK_WAIT)` | `Nack`, then the subscription's retry policy |
| `Term` | `Term` | `Ack`, plus a warning; Pub/Sub has no terminate, so the message leaves no dead-letter trace |
| keepalive | `InProgress` every `MQ_ACK_WAIT`/2, capped at `MQ_MAX_PROCESSING_TIME` | the client extends the deadline up to `MQ_MAX_PROCESSING_TIME` |
| provisioning | created and kept authoritative on every start | verified only |

The deduplication row is the one that changes how a deployment must be built. On NATS, republishing
a Discord message queues one reply. On Pub/Sub it queues two. A multi-site deployment therefore
cannot lean on the broker for exactly-once replies — see [failover](failover.md).

## Verifying

`//worker/pkg/consumer:mq_live` runs the same suite against both brokers and is the thing that
proves they agree. It skips whichever broker is not configured.

```sh
# NATS
bazel run //:nats_server -- -js -sd /tmp/jsdata -p 14222 &

# Pub/Sub emulator
docker run -d --rm --name jarvis-pubsub-emulator -p 18085:8085 \
  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
  gcloud beta emulators pubsub start --project=justin-dev-00 --host-port=0.0.0.0:8085

bazel test //worker/pkg/consumer:mq_live --test_output=all --test_timeout=1200 \
  --test_env=NATS_URL=nats://127.0.0.1:14222 \
  --test_env=PUBSUB_PROJECT_ID=justin-dev-00 \
  --test_env=PUBSUB_EMULATOR_HOST=127.0.0.1:18085
```

The suite provisions its own per-run topic, dead-letter topic, and subscription, because the driver
refuses to; what it creates mirrors the `gcloud` commands above.
