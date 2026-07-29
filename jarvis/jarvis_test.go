package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProcess struct {
	done         chan struct{}
	stopOnSignal bool
	once         sync.Once
	mu           sync.Mutex
	err          error
	signals      []os.Signal
	killed       bool
}

func newFakeProcess(stopOnSignal bool) *fakeProcess {
	return &fakeProcess{done: make(chan struct{}), stopOnSignal: stopOnSignal}
}

func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
func (p *fakeProcess) Signal(signal os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, signal)
	p.mu.Unlock()
	if p.stopOnSignal {
		p.exit(nil)
	}
	return nil
}
func (p *fakeProcess) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.exit(nil)
	return nil
}

func (p *fakeProcess) exit(err error) {
	p.once.Do(func() {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(p.done)
	})
}

func (p *fakeProcess) signalCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.signals)
}

func (p *fakeProcess) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

type startCall struct {
	binary string
	args   []string
	env    []string
}

// children drives supervise with a fake process per binary.
type children struct {
	broker, store, worker, ingestor *fakeProcess
	mu                              sync.Mutex
	calls                           []startCall
	onIngestor                      func()
	onWorker                        func()
}

func newChildren(stopOnSignal bool) *children {
	return &children{
		broker:   newFakeProcess(stopOnSignal),
		store:    newFakeProcess(stopOnSignal),
		worker:   newFakeProcess(stopOnSignal),
		ingestor: newFakeProcess(stopOnSignal),
	}
}

func (c *children) start(binary string, args []string, env []string) (managedProcess, error) {
	c.mu.Lock()
	c.calls = append(c.calls, startCall{
		binary: binary,
		args:   slices.Clone(args),
		env:    slices.Clone(env),
	})
	c.mu.Unlock()
	switch binary {
	case natsBinary:
		return c.broker, nil
	case valkeyBinary:
		return c.store, nil
	case workerBinary:
		if c.onWorker != nil {
			c.onWorker()
		}
		return c.worker, nil
	default:
		if c.onIngestor != nil {
			c.onIngestor()
		}
		return c.ingestor, nil
	}
}

func (c *children) started() []startCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

func TestSupervisorStartsBrokerThenWorkerThenIngestor(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	kids := newChildren(true)
	kids.onIngestor = cancel

	cfg := testConfig(port)
	require.NoError(t, supervise(ctx, cfg, kids.start, server.Client()))

	calls := kids.started()
	require.Len(t, calls, 3)
	assert.Equal(t, natsBinary, calls[0].binary)
	assert.Equal(t, []string{
		"--config", cfg.natsConfig,
		"--port", cfg.natsPort,
		"--http_port", cfg.natsMonitorPort,
	}, calls[0].args)
	assert.Equal(t, workerBinary, calls[1].binary)
	assert.Equal(t, ingestorBinary, calls[2].binary)
	assert.Equal(t, 1, kids.broker.signalCount())
	assert.Equal(t, 1, kids.worker.signalCount())
	assert.Equal(t, 1, kids.ingestor.signalCount())
}

func TestSupervisorConfiguresChildrenThroughTheEnvironmentOnly(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	kids := newChildren(true)
	kids.onIngestor = cancel

	cfg := testConfig(port)
	require.NoError(t, supervise(ctx, cfg, kids.start, server.Client()))

	calls := kids.started()
	require.Len(t, calls, 3)
	worker, ingestor := calls[1], calls[2]

	assert.Empty(t, worker.args, "the worker must take its whole configuration from the environment")
	assert.Empty(t, ingestor.args, "the ingestor must take its whole configuration from the environment")
	assert.Contains(t, worker.env, "PORT="+port)
	assert.Contains(t, worker.env, "HOST=127.0.0.1")
	assert.Contains(t, worker.env, "NATS_URL="+cfg.clientURL())
	assert.Contains(t, ingestor.env, "PORT=8080")
	assert.Contains(t, ingestor.env, "NATS_URL="+cfg.clientURL())
}

// TestSupervisorPortFlagsReachNatsServer is the regression test for --nats-port and
// --nats-monitor-port having configured only where the supervisor looked. nats.conf
// pins 4222 and 8222, so unless both reach the command line the broker listens
// somewhere other than where the supervisor polls and where it points the children.
func TestSupervisorPortFlagsReachNatsServer(t *testing.T) {
	server, monitorPort := readyServer(t)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	kids := newChildren(true)
	kids.onIngestor = cancel

	cfg := testConfig(monitorPort)
	cfg.natsPort = "4333"
	require.NoError(t, supervise(ctx, cfg, kids.start, server.Client()))

	calls := kids.started()
	require.Len(t, calls, 3)
	assert.Equal(t, []string{
		"--config", cfg.natsConfig,
		"--port", "4333",
		"--http_port", monitorPort,
	}, calls[0].args, "both ports must reach nats-server, which overrides nats.conf with them")
	for _, child := range calls[1:] {
		assert.Contains(t, child.env, "NATS_URL=nats://127.0.0.1:4333",
			"the children must be pointed at the port the broker was actually given")
	}
}

// TestSupervisorLeavesValkeyOffByDefault pins the opt-in decision: supervising a
// Valkey nobody asked for would switch on usage metering and rate limiting for every
// existing deployment.
func TestSupervisorLeavesValkeyOffByDefault(t *testing.T) {
	// An operator already pointing the worker at an external Valkey through the
	// environment must pass through untouched.
	t.Setenv("VALKEY_ADDRESS", "external.example:6379")
	server, port := readyServer(t)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	kids := newChildren(true)
	kids.onIngestor = cancel

	require.NoError(t, supervise(ctx, testConfig(port), kids.start, server.Client()))

	calls := kids.started()
	require.Len(t, calls, 3)
	for _, call := range calls {
		assert.NotEqual(t, valkeyBinary, call.binary, "no Valkey may be started unless it was asked for")
	}
	assert.Equal(t, 0, kids.store.signalCount())
	assert.Contains(t, calls[1].env, "VALKEY_ADDRESS=external.example:6379")
	assert.NotContains(t, calls[1].env, "VALKEY_ENABLED=true")
}

// TestSupervisorDefersToAnExternalValkey is the combined image's documented deployment:
// VALKEY_ENABLED turns on the worker's metering while VALKEY_ADDRESS names a server
// outside the container. The image ships no valkey-server, so supervising one here would
// abort startup before NATS; overwriting the address would silently ignore the operator.
func TestSupervisorDefersToAnExternalValkey(t *testing.T) {
	t.Setenv("VALKEY_ADDRESS", "external.example:6379")
	t.Setenv("VALKEY_ENABLED", "true")
	server, port := readyServer(t)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	kids := newChildren(true)
	kids.onIngestor = cancel

	cfg := testConfig(port)
	cfg.valkeyEnabled = true
	cfg.valkeyAddress = "external.example:6379"
	// Nothing may be resolved or launched for it, so a path that cannot exist is fine.
	cfg.valkeyBinary = "/nonexistent/valkey-server"

	require.NoError(t, supervise(ctx, cfg, kids.start, server.Client()))

	calls := kids.started()
	require.Len(t, calls, 3, "the external server must not be supervised")
	for _, call := range calls {
		assert.NotEqual(t, "/nonexistent/valkey-server", call.binary)
	}
	assert.Contains(t, calls[1].env, "VALKEY_ADDRESS=external.example:6379",
		"the operator's address must survive")
	assert.NotContains(t, calls[1].env, "VALKEY_ADDRESS=127.0.0.1:"+cfg.valkeyPort,
		"loopback must not be forced over an external server")
}

// TestResolveSkipsValkeyForAnExternalServer is the regression test for the combined image
// failing to start at all: resolve ran before anything else, so a missing valkey-server
// aborted the whole supervisor even though no Valkey was going to be launched.
func TestResolveSkipsValkeyForAnExternalServer(t *testing.T) {
	cfg := defaultConfig()
	cfg.valkeyEnabled = true
	cfg.valkeyAddress = "external.example:6379"
	cfg.valkeyBinary = "/nonexistent/valkey-server"

	resolved, err := cfg.resolve()

	require.NoError(t, err, "an external Valkey needs no local binary")
	assert.Equal(t, "/nonexistent/valkey-server", resolved.valkeyBinary)
}

// TestSupervisesValkeyOnlyForALocalAddress pins which addresses this process owns. The
// loopback cases are the regression: a .env naming 127.0.0.1 is the local development
// setup asking for a supervised server, not an announcement that one is already running,
// and reading it as external left the worker with a connection refused.
func TestSupervisesValkeyOnlyForALocalAddress(t *testing.T) {
	for name, test := range map[string]struct {
		enabled bool
		address string
		want    bool
	}{
		"disabled":                    {false, "", false},
		"disabled with an address":    {false, "external.example:6379", false},
		"enabled without an address":  {true, "", true},
		"enabled with blank address":  {true, "   ", true},
		"enabled with loopback":       {true, "127.0.0.1:6379", true},
		"enabled with localhost":      {true, "localhost:6379", true},
		"enabled with IPv6 loopback":  {true, "[::1]:6379", true},
		"enabled with a bare host":    {true, "127.0.0.1", true},
		"enabled with another host":   {true, "external.example:6379", false},
		"enabled with a routable IP":  {true, "10.0.0.5:6379", false},
		"enabled with a bare foreign": {true, "external.example", false},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.valkeyEnabled, cfg.valkeyAddress = test.enabled, test.address
			assert.Equal(t, test.want, cfg.supervisesValkey())
		})
	}
}

// TestSupervisedValkeyUsesThePortItsAddressNames keeps the supervisor from redirecting
// the worker off the port its own configuration asked for: a loopback VALKEY_ADDRESS is
// honoured verbatim, and --valkey-port only decides when no address named a port.
func TestSupervisedValkeyUsesThePortItsAddressNames(t *testing.T) {
	for name, test := range map[string]struct{ address, want string }{
		"no address":        {"", "6379"},
		"loopback port":     {"127.0.0.1:6380", "6380"},
		"localhost port":    {"localhost:6390", "6390"},
		"host with no port": {"127.0.0.1", "6379"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.valkeyAddress = test.address
			assert.Equal(t, test.want, cfg.supervisedValkeyPort())
			assert.Equal(t, "127.0.0.1:"+test.want, cfg.supervisedValkeyAddress())
			assert.Contains(t, cfg.valkeyArgs(), test.want, "the server must listen where the worker is sent")
		})
	}
}

func TestSupervisorStartsValkeyBeforeTheWorker(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	valkeyPort := pongListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	kids := newChildren(true)
	kids.onIngestor = cancel

	cfg := testConfig(port)
	cfg.valkeyEnabled = true
	cfg.valkeyPort = valkeyPort
	require.NoError(t, supervise(ctx, cfg, kids.start, server.Client()))

	calls := kids.started()
	require.Len(t, calls, 4)
	assert.Equal(t, natsBinary, calls[0].binary)
	assert.Equal(t, valkeyBinary, calls[1].binary,
		"the worker dials Valkey while starting and fails closed, so it must already be up")
	assert.Equal(t, workerBinary, calls[2].binary)
	assert.Equal(t, ingestorBinary, calls[3].binary)
	assert.Equal(t, []string{
		"--bind", "127.0.0.1",
		"--port", valkeyPort,
		"--save", "",
		"--appendonly", "no",
		"--dir", os.TempDir(),
	}, calls[1].args, "Valkey is configured entirely on its command line, with persistence off")
	assert.Equal(t, 1, kids.store.signalCount(), "Valkey must stop with everything else")
}

func TestSupervisorPointsWorkerAtSupervisedValkey(t *testing.T) {
	// The supervised server must win over an inherited address, not sit beside it.
	t.Setenv("VALKEY_ADDRESS", "external.example:6379")
	server, port := readyServer(t)
	defer server.Close()
	valkeyPort := pongListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	kids := newChildren(true)
	kids.onIngestor = cancel

	cfg := testConfig(port)
	cfg.valkeyEnabled = true
	cfg.valkeyPort = valkeyPort
	require.NoError(t, supervise(ctx, cfg, kids.start, server.Client()))

	calls := kids.started()
	require.Len(t, calls, 4)
	worker := calls[2]
	assert.Contains(t, worker.env, "VALKEY_ENABLED=true")
	assert.Contains(t, worker.env, "VALKEY_ADDRESS=127.0.0.1:"+valkeyPort)
	addresses := 0
	for _, entry := range worker.env {
		if strings.HasPrefix(entry, "VALKEY_ADDRESS=") {
			addresses++
		}
	}
	assert.Equal(t, 1, addresses, "the worker must see exactly one VALKEY_ADDRESS")
}

func TestSupervisorDoesNotStartWorkerBeforeValkeyIsReady(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	kids := newChildren(true)

	cfg := testConfig(port)
	cfg.valkeyEnabled = true
	cfg.valkeyPort = "1"
	cfg.valkeyStartTimeout = 20 * time.Millisecond
	require.Error(t, supervise(context.Background(), cfg, kids.start, server.Client()))

	assert.Len(t, kids.started(), 2, "the worker must not start until Valkey answers")
	assert.Equal(t, 1, kids.store.signalCount(), "the Valkey that never came up must still be stopped")
}

// TestPingValkey covers the readiness probe, which is the one piece of this that a
// fake process cannot exercise: Valkey has no HTTP endpoint to poll.
func TestPingValkey(t *testing.T) {
	ctx := context.Background()

	t.Run("answers PONG", func(t *testing.T) {
		assert.True(t, pingValkey(ctx, "127.0.0.1:"+pongListener(t)))
	})

	t.Run("accepts but never replies", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			if conn, err := listener.Accept(); err == nil {
				_ = conn.Close()
			}
		}()
		assert.False(t, pingValkey(ctx, listener.Addr().String()),
			"a bound listener that serves no commands is not ready")
	})

	t.Run("nothing listening", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		require.NoError(t, listener.Close())
		assert.False(t, pingValkey(ctx, addr))
	})
}

func TestSupervisorDoesNotStartWorkerBeforeBrokerIsReady(t *testing.T) {
	kids := newChildren(true)
	cfg := testConfig("1")
	cfg.natsMonitorPort = "1"
	cfg.natsStartTimeout = 20 * time.Millisecond

	err := supervise(context.Background(), cfg, kids.start, &http.Client{Timeout: 5 * time.Millisecond})

	assert.ErrorContains(t, err, "wait for NATS readiness")
	assert.Len(t, kids.started(), 1)
	assert.Equal(t, 1, kids.broker.signalCount())
}

func TestSupervisorDoesNotStartIngestorBeforeWorkerIsReady(t *testing.T) {
	// The broker's monitoring endpoint is ready; the worker's is not.
	broker, brokerPort := readyServer(t)
	defer broker.Close()
	kids := newChildren(true)
	cfg := testConfig("1")
	cfg.natsMonitorPort = brokerPort
	cfg.workerStartTimeout = 20 * time.Millisecond

	err := supervise(context.Background(), cfg, kids.start, &http.Client{Timeout: 5 * time.Millisecond})

	assert.ErrorContains(t, err, "wait for worker readiness")
	assert.Len(t, kids.started(), 2)
	assert.Equal(t, 1, kids.worker.signalCount())
	assert.Equal(t, 1, kids.broker.signalCount())
}

func TestSupervisorStopsEveryoneWhenOneChildExits(t *testing.T) {
	for _, test := range []struct {
		name    string
		failing func(*children) *fakeProcess
		want    string
	}{
		{"worker", func(c *children) *fakeProcess { return c.worker }, "worker exited unexpectedly"},
		{"ingestor", func(c *children) *fakeProcess { return c.ingestor }, "ingestor exited unexpectedly"},
		{"NATS", func(c *children) *fakeProcess { return c.broker }, "NATS exited unexpectedly"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, port := readyServer(t)
			defer server.Close()
			kids := newChildren(true)
			failing := test.failing(kids)
			failing.stopOnSignal = false
			kids.onIngestor = func() { failing.exit(errors.New(test.name + " failed")) }

			err := supervise(context.Background(), testConfig(port), kids.start, server.Client())

			assert.ErrorContains(t, err, test.want)
			assert.ErrorContains(t, err, test.name+" failed")
			for _, child := range []*fakeProcess{kids.broker, kids.worker, kids.ingestor} {
				if child == failing {
					continue
				}
				assert.Equal(t, 1, child.signalCount(), "surviving children must be stopped")
			}
		})
	}
}

func TestSupervisorKillsChildrenAfterShutdownTimeout(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	kids := newChildren(false)
	kids.onIngestor = cancel
	cfg := testConfig(port)
	cfg.shutdownTimeout = 10 * time.Millisecond

	err := supervise(ctx, cfg, kids.start, server.Client())

	assert.ErrorContains(t, err, "did not stop")
	assert.True(t, kids.broker.wasKilled())
	assert.True(t, kids.worker.wasKilled())
	assert.True(t, kids.ingestor.wasKilled())
}

func TestChildEnvReplacesInheritedKeys(t *testing.T) {
	// Go resolves a duplicated key to its first occurrence, so an override that
	// is merely appended would lose to the inherited value.
	base := []string{"PORT=8080", "HOME=/home/nonroot", "NATS_URL=nats://elsewhere:4222", "MALFORMED"}
	env := childEnv(base, map[string]string{"PORT": "8081", "NATS_URL": "nats://127.0.0.1:4222"})

	assert.NotContains(t, env, "PORT=8080")
	assert.NotContains(t, env, "NATS_URL=nats://elsewhere:4222")
	assert.Contains(t, env, "PORT=8081")
	assert.Contains(t, env, "NATS_URL=nats://127.0.0.1:4222")
	assert.Contains(t, env, "HOME=/home/nonroot", "unrelated variables must pass through")
	assert.Contains(t, env, "MALFORMED", "entries without = must pass through")

	seen := map[string]int{}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		seen[key]++
	}
	assert.Equal(t, 1, seen["PORT"], "the child must see exactly one PORT")
	assert.Equal(t, 1, seen["NATS_URL"])
}

func TestSupervisorConfigDefaults(t *testing.T) {
	command := newRootCommand()
	for _, test := range []struct{ flag, want string }{
		{"port", "8080"},
		{"worker-port", "8081"},
		{"nats-port", "4222"},
		{"nats-monitor-port", "8222"},
		{"nats-binary", natsBinary},
		{"nats-config", natsConfig},
		{"valkey-binary", valkeyBinary},
		{"valkey-port", "6379"},
	} {
		got, err := command.Flags().GetString(test.flag)
		require.NoError(t, err)
		assert.Equal(t, test.want, got, test.flag)
	}
}

func TestSupervisorConfigValidation(t *testing.T) {
	valid := testConfig("8081")
	require.NoError(t, valid.validate())

	for _, test := range []struct {
		name   string
		mutate func(*supervisorConfig)
	}{
		{"port", func(c *supervisorConfig) { c.port = "" }},
		{"worker port", func(c *supervisorConfig) { c.workerPort = "" }},
		{"nats port", func(c *supervisorConfig) { c.natsPort = "" }},
		{"nats monitor port", func(c *supervisorConfig) { c.natsMonitorPort = "" }},
		{"nats binary", func(c *supervisorConfig) { c.natsBinary = "" }},
		{"nats config", func(c *supervisorConfig) { c.natsConfig = "" }},
		{"ingestor binary", func(c *supervisorConfig) { c.ingestorBinary = "" }},
		{"worker binary", func(c *supervisorConfig) { c.workerBinary = "" }},
		{"valkey binary", func(c *supervisorConfig) { c.valkeyBinary = "" }},
		{"valkey port", func(c *supervisorConfig) { c.valkeyPort = "" }},
		{"nats start timeout", func(c *supervisorConfig) { c.natsStartTimeout = 0 }},
		{"valkey start timeout", func(c *supervisorConfig) { c.valkeyStartTimeout = 0 }},
		{"worker start timeout", func(c *supervisorConfig) { c.workerStartTimeout = 0 }},
		{"shutdown timeout", func(c *supervisorConfig) { c.shutdownTimeout = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			assert.Error(t, cfg.validate())
		})
	}
}

func readyServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, []string{"/readyz", "/healthz"}, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	return server, port
}

// testConfig points both readiness probes at one server on port. Valkey is left off,
// matching the default, so the tests that do not care about it start three children.
func testConfig(port string) supervisorConfig {
	return supervisorConfig{
		port:               "8080",
		workerPort:         port,
		natsPort:           "4222",
		natsMonitorPort:    port,
		natsBinary:         natsBinary,
		natsConfig:         natsConfig,
		ingestorBinary:     ingestorBinary,
		workerBinary:       workerBinary,
		valkeyBinary:       valkeyBinary,
		valkeyPort:         "6379",
		natsStartTimeout:   time.Second,
		valkeyStartTimeout: time.Second,
		workerStartTimeout: time.Second,
		shutdownTimeout:    time.Second,
	}
}

// pongListener answers an inline PING on a loopback port, standing in for a Valkey
// that has finished starting. It returns the port.
func pongListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = conn.Write([]byte("+PONG\r\n"))
			}()
		}
	}()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	return port
}
