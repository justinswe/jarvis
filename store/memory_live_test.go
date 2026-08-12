package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			{GuildID: "100", CharacterID: 7, Kind: MemoryKindEntity, EntityKey: "character:elyra",
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
		require.NoError(t, s.AddMemory(ctx, MemoryRecord{GuildID: "100", CharacterID: 7,
			Kind: MemoryKindEntity, EntityKey: "character:elyra",
			Content: "Elyra lost the Gull in a storm.", Importance: 0.8, Embedding: vector(0.2)}))
		records, err := s.ListMemories(ctx, "100", 7, 100)
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
		require.NoError(t, s.AddMemory(ctx, MemoryRecord{GuildID: "100", CharacterID: 7,
			Kind: MemoryKindEvent, Content: "Justin owes Elyra ten crowns.", Pinned: true, Importance: 0.9}))
		// Vector search finds the nearest embedded record.
		results, err := s.SearchMemory(ctx, "100", 7, vector(0.9), "", 2)
		require.NoError(t, err)
		require.NotEmpty(t, results)
		var contents []string
		for _, record := range results {
			contents = append(contents, record.Content)
		}
		assert.Contains(t, contents, "Justin owes Elyra ten crowns.", "pinned records always return")
		assert.Contains(t, contents, "Elyra and Justin reached the harbor.", "vector neighborhood returns")
		// Full-text fallback works without an embedding.
		results, err = s.SearchMemory(ctx, "100", 7, nil, "harbor", 5)
		require.NoError(t, err)
		found := false
		for _, record := range results {
			found = found || record.Content == "Elyra and Justin reached the harbor."
		}
		assert.True(t, found, "tsvector retrieval")
		// Other scopes see nothing.
		results, err = s.SearchMemory(ctx, "100", 99, nil, "harbor", 5)
		require.NoError(t, err)
		for _, record := range results {
			assert.NotEqual(t, int64(7), record.CharacterID)
		}
	})

	t.Run("book and consolidation", func(t *testing.T) {
		records, err := s.ListMemories(ctx, "100", 7, 100)
		require.NoError(t, err)
		require.NotEmpty(t, records)
		target := records[len(records)-1]
		require.NoError(t, s.SetMemoryPinned(ctx, "100", target.ID, true))
		require.NoError(t, s.SetMemoryPinned(ctx, "100", target.ID, false))
		require.NoError(t, s.EditMemory(ctx, "100", target.ID, "Edited content.", nil))
		assert.ErrorIs(t, s.EditMemory(ctx, "100", 99999999, "x", nil), ErrMemoryNotFound)

		evictable, err := s.EvictableMemories(ctx, "100", 7, 0, 10)
		require.NoError(t, err)
		require.NotEmpty(t, evictable, "keep budget zero surfaces unpinned non-entity records")
		ids := make([]int64, 0, len(evictable))
		for _, record := range evictable {
			ids = append(ids, record.ID)
		}
		require.NoError(t, s.ReplaceMemoriesWithDigest(ctx, ids, MemoryRecord{
			GuildID: "100", CharacterID: 7, Kind: MemoryKindSummary, Content: "Digest of old events.", Importance: 0.3}))
		after, err := s.ListMemories(ctx, "100", 7, 100)
		require.NoError(t, err)
		for _, record := range after {
			assert.NotContains(t, ids, record.ID, "consolidated records are gone from reads")
		}

		require.NoError(t, s.DeleteMemory(ctx, "100", after[0].ID))
		assert.ErrorIs(t, s.DeleteMemory(ctx, "100", after[0].ID), ErrMemoryNotFound)
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
