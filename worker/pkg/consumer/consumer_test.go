package consumer

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fakeProcessor struct {
	message *discordgo.MessageCreate
	err     error
	process func(context.Context, *discordgo.MessageCreate) error
}

func (p *fakeProcessor) Process(ctx context.Context, message *discordgo.MessageCreate) error {
	p.message = message
	if p.process != nil {
		return p.process(ctx, message)
	}
	return p.err
}

type fakeRecorder struct {
	event  *discordv1.DiscordMessageCreateEvent
	err    error
	called bool
}

func (r *fakeRecorder) Record(_ context.Context, event *discordv1.DiscordMessageCreateEvent) error {
	r.called = true
	r.event = event
	return r.err
}

type fakeMessage struct {
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

func (m *fakeMessage) Data() []byte { return m.data }

func (m *fakeMessage) Ack() error {
	err := m.record(&m.acked, "ack")
	time.Sleep(m.settleDelay)
	return err
}
func (m *fakeMessage) NakWithDelay(delay time.Duration) error {
	m.mu.Lock()
	m.nakDelays = append(m.nakDelays, delay)
	m.mu.Unlock()
	err := m.record(&m.naked, "nak")
	time.Sleep(m.settleDelay)
	return err
}
func (m *fakeMessage) Term() error {
	return m.record(&m.termed, "term")
}
func (m *fakeMessage) InProgress() error { return m.record(&m.inProgress, "in_progress") }

func (m *fakeMessage) delays() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.nakDelays)
}

// ordered returns the calls made against the message, in the order they happened.
func (m *fakeMessage) ordered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

func (m *fakeMessage) record(counter *int, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	*counter++
	m.calls = append(m.calls, name)
	return nil
}

func (m *fakeMessage) counts() (ack, nak, term, progress int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked, m.naked, m.termed, m.inProgress
}

func validRequest() *discordv1.IngestMessageRequest {
	return &discordv1.IngestMessageRequest{Event: &discordv1.DiscordMessageCreateEvent{
		MessageId:        "message",
		GuildId:          "guild",
		ChannelId:        "channel",
		Content:          "hello",
		Kind:             discordv1.MessageKind_MESSAGE_KIND_REPLY,
		Author:           &discordv1.MessageAuthor{Id: "user", Username: "alice"},
		MentionedUserIds: []string{"bot"},
		Reference:        &discordv1.MessageReference{MessageId: "parent", ChannelId: "channel"},
		Attachments: []*discordv1.MessageAttachment{{Id: "image", Filename: "photo.png", ContentType: "image/png", Size: 42,
			Url: "https://cdn.discordapp.com/attachments/a/b/photo.png", Width: 10, Height: 20}},
	}}
}

func encode(t *testing.T, request *discordv1.IngestMessageRequest) *fakeMessage {
	t.Helper()
	body, err := proto.Marshal(request)
	require.NoError(t, err)
	return &fakeMessage{data: body}
}

func TestHandleAcknowledgesAfterProcessing(t *testing.T) {
	request := validRequest()
	msg := encode(t, request)
	processor := &fakeProcessor{}

	handle(context.Background(), msg, processor, nil, timing{heartbeat: 0})

	require.NotNil(t, processor.message)
	assert.Equal(t, "message", processor.message.ID)
	assert.Equal(t, "guild", processor.message.GuildID)
	assert.Equal(t, discordgo.MessageTypeReply, processor.message.Type)
	require.Len(t, processor.message.Attachments, 1)
	assert.Equal(t, "photo.png", processor.message.Attachments[0].Filename)

	ack, nak, term, _ := msg.counts()
	assert.Equal(t, 1, ack)
	assert.Zero(t, nak)
	assert.Zero(t, term)
}

func TestHandleAcknowledgesOnlyAfterProcessingCompletes(t *testing.T) {
	msg := encode(t, validRequest())
	release := make(chan struct{})
	var ackedDuringProcessing int
	processor := &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		ackedDuringProcessing, _, _, _ = msg.counts()
		close(release)
		return nil
	}}

	handle(context.Background(), msg, processor, nil, timing{heartbeat: 0})

	<-release
	assert.Zero(t, ackedDuringProcessing, "message must stay un-acked while processing")
	ack, _, _, _ := msg.counts()
	assert.Equal(t, 1, ack)
}

// TestHandleStopsKeepaliveBeforeAcknowledging pins the ordering: an InProgress landing
// after a Nak resets the redelivery delay that Nak just asked for, and one landing after
// an Ack touches a message that is already gone. The message holds settleDelay inside
// Ack, so a keepalive still ticking would be recorded behind it.
func TestHandleStopsKeepaliveBeforeAcknowledging(t *testing.T) {
	msg := encode(t, validRequest())
	msg.settleDelay = 60 * time.Millisecond
	beat := make(chan struct{})
	var once sync.Once
	processor := &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		<-beat
		return nil
	}}
	go func() {
		for {
			if _, _, _, progress := msg.counts(); progress > 0 {
				once.Do(func() { close(beat) })
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	handle(context.Background(), msg, processor, nil, timing{heartbeat: 2 * time.Millisecond, retryDelay: time.Second})

	calls := msg.ordered()
	require.Contains(t, calls, "in_progress", "the heartbeat must fire, or this test proves nothing")
	acked := slices.Index(calls, "ack")
	require.NotEqual(t, -1, acked, "the message must be acknowledged")
	assert.NotContains(t, calls[acked:], "in_progress", "keepalive must stop before the acknowledgement")
}

// TestAwaitInFlightAbandonsMessagesThatIgnoreCancellation is the shutdown-deadlock
// regression: cancellation is a request, not a guarantee, so a message that never
// watches its context must not hold shutdown open forever.
func TestAwaitInFlightAbandonsMessagesThatIgnoreCancellation(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscription := &Consumer{
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

func TestHandleRecordsBeforeProcessing(t *testing.T) {
	msg := encode(t, validRequest())
	recorder := &fakeRecorder{}
	var recordedFirst bool
	processor := &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		recordedFirst = recorder.called
		return nil
	}}

	handle(context.Background(), msg, processor, recorder, timing{heartbeat: 0})

	assert.True(t, recordedFirst)
	require.NotNil(t, recorder.event)
	assert.Equal(t, "message", recorder.event.MessageId)
}

func TestHandleProcessesWhenRecordingFails(t *testing.T) {
	msg := encode(t, validRequest())
	recorder := &fakeRecorder{err: errors.New("dynamo unavailable")}
	processor := &fakeProcessor{}

	handle(context.Background(), msg, processor, recorder, timing{heartbeat: 0})

	assert.NotNil(t, processor.message, "recording failure must not block processing")
	ack, _, _, _ := msg.counts()
	assert.Equal(t, 1, ack)
}

func TestHandleNaksAfterProcessingError(t *testing.T) {
	msg := encode(t, validRequest())
	processor := &fakeProcessor{err: errors.New("model unavailable")}

	handle(context.Background(), msg, processor, nil, newTiming(30*time.Second))

	ack, nak, term, _ := msg.counts()
	assert.Zero(t, ack)
	assert.Equal(t, 1, nak, "a transient failure must be redelivered")
	assert.Zero(t, term)
	// Without a delay all MaxDeliver attempts are spent within milliseconds and the
	// transient failure never gets a chance to clear.
	assert.Equal(t, []time.Duration{30 * time.Second}, msg.delays(),
		"redelivery must be delayed, not immediate")
}

func TestTimingDerivesFromAckWait(t *testing.T) {
	schedule := newTiming(30 * time.Second)

	assert.Equal(t, 15*time.Second, schedule.heartbeat,
		"a held message must report progress well before AckWait expires")
	assert.Equal(t, 30*time.Second, schedule.retryDelay)
}

func TestHandleTerminatesUnprocessableMessages(t *testing.T) {
	for _, test := range []struct {
		name string
		msg  *fakeMessage
	}{
		{"invalid protobuf", &fakeMessage{data: []byte("not protobuf at all")}},
		{"missing event", encode(t, &discordv1.IngestMessageRequest{})},
		{"missing message id", encode(t, &discordv1.IngestMessageRequest{
			Event: &discordv1.DiscordMessageCreateEvent{ChannelId: "channel", Author: &discordv1.MessageAuthor{Id: "user"}},
		})},
		{"missing author", encode(t, &discordv1.IngestMessageRequest{
			Event: &discordv1.DiscordMessageCreateEvent{MessageId: "message", ChannelId: "channel"},
		})},
	} {
		t.Run(test.name, func(t *testing.T) {
			processor := &fakeProcessor{}
			handle(context.Background(), test.msg, processor, nil, timing{heartbeat: 0})

			assert.Nil(t, processor.message, "unprocessable messages must not reach the processor")
			ack, nak, term, _ := test.msg.counts()
			assert.Zero(t, ack)
			assert.Zero(t, nak, "redelivery can never fix these messages")
			assert.Equal(t, 1, term)
		})
	}
}

func TestHandleExtendsRedeliveryTimerWhileProcessing(t *testing.T) {
	msg := encode(t, validRequest())
	processor := &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		time.Sleep(120 * time.Millisecond)
		return nil
	}}

	handle(context.Background(), msg, processor, nil, timing{heartbeat: 20 * time.Millisecond})

	ack, _, _, progress := msg.counts()
	assert.Equal(t, 1, ack)
	assert.Positive(t, progress, "slow processing must reset the redelivery timer")
}

func TestKeepAliveStopsHeartbeating(t *testing.T) {
	msg := &fakeMessage{}
	stop := keepAlive(msg, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	stop()
	_, _, _, settled := msg.counts()
	time.Sleep(50 * time.Millisecond)
	_, _, _, after := msg.counts()
	assert.Equal(t, settled, after, "stop must end the heartbeat")
}

// TestStopDrainsInFlightMessages is the regression test for shutdown abandoning work.
// The consumer used to hand in-flight generations the caller's context, which in the
// worker is the signal context — so SIGTERM cancelled every reply a user was waiting
// on and Nak'd it, rather than letting it finish.
func TestStopDrainsInFlightMessages(t *testing.T) {
	msg := encode(t, validRequest())
	started, release := make(chan struct{}), make(chan struct{})
	var seen error
	processor := &fakeProcessor{process: func(ctx context.Context, _ *discordgo.MessageCreate) error {
		close(started)
		<-release
		seen = ctx.Err()
		return nil
	}}

	// The caller's context is cancelled the way a SIGTERM cancels the worker's.
	ctx, cancel := context.WithCancel(context.Background())
	subscription := testConsumer(ctx, time.Second)
	subscription.dispatch(msg, processor, nil, timing{})
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
	msg := encode(t, validRequest())
	started := make(chan struct{})
	processor := &fakeProcessor{process: func(ctx context.Context, _ *discordgo.MessageCreate) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}

	subscription := testConsumer(context.Background(), 20*time.Millisecond)
	subscription.dispatch(msg, processor, nil, timing{})
	<-started
	subscription.Stop()

	ack, nak, _, _ := msg.counts()
	assert.Zero(t, ack)
	assert.Equal(t, 1, nak, "work past the deadline is cancelled and left for redelivery")
}

// testConsumer builds a Consumer whose dispatch path is usable without a live broker.
// consume stays nil, so Stop's Drain is skipped and only the drain budget is exercised.
func testConsumer(ctx context.Context, drainTimeout time.Duration) *Consumer {
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &Consumer{
		slots:        make(chan struct{}, 1),
		lifetime:     lifetime,
		cancel:       cancel,
		drainTimeout: drainTimeout,
	}
}

func TestConfigValidation(t *testing.T) {
	valid := Config{
		Stream: discordv1.StreamName, Subject: discordv1.Subject, Durable: discordv1.DurableName,
		AckWait: time.Second, MaxDeliver: 3, MaxAckPending: 4, DrainTimeout: time.Second,
	}
	require.NoError(t, valid.validate())

	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"stream", func(c *Config) { c.Stream = "" }},
		{"subject", func(c *Config) { c.Subject = "" }},
		{"durable", func(c *Config) { c.Durable = "" }},
		{"ack wait", func(c *Config) { c.AckWait = 0 }},
		{"max deliver", func(c *Config) { c.MaxDeliver = 0 }},
		{"max ack pending", func(c *Config) { c.MaxAckPending = 0 }},
		{"drain timeout", func(c *Config) { c.DrainTimeout = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			assert.Error(t, cfg.validate())
		})
	}
}
