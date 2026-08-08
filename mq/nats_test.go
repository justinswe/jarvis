package mq

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeNATSMsg struct {
	data []byte
	// settleDelay is held inside Ack and NakWithDelay, after the call is recorded, so a
	// keepalive that is still running has time to record a tick behind them.
	settleDelay time.Duration

	mu         sync.Mutex
	acked      int
	naked      int
	nakDelays  []time.Duration
	termed     int
	inProgress int
	calls      []string
}

func (m *fakeNATSMsg) Data() []byte { return m.data }

func (m *fakeNATSMsg) Ack() error {
	err := m.record(&m.acked, "ack")
	time.Sleep(m.settleDelay)
	return err
}

func (m *fakeNATSMsg) NakWithDelay(delay time.Duration) error {
	m.mu.Lock()
	m.nakDelays = append(m.nakDelays, delay)
	m.mu.Unlock()
	err := m.record(&m.naked, "nak")
	time.Sleep(m.settleDelay)
	return err
}

func (m *fakeNATSMsg) Term() error       { return m.record(&m.termed, "term") }
func (m *fakeNATSMsg) InProgress() error { return m.record(&m.inProgress, "in_progress") }

func (m *fakeNATSMsg) record(counter *int, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	*counter++
	m.calls = append(m.calls, name)
	return nil
}

func (m *fakeNATSMsg) counts() (ack, nak, term, progress int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked, m.naked, m.termed, m.inProgress
}

// ordered returns the calls made against the message, in the order they happened.
func (m *fakeNATSMsg) ordered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

func (m *fakeNATSMsg) delays() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.nakDelays)
}

func TestTimingDerivesFromAckWait(t *testing.T) {
	schedule := newTiming(30*time.Second, 10*time.Minute)

	assert.Equal(t, 15*time.Second, schedule.heartbeat,
		"a held message must report progress well before AckWait expires")
	assert.Equal(t, 30*time.Second, schedule.retryDelay)
	assert.Equal(t, 10*time.Minute, schedule.maxProcessing)
}

// TestNakAppliesTheRetryDelay pins the driver's translation of the broker-neutral Nak.
// Without a delay all MaxDeliver attempts are spent within milliseconds and the transient
// failure never gets a chance to clear.
func TestNakAppliesTheRetryDelay(t *testing.T) {
	msg := &fakeNATSMsg{}
	message := &natsMessage{msg: msg, retryDelay: 30 * time.Second, stop: func() {}}

	require.NoError(t, message.Nak())

	assert.Equal(t, []time.Duration{30 * time.Second}, msg.delays(),
		"redelivery must be delayed, not immediate")
}

// TestAcknowledgementsStopTheKeepaliveFirst pins the ordering: an InProgress landing
// after a Nak resets the redelivery delay that Nak just asked for, and one landing after
// an Ack touches a message that is already gone.
func TestAcknowledgementsStopTheKeepaliveFirst(t *testing.T) {
	for _, test := range []struct {
		name   string
		settle func(*natsMessage) error
	}{
		{"ack", (*natsMessage).Ack},
		{"nak", (*natsMessage).Nak},
		{"term", (*natsMessage).Term},
	} {
		t.Run(test.name, func(t *testing.T) {
			msg := &fakeNATSMsg{settleDelay: 60 * time.Millisecond}
			stop := keepAlive(msg, 2*time.Millisecond, time.Minute)
			defer stop()
			// Let the heartbeat actually fire, or this test proves nothing.
			require.Eventually(t, func() bool {
				_, _, _, progress := msg.counts()
				return progress > 0
			}, time.Second, time.Millisecond)

			require.NoError(t, test.settle(&natsMessage{msg: msg, retryDelay: time.Second, stop: stop}))

			calls := msg.ordered()
			settled := slices.Index(calls, test.name)
			require.NotEqual(t, -1, settled, "the message must be settled")
			assert.NotContains(t, calls[settled:], "in_progress",
				"keepalive must stop before the acknowledgement")
		})
	}
}

func TestKeepAliveStopsHeartbeating(t *testing.T) {
	msg := &fakeNATSMsg{}
	stop := keepAlive(msg, 10*time.Millisecond, time.Minute)
	time.Sleep(50 * time.Millisecond)
	stop()
	_, _, _, settled := msg.counts()
	time.Sleep(50 * time.Millisecond)
	_, _, _, after := msg.counts()
	assert.Equal(t, settled, after, "stop must end the heartbeat")
}

// TestKeepAliveReleasesAMessageHeldTooLong is what bounds a wedged handler. Without it a
// handler that never returns holds its message for the process lifetime, and no other
// replica can pick the message up.
func TestKeepAliveReleasesAMessageHeldTooLong(t *testing.T) {
	msg := &fakeNATSMsg{}
	stop := keepAlive(msg, 5*time.Millisecond, 25*time.Millisecond)
	defer stop()

	require.Eventually(t, func() bool {
		_, _, _, progress := msg.counts()
		return progress > 0
	}, time.Second, time.Millisecond)

	time.Sleep(60 * time.Millisecond)
	_, _, _, settled := msg.counts()
	time.Sleep(60 * time.Millisecond)
	_, _, _, after := msg.counts()
	assert.Equal(t, settled, after, "the heartbeat must stop at the processing limit")
}

// TestAwaitInFlightAbandonsMessagesThatIgnoreCancellation is the shutdown-deadlock
// regression: cancellation is a request, not a guarantee, so a message that never
// watches its context must not hold shutdown open forever.
func TestAwaitInFlightAbandonsMessagesThatIgnoreCancellation(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscription := &natsSubscription{
		lifetime:     lifetime,
		cancel:       cancel,
		drainTimeout: 10 * time.Millisecond,
		cancelGrace:  10 * time.Millisecond,
	}
	blocked := make(chan struct{})
	defer close(blocked)
	subscription.inFlight.Add(1)
	go func() {
		defer subscription.inFlight.Done()
		<-blocked // Deliberately ignores lifetime cancellation.
	}()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		subscription.awaitInFlight()
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitInFlight did not return after the cancel grace period")
	}
	assert.Error(t, lifetime.Err(), "the straggler must have been cancelled")
}

// TestStopDrainsInFlightMessages is the regression test for shutdown abandoning work.
// The subscription used to hand in-flight generations the caller's context, which in the
// worker is the signal context — so SIGTERM cancelled every reply a user was waiting on
// and Nak'd it, rather than letting it finish.
func TestStopDrainsInFlightMessages(t *testing.T) {
	msg := &fakeNATSMsg{}
	started, release := make(chan struct{}), make(chan struct{})
	var seen error

	// The caller's context is cancelled the way a SIGTERM cancels the worker's.
	ctx, cancel := context.WithCancel(context.Background())
	subscription := testSubscription(ctx, time.Second)
	subscription.dispatch(msg, func(msgCtx context.Context, delivered Message) {
		close(started)
		<-release
		seen = msgCtx.Err()
		require.NoError(t, delivered.Ack())
	}, timing{})
	<-started
	cancel()

	stopped := make(chan struct{})
	go func() { defer close(stopped); subscription.Stop() }()
	close(release)
	<-stopped

	require.NoError(t, seen, "an in-flight message must not be cancelled by the caller's context")
	ack, nak, _, _ := msg.counts()
	assert.Equal(t, 1, ack, "a message that finished during the drain must be acknowledged")
	assert.Zero(t, nak)
}

func TestStopCancelsMessagesPastTheDrainDeadline(t *testing.T) {
	msg := &fakeNATSMsg{}
	started := make(chan struct{})

	subscription := testSubscription(context.Background(), 20*time.Millisecond)
	subscription.dispatch(msg, func(msgCtx context.Context, delivered Message) {
		close(started)
		<-msgCtx.Done()
		require.NoError(t, delivered.Nak())
	}, timing{})
	<-started
	subscription.Stop()

	ack, nak, _, _ := msg.counts()
	assert.Zero(t, ack)
	assert.Equal(t, 1, nak, "work past the deadline is cancelled and left for redelivery")
}

// testSubscription builds a subscription whose dispatch path is usable without a live
// broker. consume and connection stay nil, so Stop skips the drain and only the in-flight
// budget is exercised.
func testSubscription(ctx context.Context, drainTimeout time.Duration) *natsSubscription {
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &natsSubscription{
		slots:        make(chan struct{}, 1),
		lifetime:     lifetime,
		cancel:       cancel,
		drainTimeout: drainTimeout,
	}
}
