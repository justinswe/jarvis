package usage

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeterAccumulatesRepeatedReports(t *testing.T) {
	meter := newMeter()
	key := meterKey{guildID: "g1", tier: "pro", model: "vertex/gemini"}
	meter.add(key, meterDelta{input: 10, output: 4, reasoning: 1, total: 15, calls: 1})
	meter.add(key, meterDelta{input: 5, output: 2, reasoning: 0, total: 7, calls: 1})

	deltas, dropped := meter.drain()
	assert.Zero(t, dropped)
	require.Len(t, deltas, 1)
	assert.Equal(t, meterDelta{input: 15, output: 6, reasoning: 1, total: 22, calls: 2}, *deltas[key])
}

func TestMeterDrainSwapsTheAccumulator(t *testing.T) {
	meter := newMeter()
	meter.add(meterKey{guildID: "g1", model: "m"}, meterDelta{total: 5, calls: 1})

	first, _ := meter.drain()
	require.Len(t, first, 1)
	second, _ := meter.drain()
	assert.Empty(t, second)
}

func TestMeterAccumulatesConcurrentReports(t *testing.T) {
	meter := newMeter()
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for round := 0; round < 100; round++ {
				meter.add(meterKey{guildID: "g1", model: "m"}, meterDelta{total: 1, calls: 1})
			}
		}()
	}
	wait.Wait()

	deltas, _ := meter.drain()
	require.Len(t, deltas, 1)
	assert.EqualValues(t, 800, deltas[meterKey{guildID: "g1", model: "m"}].total)
}

func TestMeterDropsAboveTheGuildCap(t *testing.T) {
	meter := newMeter()
	for guild := 0; guild < maxTrackedGuilds+10; guild++ {
		meter.add(meterKey{guildID: strconv.Itoa(guild), model: "m"}, meterDelta{total: 1})
	}

	deltas, dropped := meter.drain()
	assert.Len(t, deltas, maxTrackedGuilds)
	assert.Equal(t, 10, dropped)
}

func TestMeterDropsAboveThePerGuildModelCap(t *testing.T) {
	meter := newMeter()
	for model := 0; model < maxModelsPerGuild+5; model++ {
		meter.add(meterKey{guildID: "g1", model: strconv.Itoa(model)}, meterDelta{total: 1})
	}

	deltas, dropped := meter.drain()
	assert.Len(t, deltas, maxModelsPerGuild)
	assert.Equal(t, 5, dropped)
}

func TestBatchesGroupModelsByGuild(t *testing.T) {
	deltas := map[meterKey]*meterDelta{
		{guildID: "g1", tier: "pro", model: "a"}:  {input: 1, total: 1},
		{guildID: "g1", tier: "pro", model: "b"}:  {output: 2, total: 2},
		{guildID: "g2", tier: "free", model: "a"}: {calls: 3},
	}
	pending := batches("jarvis", deltas)
	require.Len(t, pending, 2)

	byGuild := make(map[string]guildBatch, len(pending))
	for _, batch := range pending {
		byGuild[batch.guildID] = batch
	}

	first := byGuild["g1"]
	assert.Equal(t, "jarvis:v1:g:{g1}", first.base)
	assert.Equal(t, "pro", first.tier)
	// Two models contributing two non-zero metrics each, as flat triples.
	assert.Len(t, first.args, 4*3)

	second := byGuild["g2"]
	assert.Equal(t, []string{"a", metricCalls, "3"}, second.args)
}

func TestTriplesOmitZeroMetrics(t *testing.T) {
	args := triples("vertex/gemini", &meterDelta{input: 7, total: 7})
	assert.Equal(t, []string{
		"vertex/gemini", metricInput, "7",
		"vertex/gemini", metricTotal, "7",
	}, args)
}
