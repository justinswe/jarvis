# Storage: PostgreSQL and SQLite

Jarvis can persist bot-involved conversation, per-guild configuration, and reply claims in one SQL
database. Two backends run the same implementation and the same schema:

| Driver | For | HA |
| --- | --- | --- |
| `postgres` | Shared deployments; **required for multi-site** (see [failover.md](failover.md)) | As HA as the PostgreSQL behind the DSN — managed service or Patroni; Jarvis needs only a connection string |
| `sqlite` | Single-site, zero-infrastructure deployments | Deliberately none. The database is a file inside one site; nothing can fail over to it |

The integration belongs only to the worker; the ingestor and protobuf transport are unchanged.

## Runtime configuration

The store is disabled by default. Every flag is also available through the app package's uppercase
environment-variable mapping.

| Flag | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `--store-driver` | `STORE_DRIVER` | `none` | `none`, `postgres`, or `sqlite`. |
| `--postgres-dsn` | `POSTGRES_DSN` | empty | Connection string, e.g. `postgres://user:pass@host:5432/jarvis?sslmode=require`. |
| `--sqlite-path` | `SQLITE_PATH` | empty | Database file path. Put it on a volume; WAL journaling is enabled automatically. |
| `--store-sweep-interval` | `STORE_SWEEP_INTERVAL` | `1h` | How often expired messages and lapsed reply claims are deleted. |
| `--mcp-encryption-key` | `MCP_ENCRYPTION_KEY` | empty | 64 hex chars (32 bytes) — the AES-256 key sealing guild MCP auth tokens at rest. Required to attach an MCP server with a token. |
| `--message-retention-days` | `MESSAGE_RETENTION_DAYS` | `14` | Default retention for newly recorded messages. |
| `--root-user-ids` | `ROOT_USER_IDS` | empty | Discord user IDs with cross-guild configuration access. |

With the store disabled, configuration comes from command flags and prompt history comes from
Discord REST. With it enabled, history comes only from the store; a failed history read is marked
incomplete in the model context and never falls back to Discord. Current-channel search is exposed
only when the store and the guild's `channel_search_enabled` setting are both enabled.

Startup fails closed — the connection, migrations, and a ping must succeed before the worker
starts. Request-time failures after startup fail open: configuration falls back to validated
defaults, reply claims admit the request, and a failed message record costs stored context, never
the reply.

## Provisioning

Jarvis owns the schema. At startup it applies its embedded, versioned migrations
(`store/migrations/`), so provisioning is only the database itself:

```sh
# PostgreSQL 16
docker run -d -p 5432:5432 -e POSTGRES_PASSWORD=... postgres:16
# SQLite: nothing to run — the file is created on first start
```

The PostgreSQL role needs `CREATE` on the target database for migrations, plus ordinary DML. No
extensions are required.

## What Jarvis records

**Only bot-involved conversation.** A message is recorded after targeting decides it is addressed
to the bot — a mention, a reply to the bot, or a bot-owned/bot-active thread — and the bot's own
replies are recorded when posted. Surrounding channel traffic, other bots, and untargeted DMs are
never stored. Recording happens even for requests the rate limiter turns away, since they are part
of the conversation.

Every message expires `message_retention_days` after ingestion (per guild, resolved at write time).
The sweeper deletes expired rows in batches; reads filter on expiry independently, so a delayed
sweep is invisible. The stored record is a creation-time snapshot: edits and deletes in Discord are
not ingested.

## Schema

All Discord identifiers are stored as `BIGINT` snowflakes (numeric order is snowflake order), all
timestamps as unix seconds, and booleans as `INTEGER` 0/1 — the dialect subset PostgreSQL and
SQLite share. Message content is plain `TEXT`.

### Written by Jarvis

| Table | Keyed by | Purpose |
| --- | --- | --- |
| `guild_configs` | `guild_id` | One row of `ServerSettings` per configured guild, with `version` incremented per mutation and `updated_by_user_id` for audit. Mutations run in one transaction (`SELECT … FOR UPDATE` on PostgreSQL). |
| `guild_admins` | `(guild_id, user_id)` | Delegated configuration administrators. |
| `messages` | `(channel_id, message_id)` | Recorded conversation. Inserts are `ON CONFLICT DO NOTHING`, so duplicate delivery is idempotent. `mentioned_user_ids` is a JSON array — read whole, never queried. |
| `reply_claims` | `(channel_id, message_id)` | The multi-site reply dedup. Claims compare expiries against the **database server's clock**, never a worker's. See [failover.md](failover.md). |
| `schema_migrations` | `version` | Applied migration versions. |

### Written by Jarvis and the external accounts API — `guild_mcp_servers`

Remote MCP tool servers attached to a guild, keyed by `(guild_id, name)`. Jarvis writes rows
through the root-only `add_mcp_server`/`remove_mcp_server` tools; the external accounts API may
also manage them directly (self-service attachment). The write contract:

- `name`: 1–32 lowercase letters, digits, or hyphens. It becomes part of the model-facing tool
  namespace `mcp_<name>_<tool>`.
- `url`: `https` only, no userinfo. The worker additionally refuses URLs resolving to loopback,
  private, or link-local addresses at dial time (`--mcp-allow-private-networks` overrides), so
  writes are not the last line of defense.
- `auth_ciphertext`: empty for unauthenticated servers, otherwise AES-256-GCM under
  `--mcp-encryption-key`, encoded `base64(nonce || ciphertext)` with the AEAD's 12-byte nonce
  prefixed. An external writer must use the same key and format. The plaintext token is sent as
  an `Authorization: Bearer` header and is never readable back through any Jarvis surface.
- `enabled`: 0/1; disabled rows are kept but never dialed.

Changes become visible within `--valkey-config-cache-ttl` when the configuration cache is
enabled, exactly like `accounts.tier` writes; Jarvis-side mutations invalidate immediately.

### Written by the external accounts API — the write contract

Jarvis's migrations create these tables but Jarvis only ever **reads** them, and only to resolve a
guild's effective tier. Signup, payment-processor webhooks, tier assignment, and guild linking
belong to a separate accounts service writing through ordinary SQL:

| Table | Keyed by | Contract |
| --- | --- | --- |
| `tiers` | `name` | Tier names referenced by accounts. Lowercase alphanumeric with hyphens, ≤32 chars. Keep aligned with the worker's `--guild-tier` flags: enforcement budgets still come from the flags, and an undeclared tier degrades to the default tier, never to an error. |
| `accounts` | `user_id` (Discord snowflake) | One row per customer. `tier` is the single source the hot path resolves — subscription outcomes must be written through to it. |
| `account_guilds` | `guild_id` | A guild belongs to exactly one account. `ON DELETE CASCADE` from accounts. |
| `subscriptions` | `(provider, provider_subscription_id)` | Payment-processor state: `provider` (`stripe`, `paypal`, `apple`, …), `tier`, `status`, `current_period_end`, and `event_created_at` to guard out-of-order webhook delivery. Jarvis never reads this table; it exists so one schema carries any processor. |

A guild with no account link runs at the deployment default tier. Tier changes written to
`accounts.tier` become visible to workers within `--valkey-config-cache-ttl` (default 5m) when the
Valkey configuration cache is enabled, immediately otherwise — there is no cross-service cache
invalidation channel. If faster propagation ever matters, the API can delete the guild's
documented cache key ([valkey.md](valkey.md)) after writing.

The SQLite backend gets the same tables, but the external-API integration presumes PostgreSQL —
two services sharing one SQLite file is not a supported topology. On SQLite, accounts are managed
by direct SQL or left unused, in which case every guild runs at the default tier.

## Current-channel search

The `search_current_channel` model tool reads stored messages newest first in pages of 100 and
filters by text, author, and time range, stopping after the newest eight matches. Because only
bot-involved conversation is stored, the searchable set is the bot's own conversation history, not
the channel's traffic. Search never mixes stored and Discord REST results.

The same `channel_search_enabled` setting also exposes the Discord MCP read tools —
`read_messages` and `get_message` — which serve live Discord history for the **current channel
only**, complementing the stored view. They run through an in-process MCP server on a view pinned
to the message's guild and channel, so neither the model nor a tool result can redirect them
elsewhere. `list_channels` and guild-wide `search_messages` are deliberately not registered: they
are scoped to the bot's permissions rather than the requesting user's, so offering them would let
any member read a channel they cannot see themselves.

## Administration tools

Unchanged from before, with one removal: there is no `set_server_tier` tool. The subscription tier
belongs to the guild's owning account and is written by the external accounts API; root reads of
`get_server_configuration` still report the effective tier.

## Migration from DynamoDB

The 0.9.0 release replaces DynamoDB entirely: `DYNAMODB_ENABLED`, `DYNAMODB_TABLE`,
`AWS_ROLE_ARN`, and `AWS_WEB_IDENTITY_AUDIENCE` are removed, and there is no automated data
migration. Guild configurations are few and re-enterable through the existing Discord tools;
message history ages out within its retention window by design.
