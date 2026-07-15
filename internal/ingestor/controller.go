package ingestor

import (
	"context"
	"time"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/app"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Controller translates Gateway events into worker requests.
type Controller struct {
	publisher MessagePublisher
	now       func() time.Time
}

// NewController creates a Discord event controller.
func NewController(publisher MessagePublisher) *Controller {
	return &Controller{publisher: publisher, now: time.Now}
}

// HandleMessage forwards one normalized Gateway event to the worker.
func (c *Controller) HandleMessage(ctx context.Context, event *discordgo.MessageCreate) {
	request := c.request(event)
	if request == nil {
		return
	}
	if err := c.publisher.Publish(ctx, request); err != nil {
		message := request.Event
		app.L().Warn("Worker failed to process Discord message",
			zap.String("guild_id", message.GuildId),
			zap.String("channel_id", message.ChannelId),
			zap.String("message_id", message.MessageId),
			zap.Error(err),
		)
	}
}

func (c *Controller) request(event *discordgo.MessageCreate) *discordv1.IngestMessageRequest {
	if event == nil || event.Message == nil || event.Author == nil {
		return nil
	}
	message := event.Message
	kind := discordv1.MessageKind_MESSAGE_KIND_UNSPECIFIED
	switch message.Type {
	case discordgo.MessageTypeDefault:
		kind = discordv1.MessageKind_MESSAGE_KIND_DEFAULT
	case discordgo.MessageTypeReply:
		kind = discordv1.MessageKind_MESSAGE_KIND_REPLY
	}
	normalized := &discordv1.DiscordMessageCreateEvent{
		MessageId:  message.ID,
		GuildId:    message.GuildID,
		ChannelId:  message.ChannelID,
		Content:    message.Content,
		Kind:       kind,
		IngestedAt: timestamppb.New(c.now()),
		Author: &discordv1.MessageAuthor{
			Id:         message.Author.ID,
			Username:   message.Author.Username,
			GlobalName: message.Author.GlobalName,
			Bot:        message.Author.Bot,
		},
	}
	for _, mention := range message.Mentions {
		if mention != nil && mention.ID != "" {
			normalized.MentionedUserIds = append(normalized.MentionedUserIds, mention.ID)
		}
	}
	if message.MessageReference != nil {
		normalized.Reference = &discordv1.MessageReference{
			MessageId: message.MessageReference.MessageID,
			ChannelId: message.MessageReference.ChannelID,
		}
	}
	return &discordv1.IngestMessageRequest{Event: normalized}
}
