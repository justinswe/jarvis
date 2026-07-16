package main

import (
	"os"
	"os/exec"
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

type processStarter func(string, []string) (managedProcess, error)

func startProcess(binary string, args []string) (managedProcess, error) {
	command := exec.Command(binary, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
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
