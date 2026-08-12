package memory

// The FR-1 acceptance eval: a scripted long conversation (default 500 turns) with
// annotated ground truth runs through the real pipeline — real Postgres + pgvector,
// real extraction model, real embeddings. At each checkpoint every fact so far is
// probed with a natural retrieval query and scored by keyword containment.
//
// Gates: ≥95% pinned-fact recall and ≥80% significant-event recall at the final
// checkpoint. Run on every model change:
//
//	bazel test //worker/pkg/memory:memory_eval --test_env=POSTGRES_DSN=... \
//	    --test_env=JARVIS_EVAL_MODEL_PROFILE=... --test_env=JARVIS_EVAL_PRIMARY_MODEL_PROFILE=... \
//	    --test_env=JARVIS_EVAL_EMBEDDING_MODEL_PROFILE=...

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
)

// evalFact is one ground-truth item: spoken at a turn, probed later by query, scored by
// any-keyword containment in the retrieved block.
type evalFact struct {
	atTurn   int
	pinned   bool
	spoken   string // what is said in conversation (empty for pinned facts added via the book)
	query    string
	keywords []string
}

// evalFacts spreads distinctive facts across the conversation. Keywords are proper
// nouns and figures an extractor that understood the scene will preserve.
var evalFacts = []evalFact{
	{atTurn: 5, spoken: "By the way, my name is Wren and I grew up on Isla Morrow.", query: "Where did I grow up?", keywords: []string{"Isla Morrow"}},
	{atTurn: 12, pinned: true, query: "What do I owe you?", keywords: []string{"ten crowns"}},
	{atTurn: 20, spoken: "Your ship is called the Grey Gull, right? She's beautiful.", query: "What is the ship called?", keywords: []string{"Grey Gull"}},
	{atTurn: 33, spoken: "I lost my brother Tomas to the sea three winters ago.", query: "What happened to my brother?", keywords: []string{"Tomas"}},
	{atTurn: 47, pinned: true, query: "What am I allergic to?", keywords: []string{"oysters"}},
	{atTurn: 61, spoken: "The harbormaster, old Bex, demanded forty crowns in docking fees.", query: "Who is the harbormaster and what did they demand?", keywords: []string{"Bex", "forty"}},
	{atTurn: 78, spoken: "We agreed to sail for Port Callow at the next spring tide.", query: "Where are we sailing next?", keywords: []string{"Port Callow"}},
	{atTurn: 95, pinned: true, query: "What is my favorite drink?", keywords: []string{"blackberry wine"}},
	{atTurn: 110, spoken: "The cargo manifest listed twelve barrels of saltpeter, not ten.", query: "What was wrong with the cargo manifest?", keywords: []string{"saltpeter", "twelve"}},
	{atTurn: 128, spoken: "A stranger in a green cloak was asking about you at the Drowned Lantern inn.", query: "Who was asking about you and where?", keywords: []string{"green cloak", "Drowned Lantern"}},
	{atTurn: 145, pinned: true, query: "What is the name of my cat?", keywords: []string{"Biscuit"}},
	{atTurn: 163, spoken: "I found a brass key hidden inside the broken compass.", query: "What was hidden in the compass?", keywords: []string{"brass key"}},
	{atTurn: 180, spoken: "The first mate, Sorrel, confessed she can't actually swim.", query: "What is the first mate's secret?", keywords: []string{"Sorrel", "swim"}},
	{atTurn: 199, pinned: true, query: "When is my birthday?", keywords: []string{"midsummer"}},
	{atTurn: 221, spoken: "The lighthouse on Wrack Point has been dark for nine nights.", query: "What is wrong at Wrack Point?", keywords: []string{"Wrack Point", "dark"}},
	{atTurn: 240, spoken: "We buried the strongbox under the third pier at low tide.", query: "Where did we bury the strongbox?", keywords: []string{"third pier"}},
	{atTurn: 262, pinned: true, query: "Which hand do I favor?", keywords: []string{"left"}},
	{atTurn: 285, spoken: "Captain Held of the Iron Petrel offered to buy the Grey Gull for two hundred crowns.", query: "Who offered to buy the ship?", keywords: []string{"Held", "Iron Petrel"}},
	{atTurn: 308, spoken: "The storm cracked the mainmast; the shipwright says a week of repairs.", query: "What did the storm damage?", keywords: []string{"mainmast"}},
	{atTurn: 330, spoken: "I promised Bex we'd smuggle no more redleaf through her harbor.", query: "What did we promise the harbormaster?", keywords: []string{"redleaf"}},
	{atTurn: 355, spoken: "My mother's ring — the silver one with the kraken crest — went missing today.", query: "What went missing?", keywords: []string{"kraken", "ring"}},
	{atTurn: 380, spoken: "Sorrel spotted the green-cloaked stranger boarding the Iron Petrel.", query: "Where was the stranger last seen?", keywords: []string{"Iron Petrel"}},
	{atTurn: 410, spoken: "The chart shows a reef called the Teeth guarding Port Callow's approach.", query: "What guards the approach to Port Callow?", keywords: []string{"Teeth", "reef"}},
	{atTurn: 445, spoken: "We signed the salvage contract with the Guild of Divers for a third of the take.", query: "What are the salvage terms?", keywords: []string{"third", "Divers"}},
	{atTurn: 470, spoken: "The password for the Guild's vault is 'saltmarsh'.", query: "What is the vault password?", keywords: []string{"saltmarsh"}},
}

// pinnedBookEntries is what the user pins through the memory book; the eval adds them
// at their turn exactly as pin_memory would.
var pinnedBookEntries = map[int]string{
	12:  "Wren owes Elyra ten crowns.",
	47:  "Wren is allergic to oysters.",
	95:  "Wren's favorite drink is blackberry wine.",
	145: "Wren's cat is named Biscuit.",
	199: "Wren's birthday is midsummer.",
	262: "Wren favors her left hand.",
}

// distractors is rotating filler chatter — the noise recall must survive.
var distractors = []string{
	"The rain hasn't let up all day.",
	"Pass me that rope, would you?",
	"*laughs* You always say that.",
	"The gulls are loud this morning.",
	"Did you eat yet? The inn does a decent stew.",
	"That merchant tried to overcharge me again.",
	"The deck needs scrubbing before nightfall.",
	"I heard a song about that once.",
	"Wind's turning east again.",
	"Careful with that crate.",
}

type evalCheckpoint struct {
	Turn          int      `json:"turn"`
	PinnedRecall  float64  `json:"pinned_recall"`
	EventRecall   float64  `json:"event_recall"`
	PinnedHits    int      `json:"pinned_hits"`
	PinnedTotal   int      `json:"pinned_total"`
	EventHits     int      `json:"event_hits"`
	EventTotal    int      `json:"event_total"`
	Misses        []string `json:"misses,omitempty"`
	RetrieveP99MS int64    `json:"retrieve_p99_ms"`
}

func TestMemoryRecallAt500Turns(t *testing.T) {
	if manualTestOptions.postgresDSN == "" || len(manualTestOptions.evalModelProfiles) == 0 ||
		strings.TrimSpace(manualTestOptions.evalPrimaryModelProfile) == "" {
		t.Skip("set POSTGRES_DSN, JARVIS_EVAL_MODEL_PROFILE, and JARVIS_EVAL_PRIMARY_MODEL_PROFILE to run the memory evaluation")
	}
	ctx := context.Background()
	handler, err := genai.New(ctx, genai.Config{
		ProjectID: manualTestOptions.evalProjectID, Location: manualTestOptions.evalLocation,
		GoogleAIAPIKey: manualTestOptions.googleAIAPIKey, OpenRouterAPIKey: manualTestOptions.openRouterAPIKey,
		NVIDIAAPIKey:  manualTestOptions.nvidiaAPIKey,
		ModelProfiles: manualTestOptions.evalModelProfiles, PrimaryModelProfile: manualTestOptions.evalPrimaryModelProfile,
		EmbeddingModelProfile: manualTestOptions.evalEmbeddingProfile,
		MaxOutputTokens:       genai.DefaultMaxOutputTokens,
	})
	if err != nil {
		t.Fatalf("create evaluation handler: %v", err)
	}
	defer handler.Close()

	persistent, err := store.Open(ctx, store.Config{
		Driver: store.DriverPostgres, PostgresDSN: manualTestOptions.postgresDSN,
		Defaults: evalGuildDefaults(),
	})
	if err != nil {
		t.Fatalf("open evaluation store: %v", err)
	}
	defer persistent.Close()

	pipelineConfig := Config{Store: persistent, Registry: handler.Registry(), SummarizeTurns: 20}
	if embedder := handler.Embedder(); embedder != nil {
		pipelineConfig.Embedder = embedder
	}
	pipeline := New(pipelineConfig)

	// A fresh guild per run isolates the eval from previous runs without truncation.
	guildID := strconv.FormatInt(time.Now().UnixNano(), 10)
	channelID := strconv.FormatInt(time.Now().UnixNano()+1, 10)
	const characterID = int64(1)
	baseID := time.Now().UnixNano()

	totalTurns := manualTestOptions.evalTurns
	factsByTurn := map[int]evalFact{}
	for _, fact := range evalFacts {
		if fact.atTurn <= totalTurns {
			factsByTurn[fact.atTurn] = fact
		}
	}
	checkpoints := map[int]struct{}{100: {}, totalTurns / 2: {}, totalTurns: {}}
	var results []evalCheckpoint

	for turn := 1; turn <= totalTurns; turn++ {
		text := distractors[turn%len(distractors)]
		if fact, ok := factsByTurn[turn]; ok && fact.spoken != "" {
			text = fact.spoken
		}
		author := &discordgo.User{ID: "30000000000000001", Username: "wren"}
		if turn%2 == 0 {
			author = &discordgo.User{ID: "30000000000000002", Username: "Elyra", Bot: true}
		}
		if err := persistent.Record(ctx, &discordgo.Message{
			ID: strconv.FormatInt(baseID+int64(turn), 10), ChannelID: channelID, GuildID: guildID,
			Content: text, Timestamp: time.Now(), Author: author,
		}, 14); err != nil {
			t.Fatalf("record turn %d: %v", turn, err)
		}
		if entry, ok := pinnedBookEntries[turn]; ok {
			if err := pipeline.Add(ctx, guildID, characterID, entry, true); err != nil {
				t.Fatalf("pin fact at turn %d: %v", turn, err)
			}
		}
		// Deterministic summarization cadence instead of NoteTurn's async trigger.
		if turn%pipeline.cfg.SummarizeTurns == 0 || turn == totalTurns {
			pipeline.summarize(guildID, channelID, characterID, "")
		}
		if _, ok := checkpoints[turn]; ok {
			results = append(results, probeRecall(t, ctx, pipeline, guildID, characterID, turn))
		}
	}

	writeEvalResults(t, results)
	final := results[len(results)-1]
	if final.PinnedRecall < 0.95 {
		t.Errorf("pinned recall %.2f below the 0.95 gate: misses %v", final.PinnedRecall, final.Misses)
	}
	if final.EventRecall < 0.80 {
		t.Errorf("event recall %.2f below the 0.80 gate: misses %v", final.EventRecall, final.Misses)
	}
}

func probeRecall(t *testing.T, ctx context.Context, pipeline *Pipeline, guildID string, characterID int64, upToTurn int) evalCheckpoint {
	t.Helper()
	checkpoint := evalCheckpoint{Turn: upToTurn}
	var worst time.Duration
	for _, fact := range evalFacts {
		if fact.atTurn > upToTurn {
			continue
		}
		started := time.Now()
		block := pipeline.Retrieve(ctx, guildID, characterID, fact.query, 4000)
		if elapsed := time.Since(started); elapsed > worst {
			worst = elapsed
		}
		hit := false
		for _, keyword := range fact.keywords {
			hit = hit || strings.Contains(strings.ToLower(block), strings.ToLower(keyword))
		}
		if fact.pinned {
			checkpoint.PinnedTotal++
			if hit {
				checkpoint.PinnedHits++
			}
		} else {
			checkpoint.EventTotal++
			if hit {
				checkpoint.EventHits++
			}
		}
		if !hit {
			checkpoint.Misses = append(checkpoint.Misses, fact.query)
		}
	}
	if checkpoint.PinnedTotal > 0 {
		checkpoint.PinnedRecall = float64(checkpoint.PinnedHits) / float64(checkpoint.PinnedTotal)
	}
	if checkpoint.EventTotal > 0 {
		checkpoint.EventRecall = float64(checkpoint.EventHits) / float64(checkpoint.EventTotal)
	}
	checkpoint.RetrieveP99MS = worst.Milliseconds()
	t.Logf("checkpoint %d: pinned %.2f (%d/%d) events %.2f (%d/%d) worst retrieve %dms",
		upToTurn, checkpoint.PinnedRecall, checkpoint.PinnedHits, checkpoint.PinnedTotal,
		checkpoint.EventRecall, checkpoint.EventHits, checkpoint.EventTotal, checkpoint.RetrieveP99MS)
	return checkpoint
}

func writeEvalResults(t *testing.T, results []evalCheckpoint) {
	t.Helper()
	directory := strings.TrimSpace(manualTestOptions.evalOutputDirectory)
	if directory == "" {
		directory = t.TempDir()
	}
	path := filepath.Join(directory, "memory-"+time.Now().UTC().Format("20060102-150405")+".jsonl")
	output, err := os.Create(path)
	if err != nil {
		t.Fatalf("create evaluation output: %v", err)
	}
	defer output.Close()
	for _, checkpoint := range results {
		line, _ := json.Marshal(checkpoint)
		_, _ = output.Write(append(line, '\n'))
	}
	t.Logf("memory evaluation records: %s", path)
}

func evalGuildDefaults() config.GuildConfig {
	return config.GuildConfig{Settings: config.ServerSettings{
		Prompt: "eval", ThreadMessages: 15, ParentMessages: 10, ChannelMessages: 8,
		HistoryRunes: 4000, MaxOutputTokens: 1024, MessageTimeout: time.Minute,
		MessageRetentionDays: 14, ReasoningEffort: llm.ReasoningLow,
	}}
}
