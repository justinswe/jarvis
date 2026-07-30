package usage

import (
	"strconv"
	"sync"
)

const (
	// maxTrackedGuilds bounds aggregator memory between flushes.
	maxTrackedGuilds = 4096
	// maxModelsPerGuild bounds token field cardinality for one guild.
	maxModelsPerGuild = 64
)

// meterKey identifies one guild, tier, and model whose token deltas accumulate together.
type meterKey struct {
	guildID string
	tier    string
	model   string
}

// meterDelta is one accumulation window's token counts.
type meterDelta struct {
	input     int64
	output    int64
	reasoning int64
	total     int64
	calls     int64
}

// meter accumulates token usage in memory so metering never blocks a request.
type meter struct {
	mu      sync.Mutex
	deltas  map[meterKey]*meterDelta
	models  map[string]int
	dropped int
}

// newMeter creates an empty token accumulator.
func newMeter() *meter {
	return &meter{deltas: make(map[meterKey]*meterDelta), models: make(map[string]int)}
}

// add accumulates one report, dropping it when the guild or model caps are exceeded.
func (m *meter) add(key meterKey, delta meterDelta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, tracked := m.deltas[key]
	if !tracked {
		if _, known := m.models[key.guildID]; !known && len(m.models) >= maxTrackedGuilds {
			m.dropped++
			return
		}
		if m.models[key.guildID] >= maxModelsPerGuild {
			m.dropped++
			return
		}
		existing = &meterDelta{}
		m.deltas[key] = existing
		m.models[key.guildID]++
	}
	existing.input += delta.input
	existing.output += delta.output
	existing.reasoning += delta.reasoning
	existing.total += delta.total
	existing.calls += delta.calls
}

// drain swaps out the accumulated deltas and reports how many were dropped.
func (m *meter) drain() (map[meterKey]*meterDelta, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	deltas, dropped := m.deltas, m.dropped
	m.deltas, m.models, m.dropped = make(map[meterKey]*meterDelta), make(map[string]int), 0
	return deltas, dropped
}

// guildBatch is one guild's flush payload: a slot anchor plus token delta triples.
type guildBatch struct {
	guildID string
	base    string
	tier    string
	args    []string
}

// batches groups drained deltas into one flush payload per guild and tier.
func batches(prefix string, deltas map[meterKey]*meterDelta) []guildBatch {
	grouped := make(map[meterKey]*guildBatch)
	ordered := make([]meterKey, 0, len(deltas))
	for key, delta := range deltas {
		group := meterKey{guildID: key.guildID, tier: key.tier}
		batch, seen := grouped[group]
		if !seen {
			batch = &guildBatch{guildID: key.guildID, base: guildBase(prefix, key.guildID), tier: key.tier}
			grouped[group] = batch
			ordered = append(ordered, group)
		}
		batch.args = append(batch.args, triples(key.model, delta)...)
	}
	result := make([]guildBatch, 0, len(ordered))
	for _, group := range ordered {
		result = append(result, *grouped[group])
	}
	return result
}

// triples renders one model's non-zero deltas as flat model/metric/delta script arguments.
func triples(model string, delta *meterDelta) []string {
	pairs := []struct {
		metric string
		value  int64
	}{
		{metricInput, delta.input},
		{metricOutput, delta.output},
		{metricReasoning, delta.reasoning},
		{metricTotal, delta.total},
		{metricCalls, delta.calls},
	}
	args := make([]string, 0, len(pairs)*3)
	for _, pair := range pairs {
		if pair.value == 0 {
			continue
		}
		args = append(args, model, pair.metric, strconv.FormatInt(pair.value, 10))
	}
	return args
}
