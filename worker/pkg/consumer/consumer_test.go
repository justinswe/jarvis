package consumer

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/mq"
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

// fakeMessage is an mq.Message. How a broker holds a message while it is being worked on
// belongs to the driver; this package only decides which of the three settlements to use.
type fakeMessage struct {
	data []byte

	mu     sync.Mutex
	acked  int
	naked  int
	termed int
	calls  []string
}

func (m *fakeMessage) Data() []byte { return m.data }
func (m *fakeMessage) Ack() error   { return m.record(&m.acked, "ack") }
func (m *fakeMessage) Nak() error   { return m.record(&m.naked, "nak") }
func (m *fakeMessage) Term() error  { return m.record(&m.termed, "term") }

func (m *fakeMessage) record(counter *int, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	*counter++
	m.calls = append(m.calls, name)
	return nil
}

func (m *fakeMessage) counts() (ack, nak, term int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked, m.naked, m.termed
}

func (m *fakeMessage) ordered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
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
	msg := encode(t, validRequest())
	processor := &fakeProcessor{}

	handle(t.Context(), msg, processor, nil)

	require.NotNil(t, processor.message)
	assert.Equal(t, "message", processor.message.ID)
	assert.Equal(t, "guild", processor.message.GuildID)
	assert.Equal(t, discordgo.MessageTypeReply, processor.message.Type)
	require.Len(t, processor.message.Attachments, 1)
	assert.Equal(t, "photo.png", processor.message.Attachments[0].Filename)

	ack, nak, term := msg.counts()
	assert.Equal(t, 1, ack)
	assert.Zero(t, nak)
	assert.Zero(t, term)
}

func TestHandleAcknowledgesOnlyAfterProcessingCompletes(t *testing.T) {
	msg := encode(t, validRequest())
	var ackedDuringProcessing int
	processor := &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		ackedDuringProcessing, _, _ = msg.counts()
		return nil
	}}

	handle(t.Context(), msg, processor, nil)

	assert.Zero(t, ackedDuringProcessing, "message must stay un-acked while processing")
	ack, _, _ := msg.counts()
	assert.Equal(t, 1, ack)
}

func TestHandleRecordsBeforeProcessing(t *testing.T) {
	msg := encode(t, validRequest())
	recorder := &fakeRecorder{}
	var recordedFirst bool
	processor := &fakeProcessor{process: func(context.Context, *discordgo.MessageCreate) error {
		recordedFirst = recorder.called
		return nil
	}}

	handle(t.Context(), msg, processor, recorder)

	assert.True(t, recordedFirst)
	require.NotNil(t, recorder.event)
	assert.Equal(t, "message", recorder.event.MessageId)
}

func TestHandleProcessesWhenRecordingFails(t *testing.T) {
	msg := encode(t, validRequest())
	recorder := &fakeRecorder{err: errors.New("dynamo unavailable")}
	processor := &fakeProcessor{}

	handle(t.Context(), msg, processor, recorder)

	assert.NotNil(t, processor.message, "recording failure must not block processing")
	ack, _, _ := msg.counts()
	assert.Equal(t, 1, ack)
}

func TestHandleNaksAfterProcessingError(t *testing.T) {
	msg := encode(t, validRequest())
	processor := &fakeProcessor{err: errors.New("model unavailable")}

	handle(t.Context(), msg, processor, nil)

	ack, nak, term := msg.counts()
	assert.Zero(t, ack)
	assert.Equal(t, 1, nak, "a transient failure must be redelivered")
	assert.Zero(t, term)
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
			handle(t.Context(), test.msg, processor, nil)

			assert.Nil(t, processor.message, "unprocessable messages must not reach the processor")
			ack, nak, term := test.msg.counts()
			assert.Zero(t, ack)
			assert.Zero(t, nak, "redelivery can never fix these messages")
			assert.Equal(t, 1, term)
		})
	}
}

// TestHandleSettlesEveryMessageExactlyOnce is the contract mq.Message states: a message
// left unsettled is held until the broker takes it back, and one settled twice is an
// error against a message that is already gone.
func TestHandleSettlesEveryMessageExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name      string
		msg       *fakeMessage
		processor *fakeProcessor
	}{
		{"processed", encode(t, validRequest()), &fakeProcessor{}},
		{"failed", encode(t, validRequest()), &fakeProcessor{err: errors.New("model unavailable")}},
		{"unprocessable", &fakeMessage{data: []byte("not protobuf at all")}, &fakeProcessor{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handle(t.Context(), test.msg, test.processor, nil)

			assert.Len(t, test.msg.ordered(), 1)
		})
	}
}

func TestStartRequiresAProcessor(t *testing.T) {
	_, err := Start(t.Context(), mq.Config{}, nil, nil)

	assert.Error(t, err)
}
