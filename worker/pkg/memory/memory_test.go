package memory

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMemoryStore is an in-memory Store honoring the claim and watermark contracts.
type fakeMemoryStore struct {
	mu            sync.Mutex
	lastMessageID int64
	pendingCount  int64
	claimedUntil  time.Time
	messages      []*discordgo.Message
	records       []store.MemoryRecord
	nextID        int64
	commitHook    func() error // fault injection between claim and commit
}

func (f *fakeMemoryStore) NoteMemoryTurn(_ context.Context, guildID, channelID string, characterID int64) (store.MemoryWatermark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingCount++
	return store.MemoryWatermark{GuildID: guildID, ChannelID: channelID, CharacterID: characterID,
		LastMessageID: f.lastMessageID, PendingCount: f.pendingCount, PendingSince: time.Now()}, nil
}

func (f *fakeMemoryStore) ClaimMemoryWatermark(_ context.Context, _ string, _ int64, ttl time.Duration) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Now().Before(f.claimedUntil) {
		return 0, false, nil
	}
	f.claimedUntil = time.Now().Add(ttl)
	return f.lastMessageID, true, nil
}

func (f *fakeMemoryStore) CommitMemory(_ context.Context, _ string, _ int64, lastMessageID int64, records []store.MemoryRecord) error {
	if f.commitHook != nil {
		if err := f.commitHook(); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range records {
		f.nextID++
		record.ID = f.nextID
		f.records = append(f.records, record)
	}
	f.lastMessageID = lastMessageID
	f.pendingCount = 0
	f.claimedUntil = time.Time{}
	return nil
}

func (f *fakeMemoryStore) MessagesAfter(_ context.Context, _, _ string, afterID int64, _ int) ([]*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []*discordgo.Message
	for _, m := range f.messages {
		if id, _ := strconv.ParseInt(m.ID, 10, 64); id > afterID {
			result = append(result, m)
		}
	}
	return result, nil
}

func (f *fakeMemoryStore) SearchMemory(_ context.Context, _ string, _ int64, _ []float32, _ string, _ int) ([]store.MemoryRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.MemoryRecord(nil), f.records...), nil
}

func (f *fakeMemoryStore) EvictableMemories(context.Context, string, int64, int, int) ([]store.MemoryRecord, error) {
	return nil, nil
}
func (f *fakeMemoryStore) ReplaceMemoriesWithDigest(context.Context, []int64, store.MemoryRecord) error {
	return nil
}
func (f *fakeMemoryStore) StaleMemoryWatermarks(context.Context, time.Duration, int) ([]store.MemoryWatermark, error) {
	return nil, nil
}
func (f *fakeMemoryStore) ListMemories(_ context.Context, _ string, _ int64, _ int) ([]store.MemoryRecord, error) {
	return append([]store.MemoryRecord(nil), f.records...), nil
}
func (f *fakeMemoryStore) SetMemoryPinned(context.Context, string, int64, bool) error { return nil }
func (f *fakeMemoryStore) EditMemory(context.Context, string, int64, string, []float32) error {
	return nil
}
func (f *fakeMemoryStore) DeleteMemory(context.Context, string, int64) error { return nil }
func (f *fakeMemoryStore) AddMemory(_ context.Context, record store.MemoryRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, record)
	return nil
}

type extractorHost struct{ response string }

func (h extractorHost) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Message: llm.TextMessage(llm.RoleAssistant, h.response)}, nil
}

type fakeEmbedder struct{ calls int }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, 2, 3}
	}
	return vectors, nil
}

const extractionResponse = `{"summary":"Elyra and Justin sailed to the harbor.","records":[
	{"kind":"event","content":"The cargo went missing.","importance":0.8},
	{"kind":"entity","entity_key":"Character:Elyra","content":"Elyra captains the Gull.","importance":0.7},
	{"kind":"bogus","content":"dropped"},
	{"kind":"plot","entity_key":"plot:missing-cargo","content":"","importance":0.5}]}`

func testRegistry(t *testing.T, host llm.Host) *llm.Registry {
	t.Helper()
	profile := llm.Profile{Name: "primary", Provider: llm.ProviderOpenRouter, ModelID: "m",
		Capabilities: llm.Capabilities{Tools: true, ToolChoice: true}}
	registry, err := llm.NewRegistry([]llm.Profile{profile}, map[string]llm.Host{"primary": host}, llm.Selection{Primary: "primary"})
	require.NoError(t, err)
	return registry
}

func storedMessage(id int64, author, content string) *discordgo.Message {
	return &discordgo.Message{ID: strconv.FormatInt(id, 10), Content: content,
		Timestamp: time.Unix(1700000000, 0), Author: &discordgo.User{Username: author}}
}

func testStoreWithMessages() *fakeMemoryStore {
	return &fakeMemoryStore{messages: []*discordgo.Message{
		storedMessage(101, "justin", "Let's sail at dawn."),
		storedMessage(102, "Elyra", "The tide waits for no one."),
	}}
}

func TestSummarizePassCommitsRecordsAndAdvancesWatermark(t *testing.T) {
	fake := testStoreWithMessages()
	embedder := &fakeEmbedder{}
	p := New(Config{Store: fake, Registry: testRegistry(t, extractorHost{response: extractionResponse}), Embedder: embedder})

	p.summarize("100", "555", 7, "free")

	require.Len(t, fake.records, 3, "summary + event + entity; invalid records dropped")
	byKind := map[string]store.MemoryRecord{}
	for _, record := range fake.records {
		byKind[record.Kind] = record
		assert.Equal(t, "100", record.GuildID)
		assert.Equal(t, int64(7), record.CharacterID)
		assert.Equal(t, int64(101), record.SourceFromID)
		assert.Equal(t, int64(102), record.SourceToID)
		assert.Equal(t, []float32{1, 2, 3}, record.Embedding)
	}
	assert.Contains(t, byKind["summary"].Content, "harbor")
	assert.Equal(t, "character:elyra", byKind["entity"].EntityKey, "entity keys normalize to lowercase")
	assert.Equal(t, int64(102), fake.lastMessageID, "watermark advanced to the newest summarized message")
	assert.Equal(t, int64(0), fake.pendingCount)
}

func TestSummarizeClaimContention(t *testing.T) {
	fake := testStoreWithMessages()
	fake.claimedUntil = time.Now().Add(time.Minute) // another replica holds the claim
	p := New(Config{Store: fake, Registry: testRegistry(t, extractorHost{response: extractionResponse})})

	p.summarize("100", "555", 7, "")
	assert.Empty(t, fake.records, "a lost claim writes nothing")
	assert.Equal(t, int64(0), fake.lastMessageID)
}

func TestSummarizeCrashRedoesRangeWithoutDuplicates(t *testing.T) {
	fake := testStoreWithMessages()
	fail := true
	fake.commitHook = func() error {
		if fail {
			return assert.AnError
		}
		return nil
	}
	p := New(Config{Store: fake, Registry: testRegistry(t, extractorHost{response: extractionResponse})})

	// First pass dies at commit: nothing persists, watermark stays.
	p.summarize("100", "555", 7, "")
	assert.Empty(t, fake.records)
	assert.Equal(t, int64(0), fake.lastMessageID)

	// Claim lapses, rerun succeeds over the same range, exactly once.
	fake.claimedUntil = time.Time{}
	fail = false
	p.summarize("100", "555", 7, "")
	assert.Len(t, fake.records, 3)
	assert.Equal(t, int64(102), fake.lastMessageID)
}

func TestSummarizeExtractionFailureAbandonsClaim(t *testing.T) {
	fake := testStoreWithMessages()
	p := New(Config{Store: fake, Registry: testRegistry(t, extractorHost{response: "not json at all"})})

	p.summarize("100", "555", 7, "")
	assert.Empty(t, fake.records, "unparseable extraction stores nothing")
	assert.Equal(t, int64(0), fake.lastMessageID, "watermark never advances past unsummarized turns")
}

type failingHost struct{ err error }

func (h failingHost) Generate(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, h.err
}

// TestSummarizeSkipsRangesThatCanNeverSummarize is why a permanent rejection advances the
// watermark: prod retried one image-only window every ten minutes for three days.
func TestSummarizeSkipsRangesThatCanNeverSummarize(t *testing.T) {
	t.Run("blank transcript", func(t *testing.T) {
		fake := &fakeMemoryStore{messages: []*discordgo.Message{storedMessage(101, "justin", ""), storedMessage(102, "Elyra", "  ")}}
		p := New(Config{Store: fake, Registry: testRegistry(t, failingHost{err: errors.New("must not be called")})})

		p.summarize("100", "555", 7, "")
		assert.Empty(t, fake.records)
		assert.Equal(t, int64(102), fake.lastMessageID)
	})
	t.Run("non-retryable provider error", func(t *testing.T) {
		fake := testStoreWithMessages()
		p := New(Config{Store: fake, Registry: testRegistry(t, failingHost{err: &llm.Error{Kind: llm.ErrorInvalidRequest}})})

		p.summarize("100", "555", 7, "")
		assert.Empty(t, fake.records)
		assert.Equal(t, int64(102), fake.lastMessageID)
	})
	t.Run("retryable provider error keeps the range", func(t *testing.T) {
		fake := testStoreWithMessages()
		p := New(Config{Store: fake, Registry: testRegistry(t, failingHost{err: &llm.Error{Kind: llm.ErrorTimeout}})})

		p.summarize("100", "555", 7, "")
		assert.Equal(t, int64(0), fake.lastMessageID)
	})
}

func TestRetrieveSkipsEmbeddingBlankQueries(t *testing.T) {
	embedder := &fakeEmbedder{}
	p := New(Config{Store: &fakeMemoryStore{}, Registry: testRegistry(t, extractorHost{}), Embedder: embedder})

	p.Retrieve(context.Background(), "100", 7, "  ", 3000)
	assert.Zero(t, embedder.calls)
}

func TestRetrieveRendersBudgetedBlock(t *testing.T) {
	fake := &fakeMemoryStore{records: []store.MemoryRecord{
		{ID: 1, Pinned: true, Kind: store.MemoryKindEvent, Content: "Justin owes Elyra ten crowns."},
		{ID: 2, Kind: store.MemoryKindEntity, EntityKey: "location:harbor", Content: "The harbor is\nfogbound."},
	}}
	embedder := &fakeEmbedder{}
	p := New(Config{Store: fake, Registry: testRegistry(t, extractorHost{}), Embedder: embedder})

	block := p.Retrieve(context.Background(), "100", 7, "what about the harbor?", 3000)
	assert.Contains(t, block, "- [pinned] Justin owes Elyra ten crowns.")
	assert.Contains(t, block, "- location:harbor: The harbor is fogbound.")
	assert.Equal(t, 1, embedder.calls)

	// Tight budgets truncate whole lines, never mid-record.
	tight := p.Retrieve(context.Background(), "100", 7, "query", 45)
	assert.Contains(t, tight, "[pinned]")
	assert.NotContains(t, tight, "harbor is fogbound")

	// Empty scope renders nothing.
	empty := New(Config{Store: &fakeMemoryStore{}, Registry: testRegistry(t, extractorHost{})})
	assert.Empty(t, empty.Retrieve(context.Background(), "100", 7, "query", 3000))
}

func TestNoteTurnTriggersOnThreshold(t *testing.T) {
	fake := testStoreWithMessages()
	p := New(Config{Store: fake, Registry: testRegistry(t, extractorHost{response: extractionResponse}), SummarizeTurns: 2})

	p.NoteTurn(context.Background(), Turn{GuildID: "100", ChannelID: "555", CharacterID: 7})
	assert.Empty(t, fake.records, "below threshold: no pass")

	p.NoteTurn(context.Background(), Turn{GuildID: "100", ChannelID: "555", CharacterID: 7})
	require.Eventually(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.records) > 0
	}, 5*time.Second, 10*time.Millisecond, "threshold reached: async pass runs")
}
