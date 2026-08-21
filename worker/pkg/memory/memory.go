// Package memory implements the persistent-memory pipeline: async rolling
// summarization of recorded conversation into durable records, hybrid retrieval for
// prompt injection, and count-triggered consolidation. Durable state is the store's
// watermark, so a crash anywhere redoes the same range instead of losing it.
package memory

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

// Store is the persistence the pipeline drives; implemented by *store.Store under the
// postgres driver.
type Store interface {
	NoteMemoryTurn(ctx context.Context, guildID, channelID string, characterID int64) (store.MemoryWatermark, error)
	ClaimMemoryWatermark(ctx context.Context, channelID string, characterID int64, ttl time.Duration) (int64, bool, error)
	CommitMemory(ctx context.Context, channelID string, characterID, lastMessageID int64, records []store.MemoryRecord) error
	MessagesAfter(ctx context.Context, guildID, channelID string, afterID int64, limit int) ([]*discordgo.Message, error)
	SearchMemory(ctx context.Context, guildID string, characterID int64, embedding []float32, query string, k int) ([]store.MemoryRecord, error)
	EvictableMemories(ctx context.Context, guildID string, characterID int64, keep, batch int) ([]store.MemoryRecord, error)
	ReplaceMemoriesWithDigest(ctx context.Context, ids []int64, digest store.MemoryRecord) error
	StaleMemoryWatermarks(ctx context.Context, olderThan time.Duration, limit int) ([]store.MemoryWatermark, error)
	ListMemories(ctx context.Context, guildID string, characterID int64, limit int) ([]store.MemoryRecord, error)
	SetMemoryPinned(ctx context.Context, guildID string, id int64, pinned bool) error
	EditMemory(ctx context.Context, guildID string, id int64, content string, embedding []float32) error
	DeleteMemory(ctx context.Context, guildID string, id int64) error
	AddMemory(ctx context.Context, record store.MemoryRecord) error
}

// Embedder produces query and record vectors. Nil degrades retrieval to full-text.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Config configures one Pipeline.
type Config struct {
	Store    Store
	Registry *llm.Registry
	Embedder Embedder
	// UsageRecorder meters extraction and consolidation calls against guild tiers so
	// background summarization is not a free-tier cost leak. Nil skips metering.
	UsageRecorder genai.UsageRecorder
	// SummarizeTurns is how many turns accumulate behind the watermark before a
	// summarization pass runs.
	SummarizeTurns int
	// IdleFlush summarizes a quiet channel's tail after this long.
	IdleFlush time.Duration
	// ClaimTTL bounds one summarization claim.
	ClaimTTL time.Duration
	// MaxRecords bounds unpinned, non-entity records per scope before consolidation.
	MaxRecords int
}

// Pipeline runs summarization and retrieval. Safe for concurrent use.
type Pipeline struct {
	cfg Config
}

// New creates a Pipeline with defaults applied.
func New(cfg Config) *Pipeline {
	if cfg.SummarizeTurns <= 0 {
		cfg.SummarizeTurns = 20
	}
	if cfg.IdleFlush <= 0 {
		cfg.IdleFlush = 30 * time.Minute
	}
	if cfg.ClaimTTL <= 0 {
		cfg.ClaimTTL = 5 * time.Minute
	}
	if cfg.MaxRecords <= 0 {
		cfg.MaxRecords = 500
	}
	return &Pipeline{cfg: cfg}
}

// Start runs the stale-watermark flusher until ctx ends, so channels that go quiet
// before reaching the turn threshold still get their tail summarized.
func (p *Pipeline) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.flushStale(ctx)
			}
		}
	}()
}

func (p *Pipeline) flushStale(ctx context.Context) {
	stale, err := p.cfg.Store.StaleMemoryWatermarks(ctx, p.cfg.IdleFlush, 20)
	if err != nil {
		app.L().Warn("Stale memory watermark scan failed", zap.Error(err))
		return
	}
	for _, watermark := range stale {
		p.summarize(watermark.GuildID, watermark.ChannelID, watermark.CharacterID, "")
	}
}

// Turn describes one completed conversation turn.
type Turn struct {
	GuildID     string
	ChannelID   string
	CharacterID int64
	Tier        string
}

// NoteTurn records that a turn completed and starts an async summarization pass when
// the scope's backlog warrants one. It never blocks or fails the reply path.
func (p *Pipeline) NoteTurn(ctx context.Context, turn Turn) {
	watermark, err := p.cfg.Store.NoteMemoryTurn(ctx, turn.GuildID, turn.ChannelID, turn.CharacterID)
	if err != nil {
		app.L().Warn("Memory turn note failed", zap.String("channel_id", turn.ChannelID), zap.Error(err))
		return
	}
	due := watermark.PendingCount >= int64(p.cfg.SummarizeTurns) ||
		(watermark.PendingCount > 0 && time.Since(watermark.PendingSince) >= p.cfg.IdleFlush)
	if !due {
		return
	}
	go p.summarize(turn.GuildID, turn.ChannelID, turn.CharacterID, turn.Tier)
}

// summarize runs one claim → read → extract → embed → commit pass, then consolidation.
// Any failure abandons the claim to lapse; the range is redone on the next trigger.
func (p *Pipeline) summarize(guildID, channelID string, characterID int64, tier string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	lastID, claimed, err := p.cfg.Store.ClaimMemoryWatermark(ctx, channelID, characterID, p.cfg.ClaimTTL)
	if err != nil || !claimed {
		if err != nil {
			app.L().Warn("Memory claim failed", zap.String("channel_id", channelID), zap.Error(err))
		}
		return
	}
	messages, err := p.cfg.Store.MessagesAfter(ctx, guildID, channelID, lastID, 400)
	if err != nil {
		app.L().Warn("Memory message read failed", zap.String("channel_id", channelID), zap.Error(err))
		return
	}
	if len(messages) == 0 {
		// Retention beat the flush to the tail; clear the backlog so the trigger rests.
		if err := p.cfg.Store.CommitMemory(ctx, channelID, characterID, lastID, nil); err != nil {
			app.L().Warn("Memory watermark reset failed", zap.String("channel_id", channelID), zap.Error(err))
		}
		return
	}
	newLastID := snowflakeID(messages[len(messages)-1].ID)
	text := transcript(messages)
	if strings.TrimSpace(text) == "" {
		// Nothing but image-only or blank posts; a zero-text request is rejected upstream.
		p.skipRange(ctx, channelID, characterID, newLastID, "blank transcript")
		return
	}
	extraction, usage, err := p.extract(ctx, text)
	p.recordUsage(guildID, tier, usage)
	if err != nil {
		app.L().Warn("Memory extraction failed", zap.String("channel_id", channelID), zap.Error(err))
		if permanentProviderRejection(err) {
			// A permanent rejection would otherwise be retried on every tick forever.
			p.skipRange(ctx, channelID, characterID, newLastID, "non-retryable extraction error")
		}
		return
	}
	records := extraction.records(guildID, channelID, characterID, messages)
	if err := p.embedRecords(ctx, records); err != nil {
		app.L().Warn("Memory embedding failed; storing records for full-text retrieval",
			zap.String("channel_id", channelID), zap.Error(err))
	}
	if err := p.cfg.Store.CommitMemory(ctx, channelID, characterID, newLastID, records); err != nil {
		app.L().Warn("Memory commit failed", zap.String("channel_id", channelID), zap.Error(err))
		return
	}
	app.L().Info("Memory summarized", zap.String("guild_id", guildID), zap.String("channel_id", channelID),
		zap.Int64("character_id", characterID), zap.Int("messages", len(messages)), zap.Int("records", len(records)))
	p.consolidate(ctx, guildID, characterID, tier)
}

// permanentProviderRejection reports a provider error that no retry will change.
func permanentProviderRejection(err error) bool {
	var modelErr *llm.Error
	return errors.As(err, &modelErr) && !modelErr.Retryable()
}

// skipRange advances the watermark past a range that can never summarize.
func (p *Pipeline) skipRange(ctx context.Context, channelID string, characterID, lastID int64, reason string) {
	app.L().Warn("Memory range skipped", zap.String("channel_id", channelID), zap.String("reason", reason))
	if err := p.cfg.Store.CommitMemory(ctx, channelID, characterID, lastID, nil); err != nil {
		app.L().Warn("Memory watermark advance failed", zap.String("channel_id", channelID), zap.Error(err))
	}
}

// Retrieve returns the rendered memory block for prompt injection, empty when the scope
// has no records. Failures degrade to no memory, never to a failed reply.
func (p *Pipeline) Retrieve(ctx context.Context, guildID string, characterID int64, query string, budgetRunes int) string {
	records, err := p.cfg.Store.SearchMemory(ctx, guildID, characterID, p.embedOne(ctx, query), query, 8)
	if err != nil {
		if !errors.Is(err, store.ErrMemoryUnavailable) {
			app.L().Warn("Memory retrieval failed", zap.String("guild_id", guildID), zap.Error(err))
		}
		return ""
	}
	return renderRecords(records, budgetRunes)
}

// renderRecords renders retrieval results as one labeled block within the rune budget.
func renderRecords(records []store.MemoryRecord, budgetRunes int) string {
	if len(records) == 0 {
		return ""
	}
	var b strings.Builder
	for _, record := range records {
		line := "- "
		if record.Pinned {
			line += "[pinned] "
		}
		if record.EntityKey != "" {
			line += record.EntityKey + ": "
		}
		line += strings.ReplaceAll(record.Content, "\n", " ") + "\n"
		if b.Len()+len(line) > budgetRunes {
			break
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (p *Pipeline) embedRecords(ctx context.Context, records []store.MemoryRecord) error {
	if p.cfg.Embedder == nil || len(records) == 0 {
		return nil
	}
	texts := make([]string, len(records))
	for i, record := range records {
		texts[i] = truncateRunes(record.Content, 2000)
	}
	vectors, err := p.cfg.Embedder.Embed(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) != len(records) {
		return errors.Errorf("embedded %d of %d records", len(vectors), len(records))
	}
	for i := range records {
		records[i].Embedding = vectors[i]
	}
	return nil
}

// consolidate merges the oldest low-importance records into one digest when the scope
// outgrows its budget. Pinned and entity records are never candidates.
func (p *Pipeline) consolidate(ctx context.Context, guildID string, characterID int64, tier string) {
	evictable, err := p.cfg.Store.EvictableMemories(ctx, guildID, characterID, p.cfg.MaxRecords, 50)
	if err != nil || len(evictable) == 0 {
		if err != nil {
			app.L().Warn("Memory consolidation scan failed", zap.Error(err))
		}
		return
	}
	var lines []string
	ids := make([]int64, len(evictable))
	for i, record := range evictable {
		ids[i] = record.ID
		lines = append(lines, "- "+record.Content)
	}
	digestText, usage, err := p.complete(ctx, consolidationRubric, strings.Join(lines, "\n"), 1000)
	p.recordUsage(guildID, tier, usage)
	if err != nil {
		app.L().Warn("Memory consolidation failed", zap.Error(err))
		return
	}
	digests := []store.MemoryRecord{{
		GuildID: guildID, CharacterID: characterID, Kind: store.MemoryKindSummary,
		Content: strings.TrimSpace(digestText), Importance: 0.3,
	}}
	if err := p.embedRecords(ctx, digests); err != nil {
		app.L().Warn("Digest embedding failed; storing for full-text retrieval", zap.Error(err))
	}
	if err := p.cfg.Store.ReplaceMemoriesWithDigest(ctx, ids, digests[0]); err != nil {
		app.L().Warn("Memory digest write failed", zap.Error(err))
		return
	}
	app.L().Info("Memory consolidated", zap.String("guild_id", guildID),
		zap.Int64("character_id", characterID), zap.Int("merged", len(ids)))
}

// List reads a scope's memory book, pinned first.
func (p *Pipeline) List(ctx context.Context, guildID string, characterID int64, limit int) ([]store.MemoryRecord, error) {
	return p.cfg.Store.ListMemories(ctx, guildID, characterID, limit)
}

// Pin marks or unmarks one record as permanent; pinned records are never evicted.
func (p *Pipeline) Pin(ctx context.Context, guildID string, id int64, pinned bool) error {
	return p.cfg.Store.SetMemoryPinned(ctx, guildID, id, pinned)
}

// Edit rewrites one record's content and re-embeds it.
func (p *Pipeline) Edit(ctx context.Context, guildID string, id int64, content string) error {
	return p.cfg.Store.EditMemory(ctx, guildID, id, content, p.embedOne(ctx, content))
}

// Delete removes one record permanently.
func (p *Pipeline) Delete(ctx context.Context, guildID string, id int64) error {
	return p.cfg.Store.DeleteMemory(ctx, guildID, id)
}

// Add writes one user-authored record.
func (p *Pipeline) Add(ctx context.Context, guildID string, characterID int64, content string, pinned bool) error {
	return p.cfg.Store.AddMemory(ctx, store.MemoryRecord{
		GuildID: guildID, CharacterID: characterID, Kind: store.MemoryKindEvent,
		Content: strings.TrimSpace(content), Pinned: pinned, Importance: 0.8,
		Embedding: p.embedOne(ctx, content),
	})
}

// embedOne embeds one text, degrading to no vector on blank input or any failure.
func (p *Pipeline) embedOne(ctx context.Context, text string) []float32 {
	if p.cfg.Embedder == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	vectors, err := p.cfg.Embedder.Embed(ctx, []string{truncateRunes(text, 2000)})
	if err != nil || len(vectors) != 1 {
		app.L().Warn("Memory embedding failed; record stays full-text-searchable", zap.Error(err))
		return nil
	}
	return vectors[0]
}

func (p *Pipeline) recordUsage(guildID, tier string, usage *genai.UsageReport) {
	if usage == nil || p.cfg.UsageRecorder == nil {
		return
	}
	usage.GuildID, usage.Tier = guildID, tier
	p.cfg.UsageRecorder.RecordUsage(*usage)
}

// transcript renders messages in the same "[timestamp] Name: text" shape the prompt
// context uses.
func transcript(messages []*discordgo.Message) string {
	var b strings.Builder
	for _, m := range messages {
		if m == nil || m.Author == nil || strings.TrimSpace(m.Content) == "" {
			continue
		}
		name := m.Author.GlobalName
		if name == "" {
			name = m.Author.Username
		}
		b.WriteString("[" + m.Timestamp.UTC().Format(time.RFC3339) + "] " + name + ": " + m.Content + "\n")
	}
	return b.String()
}

func snowflakeID(id string) int64 {
	value, err := json.Number(id).Int64()
	if err != nil {
		return 0
	}
	return value
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
