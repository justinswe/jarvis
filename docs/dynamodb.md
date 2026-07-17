# DynamoDB storage

Jarvis can use one externally provisioned DynamoDB table for Discord message history and per-guild configuration. The integration belongs only to the worker; the ingestor and protobuf transport are unchanged.

## Runtime configuration

DynamoDB is disabled by default. Every flag is also available through the app package's uppercase environment-variable mapping.

| Flag | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `--dynamodb-enabled` | `DYNAMODB_ENABLED` | `false` | Enable message storage, DynamoDB history, and mutable guild configuration. |
| `--dynamodb-table` | `DYNAMODB_TABLE` | `jarvis` | Existing table name. |
| `--aws-role-arn` | `AWS_ROLE_ARN` | empty | AWS role assumed with a Google identity token. |
| `--aws-web-identity-audience` | `AWS_WEB_IDENTITY_AUDIENCE` | empty | Audience placed in the Google identity token exchanged with AWS. |
| `--message-retention-days` | `MESSAGE_RETENTION_DAYS` | `30` | Default retention for new message items. |
| `--root-user-ids` | `ROOT_USER_IDS` | empty | Discord user IDs with cross-guild configuration access. Repeat the flag or use the app package's string-slice environment format. |

Jarvis supports two AWS authentication modes. When the role ARN and web identity audience are both set, the worker retrieves a short-lived Google ID token from the attached Google Cloud service account and exchanges it through AWS STS `AssumeRoleWithWebIdentity`. Both the Google token and resulting AWS credentials are cached and refreshed before expiration. Configure both values together; setting only one is an error. Do not mount Google service-account keys or configure static AWS access keys for this mode.

When both federation settings are empty, the AWS SDK uses its normal credential chain for local profiles, environment credentials, ECS, or EC2. Region resolution remains native to the AWS SDK, including `AWS_REGION`.

When DynamoDB is enabled, AWS configuration, initial credential retrieval, and repository initialization must succeed before the worker starts. Request-time data failures after successful startup remain fail-open. With integration disabled, configuration comes from command flags and prompt history comes from Discord REST. With integration enabled, history comes only from DynamoDB; a partial or failed history read is explicitly marked as incomplete in the model context and never falls back to Discord. Current-channel search is exposed only when DynamoDB and the guild's `channel_search_enabled` setting are both enabled; it never falls back to Discord REST.

## Table contract

Provision a table with string partition key `pk` and string sort key `sk`. On-demand capacity is the recommended starting mode because Discord traffic is bursty. Enable DynamoDB TTL on the numeric `expires_at` attribute. TTL removal is asynchronous, so Jarvis also filters expired items while reading.

No secondary index is required.

### Message item

| Attribute | Type | Value |
| --- | --- | --- |
| `pk` | String | `CHANNEL#<channel_id>` |
| `sk` | String | `MESSAGE#<zero-padded-message_id>` |
| `entity_type` | String | `MESSAGE` |
| `schema_version` | Number | `2` for new writes; version `1` remains readable. |
| `message_id`, `guild_id`, `channel_id` | String | Discord identifiers. |
| `content` | String or Binary | Raw UTF-8 String, or zstd-compressed Binary when compression reduces storage. |
| `compressed` | Boolean | Whether `content` uses zstd. |
| `message_kind` | Number | Normalized protobuf message kind. |
| `author_id`, `author_username`, `author_global_name` | String | Normalized author fields. |
| `author_bot` | Boolean | Whether the author is a bot. |
| `mentioned_user_ids` | List | Mentioned Discord user IDs. |
| `reference_message_id`, `reference_channel_id` | String | Reply/thread reference when present. |
| `created_at`, `ingested_at` | Number | Unix milliseconds. |
| `expires_at` | Number | Unix seconds used by DynamoDB TTL. |

For content over 100 UTF-8 bytes, Jarvis produces a zstd candidate and stores it as Binary only when the result is smaller than the original bytes. Otherwise it stores a readable String with `compressed:false`. Version 1 records used Binary for both raw and compressed content; the worker continues to decode both forms. Consequently, a version 1 `compressed:false` value may look base64-encoded in the DynamoDB console even though it is raw UTF-8 rather than zstd. The raw protobuf contract remains unchanged because compression is an internal storage concern. Retention is calculated at ingestion time, so changing a guild's retention affects only new writes.

Message writes are conditional and deterministic, making duplicate delivery of the same channel/message pair idempotent. History queries are bounded by the request's configured context window, ordered newest first in storage, and returned chronologically to the model.

The stored record is a creation-time snapshot. Jarvis does not ingest Discord message update or delete events, so search can return original content that was later edited or deleted in Discord.

### Current-channel search

The `search_current_channel` model tool queries only the current channel or thread partition. It accepts any combination of case-insensitive message text, an exact author ID/mention/username/display name, an inclusive RFC3339 start time, and an exclusive RFC3339 end time. At least one criterion is required. Results include direct Discord links and author IDs.

Search reads stored messages newest first in pages of 100, excludes the current request, and stops after the newest eight matches. The selected matches are returned chronologically. A rare or absent query can therefore read every unexpired item in a channel, while a common query normally stops early. Expired records remain excluded even while DynamoDB TTL deletion is pending.

Usable messages returned alongside a DynamoDB or decode error are supplied to the model with `incomplete:true`; a failure before any usable page returns a sanitized `channel_search_unavailable` error. Search never mixes DynamoDB and Discord REST results. No secondary index or backfill is required, but read cost and latency grow with the amount of eligible channel history.

### Guild configuration item

| Attribute | Type | Value |
| --- | --- | --- |
| `pk` | String | `GUILD#<guild_id>` |
| `sk` | String | `CONFIG` |
| `entity_type` | String | `GUILD_CONFIG` |
| `schema_version` | Number | `1` |
| `prompt` | String | Root-controlled assistant customization, including an optional name and personality. |
| `guild_prompt` | String | Optional subordinate guild-admin instructions appended to the root-controlled prompt. |
| `thread_messages`, `parent_messages`, `channel_messages` | Number | Context-window limits. |
| `history_runes`, `max_output_tokens` | Number | Context-rune budget and total generated-token budget, including thinking and visible text. |
| `message_timeout_seconds` | Number | Processing deadline. |
| `message_retention_days` | Number | Retention for new messages, 1 through 3650 days. |
| `web_search_enabled`, `channel_search_enabled` | Boolean | Web-search eligibility and DynamoDB current-channel-search eligibility. |
| `admin_user_ids` | String set | Delegated Jarvis configuration administrators. |
| `version` | Number | Optimistic concurrency version. |
| `updated_at` | Number | Unix milliseconds. |
| `updated_by_user_id` | String | Discord actor ID for the latest update. |

Missing guild configuration items materialize from the worker's validated defaults on their first mutation. Existing items without `guild_prompt` load it as empty. Legacy `temperature` attributes are ignored during reads and disappear on the next full configuration write, so no migration or backfill is required. Updates use conditional writes and bounded conflict retries.

When present, the guild prompt is trimmed, limited to 4,000 runes, and composed as:

```text
<base prompt>

Guild-specific instructions:
<guild prompt>
```

An empty guild prompt leaves the base prompt unchanged.

The root-controlled prompt is the only customization allowed to assign the assistant's name. The default prompt is empty, so the assistant remains unnamed unless root customization supplies one. Guild-specific instructions cannot assign or change the name and remain subordinate to the root-controlled prompt and the core behavioral and reliability rules.

## Administration tools

The model receives five narrow tools only when the caller is authorized:

- `get_server_configuration`
- `update_server_configuration`
- `set_guild_prompt`
- `add_server_admin`
- `remove_server_admin`

Authorization is granted to configured root users, stored delegated administrators, the Discord guild owner, Discord administrators, and users with Manage Guild permission. Those administrators may set or clear `guild_prompt`. Root users apply across guilds and are the only callers allowed to change `prompt`, `thread_context_window`, `parent_context_window`, or `message_retention_days`; protected fields are omitted from every other caller's tool schema and checked again during execution.

The tools use flat, typed schemas with explicit bounds and return the complete effective state after a successful mutation. Mutation descriptions require an explicit, unambiguous administrator request. When these tools are exposed, Jarvis raises Gemini's thinking level from medium to high. Database errors are returned to the model as stable, sanitized error codes without backend details.

## IAM and operations

The worker identity needs these actions on only the configured table:

- `dynamodb:GetItem`
- `dynamodb:PutItem`
- `dynamodb:Query`

Table creation, backups, point-in-time recovery, encryption policy, alarms, and TTL enablement remain deployment responsibilities. Monitor conditional-check failures, throttling, read/write latency, item size, TTL backlog, channel-search duration, scanned-message count, incomplete searches, and worker timeouts. Search logs record filter presence and counts but exclude criteria and message content. Message content is user data; choose retention, encryption, backup, and access policies appropriate for the deployment.
