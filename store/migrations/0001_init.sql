-- Schema v1. Written in the dialect subset PostgreSQL 16 and SQLite share: BIGINT
-- snowflakes (numeric order is snowflake order), unix seconds for every timestamp, and
-- INTEGER 0/1 booleans.
--
-- Jarvis writes guild_configs, guild_admins, messages, and reply_claims. The tiers,
-- accounts, account_guilds, and subscriptions tables are written by the external accounts
-- API and only read here; docs/store.md is the write contract.

CREATE TABLE tiers (
  name                TEXT PRIMARY KEY,
  requests_per_second INTEGER NOT NULL,
  burst               INTEGER NOT NULL,
  tokens_per_hour     INTEGER NOT NULL
);

CREATE TABLE accounts (
  user_id    BIGINT PRIMARY KEY,
  tier       TEXT NOT NULL REFERENCES tiers(name),
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);

CREATE TABLE account_guilds (
  guild_id        BIGINT PRIMARY KEY,
  account_user_id BIGINT NOT NULL REFERENCES accounts(user_id) ON DELETE CASCADE,
  created_at      BIGINT NOT NULL
);

CREATE INDEX account_guilds_account ON account_guilds(account_user_id);

CREATE TABLE subscriptions (
  provider                 TEXT NOT NULL,
  provider_subscription_id TEXT NOT NULL,
  account_user_id          BIGINT NOT NULL REFERENCES accounts(user_id),
  tier                     TEXT NOT NULL REFERENCES tiers(name),
  status                   TEXT NOT NULL,
  current_period_end       BIGINT NOT NULL DEFAULT 0,
  event_created_at         BIGINT NOT NULL,
  created_at               BIGINT NOT NULL,
  updated_at               BIGINT NOT NULL,
  PRIMARY KEY (provider, provider_subscription_id)
);

CREATE INDEX subscriptions_account ON subscriptions(account_user_id);

CREATE TABLE guild_configs (
  guild_id                BIGINT PRIMARY KEY,
  prompt                  TEXT NOT NULL DEFAULT '',
  guild_prompt            TEXT NOT NULL DEFAULT '',
  thread_messages         INTEGER NOT NULL,
  parent_messages         INTEGER NOT NULL,
  channel_messages        INTEGER NOT NULL,
  history_runes           INTEGER NOT NULL,
  max_output_tokens       INTEGER NOT NULL,
  message_timeout_seconds BIGINT  NOT NULL,
  message_retention_days  INTEGER NOT NULL,
  web_search_enabled      INTEGER NOT NULL DEFAULT 0,
  channel_search_enabled  INTEGER NOT NULL DEFAULT 0,
  reasoning_effort        TEXT NOT NULL DEFAULT '',
  primary_model_profile   TEXT NOT NULL DEFAULT '',
  fallback_model_profile  TEXT NOT NULL DEFAULT '',
  version                 BIGINT NOT NULL DEFAULT 1,
  updated_at              BIGINT NOT NULL,
  updated_by_user_id      BIGINT NOT NULL
);

CREATE TABLE guild_admins (
  guild_id BIGINT NOT NULL,
  user_id  BIGINT NOT NULL,
  PRIMARY KEY (guild_id, user_id)
);

CREATE TABLE messages (
  channel_id           BIGINT NOT NULL,
  message_id           BIGINT NOT NULL,
  guild_id             BIGINT NOT NULL DEFAULT 0,
  author_id            BIGINT NOT NULL,
  author_username      TEXT NOT NULL DEFAULT '',
  author_global_name   TEXT NOT NULL DEFAULT '',
  author_bot           INTEGER NOT NULL DEFAULT 0,
  message_kind         INTEGER NOT NULL DEFAULT 0,
  content              TEXT NOT NULL DEFAULT '',
  mentioned_user_ids   TEXT NOT NULL DEFAULT '[]',
  reference_channel_id BIGINT NOT NULL DEFAULT 0,
  reference_message_id BIGINT NOT NULL DEFAULT 0,
  created_at           BIGINT NOT NULL,
  ingested_at          BIGINT NOT NULL,
  expires_at           BIGINT NOT NULL,
  PRIMARY KEY (channel_id, message_id)
);

CREATE INDEX messages_expiry ON messages(expires_at);

CREATE TABLE reply_claims (
  channel_id BIGINT NOT NULL,
  message_id BIGINT NOT NULL,
  owner      TEXT NOT NULL,
  expires_at BIGINT NOT NULL,
  PRIMARY KEY (channel_id, message_id)
);
