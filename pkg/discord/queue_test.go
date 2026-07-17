package discord

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadRequestQueueRunsOnlyLatestPendingRequest(t *testing.T) {
	queue := &threadRequestQueue{}
	firstStarted := make(chan struct{})
	firstCanceled := make(chan error, 1)
	releaseFirst := make(chan struct{})
	thirdStarted := make(chan struct{})
	var secondRan atomic.Bool

	firstResult := runQueuedRequest(queue, "thread", func(ctx context.Context) error {
		close(firstStarted)
		<-ctx.Done()
		firstCanceled <- context.Cause(ctx)
		<-releaseFirst
		return ctx.Err()
	})
	requireReceive(t, firstStarted)

	secondResult := runQueuedRequest(queue, "thread", func(context.Context) error {
		secondRan.Store(true)
		return nil
	})
	assert.ErrorIs(t, requireReceive(t, firstCanceled), errThreadRequestSuperseded)
	require.Eventually(t, func() bool { return queue.hasPending("thread") }, time.Second, time.Millisecond)

	thirdResult := runQueuedRequest(queue, "thread", func(context.Context) error {
		close(thirdStarted)
		return nil
	})
	assert.ErrorIs(t, requireReceive(t, secondResult), errThreadRequestSuperseded)
	assert.False(t, secondRan.Load())
	select {
	case <-thirdStarted:
		t.Fatal("latest request started before the active request stopped")
	default:
	}

	close(releaseFirst)
	assert.ErrorIs(t, requireReceive(t, firstResult), errThreadRequestSuperseded)
	requireReceive(t, thirdStarted)
	assert.NoError(t, requireReceive(t, thirdResult))
	require.Eventually(t, func() bool { return !queue.hasThread("thread") }, time.Second, time.Millisecond)
}

func TestThreadRequestQueueRunsDifferentThreadsConcurrently(t *testing.T) {
	queue := &threadRequestQueue{}
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})

	firstResult := runQueuedRequest(queue, "thread-one", func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		return nil
	})
	requireReceive(t, firstStarted)
	secondResult := runQueuedRequest(queue, "thread-two", func(context.Context) error {
		close(secondStarted)
		return nil
	})
	requireReceive(t, secondStarted)
	assert.NoError(t, requireReceive(t, secondResult))

	close(releaseFirst)
	assert.NoError(t, requireReceive(t, firstResult))
}

func runQueuedRequest(queue *threadRequestQueue, threadID string, run func(context.Context) error) <-chan error {
	result := make(chan error, 1)
	go func() { result <- queue.Run(context.Background(), threadID, run) }()
	return result
}

func requireReceive[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for test event")
		var zero T
		return zero
	}
}

func (q *threadRequestQueue) hasPending(threadID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.threads[threadID] != nil && q.threads[threadID].pending != nil
}

func (q *threadRequestQueue) hasThread(threadID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.threads[threadID] != nil
}
