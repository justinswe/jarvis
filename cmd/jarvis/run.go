package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

const (
	ingestorBinary = "/app/ingestor"
	workerBinary   = "/app/worker"
	processPath    = "/v1/messages:process"
)

func runJarvis(parent context.Context, cfg supervisorConfig) error {
	if cfg.port == "" {
		return errors.New("port is required")
	}
	if cfg.workerPort == "" {
		return errors.New("worker port is required")
	}
	if cfg.workerStartTimeout <= 0 {
		return errors.New("worker start timeout must be positive")
	}
	if cfg.shutdownTimeout <= 0 {
		return errors.New("shutdown timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return supervise(ctx, cfg, startProcess, &http.Client{Timeout: 500 * time.Millisecond})
}

func supervise(ctx context.Context, cfg supervisorConfig, start processStarter, client *http.Client) error {
	worker, err := start(workerBinary, []string{
		"--host=127.0.0.1",
		"--port=" + cfg.workerPort,
	})
	if err != nil {
		return err
	}
	app.L().Info("Started combined worker", zap.String("port", cfg.workerPort))
	if err := waitReady(ctx, worker, client, cfg.workerPort, cfg.workerStartTimeout); err != nil {
		stopErr := stopAll(cfg.shutdownTimeout, worker)
		if ctx.Err() != nil && stopErr == nil {
			return nil
		}
		if stopErr != nil {
			return errors.Join(err, errors.Wrap(stopErr, "stop worker"))
		}
		return err
	}

	ingestor, err := start(ingestorBinary, []string{
		"--port=" + cfg.port,
		"--worker-url=http://" + net.JoinHostPort("127.0.0.1", cfg.workerPort) + processPath,
	})
	if err != nil {
		return errors.Join(err, errors.Wrap(stopAll(cfg.shutdownTimeout, worker), "stop worker"))
	}
	app.L().Info("Started combined ingestor", zap.String("port", cfg.port))

	select {
	case <-ctx.Done():
		return stopAll(cfg.shutdownTimeout, ingestor, worker)
	case <-worker.Done():
		result := unexpectedExit("worker", worker.Err())
		if stopErr := stopAll(cfg.shutdownTimeout, ingestor); stopErr != nil {
			return errors.Join(result, errors.Wrap(stopErr, "stop ingestor"))
		}
		return result
	case <-ingestor.Done():
		result := unexpectedExit("ingestor", ingestor.Err())
		if stopErr := stopAll(cfg.shutdownTimeout, worker); stopErr != nil {
			return errors.Join(result, errors.Wrap(stopErr, "stop worker"))
		}
		return result
	}
}

func waitReady(ctx context.Context, worker managedProcess, client *http.Client, port string, timeout time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := "http://" + net.JoinHostPort("127.0.0.1", port) + "/readyz"
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(readyCtx, http.MethodGet, url, nil)
		if err != nil {
			return errors.Wrap(err, "create worker readiness request")
		}
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-worker.Done():
			return unexpectedExit("worker before readiness", worker.Err())
		case <-readyCtx.Done():
			return errors.Wrap(readyCtx.Err(), "wait for worker readiness")
		case <-ticker.C:
		}
	}
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
