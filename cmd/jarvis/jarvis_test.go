package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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
}

func TestSupervisorStartsReadyWorkerBeforeIngestor(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	worker := newFakeProcess(true)
	ingestor := newFakeProcess(true)
	ctx, cancel := context.WithCancel(context.Background())
	calls := []startCall{}
	start := func(binary string, args []string) (managedProcess, error) {
		calls = append(calls, startCall{binary: binary, args: append([]string(nil), args...)})
		if binary == workerBinary {
			return worker, nil
		}
		cancel()
		return ingestor, nil
	}
	cfg := testConfig(port)
	require.NoError(t, supervise(ctx, cfg, start, server.Client()))
	require.Len(t, calls, 2)
	assert.Equal(t, workerBinary, calls[0].binary)
	assert.Equal(t, []string{"--host=127.0.0.1", "--port=" + port}, calls[0].args)
	assert.Equal(t, ingestorBinary, calls[1].binary)
	assert.Equal(t, []string{"--port=8080", "--worker-url=http://127.0.0.1:" + port + processPath}, calls[1].args)
	assert.Equal(t, 1, worker.signalCount())
	assert.Equal(t, 1, ingestor.signalCount())
}

func TestSupervisorDoesNotStartIngestorBeforeWorkerIsReady(t *testing.T) {
	worker := newFakeProcess(true)
	calls := 0
	start := func(_ string, _ []string) (managedProcess, error) {
		calls++
		return worker, nil
	}
	cfg := testConfig("1")
	cfg.workerStartTimeout = 20 * time.Millisecond
	err := supervise(context.Background(), cfg, start, &http.Client{Timeout: 5 * time.Millisecond})
	assert.ErrorContains(t, err, "wait for worker readiness")
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, worker.signalCount())
}

func TestSupervisorStopsIngestorWhenWorkerExits(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	worker := newFakeProcess(false)
	ingestor := newFakeProcess(true)
	start := func(binary string, _ []string) (managedProcess, error) {
		if binary == workerBinary {
			return worker, nil
		}
		worker.exit(errors.New("worker failed"))
		return ingestor, nil
	}
	err := supervise(context.Background(), testConfig(port), start, server.Client())
	assert.ErrorContains(t, err, "worker exited unexpectedly")
	assert.ErrorContains(t, err, "worker failed")
	assert.Equal(t, 1, ingestor.signalCount())
}

func TestSupervisorStopsWorkerWhenIngestorExits(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	worker := newFakeProcess(true)
	ingestor := newFakeProcess(false)
	start := func(binary string, _ []string) (managedProcess, error) {
		if binary == workerBinary {
			return worker, nil
		}
		ingestor.exit(errors.New("ingestor failed"))
		return ingestor, nil
	}
	err := supervise(context.Background(), testConfig(port), start, server.Client())
	assert.ErrorContains(t, err, "ingestor exited unexpectedly")
	assert.ErrorContains(t, err, "ingestor failed")
	assert.Equal(t, 1, worker.signalCount())
}

func TestSupervisorKillsChildrenAfterShutdownTimeout(t *testing.T) {
	server, port := readyServer(t)
	defer server.Close()
	worker := newFakeProcess(false)
	ingestor := newFakeProcess(false)
	ctx, cancel := context.WithCancel(context.Background())
	start := func(binary string, _ []string) (managedProcess, error) {
		if binary == workerBinary {
			return worker, nil
		}
		cancel()
		return ingestor, nil
	}
	cfg := testConfig(port)
	cfg.shutdownTimeout = 10 * time.Millisecond
	err := supervise(ctx, cfg, start, server.Client())
	assert.ErrorContains(t, err, "did not stop")
	assert.True(t, worker.wasKilled())
	assert.True(t, ingestor.wasKilled())
}

func TestSupervisorConfigDefaults(t *testing.T) {
	command := newRootCommand()
	port, err := command.Flags().GetString("port")
	require.NoError(t, err)
	workerPort, err := command.Flags().GetString("worker-port")
	require.NoError(t, err)
	assert.Equal(t, "8080", port)
	assert.Equal(t, "8081", workerPort)
}

func readyServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/readyz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	return server, port
}

func testConfig(workerPort string) supervisorConfig {
	return supervisorConfig{
		port:               "8080",
		workerPort:         workerPort,
		workerStartTimeout: time.Second,
		shutdownTimeout:    time.Second,
	}
}
