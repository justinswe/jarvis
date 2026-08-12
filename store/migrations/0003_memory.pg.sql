-- Persistent character memory. PostgreSQL-only (.pg.sql): pgvector and tsvector have no
-- SQLite equivalent, and memory is a Postgres-gated feature by design. The vector
-- extension must be installable by the migration role or pre-provisioned; a failure here
-- is loud and stops startup.

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE memory_records (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  guild_id       BIGINT NOT NULL,
  character_id   BIGINT NOT NULL DEFAULT 0,
  channel_id     BIGINT NOT NULL DEFAULT 0,
  kind           TEXT NOT NULL,
  entity_key     TEXT NOT NULL DEFAULT '',
  content        TEXT NOT NULL,
  pinned         INTEGER NOT NULL DEFAULT 0,
  importance     REAL NOT NULL DEFAULT 0.5,
  embedding      vector(768),
  tsv            tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
  source_from_id BIGINT NOT NULL DEFAULT 0,
  source_to_id   BIGINT NOT NULL DEFAULT 0,
  created_at     BIGINT NOT NULL,
  updated_at     BIGINT NOT NULL,
  deleted_at     BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX memory_records_scope ON memory_records(guild_id, character_id, updated_at) WHERE deleted_at = 0;

CREATE UNIQUE INDEX memory_records_entity ON memory_records(guild_id, character_id, entity_key) WHERE deleted_at = 0 AND entity_key <> '';

CREATE UNIQUE INDEX memory_records_summary_range ON memory_records(guild_id, character_id, channel_id, source_to_id) WHERE kind = 'summary' AND source_to_id <> 0;

CREATE INDEX memory_records_vec ON memory_records USING hnsw (embedding vector_cosine_ops);

CREATE INDEX memory_records_tsv ON memory_records USING gin (tsv);

CREATE TABLE memory_watermarks (
  channel_id       BIGINT NOT NULL,
  character_id     BIGINT NOT NULL DEFAULT 0,
  guild_id         BIGINT NOT NULL,
  last_message_id  BIGINT NOT NULL DEFAULT 0,
  pending_since    BIGINT NOT NULL DEFAULT 0,
  pending_count    BIGINT NOT NULL DEFAULT 0,
  claimed_by       TEXT NOT NULL DEFAULT '',
  claim_expires_at BIGINT NOT NULL DEFAULT 0,
  updated_at       BIGINT NOT NULL,
  PRIMARY KEY (channel_id, character_id)
);
