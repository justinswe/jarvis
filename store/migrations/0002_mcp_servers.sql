-- Remote MCP tool servers attached to a guild. Rows are written by Jarvis's root-only
-- configuration tools AND, in the future, directly by the external accounts API; see
-- docs/store.md for the write contract. auth_ciphertext is AES-256-GCM under the
-- deployment's MCP encryption key, stored as base64(nonce||ciphertext); empty means the
-- server needs no authentication.
CREATE TABLE guild_mcp_servers (
  guild_id        BIGINT NOT NULL,
  name            TEXT NOT NULL,
  url             TEXT NOT NULL,
  auth_ciphertext TEXT NOT NULL DEFAULT '',
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      BIGINT NOT NULL,
  updated_at      BIGINT NOT NULL,
  PRIMARY KEY (guild_id, name)
);
