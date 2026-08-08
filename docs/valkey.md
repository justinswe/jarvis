# Valkey usage metering, subscription limits, and guild-configuration cache

Jarvis can record per-guild request rates and per-model token consumption in Valkey, enforce
per-guild limits derived from a subscription tier, and cache guild configuration to avoid a
store read on every Discord message. The integration belongs only to the worker; the
ingestor, the protobuf transport, and the supervisor are unchanged.

The usage-metering keys are written for a **separate external service** to consume, so that key
schema (below) is a contract: changing it breaks that reader. The guild-configuration cache
(see [Guild-configuration cache](#guild-configuration-cache)) is purely internal — Jarvis is
both writer and reader, and its schema may change freely.

## Runtime configuration

Valkey is disabled by default. Every flag is also available through the app package's uppercase
environment-variable mapping.

| Flag | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `--valkey-enabled` | `VALKEY_ENABLED` | `false` | Enable usage metering and subscription limits. |
| `--valkey-address` | `VALKEY_ADDRESS` | empty | `host:port` addresses. Required when enabled. Repeat the flag or use the string-slice environment format. |
| `--valkey-username` | `VALKEY_USERNAME` | empty | ACL username. |
| `--valkey-password` | `VALKEY_PASSWORD` | empty | ACL password. |
| `--valkey-tls-enabled` | `VALKEY_TLS_ENABLED` | `false` | Connect over TLS 1.2 or later. |
| `--valkey-select-db` | `VALKEY_SELECT_DB` | `0` | Database index for non-cluster deployments. |
| `--valkey-key-prefix` | `VALKEY_KEY_PREFIX` | `jarvis` | Key namespace root. |
| `--valkey-timeout` | `VALKEY_TIMEOUT` | `50ms` | Deadline for the inline admission check. Exceeding it allows the request. |
| `--valkey-dial-timeout` | `VALKEY_DIAL_TIMEOUT` | `5s` | Deadline for the startup connection and its verifying `PING`. Separate from `--valkey-timeout` because a deadline tight enough for one round trip cannot also cover a TLS handshake and authentication, and startup fails closed. |
| `--valkey-flush-interval` | `VALKEY_FLUSH_INTERVAL` | `1s` | How often accumulated token usage is written. |
| `--valkey-request-retention` | `VALKEY_REQUEST_RETENTION` | `48h` | TTL on the per-second request and denial hashes. |
| `--valkey-token-retention` | `VALKEY_TOKEN_RETENTION` | `720h` | TTL on the per-model token hashes and the guild index. |
| `--valkey-warn-threshold` | `VALKEY_WARN_THRESHOLD` | `95` | Utilization percentage that appends a near-limit notice to replies. |
| `--valkey-config-cache-ttl` | `VALKEY_CONFIG_CACHE_TTL` | `5m` | Guild-configuration cache time-to-live; bounds staleness if a write invalidation is missed. |
| `--guild-tier` | `GUILD_TIER` | empty | Subscription tier limits. See the grammar below. |
| `--default-guild-tier` | `DEFAULT_GUILD_TIER` | `free` | Tier used for servers with no assigned tier, or one no longer declared. |

The usage metering client and the guild-configuration cache share one Valkey connection, dialed
once with these flags.

Valkey-go's own client-side caching (RESP3 tracking) is disabled unconditionally: several managed
Valkey providers reject it. This is unrelated to the guild-configuration cache described below,
which is an explicit read-through cache Jarvis maintains itself using ordinary string keys, not
valkey-go's tracking feature. Pipelining is always on, because valkey-go only honors context
cancellation in pipeline mode — that is what makes `--valkey-timeout` a real bound rather than an
advisory one.

**Minimum server version: Valkey 7.2.** The scripts call `TIME`, which requires effect
replication.

### Supervised local Valkey

Nothing above says where the server comes from. For local runs the `jarvis` supervisor can own
one, so a developer does not have to keep a Valkey running by hand:

```
bazel run //jarvis -- --valkey-enabled
```

It starts `valkey-server` after the bus and before the worker, waits for it to answer `PING`,
injects `VALKEY_ENABLED=true` and `VALKEY_ADDRESS=127.0.0.1:6379` into the worker's environment,
and stops it with every other child. It is off by default, and while off the supervisor does not
touch `VALKEY_*` at all — an external Valkey configured through the environment keeps working.

Persistence is off (`--save "" --appendonly no`) because everything here is derived state: the
rate-limit counters expire on their own and the guild-configuration cache is read-through. Nothing
sets `maxmemory`, deliberately: an eviction policy could drop a live rate-limit counter and
quietly admit more requests than a tier allows.

The binary is pinned under `third_party/valkey` and fetched by Bazel. Upstream publishes prebuilt
servers for **Linux only**, so on macOS there is nothing to stage and the supervisor falls back to
a `valkey-server` on `PATH`:

```
brew install valkey
```

An explicit `--valkey-binary` is never second-guessed — a wrong path is an error, not a silent
fallback to whatever is installed on the host.

No container image bakes `valkey-server` in, including the combined one: every upstream build
links `libsystemd`, which the distroless base does not carry. Images expect an external Valkey,
and the host in `VALKEY_ADDRESS` is what tells the supervisor which server is its own:

| `VALKEY_ENABLED` | `VALKEY_ADDRESS` | Supervisor behavior |
| --- | --- | --- |
| unset | anything | No Valkey. `VALKEY_*` passes through to the worker untouched. |
| `true` | another host | Uses that server. Starts nothing, and does **not** overwrite the address. This is the combined image's deployment. |
| `true` | loopback | Starts and supervises a local `valkey-server` on the port the address names. |
| `true` | unset | The same, on `--valkey-port`, injecting `127.0.0.1:6379`. |

Loopback and unset are the same case, because `127.0.0.1` is the address the supervisor would
hand the worker anyway: a `.env` naming it is a developer asking for a supervised server, not
telling the supervisor one is already running. Only a different host means somebody else owns it.
Both local rows need a binary, which is why an image run with metering enabled must name a
server on another host — it has none to start, and would fail before the bus.

### Failure behavior

Startup **fails closed**, request time **fails open** — the same contract the SQL store uses. When
`--valkey-enabled` is set, connecting and pinging Valkey must succeed before the worker starts,
so a misconfiguration is loud instead of silently metering nothing. After a successful start,
every limiter error, timeout, or malformed reply admits the request. Metering failures drop the
affected deltas and log; they never block or fail a Discord request.

## Subscription tiers

```
--guild-tier <name>=<requests-per-second>:<burst>:<tokens-per-hour>

--guild-tier free=1:2:200000
--guild-tier pro=10:20:20000000
--default-guild-tier free
```

Tier names are lowercase alphanumeric with hyphens, at most 32 characters. A `0` for either
requests-per-second or tokens-per-hour means that dimension is **recorded but not enforced**.
Declaring no tiers at all is a valid meter-only deployment: everything is recorded and nothing
is ever denied.

A guild's tier belongs to its owning account in the SQL store and is written only by the
external accounts API — Jarvis resolves it read-only through the guild's account link (see
[store.md](store.md)). A tier that is no longer declared by the deployment falls back
to `--default-guild-tier` rather than failing the request, so removing a tier from the flag
never breaks a server. The tier recorded in Valkey is always the **effective** tier after that
fallback, so a reader never sees a tier the deployment does not define.

## Key schema

`P` is `--valkey-key-prefix`. `{G}` is the Discord guild ID inside a cluster hash tag, and
`base` is `P:v1:g:{G}`.

| Key | Type | Contents | TTL |
| --- | --- | --- | --- |
| `base:gcra` | String | Rate-limiter state (theoretical arrival time, milliseconds). Internal; not intended for readers. | derived from the tier |
| `base:req:<minute>` | Hash | Fields `"0"`–`"59"`: requests **admitted** in that second of the minute. Plus `_tier`. | `--valkey-request-retention` |
| `base:den:<minute>` | Hash | Same layout, requests **denied**. Plus `_tier`. | `--valkey-request-retention` |
| `base:tok:<hour>` | Hash | Fields `<provider>/<model>\|<metric>` where metric is `in`, `out`, `reason`, `total`, or `calls`. `*\|<metric>` holds the cross-model aggregate. Plus `_tier`. | `--valkey-token-retention` |
| `P:v1:guilds:<day>` | Set | Guild IDs with recorded activity that UTC day. | `--valkey-token-retention` |

`<minute>` is `floor(unix/60)`, `<hour>` is `floor(unix/3600)`, and `<day>` is
`floor(unix/86400)`. All three are derived from the Valkey server's own `TIME` inside the
scripts, never from a worker clock, so replicas with skewed clocks agree and a request near a
minute boundary can never be filed into the wrong minute.

### Reader contract

- **Hash fields beginning with `_` are reserved metadata and are never numeric.** Skip them when
  summing counters. Today the only one is `_tier`.
- `SMEMBERS P:v1:guilds:<day>` enumerates active guilds. Readers never need `SCAN`.
- One `HGETALL` on `base:req:<minute>` yields a complete 60-point per-second profile for that
  minute *and* the tier it was served under — no second lookup.
- `req:` and `den:` are separate so a reader can distinguish **offered** load from **admitted**
  load. Offered load for a second is the sum of both.
- `tok:<hour>` field `*|total` is also the enforcement counter for the hourly token budget.
- There is no lifetime rollup. Valkey is memory-resident, so unbounded accumulation is the
  reader's job, from the hourly buckets.

### The hash-tag invariant

Every key derived for one guild carries the identical `{G}` hash tag. This is load-bearing, not
cosmetic: the admission script declares only `base:gcra` in `KEYS` and constructs `req:`, `den:`,
and `tok:` names in Lua from server `TIME`, and a Valkey cluster client rejects a script that
touches keys in more than one slot. **Any new key added to a guild's namespace must keep the
tag**, or the integration breaks under cluster mode only — which local single-node testing will
not catch.

## Guild-configuration cache

A read-through cache in front of `config.Provider`/`config.Manager`, built on the generic
`worker/pkg/cache` package (see that package for the reusable `Get`/`Set`/`Delete`/`GetOrLoad`
primitives; `worker/pkg/config`'s `CachedProvider`/`CachedManager` are the guild-configuration-
specific wiring around them). It removes what would otherwise be up to two store reads
calls per Discord message — one to resolve behavior settings, one for `Repository.Record`'s
message-retention lookup — and, once populated, serves every worker replica on every subsequent
message for the same guild until the entry expires or is invalidated.

| Key | Type | Contents | TTL |
| --- | --- | --- | --- |
| `P:v1:c:{G}:guildconfig` | String | JSON-encoded `config.GuildConfig` for guild `G`, including its attached MCP server rows — never their auth tokens, which exist only encrypted in the store. | `--valkey-config-cache-ttl` (default `5m`) |

`{G}` carries the same cluster hash tag as the usage-metering keys, for the same reason: it keeps
a guild's keys in one cluster slot.

**Invalidation.** `Update`, `AddAdmin`, `RemoveAdmin`, `AddMCPServer`, and `RemoveMCPServer` each
delete the guild's cache entry immediately after their store write commits, so every worker replica observes the change
on its next read — Valkey is the shared cache every replica already reads from, so no separate
invalidation broadcast is needed. `--valkey-config-cache-ttl` is a backstop for the rare case a
delete itself fails (for example, a transient Valkey error): worst case, a replica serves a stale
configuration for up to that long.

The strict, admin-facing read used by the `get_server_configuration` tool (and any read
immediately following a mutation in the same tool round) bypasses this cache entirely and always
reads the store directly, so an administrator never sees a stale result from their own change.

Cache misses and write/delete failures are silent and fail open: they cost a store read (or a
future re-read), never a failed request — the same philosophy as [Failure
behavior](#failure-behavior) above.

## Behavior in Discord

A server over its limit gets a ⏳ reaction on the triggering message and the request is dropped.
There is no reply text, so a rate-limited server cannot be used to spam a channel. The check runs
before the 🤔 processing reaction is added, so there is no flicker, and no model call is made.

At or above `--valkey-warn-threshold` utilization, a normal reply gains one line of Discord
subtext:

```
-# ⏳ This server is near its request limit. Replies may pause shortly.
```

Utilization is measured at admission, so the notice can be one generation stale. That is
deliberate: a fresh reading would cost a second round trip for a cosmetic hint.

## Cost on the request path

Exactly **one** Valkey round trip per Discord request for usage metering: a single script that
makes the admission decision and records the per-second request count together. Token accounting
adds none — it accumulates in memory and is flushed on `--valkey-flush-interval` as one batched
call. Request counts are deliberately *not* aggregated: smearing them across a flush window would
destroy the per-second resolution that is the whole point of recording them.

The guild-configuration cache adds up to one more round trip: a `GET` on a cache hit, or a `GET`
followed by a `SET` on a miss (once per guild per `--valkey-config-cache-ttl` window, absent a
write invalidation). Either is still cheaper than the store read it replaces.
