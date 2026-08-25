-- Durable terminal state for each bot-targeted Discord request. Content stays in the
-- messages table; this table contains only operational metadata.

CREATE TABLE request_outcomes (
  channel_id        BIGINT NOT NULL,
  message_id        BIGINT NOT NULL,
  guild_id          BIGINT NOT NULL DEFAULT 0,
  status            TEXT NOT NULL,
  reply_message_ids TEXT NOT NULL DEFAULT '[]',
  error_kind        TEXT NOT NULL DEFAULT '',
  duration_ms       BIGINT NOT NULL DEFAULT 0,
  created_at        BIGINT NOT NULL,
  updated_at        BIGINT NOT NULL,
  expires_at        BIGINT NOT NULL,
  PRIMARY KEY (channel_id, message_id)
);

CREATE INDEX request_outcomes_expiry ON request_outcomes(expires_at);
