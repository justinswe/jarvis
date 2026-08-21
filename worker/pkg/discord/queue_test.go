package discord

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadRequestQueueRunsRequestsInOrder(t *testing.T) {
	queue := &threadRequestQueue{}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var order []int

	firstResult := runQueuedRequest(queue, "thread", func(context.Context) error {
		close(firstStarted)
		<-releaseFirst
		order = append(order, 1)
		return nil
	})
	requireReceive(t, firstStarted)
	secondResult := runQueuedRequest(queue, "thread", func(context.Context) error {
		order = append(order, 2)
		return nil
	})
	require.Eventually(t, func() bool { return queue.pendingCount("thread") == 1 }, time.Second, time.Millisecond)
	thirdResult := runQueuedRequest(queue, "thread", func(context.Context) error {
		order = append(order, 3)
		return nil
	})
	require.Eventually(t, func() bool { return queue.pendingCount("thread") == 2 }, time.Second, time.Millisecond)
	assert.Empty(t, order, "queued requests must wait for the active one")

	close(releaseFirst)
	assert.NoError(t, requireReceive(t, firstResult))
	assert.NoError(t, requireReceive(t, secondResult))
	assert.NoError(t, requireReceive(t, thirdResult))
	assert.Equal(t, []int{1, 2, 3}, order)
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

func (q *threadRequestQueue) pendingCount(threadID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.threads[threadID])
}

func (q *threadRequestQueue) hasThread(threadID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.threads[threadID]
	return ok
}
