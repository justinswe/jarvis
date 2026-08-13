package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type managedProcess interface {
	Done() <-chan struct{}
	Err() error
	Signal(os.Signal) error
	Kill() error
}

type childProcess struct {
	command *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

type processStarter func(string, []string, []string) (managedProcess, error)

func startProcess(binary string, args []string, env []string) (managedProcess, error) {
	command := exec.Command(binary, args...)
	command.Env = env
	command.Stdout = os.Stdout
	command.Stderr = &childLog{name: filepath.Base(binary)}
	// Stdin is deliberately left closed. Every child is a server that never reads it, and
	// sharing the supervisor's stdin between them would only let one steal the others' input.
	if err := command.Start(); err != nil {
		return nil, errors.Wrapf(err, "start %s", binary)
	}
	process := &childProcess{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()
	return process, nil
}

// childLog forwards a child's stderr into the supervisor's own structured log.
type childLog struct {
	name string
	mu   sync.Mutex
	buf  []byte
}

// childLevels maps the nats-server log tags to levels, longest concern first.
var childLevels = []struct {
	tag   string
	level zapcore.Level
}{
	{"[FTL]", zapcore.ErrorLevel},
	{"[ERR]", zapcore.ErrorLevel},
	{"[WRN]", zapcore.WarnLevel},
	{"[INF]", zapcore.InfoLevel},
	{"[DBG]", zapcore.DebugLevel},
	{"[TRC]", zapcore.DebugLevel},
}

// Write buffers whole lines out of a stream that does not arrive line-aligned.
//
// os/exec copies through a 32KiB buffer, so one call routinely carries several
// lines and can split the last one.
func (w *childLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)
	for {
		end := bytes.IndexByte(w.buf, '\n')
		if end < 0 {
			return len(p), nil
		}
		line := strings.TrimSpace(string(w.buf[:end]))
		w.buf = w.buf[end+1:]
		if line != "" {
			w.log(line)
		}
	}
}

// log re-emits one child line at the level its tag names.
func (w *childLog) log(line string) {
	level, message := zapcore.ErrorLevel, line
	for _, candidate := range childLevels {
		at := strings.Index(line, candidate.tag)
		if at < 0 {
			continue
		}
		// Everything before the tag is the child's own pid and timestamp prefix, which
		// the supervisor's encoder supplies again.
		level, message = candidate.level, strings.TrimSpace(line[at+len(candidate.tag):])
		break
	}
	if entry := app.L().Check(level, message); entry != nil {
		entry.Write(zap.String("child", w.name))
	}
}

// childEnv applies overrides to base, replacing rather than shadowing.
//
// Appending "PORT=8081" to os.Environ() would not take effect: Go resolves a
// duplicated key to its *first* occurrence and discards the rest, so an
// inherited PORT would win over the appended one. Overridden keys are therefore
// removed from base before the new values are added.
func childEnv(base []string, overrides map[string]string) []string {
	env := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := overrides[key]; overridden {
				continue
			}
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func (p *childProcess) Done() <-chan struct{} { return p.done }

func (p *childProcess) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *childProcess) Signal(signal os.Signal) error {
	return p.command.Process.Signal(signal)
}

func (p *childProcess) Kill() error { return p.command.Process.Kill() }
