package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

func runJarvis(parent context.Context, cfg supervisorConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	cfg, err := cfg.resolve()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return supervise(ctx, cfg, startProcess, &http.Client{Timeout: 500 * time.Millisecond})
}

// validate reports whether the supervisor can start every child.
func (cfg supervisorConfig) validate() error {
	for _, required := range []struct{ name, value string }{
		{"port", cfg.port},
		{"worker port", cfg.workerPort},
		{"NATS port", cfg.natsPort},
		{"NATS monitor port", cfg.natsMonitorPort},
		{"NATS binary", cfg.natsBinary},
		{"NATS config", cfg.natsConfig},
		{"ingestor binary", cfg.ingestorBinary},
		{"worker binary", cfg.workerBinary},
		{"Valkey binary", cfg.valkeyBinary},
		{"Valkey port", cfg.valkeyPort},
	} {
		if required.value == "" {
			return errors.Errorf("%s is required", required.name)
		}
	}
	for _, required := range []struct {
		name  string
		value time.Duration
	}{
		{"NATS start timeout", cfg.natsStartTimeout},
		{"Valkey start timeout", cfg.valkeyStartTimeout},
		{"worker start timeout", cfg.workerStartTimeout},
		{"shutdown timeout", cfg.shutdownTimeout},
	} {
		if required.value <= 0 {
			return errors.Errorf("%s must be positive", required.name)
		}
	}
	return nil
}

// supervise starts NATS, optionally Valkey, then the worker, then the ingestor, and
// stops every remaining child as soon as any one of them exits.
//
// Each child is only started once the one before it is answering, because each one
// depends on its predecessor: the worker dials both NATS and Valkey while starting
// and fails closed if either is missing. Valkey is skipped unless it was asked for,
// so the default deployment behaves exactly as it did before it could be supervised
// and keeps using whatever VALKEY_ADDRESS it inherits.
//
// Only the servers take arguments. nats-server's ports are passed explicitly rather
// than left to nats.conf, because command-line flags override the config file and the
// supervisor stays the one place that decides where the bus listens and where it polls
// for readiness. The two Jarvis binaries are started with no arguments at all so the
// app package resolves their whole configuration from the environment, which they
// inherit from this process.
func supervise(ctx context.Context, cfg supervisorConfig, start processStarter, client *http.Client) error {
	// Newest first, so every failure path stops "everything running so far" in the
	// reverse of the order it was started, and no call site has to name its children.
	var running []namedProcess
	add := func(name string, process managedProcess) {
		running = append([]namedProcess{{name, process}}, running...)
	}
	stopStarted := func(cause error) error {
		stopErr := stopAll(cfg.shutdownTimeout, processes(running)...)
		return errors.Join(cause, errors.Wrap(stopErr, "stop running children"))
	}

	broker, err := start(cfg.natsBinary, cfg.natsArgs(), nil)
	if err != nil {
		return err
	}
	add("NATS", broker)
	app.L().Info("Started combined NATS server", zap.String("port", cfg.natsPort))
	if err := waitReady(ctx, broker, client, cfg.natsURL("healthz"), cfg.natsStartTimeout, "NATS"); err != nil {
		return stopAfter(ctx, cfg.shutdownTimeout, err, processes(running)...)
	}

	if cfg.valkeyEnabled && !cfg.supervisesValkey() {
		app.L().Info("Using the external Valkey named by --valkey-address; not starting one",
			zap.String("address", cfg.valkeyAddress))
	}
	if cfg.supervisesValkey() {
		store, err := start(cfg.valkeyBinary, cfg.valkeyArgs(), nil)
		if err != nil {
			return stopStarted(err)
		}
		add("Valkey", store)
		app.L().Info("Started supervised Valkey", zap.String("port", cfg.supervisedValkeyPort()))
		if err := waitFor(ctx, store, cfg.valkeyStartTimeout, "Valkey", func(probeCtx context.Context) bool {
			return pingValkey(probeCtx, cfg.supervisedValkeyAddress())
		}); err != nil {
			return stopAfter(ctx, cfg.shutdownTimeout, err, processes(running)...)
		}
	}

	worker, err := start(cfg.workerBinary, nil, cfg.workerEnv())
	if err != nil {
		return stopStarted(err)
	}
	add("worker", worker)
	app.L().Info("Started combined worker", zap.String("port", cfg.workerPort))
	if err := waitReady(ctx, worker, client, cfg.workerURL("readyz"), cfg.workerStartTimeout, "worker"); err != nil {
		return stopAfter(ctx, cfg.shutdownTimeout, err, processes(running)...)
	}

	ingestor, err := start(cfg.ingestorBinary, nil, cfg.ingestorEnv())
	if err != nil {
		return stopStarted(err)
	}
	add("ingestor", ingestor)
	app.L().Info("Started combined ingestor", zap.String("port", cfg.port))

	select {
	case <-ctx.Done():
		return stopAll(cfg.shutdownTimeout, processes(running)...)
	case exited := <-firstExit(running):
		result := unexpectedExit(exited.name, exited.process.Err())
		if stopErr := stopAll(cfg.shutdownTimeout, others(running, exited.name)...); stopErr != nil {
			return errors.Join(result, errors.Wrap(stopErr, "stop remaining children"))
		}
		return result
	}
}

// namedProcess pairs a child with the name used in its log and error messages.
type namedProcess struct {
	name    string
	process managedProcess
}

// stopAfter stops the started children after a startup failure, preferring the
// startup error unless shutdown itself failed.
func stopAfter(ctx context.Context, timeout time.Duration, cause error, processes ...managedProcess) error {
	stopErr := stopAll(timeout, processes...)
	if ctx.Err() != nil && stopErr == nil {
		return nil
	}
	if stopErr != nil {
		return errors.Join(cause, errors.Wrap(stopErr, "stop children"))
	}
	return cause
}

// firstExit reports whichever child exits first.
func firstExit(running []namedProcess) <-chan namedProcess {
	exited := make(chan namedProcess, len(running))
	for _, child := range running {
		go func() {
			<-child.process.Done()
			exited <- child
		}()
	}
	return exited
}

// processes returns every child, in the order running holds them.
func processes(running []namedProcess) []managedProcess {
	all := make([]managedProcess, 0, len(running))
	for _, child := range running {
		all = append(all, child.process)
	}
	return all
}

// others returns every child except the named one.
func others(running []namedProcess, name string) []managedProcess {
	processes := make([]managedProcess, 0, len(running))
	for _, child := range running {
		if child.name != name {
			processes = append(processes, child.process)
		}
	}
	return processes
}

// supervisesValkey reports whether this process should start a Valkey of its own.
//
// VALKEY_ENABLED is the worker's switch for using Valkey at all, and this supervisor is
// bound to the same variable, so the address is what decides ownership. Loopback, or no
// address at all, is the server this process supervises: that is exactly the address it
// would hand the worker itself. Anything else is somebody else's server, already running,
// and starting a second one while overwriting their address would be wrong.
//
// The combined image depends on the second case. It ships no valkey-server, because no
// upstream build runs on the distroless base, and its documented deployment is exactly
// VALKEY_ENABLED with a VALKEY_ADDRESS outside the container.
func (cfg supervisorConfig) supervisesValkey() bool {
	return cfg.valkeyEnabled && isLoopback(cfg.valkeyAddress)
}

// isLoopback reports whether addr names this machine, or no machine at all.
//
// A bare host with no port counts, so that half-written configuration is read as the
// local server it was reaching for rather than as an external one.
func isLoopback(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// natsArgs is the nats-server command line.
//
// The ports repeat what nats.conf already sets so that the two never disagree:
// a flag overrides the file, and every URL the supervisor builds comes from the
// same fields.
func (cfg supervisorConfig) natsArgs() []string {
	return []string{
		"--config", cfg.natsConfig,
		"--port", cfg.natsPort,
		"--http_port", cfg.natsMonitorPort,
	}
}

// valkeyArgs is the valkey-server command line.
//
// Valkey needs no configuration file the way nats-server does: everything the
// supervised server differs from upstream defaults on fits here. Like the bus it is
// bound to loopback and carries no authentication, so only this container's worker can
// reach it. Its contents are derived state — rate-limit counters expire on their own
// and the guild-configuration cache is read-through — so persistence is off and losing
// the lot on restart costs nothing.
//
// maxmemory is deliberately left alone: an eviction policy could drop a live
// rate-limit counter and quietly hand someone more requests than their tier allows.
func (cfg supervisorConfig) valkeyArgs() []string {
	return []string{
		"--bind", "127.0.0.1",
		"--port", cfg.supervisedValkeyPort(),
		"--save", "",
		"--appendonly", "no",
		// Persistence is off, so nothing should be written here; --dir only decides where
		// that nothing would land. Without it Valkey inherits this process's working
		// directory, which under `bazel run` is the runfiles tree.
		"--dir", os.TempDir(),
	}
}

// natsURL builds a loopback URL on the NATS monitoring port.
func (cfg supervisorConfig) natsURL(path string) string {
	return "http://" + net.JoinHostPort("127.0.0.1", cfg.natsMonitorPort) + "/" + path
}

// workerURL builds a loopback URL on the worker health port.
func (cfg supervisorConfig) workerURL(path string) string {
	return "http://" + net.JoinHostPort("127.0.0.1", cfg.workerPort) + "/" + path
}

// clientURL is the loopback NATS address both services connect to.
func (cfg supervisorConfig) clientURL() string {
	return "nats://" + net.JoinHostPort("127.0.0.1", cfg.natsPort)
}

// supervisedValkeyAddress is the loopback address of the Valkey this process started.
// Distinct from the valkeyAddress field, which may name an external server instead.
func (cfg supervisorConfig) supervisedValkeyAddress() string {
	return net.JoinHostPort("127.0.0.1", cfg.supervisedValkeyPort())
}

// supervisedValkeyPort is the port the supervised server listens on: the one a loopback
// VALKEY_ADDRESS named, so the worker is never redirected off the address its own
// configuration asked for, and --valkey-port when it named none.
func (cfg supervisorConfig) supervisedValkeyPort() string {
	if _, port, err := net.SplitHostPort(strings.TrimSpace(cfg.valkeyAddress)); err == nil && port != "" {
		return port
	}
	return cfg.valkeyPort
}

// workerEnv is the environment the worker is started with.
func (cfg supervisorConfig) workerEnv() []string {
	env := map[string]string{
		"HOST":     "127.0.0.1",
		"PORT":     cfg.workerPort,
		"NATS_URL": cfg.clientURL(),
	}
	// Only when the supervisor owns the server. Otherwise VALKEY_* passes through
	// untouched, so an operator pointing the worker at an external Valkey through the
	// environment keeps the address they chose rather than having loopback forced on them.
	if cfg.supervisesValkey() {
		env["VALKEY_ENABLED"] = "true"
		env["VALKEY_ADDRESS"] = cfg.supervisedValkeyAddress()
	}
	return childEnv(os.Environ(), env)
}

// ingestorEnv is the environment the ingestor is started with.
func (cfg supervisorConfig) ingestorEnv() []string {
	return childEnv(os.Environ(), map[string]string{
		"PORT":     cfg.port,
		"NATS_URL": cfg.clientURL(),
	})
}

// waitFor polls probe until it reports the child ready, the child exits, or the
// timeout passes. A child that dies while starting is reported as such rather than
// waiting out the whole budget for a process that is never going to answer.
func waitFor(ctx context.Context, process managedProcess, timeout time.Duration, name string, probe func(context.Context) bool) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if probe(readyCtx) {
			return nil
		}
		select {
		case <-process.Done():
			return unexpectedExit(name+" before readiness", process.Err())
		case <-readyCtx.Done():
			return errors.Wrapf(readyCtx.Err(), "wait for %s readiness", name)
		case <-ticker.C:
		}
	}
}

// waitReady waits for a child to answer 200 on an HTTP health endpoint.
func waitReady(ctx context.Context, process managedProcess, client *http.Client, url string, timeout time.Duration, name string) error {
	return waitFor(ctx, process, timeout, name, func(probeCtx context.Context) bool {
		request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode == http.StatusOK
	})
}

// pingValkey reports whether Valkey answers PING at addr.
//
// Valkey exposes no HTTP monitoring endpoint the way nats-server does, so readiness is
// the RESP handshake itself. An inline PING proves the server is accepting connections
// and serving commands, where a bare TCP connect would only prove a listener is bound.
func pingValkey(ctx context.Context, addr string) bool {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	// Bounds this probe, not the whole readiness budget: a server that accepts and
	// then says nothing must cost one tick, not every second of --valkey-start-timeout.
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		return false
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && strings.HasPrefix(reply, "+PONG")
}

func stopAll(timeout time.Duration, processes ...managedProcess) error {
	active := make([]managedProcess, 0, len(processes))
	var signalErr error
	for _, process := range processes {
		select {
		case <-process.Done():
			continue
		default:
		}
		active = append(active, process)
		if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) && signalErr == nil {
			signalErr = errors.Wrap(err, "signal child process")
		}
	}
	if len(active) == 0 {
		return signalErr
	}
	done := make(chan struct{})
	go func() {
		var wait sync.WaitGroup
		wait.Add(len(active))
		for _, process := range active {
			go func() {
				defer wait.Done()
				<-process.Done()
			}()
		}
		wait.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		if signalErr != nil {
			return signalErr
		}
		for _, process := range active {
			if err := process.Err(); err != nil {
				return errors.Wrap(err, "child process stopped with an error")
			}
		}
		return nil
	case <-timer.C:
		for _, process := range active {
			select {
			case <-process.Done():
				continue
			default:
			}
			if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return errors.Wrap(err, "kill child process")
			}
		}
		<-done
		return errors.Errorf("child processes did not stop within %s", timeout)
	}
}

func unexpectedExit(name string, err error) error {
	if err == nil {
		return errors.Errorf("%s exited unexpectedly", name)
	}
	return errors.Wrapf(err, "%s exited unexpectedly", name)
}
