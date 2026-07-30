package main

import (
	"os"
	"os/exec"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/justinswe/std/errors"
)

// Where the combined image places each process the supervisor starts.
//
// The image ships nothing at valkeyBinary: no upstream valkey-server build runs on
// the distroless base, and Valkey is opt-in, so the image expects an external one.
// The path stays the default anyway, because locate only permits a fallback for a
// flag left where it started.
const (
	natsBinary     = "/app/nats-server"
	natsConfig     = "/app/nats.conf"
	ingestorBinary = "/app/ingestor"
	workerBinary   = "/app/worker"
	valkeyBinary   = "/app/valkey-server"
)

// Where Bazel stages the same artifacts for a local `bazel run //jarvis`.
const (
	natsBinaryRunfiles     = "jarvis/nats-server"
	natsConfigRunfiles     = "jarvis/jarvis/nats.conf"
	ingestorBinaryRunfiles = "jarvis/ingestor/ingestor_/ingestor"
	workerBinaryRunfiles   = "jarvis/worker/worker_/worker"
	valkeyBinaryRunfiles   = "jarvis/valkey-server"
)

// resolve fills in the path of every child the supervisor starts, so that a
// missing one is reported before any process is launched.
func (cfg supervisorConfig) resolve() (supervisorConfig, error) {
	for _, child := range []struct {
		name                        string
		path                        *string
		containerPath, runfilesPath string
	}{
		{"nats-server", &cfg.natsBinary, natsBinary, natsBinaryRunfiles},
		{"nats.conf", &cfg.natsConfig, natsConfig, natsConfigRunfiles},
		{"ingestor", &cfg.ingestorBinary, ingestorBinary, ingestorBinaryRunfiles},
		{"worker", &cfg.workerBinary, workerBinary, workerBinaryRunfiles},
	} {
		path, err := locate(*child.path, child.containerPath, child.runfilesPath, child.name)
		if err != nil {
			return cfg, err
		}
		*child.path = path
	}
	// Only when it will actually be started. Resolving unconditionally would break every
	// run on a machine with no valkey-server over a child the supervisor was never going
	// to launch: any Mac that has not installed one, and the combined image, which ships
	// no valkey-server and expects --valkey-address to name one outside the container.
	if cfg.supervisesValkey() {
		path, err := locateValkey(cfg.valkeyBinary)
		if err != nil {
			return cfg, err
		}
		cfg.valkeyBinary = path
	}
	return cfg, nil
}

// locateValkey resolves valkey-server, falling back to one on PATH.
//
// Upstream publishes no macOS build, so on a developer Mac there is nothing for Bazel
// to stage into the runfiles tree and `brew install valkey` is the supported route.
// The PATH rung stays out of locate itself: every other child is vendored for every
// platform, and letting one of those quietly pick up a host binary would cost more in
// confusion than it saves.
func locateValkey(configured string) (string, error) {
	path, err := locate(configured, valkeyBinary, valkeyBinaryRunfiles, "valkey-server")
	if err == nil {
		return path, nil
	}
	// The same rule locate applies to its own fallback: an operator who named a path
	// explicitly gets that path or an error, never some other server found elsewhere.
	if configured != valkeyBinary {
		return "", err
	}
	onPath, lookErr := exec.LookPath("valkey-server")
	if lookErr != nil {
		// Names both ways out, because the two callers who reach here are different
		// people: a developer wanting a local server, and an operator of an image that
		// ships none and should be pointing at one outside the container.
		return "", errors.Wrap(err,
			"or on PATH; install one (brew install valkey) or point --valkey-address at a server on another host")
	}
	return onPath, nil
}

// locate resolves one child artifact to a path that exists.
//
// An operator who points a flag somewhere explicit gets that path or an error,
// never a silent fallback to the bundled copy. Left at its default, the child is
// looked for in the image layout and then in the Bazel runfiles tree: the image
// ships every child under /app and is built without runfiles, and a `bazel run`
// has no /app, so neither needs to know which one it is.
func locate(configured, containerPath, runfilesPath, name string) (string, error) {
	if exists(configured) {
		return configured, nil
	}
	if configured != containerPath {
		return "", errors.Errorf("%s not found at %q", name, configured)
	}
	staged, err := runfiles.Rlocation(runfilesPath)
	if err == nil && exists(staged) {
		return staged, nil
	}
	return "", errors.Errorf(
		"%s not found at %q or in the Bazel runfiles tree at %q", name, configured, runfilesPath,
	)
}

// exists reports whether path is present on disk.
func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
