package ingestor

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWorkerClient struct {
	request *discordv1.IngestMessageRequest
}

func (c *fakeWorkerClient) Publish(_ context.Context, request *discordv1.IngestMessageRequest) error {
	c.request = request
	return nil
}

func TestControllerNormalizesDiscordMessage(t *testing.T) {
	client := &fakeWorkerClient{}
	controller := NewController(client)
	controller.now = func() time.Time { return time.Unix(10, 0).UTC() }
	message := &discordgo.Message{
		ID: "message", GuildID: "guild", ChannelID: "channel", Content: "hello", Type: discordgo.MessageTypeReply,
		Author:           &discordgo.User{ID: "user", Username: "alice", GlobalName: "Alice"},
		Mentions:         []*discordgo.User{{ID: "bot"}, nil},
		MessageReference: &discordgo.MessageReference{MessageID: "parent", ChannelID: "channel"},
	}
	controller.HandleMessage(context.Background(), &discordgo.MessageCreate{Message: message})

	require.NotNil(t, client.request)
	event := client.request.Event
	assert.Equal(t, "message", event.MessageId)
	assert.Equal(t, discordv1.MessageKind_MESSAGE_KIND_REPLY, event.Kind)
	assert.Equal(t, []string{"bot"}, event.MentionedUserIds)
	assert.Equal(t, "parent", event.Reference.MessageId)
	assert.Equal(t, time.Unix(10, 0).UTC(), event.IngestedAt.AsTime())
}

func TestControllerIgnoresIncompleteGatewayEvent(t *testing.T) {
	client := &fakeWorkerClient{}
	controller := NewController(client)
	controller.HandleMessage(context.Background(), &discordgo.MessageCreate{})
	assert.Nil(t, client.request)
}
