package discord

import (
	"context"
	"sync"
)

// threadRequestQueue serializes requests per thread in arrival order, so every message
// gets a reply and each reply is generated with the previous one already in history.
type threadRequestQueue struct {
	mu      sync.Mutex
	threads map[string][]*queuedThreadRequest
}

type queuedThreadRequest struct {
	ctx    context.Context
	run    func(context.Context) error
	result chan error
}

// Run enqueues one request for a thread and waits for its turn and its result.
func (q *threadRequestQueue) Run(ctx context.Context, threadID string, run func(context.Context) error) error {
	request := &queuedThreadRequest{ctx: ctx, run: run, result: make(chan error, 1)}

	q.mu.Lock()
	if q.threads == nil {
		q.threads = make(map[string][]*queuedThreadRequest)
	}
	_, running := q.threads[threadID]
	q.threads[threadID] = append(q.threads[threadID], request)
	q.mu.Unlock()

	if !running {
		go q.run(threadID)
	}
	return <-request.result
}

// run drains one thread's queue in order until it is empty.
func (q *threadRequestQueue) run(threadID string) {
	for {
		q.mu.Lock()
		pending := q.threads[threadID]
		if len(pending) == 0 {
			delete(q.threads, threadID)
			q.mu.Unlock()
			return
		}
		request := pending[0]
		q.threads[threadID] = pending[1:]
		q.mu.Unlock()

		err := request.ctx.Err()
		if err == nil {
			err = request.run(request.ctx)
		}
		request.result <- err
	}
}
