package store

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemoryScopeMigrationUpgradesLegacyRows proves the production upgrade path rather
// than only a fresh install: v1-v3 data exists before v4-v5 run, unreviewed memory is
// quarantined, pinned memory survives, and the replacement uniqueness scope works.
func TestMemoryScopeMigrationUpgradesLegacyRows(t *testing.T) {
	if manualTestOptions.postgresDSN == "" {
		t.Skip("set --postgres-dsn (or POSTGRES_DSN) to run live PostgreSQL tests")
	}
	ctx := context.Background()
	base, err := sql.Open("pgx", manualTestOptions.postgresDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })

	schema := "jarvis_migration_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	_, err = base.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = base.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })

	legacy, err := sql.Open("pgx", manualTestOptions.postgresDSN)
	require.NoError(t, err)
	legacy.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = legacy.Close() })
	require.NoError(t, legacy.PingContext(ctx))
	_, err = legacy.ExecContext(ctx, `SET search_path TO `+schema+`, public`)
	require.NoError(t, err)
	_, err = legacy.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`)
	require.NoError(t, err)
	for version, name := range []string{"0001_init.sql", "0002_mcp_servers.sql", "0003_memory.pg.sql"} {
		require.NoError(t, applyMigration(ctx, legacy, postgresDialect, name, int64(version+1)))
	}
	_, err = legacy.ExecContext(ctx, `
		INSERT INTO memory_records (guild_id, character_id, channel_id, kind, entity_key,
			content, pinned, importance, created_at, updated_at)
		VALUES
			(100, 0, 555, 'entity', 'legacy:bot-claim', 'Chow is hostile.', 0, 0.8, 1, 1),
			(100, 0, 555, 'entity', 'legacy:reviewed', 'Reviewed fact.', 1, 0.8, 1, 1)`)
	require.NoError(t, err)

	require.NoError(t, migrate(ctx, legacy, postgresDialect))
	var version int64
	require.NoError(t, legacy.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&version))
	assert.Equal(t, int64(5), version)

	var unpinnedQuarantine int64
	var reason string
	require.NoError(t, legacy.QueryRowContext(ctx, `
		SELECT quarantined_at, quarantine_reason FROM memory_records
		WHERE entity_key = 'legacy:bot-claim'`).Scan(&unpinnedQuarantine, &reason))
	assert.Positive(t, unpinnedQuarantine)
	assert.Equal(t, "legacy_auto_memory", reason)
	var pinnedQuarantine int64
	require.NoError(t, legacy.QueryRowContext(ctx, `
		SELECT quarantined_at FROM memory_records WHERE entity_key = 'legacy:reviewed'`).Scan(&pinnedQuarantine))
	assert.Zero(t, pinnedQuarantine)

	_, err = legacy.ExecContext(ctx, `
		INSERT INTO memory_records (guild_id, character_id, channel_id, subject_user_id,
			origin_role, kind, entity_key, content, importance, created_at, updated_at)
		VALUES
			(100, 0, 555, 300, 'user', 'entity', 'preference:tea', 'Alice prefers tea.', 0.8, 2, 2),
			(100, 0, 556, 300, 'user', 'entity', 'preference:tea', 'Alice prefers tea here too.', 0.8, 2, 2),
			(100, 0, 555, 301, 'user', 'entity', 'preference:tea', 'Bob prefers tea.', 0.8, 2, 2)`)
	require.NoError(t, err, "channel and subject are both part of entity uniqueness")
	_, err = legacy.ExecContext(ctx, `
		INSERT INTO memory_records (guild_id, character_id, channel_id, subject_user_id,
			origin_role, kind, entity_key, content, importance, created_at, updated_at)
		VALUES (100, 0, 555, 300, 'user', 'entity', 'preference:tea', 'duplicate', 0.8, 2, 2)`)
	assert.Error(t, err, "the same subject and channel still deduplicate")
}

// TestMemoryOnPostgres exercises the Postgres-only memory SQL — pgvector writes and
// cosine retrieval, watermark claims against the server clock, entity upserts,
// consolidation — none of which any hermetic test can reach.
func TestMemoryOnPostgres(t *testing.T) {
	if manualTestOptions.postgresDSN == "" {
		t.Skip("set --postgres-dsn (or POSTGRES_DSN) to run live PostgreSQL tests")
	}
	ctx := context.Background()
	s := postgresStore(t)
	_, err := s.db.Exec(`TRUNCATE memory_records, memory_watermarks`)
	require.NoError(t, err)
	require.True(t, s.MemoryAvailable())

	vector := func(seed float32) []float32 {
		v := make([]float32, 768)
		v[0] = seed
		v[1] = 1
		return v
	}

	t.Run("watermark", func(t *testing.T) {
		first, err := s.NoteMemoryTurn(ctx, "100", "555", 7)
		require.NoError(t, err)
		assert.Equal(t, int64(1), first.PendingCount)
		second, err := s.NoteMemoryTurn(ctx, "100", "555", 7)
		require.NoError(t, err)
		assert.Equal(t, int64(2), second.PendingCount)
		assert.Equal(t, first.PendingSince.Unix(), second.PendingSince.Unix(), "pending_since sticks to the oldest turn")

		last, claimed, err := s.ClaimMemoryWatermark(ctx, "555", 7, time.Minute)
		require.NoError(t, err)
		require.True(t, claimed)
		assert.Equal(t, int64(0), last)
		_, claimed, err = s.ClaimMemoryWatermark(ctx, "555", 7, time.Minute)
		require.NoError(t, err)
		assert.False(t, claimed, "held claims are exclusive")

		require.NoError(t, s.CommitMemory(ctx, "555", 7, 200, []MemoryRecord{
			{GuildID: "100", CharacterID: 7, ChannelID: "555", Kind: MemoryKindSummary,
				Content: "Elyra and Justin reached the harbor.", Importance: 0.5,
				SourceFromID: 101, SourceToID: 200, Embedding: vector(0.9)},
			{GuildID: "100", ChannelID: "555", CharacterID: 7, Kind: MemoryKindEntity, EntityKey: "character:elyra",
				Content: "Elyra captains the Gull.", Importance: 0.7, Embedding: vector(0.1)},
		}))
		third, err := s.NoteMemoryTurn(ctx, "100", "555", 7)
		require.NoError(t, err)
		assert.Equal(t, int64(200), third.LastMessageID, "commit advanced the watermark")
		assert.Equal(t, int64(1), third.PendingCount, "commit reset the backlog")
		_, claimed, err = s.ClaimMemoryWatermark(ctx, "555", 7, time.Minute)
		require.NoError(t, err)
		assert.True(t, claimed, "commit released the claim")
		require.NoError(t, s.CommitMemory(ctx, "555", 7, 200, nil))
	})

	t.Run("entity upsert replaces", func(t *testing.T) {
		require.NoError(t, s.AddMemory(ctx, MemoryRecord{GuildID: "100", ChannelID: "555", CharacterID: 7,
			Kind: MemoryKindEntity, EntityKey: "character:elyra",
			Content: "Elyra lost the Gull in a storm.", Importance: 0.8, Embedding: vector(0.2)}))
		records, err := s.ListMemories(ctx, "100", "555", 7, 100)
		require.NoError(t, err)
		var matches []MemoryRecord
		for _, record := range records {
			if record.EntityKey == "character:elyra" {
				matches = append(matches, record)
			}
		}
		require.Len(t, matches, 1, "entity key replaces, never duplicates")
		assert.Contains(t, matches[0].Content, "storm")
	})

	t.Run("hybrid retrieval", func(t *testing.T) {
		require.NoError(t, s.AddMemory(ctx, MemoryRecord{GuildID: "100", ChannelID: "555", CharacterID: 7,
			Kind: MemoryKindEvent, Content: "Justin owes Elyra ten crowns.", Pinned: true, Importance: 0.9}))
		// Vector search finds the nearest embedded record.
		results, err := s.SearchMemory(ctx, "100", "555", 7, vector(0.9), "", 2)
		require.NoError(t, err)
		require.NotEmpty(t, results)
		var contents []string
		for _, record := range results {
			contents = append(contents, record.Content)
		}
		assert.Contains(t, contents, "Justin owes Elyra ten crowns.", "pinned records always return")
		assert.Contains(t, contents, "Elyra and Justin reached the harbor.", "vector neighborhood returns")
		// Full-text fallback works without an embedding.
		results, err = s.SearchMemory(ctx, "100", "555", 7, nil, "harbor", 5)
		require.NoError(t, err)
		found := false
		for _, record := range results {
			found = found || record.Content == "Elyra and Justin reached the harbor."
		}
		assert.True(t, found, "tsvector retrieval")
		// Other scopes see nothing.
		results, err = s.SearchMemory(ctx, "100", "555", 99, nil, "harbor", 5)
		require.NoError(t, err)
		for _, record := range results {
			assert.NotEqual(t, int64(7), record.CharacterID)
		}
		require.NoError(t, s.AddMemory(ctx, MemoryRecord{GuildID: "100", ChannelID: "556", CharacterID: 7,
			Kind: MemoryKindEvent, Content: "This belongs to another channel.", Importance: 0.9}))
		results, err = s.SearchMemory(ctx, "100", "555", 7, nil, "another channel", 5)
		require.NoError(t, err)
		for _, record := range results {
			assert.NotEqual(t, "This belongs to another channel.", record.Content)
		}
	})

	t.Run("book and consolidation", func(t *testing.T) {
		records, err := s.ListMemories(ctx, "100", "555", 7, 100)
		require.NoError(t, err)
		require.NotEmpty(t, records)
		target := records[len(records)-1]
		_, err = s.db.Exec(`UPDATE memory_records SET expires_at = 1 WHERE id = $1`, target.ID)
		require.NoError(t, err)
		require.NoError(t, s.SetMemoryPinned(ctx, "100", "555", target.ID, true))
		var expiresAt int64
		require.NoError(t, s.db.QueryRow(`SELECT expires_at FROM memory_records WHERE id = $1`, target.ID).Scan(&expiresAt))
		assert.Zero(t, expiresAt, "pinning makes a short-lived memory permanent")
		require.NoError(t, s.SetMemoryPinned(ctx, "100", "555", target.ID, false))
		require.NoError(t, s.EditMemory(ctx, "100", "555", target.ID, "Edited content.", nil))
		assert.ErrorIs(t, s.EditMemory(ctx, "100", "555", 99999999, "x", nil), ErrMemoryNotFound)

		evictable, err := s.EvictableMemories(ctx, "100", "555", 7, 0, 10)
		require.NoError(t, err)
		require.NotEmpty(t, evictable, "keep budget zero surfaces unpinned non-entity records")
		ids := make([]int64, 0, len(evictable))
		for _, record := range evictable {
			ids = append(ids, record.ID)
		}
		require.NoError(t, s.ReplaceMemoriesWithDigest(ctx, ids, MemoryRecord{
			GuildID: "100", ChannelID: "555", CharacterID: 7, Kind: MemoryKindSummary, Content: "Digest of old events.", Importance: 0.3}))
		after, err := s.ListMemories(ctx, "100", "555", 7, 100)
		require.NoError(t, err)
		for _, record := range after {
			assert.NotContains(t, ids, record.ID, "consolidated records are gone from reads")
		}

		require.NoError(t, s.DeleteMemory(ctx, "100", "555", after[0].ID))
		assert.ErrorIs(t, s.DeleteMemory(ctx, "100", "555", after[0].ID), ErrMemoryNotFound)
	})

	t.Run("stale watermarks", func(t *testing.T) {
		_, err := s.NoteMemoryTurn(ctx, "100", "777", 7)
		require.NoError(t, err)
		stale, err := s.StaleMemoryWatermarks(ctx, 0, 10)
		require.NoError(t, err)
		found := false
		for _, watermark := range stale {
			found = found || watermark.ChannelID == "777"
		}
		assert.True(t, found)
	})
}

// TestCharactersOnPostgres runs the character suite against the postgres dialect.
func TestCharactersOnPostgres(t *testing.T) {
	if manualTestOptions.postgresDSN == "" {
		t.Skip("set --postgres-dsn (or POSTGRES_DSN) to run live PostgreSQL tests")
	}
	s := postgresStore(t)
	_, err := s.db.Exec(`TRUNCATE characters, channel_characters CASCADE`)
	require.NoError(t, err)
	ctx := context.Background()

	created, err := s.UpsertCharacter(ctx, "100", "200", "Elyra", testCardJSON, "")
	require.NoError(t, err)
	bound, err := s.BindChannelCharacter(ctx, "555", "100", "ELYRA", "200")
	require.NoError(t, err)
	assert.Equal(t, created.ID, bound.ID)
	active, err := s.ChannelCharacter(ctx, "555")
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, "Elyra", active.Name)
	require.NoError(t, s.DeleteCharacter(ctx, "100", "elyra"))
	active, err = s.ChannelCharacter(ctx, "555")
	require.NoError(t, err)
	assert.Nil(t, active, "binding cascades away with the character")
}
