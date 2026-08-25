package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The memory subsystem is Postgres-only; on SQLite every method fails with
// ErrMemoryUnavailable and the .pg.sql migration is recorded but skipped. The live
// behavior runs under //store:postgres_live.
func TestMemoryUnavailableOnSQLite(t *testing.T) {
	s := memoryStore(t)
	ctx := context.Background()

	assert.False(t, s.MemoryAvailable())
	_, err := s.NoteMemoryTurn(ctx, "100", "555", 1)
	assert.ErrorIs(t, err, ErrMemoryUnavailable)
	_, _, err = s.ClaimMemoryWatermark(ctx, "555", 1, 0)
	assert.ErrorIs(t, err, ErrMemoryUnavailable)
	assert.ErrorIs(t, s.CommitMemory(ctx, "555", 1, 0, nil), ErrMemoryUnavailable)
	_, err = s.SearchMemory(ctx, "100", "555", 1, nil, "query", 8)
	assert.ErrorIs(t, err, ErrMemoryUnavailable)
	_, err = s.ListMemories(ctx, "100", "555", 1, 10)
	assert.ErrorIs(t, err, ErrMemoryUnavailable)
	assert.ErrorIs(t, s.DeleteMemory(ctx, "100", "555", 1), ErrMemoryUnavailable)
	_, err = s.StaleMemoryWatermarks(ctx, 0, 10)
	assert.ErrorIs(t, err, ErrMemoryUnavailable)
}

func TestVectorCodec(t *testing.T) {
	assert.Equal(t, "", formatVector(nil))
	assert.Equal(t, "[1,2.5,-3]", formatVector([]float32{1, 2.5, -3}))
	assert.Equal(t, "NULLIF($9, '')::vector", vectorParameter("$9"))
}
