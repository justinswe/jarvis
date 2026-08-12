package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/justinswe/std/errors"
)

// ErrMemoryUnavailable reports that memory needs the PostgreSQL driver. The methods in
// this file are the one part of the store written in PostgreSQL-specific SQL (pgvector,
// tsvector, RETURNING) — the shared-dialect rule deliberately does not apply here.
var ErrMemoryUnavailable = errors.New("persistent memory requires the postgres store driver")

// Memory record kinds.
const (
	MemoryKindSummary      = "summary"
	MemoryKindEvent        = "event"
	MemoryKindEntity       = "entity"
	MemoryKindRelationship = "relationship"
	MemoryKindPlot         = "plot"
)

// MemoryRecord is one durable memory: a rolling summary, an event, or a piece of
// entity/relationship/plot state keyed by EntityKey.
type MemoryRecord struct {
	ID           int64
	GuildID      string
	CharacterID  int64
	ChannelID    string
	Kind         string
	EntityKey    string
	Content      string
	Pinned       bool
	Importance   float64
	Embedding    []float32
	SourceFromID int64
	SourceToID   int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// MemoryWatermark is one channel/character summarization cursor.
type MemoryWatermark struct {
	ChannelID     string
	CharacterID   int64
	GuildID       string
	LastMessageID int64
	PendingSince  time.Time
	// PendingCount is how many turns have landed behind the watermark — the database
	// counts so every replica sees one number.
	PendingCount int64
}

// MemoryAvailable reports whether this store backend supports persistent memory.
func (s *Store) MemoryAvailable() bool { return s.d.postgres }

func (s *Store) requireMemory() error {
	if !s.d.postgres {
		return ErrMemoryUnavailable
	}
	return nil
}

// NoteMemoryTurn marks new conversation behind the watermark and returns the cursor
// state the caller uses to decide whether to summarize now.
func (s *Store) NoteMemoryTurn(ctx context.Context, guildID, channelID string, characterID int64) (MemoryWatermark, error) {
	if err := s.requireMemory(); err != nil {
		return MemoryWatermark{}, err
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return MemoryWatermark{}, err
	}
	cid, err := snowflake(channelID)
	if err != nil {
		return MemoryWatermark{}, err
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO memory_watermarks (channel_id, character_id, guild_id, pending_since, pending_count, updated_at)
		VALUES ($1, $2, $3, extract(epoch from now())::bigint, 1, extract(epoch from now())::bigint)
		ON CONFLICT (channel_id, character_id) DO UPDATE SET
			guild_id = excluded.guild_id,
			pending_since = CASE WHEN memory_watermarks.pending_since = 0
				THEN excluded.pending_since ELSE memory_watermarks.pending_since END,
			pending_count = memory_watermarks.pending_count + 1,
			updated_at = excluded.updated_at
		RETURNING last_message_id, pending_since, pending_count`,
		cid, characterID, gid)
	watermark := MemoryWatermark{ChannelID: channelID, CharacterID: characterID, GuildID: guildID}
	var pending int64
	if err := row.Scan(&watermark.LastMessageID, &pending, &watermark.PendingCount); err != nil {
		return MemoryWatermark{}, errors.Wrap(err, "note memory turn")
	}
	watermark.PendingSince = time.Unix(pending, 0).UTC()
	return watermark, nil
}

// ClaimMemoryWatermark reserves one summarization pass. The condition turns on the lapse
// of the previous claim against the database clock — the same contract as reply claims,
// so replicas race one clock and a crashed worker's claim simply expires.
func (s *Store) ClaimMemoryWatermark(ctx context.Context, channelID string, characterID int64, ttl time.Duration) (int64, bool, error) {
	if err := s.requireMemory(); err != nil {
		return 0, false, err
	}
	cid, err := snowflake(channelID)
	if err != nil {
		return 0, false, err
	}
	row := s.db.QueryRowContext(ctx, `
		UPDATE memory_watermarks SET claimed_by = $1,
			claim_expires_at = extract(epoch from now())::bigint + $2
		WHERE channel_id = $3 AND character_id = $4
			AND claim_expires_at <= extract(epoch from now())::bigint
		RETURNING last_message_id`,
		s.replyOwner, int64(ttl/time.Second), cid, characterID)
	var lastMessageID int64
	switch err := row.Scan(&lastMessageID); {
	case err == nil:
		return lastMessageID, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	default:
		return 0, false, errors.Wrap(err, "claim memory watermark")
	}
}

// CommitMemory atomically writes one summarization pass: the new records, the advanced
// watermark, and the released claim. Atomicity is the zero-data-loss argument — a crash
// anywhere before commit leaves the watermark behind and the claim to lapse, and the
// range is redone cleanly.
func (s *Store) CommitMemory(ctx context.Context, channelID string, characterID, lastMessageID int64, records []MemoryRecord) error {
	if err := s.requireMemory(); err != nil {
		return err
	}
	cid, err := snowflake(channelID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin memory transaction")
	}
	defer func() { _ = tx.Rollback() }()
	for _, record := range records {
		if err := insertMemoryRecord(ctx, tx, record); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memory_watermarks SET last_message_id = $1, pending_since = 0, pending_count = 0,
			claimed_by = '', claim_expires_at = 0,
			updated_at = extract(epoch from now())::bigint
		WHERE channel_id = $2 AND character_id = $3`,
		lastMessageID, cid, characterID); err != nil {
		return errors.Wrap(err, "advance memory watermark")
	}
	return errors.Wrap(tx.Commit(), "commit memory")
}

// insertMemoryRecord writes one record: entity-keyed records replace their previous
// version, summaries deduplicate on their source range, everything else appends.
func insertMemoryRecord(ctx context.Context, tx *sql.Tx, record MemoryRecord) error {
	gid, err := snowflake(record.GuildID)
	if err != nil {
		return err
	}
	channelID, err := optionalSnowflake(record.ChannelID)
	if err != nil {
		return err
	}
	conflict := ""
	switch {
	case record.EntityKey != "":
		conflict = ` ON CONFLICT (guild_id, character_id, entity_key) WHERE deleted_at = 0 AND entity_key <> ''
			DO UPDATE SET content = excluded.content, importance = excluded.importance,
				embedding = excluded.embedding, kind = excluded.kind,
				source_from_id = excluded.source_from_id, source_to_id = excluded.source_to_id,
				updated_at = excluded.updated_at`
	case record.Kind == MemoryKindSummary && record.SourceToID != 0:
		conflict = ` ON CONFLICT (guild_id, character_id, channel_id, source_to_id) WHERE kind = 'summary' AND source_to_id <> 0
			DO NOTHING`
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO memory_records (guild_id, character_id, channel_id, kind, entity_key,
			content, pinned, importance, embedding, source_from_id, source_to_id,
			created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, `+vectorParameter("$9")+`, $10, $11,
			extract(epoch from now())::bigint, extract(epoch from now())::bigint)`+conflict,
		gid, record.CharacterID, channelID, record.Kind, record.EntityKey,
		record.Content, boolInt(record.Pinned), record.Importance, formatVector(record.Embedding),
		record.SourceFromID, record.SourceToID)
	return errors.Wrap(err, "write memory record")
}

// AddMemory writes one user- or pipeline-authored record outside a summarization pass.
func (s *Store) AddMemory(ctx context.Context, record MemoryRecord) error {
	if err := s.requireMemory(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin memory transaction")
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertMemoryRecord(ctx, tx, record); err != nil {
		return err
	}
	return errors.Wrap(tx.Commit(), "commit memory record")
}

const memoryColumns = `id, guild_id, character_id, channel_id, kind, entity_key, content,
	pinned, importance, source_from_id, source_to_id, created_at, updated_at`

// SearchMemory performs the hybrid retrieval: every pinned record, the vector
// neighborhood of the query when an embedding is supplied, a full-text fallback
// otherwise, and the newest rolling summary. The caller deduplicates and budgets.
func (s *Store) SearchMemory(ctx context.Context, guildID string, characterID int64, embedding []float32, query string, k int) ([]MemoryRecord, error) {
	if err := s.requireMemory(); err != nil {
		return nil, err
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return nil, err
	}
	scope := ` FROM memory_records WHERE guild_id = $1 AND character_id = $2 AND deleted_at = 0`
	var results []MemoryRecord
	appendQuery := func(sqlText string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, sqlText, args...)
		if err != nil {
			return errors.Wrap(err, "query memory records")
		}
		defer rows.Close()
		for rows.Next() {
			record, err := scanMemoryRecord(rows)
			if err != nil {
				return err
			}
			results = append(results, record)
		}
		return errors.Wrap(rows.Err(), "read memory records")
	}
	if err := appendQuery(`SELECT `+memoryColumns+scope+` AND pinned = 1 ORDER BY updated_at DESC LIMIT 100`, gid, characterID); err != nil {
		return nil, err
	}
	if len(embedding) > 0 {
		if err := appendQuery(`SELECT `+memoryColumns+scope+` AND embedding IS NOT NULL
			ORDER BY embedding <=> $3::vector LIMIT $4`, gid, characterID, formatVector(embedding), k); err != nil {
			return nil, err
		}
	} else if strings.TrimSpace(query) != "" {
		if err := appendQuery(`SELECT `+memoryColumns+scope+` AND tsv @@ plainto_tsquery('english', $3)
			ORDER BY ts_rank(tsv, plainto_tsquery('english', $3)) DESC LIMIT $4`, gid, characterID, query, k); err != nil {
			return nil, err
		}
	}
	if err := appendQuery(`SELECT `+memoryColumns+scope+` AND kind = 'summary'
		ORDER BY source_to_id DESC LIMIT 1`, gid, characterID); err != nil {
		return nil, err
	}
	return dedupeMemoryRecords(results), nil
}

// ListMemories returns a scope's records for the memory book, pinned first, newest next.
func (s *Store) ListMemories(ctx context.Context, guildID string, characterID int64, limit int) ([]MemoryRecord, error) {
	if err := s.requireMemory(); err != nil {
		return nil, err
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return nil, err
	}
	var results []MemoryRecord
	rows, err := s.db.QueryContext(ctx, `SELECT `+memoryColumns+`
		FROM memory_records WHERE guild_id = $1 AND character_id = $2 AND deleted_at = 0
		ORDER BY pinned DESC, updated_at DESC LIMIT $3`, gid, characterID, limit)
	if err != nil {
		return nil, errors.Wrap(err, "list memory records")
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanMemoryRecord(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, record)
	}
	return results, errors.Wrap(rows.Err(), "read memory records")
}

// SetMemoryPinned pins or unpins one record. Pinned records are never evicted.
func (s *Store) SetMemoryPinned(ctx context.Context, guildID string, id int64, pinned bool) error {
	return s.updateMemory(ctx, guildID, id, `pinned = $3`, boolInt(pinned))
}

// EditMemory rewrites one record's content and, when supplied, its embedding.
func (s *Store) EditMemory(ctx context.Context, guildID string, id int64, content string, embedding []float32) error {
	return s.updateMemory(ctx, guildID, id, `content = $3, embedding = `+vectorParameter("$4"), content, formatVector(embedding))
}

// DeleteMemory removes one record permanently.
func (s *Store) DeleteMemory(ctx context.Context, guildID string, id int64) error {
	if err := s.requireMemory(); err != nil {
		return err
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM memory_records WHERE guild_id = $1 AND id = $2`, gid, id)
	if err != nil {
		return errors.Wrap(err, "delete memory record")
	}
	return memoryRowError(result)
}

func (s *Store) updateMemory(ctx context.Context, guildID string, id int64, assignment string, args ...any) error {
	if err := s.requireMemory(); err != nil {
		return err
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE memory_records SET `+assignment+`,
		updated_at = extract(epoch from now())::bigint
		WHERE guild_id = $1 AND id = $2 AND deleted_at = 0`,
		append([]any{gid, id}, args...)...)
	if err != nil {
		return errors.Wrap(err, "update memory record")
	}
	return memoryRowError(result)
}

// ErrMemoryNotFound reports an operation on a record the scope does not have.
var ErrMemoryNotFound = errors.New("memory record not found")

func memoryRowError(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "count memory rows")
	}
	if affected == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// EvictableMemories returns the oldest, least important unpinned non-entity records
// beyond the keep budget, for consolidation into a digest.
func (s *Store) EvictableMemories(ctx context.Context, guildID string, characterID int64, keep, batch int) ([]MemoryRecord, error) {
	if err := s.requireMemory(); err != nil {
		return nil, err
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return nil, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_records
		WHERE guild_id = $1 AND character_id = $2 AND deleted_at = 0 AND pinned = 0 AND entity_key = ''`,
		gid, characterID).Scan(&count); err != nil {
		return nil, errors.Wrap(err, "count memory records")
	}
	if count <= keep {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+memoryColumns+`
		FROM memory_records
		WHERE guild_id = $1 AND character_id = $2 AND deleted_at = 0 AND pinned = 0 AND entity_key = ''
		ORDER BY importance ASC, updated_at ASC LIMIT $3`, gid, characterID, batch)
	if err != nil {
		return nil, errors.Wrap(err, "query evictable memory records")
	}
	defer rows.Close()
	var results []MemoryRecord
	for rows.Next() {
		record, err := scanMemoryRecord(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, record)
	}
	return results, errors.Wrap(rows.Err(), "read evictable memory records")
}

// ReplaceMemoriesWithDigest soft-deletes the consolidated records and writes their
// digest in one transaction.
func (s *Store) ReplaceMemoriesWithDigest(ctx context.Context, ids []int64, digest MemoryRecord) error {
	if err := s.requireMemory(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	gid, err := snowflake(digest.GuildID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin memory consolidation")
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertMemoryRecord(ctx, tx, digest); err != nil {
		return err
	}
	placeholders := make([]string, len(ids))
	args := []any{gid}
	for i, id := range ids {
		placeholders[i] = "$" + strconv.Itoa(i+2)
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_records
		SET deleted_at = extract(epoch from now())::bigint
		WHERE guild_id = $1 AND pinned = 0 AND id IN (`+strings.Join(placeholders, ",")+`)`, args...); err != nil {
		return errors.Wrap(err, "soft-delete consolidated memory records")
	}
	return errors.Wrap(tx.Commit(), "commit memory consolidation")
}

// StaleMemoryWatermarks lists cursors with unsummarized turns older than the deadline,
// so quiet channels still get flushed.
func (s *Store) StaleMemoryWatermarks(ctx context.Context, olderThan time.Duration, limit int) ([]MemoryWatermark, error) {
	if err := s.requireMemory(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT channel_id, character_id, guild_id, last_message_id, pending_since
		FROM memory_watermarks
		WHERE pending_since <> 0 AND pending_since <= extract(epoch from now())::bigint - $1
		ORDER BY pending_since ASC LIMIT $2`, int64(olderThan/time.Second), limit)
	if err != nil {
		return nil, errors.Wrap(err, "query stale memory watermarks")
	}
	defer rows.Close()
	var results []MemoryWatermark
	for rows.Next() {
		var watermark MemoryWatermark
		var cid, gid, pending int64
		if err := rows.Scan(&cid, &watermark.CharacterID, &gid, &watermark.LastMessageID, &pending); err != nil {
			return nil, errors.Wrap(err, "decode memory watermark")
		}
		watermark.ChannelID = strconv.FormatInt(cid, 10)
		watermark.GuildID = strconv.FormatInt(gid, 10)
		watermark.PendingSince = time.Unix(pending, 0).UTC()
		results = append(results, watermark)
	}
	return results, errors.Wrap(rows.Err(), "read stale memory watermarks")
}

func scanMemoryRecord(rows *sql.Rows) (MemoryRecord, error) {
	var record MemoryRecord
	var gid, channelID, createdAt, updatedAt int64
	var pinned int
	if err := rows.Scan(&record.ID, &gid, &record.CharacterID, &channelID, &record.Kind,
		&record.EntityKey, &record.Content, &pinned, &record.Importance,
		&record.SourceFromID, &record.SourceToID, &createdAt, &updatedAt); err != nil {
		return MemoryRecord{}, errors.Wrap(err, "decode memory record")
	}
	record.GuildID = strconv.FormatInt(gid, 10)
	record.ChannelID = formatOptionalID(channelID)
	record.Pinned = pinned != 0
	record.CreatedAt = time.Unix(createdAt, 0).UTC()
	record.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return record, nil
}

func dedupeMemoryRecords(records []MemoryRecord) []MemoryRecord {
	seen := make(map[int64]struct{}, len(records))
	deduped := records[:0]
	for _, record := range records {
		if _, ok := seen[record.ID]; ok {
			continue
		}
		seen[record.ID] = struct{}{}
		deduped = append(deduped, record)
	}
	return deduped
}

// formatVector renders a pgvector literal; empty input renders NULL through
// vectorParameter's NULLIF wrapping.
func formatVector(vector []float32) string {
	if len(vector) == 0 {
		return ""
	}
	parts := make([]string, len(vector))
	for i, value := range vector {
		parts[i] = strconv.FormatFloat(float64(value), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// vectorParameter casts a text parameter to vector, mapping the empty string to NULL so
// one statement serves embedded and embedding-less writes.
func vectorParameter(placeholder string) string {
	return "NULLIF(" + placeholder + ", '')::vector"
}
