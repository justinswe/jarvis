package discord

import (
	"context"
	"sync"

	"github.com/justinswe/std/errors"
)

var errThreadRequestSuperseded = errors.New("thread request superseded")

type threadRequestQueue struct {
	mu      sync.Mutex
	threads map[string]*threadQueueState
}

type threadQueueState struct {
	active  *queuedThreadRequest
	pending *queuedThreadRequest
}

type queuedThreadRequest struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	run    func(context.Context) error
	result chan error
}

// Run cancels active work and keeps only the latest pending request for a thread.
func (q *threadRequestQueue) Run(ctx context.Context, threadID string, run func(context.Context) error) error {
	requestCtx, cancel := context.WithCancelCause(ctx)
	request := &queuedThreadRequest{
		ctx: requestCtx, cancel: cancel, run: run, result: make(chan error, 1),
	}

	q.mu.Lock()
	if q.threads == nil {
		q.threads = make(map[string]*threadQueueState)
	}
	state, running := q.threads[threadID]
	if !running {
		state = &threadQueueState{}
		q.threads[threadID] = state
	}
	if state.active != nil {
		state.active.cancel(errThreadRequestSuperseded)
	}
	if state.pending != nil {
		state.pending.cancel(errThreadRequestSuperseded)
		state.pending.result <- errThreadRequestSuperseded
	}
	state.pending = request
	q.mu.Unlock()

	if !running {
		go q.run(threadID, state)
	}
	err := <-request.result
	cancel(nil)
	return err
}

// run processes one thread serially until its latest pending request finishes.
func (q *threadRequestQueue) run(threadID string, state *threadQueueState) {
	for {
		q.mu.Lock()
		request := state.pending
		state.pending = nil
		state.active = request
		q.mu.Unlock()

		err := context.Cause(request.ctx)
		if err == nil {
			err = request.run(request.ctx)
		}
		if errors.Is(context.Cause(request.ctx), errThreadRequestSuperseded) {
			err = errThreadRequestSuperseded
		}
		request.result <- err

		q.mu.Lock()
		state.active = nil
		if state.pending == nil {
			delete(q.threads, threadID)
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
	}
}

func threadRequestSuperseded(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errThreadRequestSuperseded)
}
