package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocatePrefersTheConfiguredPath(t *testing.T) {
	present := filepath.Join(t.TempDir(), "nats-server")
	require.NoError(t, os.WriteFile(present, nil, 0o600))

	got, err := locate(present, natsBinary, "jarvis/does-not-exist", "nats-server")

	require.NoError(t, err)
	assert.Equal(t, present, got)
}

func TestLocateFallsBackToRunfiles(t *testing.T) {
	// /app exists only in the image, so this is the `bazel run` path.
	got, err := locate(workerBinary, workerBinary, workerBinaryRunfiles, "worker")

	require.NoError(t, err)
	assert.FileExists(t, got)
}

func TestLocateRejectsAMissingOperatorOverride(t *testing.T) {
	// A wrong --nats-binary must fail loudly rather than quietly starting the
	// bundled copy, which the runfiles tree would otherwise supply.
	_, err := locate("/nope/nats-server", natsBinary, natsBinaryRunfiles, "nats-server")

	require.Error(t, err)
	assert.ErrorContains(t, err, `nats-server not found at "/nope/nats-server"`)
	assert.NotContains(t, err.Error(), "runfiles", "an explicit path must not mention the fallback")
}

func TestLocateReportsBothLocationsWhenMissing(t *testing.T) {
	_, err := locate(natsBinary, natsBinary, "jarvis/nowhere/nats-server", "nats-server")

	require.Error(t, err)
	assert.ErrorContains(t, err, "nats-server not found")
	assert.ErrorContains(t, err, "/app/nats-server", "the error must name the configured path")
	assert.ErrorContains(t, err, "jarvis/nowhere/nats-server", "the error must name the runfiles path")
}

// TestResolveFindsEveryChildUnderBazel is the regression test for
// `bazel run //jarvis` failing with fork/exec on /app/nats-server: none of the
// container paths exist here, so every child must come from runfiles.
func TestResolveFindsEveryChildUnderBazel(t *testing.T) {
	resolved, err := defaultConfig().resolve()

	require.NoError(t, err)
	for name, path := range map[string]string{
		"nats-server": resolved.natsBinary,
		"nats.conf":   resolved.natsConfig,
		"ingestor":    resolved.ingestorBinary,
		"worker":      resolved.workerBinary,
	} {
		assert.FileExists(t, path, name)
	}
}

// TestResolveSkipsValkeyWhenDisabled keeps the default run working on a machine that
// has no valkey-server at all, which is any Mac that has not installed one: upstream
// publishes no macOS build, so resolving a child that was never going to be started
// would break `bazel run //jarvis`.
func TestResolveSkipsValkeyWhenDisabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.valkeyBinary = "/nope/valkey-server"

	resolved, err := cfg.resolve()

	require.NoError(t, err)
	assert.Equal(t, "/nope/valkey-server", resolved.valkeyBinary, "an unstarted child must be left alone")
}

func TestLocateValkeyRejectsAMissingOverride(t *testing.T) {
	// A wrong --valkey-binary must fail rather than reaching for whatever
	// valkey-server happens to be installed on the host.
	_, err := locateValkey("/nope/valkey-server")

	require.Error(t, err)
	assert.ErrorContains(t, err, `valkey-server not found at "/nope/valkey-server"`)
	assert.NotContains(t, err.Error(), "PATH", "an explicit path must not mention the fallback")
}

func TestExists(t *testing.T) {
	assert.False(t, exists(""))
	assert.False(t, exists(filepath.Join(t.TempDir(), "absent")))

	present := filepath.Join(t.TempDir(), "present")
	require.NoError(t, os.WriteFile(present, nil, 0o600))
	assert.True(t, exists(present))
}
