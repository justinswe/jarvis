# Active-active multi-site deployment

Jarvis can run in two places at once — an on-premises cluster and a cloud region — with both sites
serving Discord continuously. Losing either one causes no interruption: there is no lease to expire,
no leader to elect, and nothing to switch. This document is what makes that safe.

```text
   on-premises                                   cloud region
   ingestor ──┐                                  ┌── ingestor
              │        both always connected     │
              └──> GCP Pub/Sub topic (global) <──┘
                   jarvis-discord-v1-messages
                            │
                    one subscription: jarvis-worker
                            │
              ┌─────────────┴─────────────┐
        on-premises workers          cloud workers
              └──────────┬────────────────┘
                         │ DynamoDB conditional write
                         │ CHANNEL#<channel> / REPLY#<message>
                         └──> exactly one worker answers
```

## Why there is no leader election

Discord does **not** enforce one Gateway connection per bot token. Two ingestors using the same
token both identify as the unsharded bot, both are accepted, and both receive a full copy of every
`MessageCreate`. Nothing evicts the older session.

That is usually described as a hazard. Here it is the failover mechanism: because both sites are
already connected, a site can disappear without anything having to notice or react. The cost is that
every Discord message is published twice, which is one extra Pub/Sub message and one extra
conditional write — both inside free tiers at this volume.

What it requires is that only one worker answers.

## The reply claim

Before a worker generates anything, it takes a claim on the message with a conditional write:

```text
pk         CHANNEL#<channel_id>
sk         REPLY#<message_id>
owner      <hostname>/<pid>
expires_at <now + MQ_ACK_WAIT>

condition  attribute_not_exists(pk) OR expires_at <= :now
```

The loser returns immediately and acknowledges. It never calls a model, so a duplicate costs one
DynamoDB write rather than a whole generation.

Once the reply has actually been posted the winner rewrites the same item unconditionally with
`expires_at = now + 1h`, matching the queue's own message retention. That second write is what makes
the guarantee hold: see below.

Three details carry the design:

- **The claim is taken before generation, not before posting.** That is what keeps the duplicate
  cheap. It only works because the claim expires.
- **`expires_at` starts at `MQ_ACK_WAIT`**, the same value that governs redelivery. A worker that
  dies mid-generation leaves a claim behind; the redelivery that follows can only answer once that
  claim has lapsed. Set it longer than the redelivery delay and every attempt is refused until the
  message is exhausted, turning a crash into a lost reply. The two are deliberately the same knob —
  `SetReplyClaimTTL` is called with `--mq-ack-wait` and nothing else.
- **A posted reply extends the claim past every copy that could still arrive.** A short claim alone
  is not enough: a generation can run for minutes, so a duplicate delayed past `MQ_ACK_WAIT` — held
  behind a full `--mq-max-in-flight` budget at the other site, or redelivered there after a
  transient failure — would find the claim lapsed and answer a second time. Extending on success
  closes that window without making a crash any more expensive, because a crash never reaches the
  extension.
- **The condition turns on the message alone, never on the owner.** One subscription is shared by
  the workers at every site, so the two copies of a message are load-balanced independently and can
  both land on the same process. An `owner = :owner` arm would let the second copy match the claim
  the first one just took, and answer twice. A process does not need one to retake a message
  redelivered to itself: that redelivery arrives at least one `MQ_ACK_WAIT` after the claim, which
  `expires_at` already admits.

Claims share the channel partition with that channel's stored messages, so they expire under the
table's existing TTL and need no new table, index, or cleanup.

### DynamoDB is mandatory for multi-site

With `DYNAMODB_ENABLED=false` there is no shared store, so there is no claim. Two ingestors then
produce two replies to every message. A single-site deployment is unaffected — one Gateway
connection means nothing to deduplicate.

### The claim fails open

If DynamoDB is unreachable the claim is treated as won and the reply is sent, matching how every
other shared dependency on this path degrades. The consequence is that an outage of the DynamoDB
region can produce duplicate replies while both sites are up. That is the deliberate trade: a
duplicate reply beats no reply.

## Why Pub/Sub

A Pub/Sub topic is global. There is no regional broker to fail over between, and messages queued by
a site that then dies are still there for the other site to pick up. Workers at both sites hold open
streams against one subscription, so Pub/Sub load-balances between them and redelivers anything an
unresponsive worker was holding — the whole worker failover story, with no code.

NATS remains the default and is the right choice for a single site: the combined image bundles it,
it needs no cloud account, and it keeps working with no internet. What it cannot do is span sites
without a stretched cluster to operate.

See [pubsub.md](pubsub.md) for provisioning, IAM, and the full driver comparison.

## What each failure does

| Failure | What happens |
| --- | --- |
| One site lost entirely | The other is already connected and publishing. Messages its workers held un-acknowledged are redelivered by Pub/Sub. No interruption. |
| One worker crashes mid-message | The message is redelivered; its claim lapses after `MQ_ACK_WAIT`, so the redelivery can answer. The crash happened before the claim was extended, which is what makes that possible. |
| Both sites healthy | Every message is published twice, both copies reach a worker, and the conditional write picks one. The loser acks without generating. |
| DynamoDB region lost | Claims fail open. Replies continue, possibly duplicated, until it returns. |
| A message can never succeed | Terminated after `MQ_MAX_DELIVER` attempts. On Pub/Sub it lands in the dead-letter topic; an unparseable payload is acknowledged and only logged. |

## Known ceilings

1. **Thread latest-message-wins is per-process.** `worker/pkg/discord/queue.go` is an in-memory map,
   so two messages in one thread that land at different sites are not ordered against each other.
   This was already true of multiple replicas; active-active makes it the normal case rather than an
   edge case. Upgrade path: Pub/Sub ordering keys on `channel_id`, noting that ordering guarantees
   weaken with publishers in two locations.
2. **DynamoDB is single-region.** Losing it fails claims open, as above. Upgrade path: Global Tables.
3. **Pub/Sub has no publisher deduplication.** The reply claim is the only thing preventing duplicate
   answers; a defect there produces duplicates, not losses.
4. **`Term` on Pub/Sub is an acknowledgement.** Unparseable messages are dropped with a warning and
   never reach the dead-letter topic — only repeated negative acknowledgements do.
5. **Valkey metering is per-site** unless both sites reach one instance. A site running with
   `VALKEY_ENABLED=false` serves unmetered requests, which is already the fail-open behaviour when
   Valkey is unreachable.
6. **On-premises AWS federation needs a metadata server.** `awsidentity` mints its Google identity
   token through the compute metadata server, which exists on GCE and Cloud Run but not in an
   on-premises cluster. Until that path supports service-account impersonation, the on-premises site
   reaches DynamoDB through the AWS SDK's ordinary credential chain instead.

## Verifying

Run two containers against one topic and one table, then mention the bot once:

```sh
docker run --env-file .env.site-a -e MQ_DRIVER=pubsub -e DYNAMODB_ENABLED=true justinswe/jarvis:0.9.0
docker run --env-file .env.site-b -e MQ_DRIVER=pubsub -e DYNAMODB_ENABLED=true justinswe/jarvis:0.9.0
```

- Exactly one reply appears — the claim worked.
- Both containers log the `MessageCreate` — both Gateway connections really are live.
- One logs `Another worker claimed this reply`.
- The claim item is readable, with the winner's owner and — because the reply has been posted and
  the claim extended — an `expires_at` about an hour out rather than `MQ_ACK_WAIT`. Reading it
  mid-generation instead shows the short one:

```sh
aws dynamodb get-item --table-name jarvis \
  --key '{"pk":{"S":"CHANNEL#<channel>"},"sk":{"S":"REPLY#<message>"}}'
```

Then kill whichever container won and mention the bot again. The survivor answers with no gap, which
is the property the whole design exists for.
