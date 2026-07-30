package usage

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// These assertions are the schema contract documented in docs/valkey.md. Changing an
// expected string here is a breaking change for the external reader.
func TestKeysMatchTheDocumentedSchema(t *testing.T) {
	base := guildBase("jarvis", "9001")
	assert.Equal(t, "jarvis:v1:g:{9001}", base)
	assert.Equal(t, "jarvis:v1:g:{9001}:gcra", gcraKey(base))
	assert.Equal(t, "jarvis:v1:g:{9001}:req:29000000", requestKey(base, 29000000))
	assert.Equal(t, "jarvis:v1:g:{9001}:den:29000000", deniedKey(base, 29000000))
	assert.Equal(t, "jarvis:v1:g:{9001}:tok:483333", tokenKey(base, 483333))
	assert.Equal(t, "jarvis:v1:guilds:20138", guildIndexKey("jarvis", 20138))
	assert.Equal(t, "vertex/gemini|in", usageField("vertex/gemini", metricInput))
	assert.Equal(t, "*|total", usageField(aggregateModel, metricTotal))
}

func TestKeysHonorTheCustomPrefix(t *testing.T) {
	base := guildBase("staging", "42")
	assert.True(t, strings.HasPrefix(base, "staging:v1:"))
	assert.True(t, strings.HasPrefix(guildIndexKey("staging", 1), "staging:v1:"))
}

// Every key a script derives for one guild must carry the identical hash tag, or a
// cluster client rejects the script for touching cross-slot keys.
func TestEveryGuildKeySharesOneHashTag(t *testing.T) {
	base := guildBase("jarvis", "9001")
	for _, key := range []string{base, gcraKey(base), requestKey(base, 1), deniedKey(base, 1), tokenKey(base, 1)} {
		assert.Contains(t, key, "{9001}", "key %q must carry the guild hash tag", key)
		assert.Equal(t, 1, strings.Count(key, "{"), "key %q must carry exactly one hash tag", key)
	}
}

// TestScriptsBuildTheSameNamesAsKeysGo is the drift guard for the one duplication this
// package cannot avoid: the scripts must derive bucket names from the server's own TIME,
// so they concatenate the schema in Lua while keys.go states it in Go and docs/valkey.md
// publishes it to an external reader. Without this, the Lua could change and every other
// test here would still pass, because none of them run it.
func TestScriptsBuildTheSameNamesAsKeysGo(t *testing.T) {
	base := guildBase("jarvis", "9001")
	for name, test := range map[string]struct {
		goKey  string
		script string
	}{
		"request bucket": {requestKey(base, 7), allowScriptBody},
		"denial bucket":  {deniedKey(base, 7), allowScriptBody},
		"token bucket":   {tokenKey(base, 7), flushScriptBody},
	} {
		t.Run(name, func(t *testing.T) {
			// ":req:" from "jarvis:v1:g:{9001}:req:7" — the piece the Lua concatenates.
			infix := strings.TrimSuffix(strings.TrimPrefix(test.goKey, base), "7")
			assert.Contains(t, test.script, "'"+infix+"'",
				"the Lua must build %s with the same separator keys.go uses", name)
		})
	}
	// The token budget is enforced against the aggregate field the flush script writes,
	// so those two names agreeing is what makes the limit real rather than decorative.
	assert.Contains(t, allowScriptBody, "'"+usageField(aggregateModel, metricTotal)+"'")
	assert.Contains(t, flushScriptBody, "'"+aggregateModel+fieldSeparator+"'")
	// Reserved metadata a reader is told to skip when summing counters.
	assert.Contains(t, allowScriptBody, "'"+tierField+"'")
	assert.Contains(t, flushScriptBody, "'"+tierField+"'")
}

func TestTierFieldIsReserved(t *testing.T) {
	assert.True(t, strings.HasPrefix(tierField, reservedFieldPrefix))
	assert.NotContains(t, tierField, fieldSeparator)
}
