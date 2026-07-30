package main

import (
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/justinswe/std/errors"
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
	command.Stderr = os.Stderr
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
