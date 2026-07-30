package usage

import (
	"strconv"

	"github.com/justinswe/jarvis/worker/pkg/valkeyconn"
)

const (
	// schemaVersion namespaces every key so the external reader contract can evolve.
	schemaVersion = "v1"
	// reservedFieldPrefix marks hash fields that carry metadata rather than counters.
	reservedFieldPrefix = "_"
	// tierField records the effective subscription tier alongside the counters it describes.
	tierField = reservedFieldPrefix + "tier"
	// aggregateModel is the reserved model name holding cross-model totals.
	aggregateModel = "*"
	// fieldSeparator joins a model identifier and its metric in token hash fields.
	fieldSeparator = "|"
)

// Token accounting metrics recorded per model.
const (
	metricInput     = "in"
	metricOutput    = "out"
	metricReasoning = "reason"
	metricTotal     = "total"
	metricCalls     = "calls"
)

// guildBase returns the hash-tagged prefix shared by every key for one guild.
//
// The hash tag is load-bearing: the admission script builds the request, denial, and
// token keys from this base in Lua, and valkey-go rejects cross-slot keys on a cluster
// client. Every key derived from this base must keep the tag intact.
func guildBase(prefix, guildID string) string {
	return prefix + ":" + schemaVersion + ":g:" + valkeyconn.HashTag(guildID)
}

// gcraKey returns the rate-limiter state key, which also anchors each script's slot.
func gcraKey(base string) string { return base + ":gcra" }

// requestKey returns the admitted-request hash for one guild-minute.
func requestKey(base string, minute int64) string {
	return base + ":req:" + strconv.FormatInt(minute, 10)
}

// deniedKey returns the denied-request hash for one guild-minute.
func deniedKey(base string, minute int64) string {
	return base + ":den:" + strconv.FormatInt(minute, 10)
}

// tokenKey returns the per-model token hash for one guild-hour.
func tokenKey(base string, hour int64) string {
	return base + ":tok:" + strconv.FormatInt(hour, 10)
}

// guildIndexKey returns the daily set enumerating guilds with recorded activity.
func guildIndexKey(prefix string, day int64) string {
	return prefix + ":" + schemaVersion + ":guilds:" + strconv.FormatInt(day, 10)
}

// usageField returns the token hash field holding one model's metric.
func usageField(model, metric string) string { return model + fieldSeparator + metric }
