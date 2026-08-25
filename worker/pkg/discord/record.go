package discord

import (
	"context"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/std/app"
	"go.uber.org/zap"
)

// recordTimeout bounds recording, which runs off the reply's critical path.
const recordTimeout = 5 * time.Second

// record persists messages as bot-involved conversation, expiring after the guild's
// retention. The context is detached: the reply record runs right where a shutdown lands
// after the answer is posted, and losing it would hollow out the thread's stored history.
// A failure costs stored context, never the reply, so it is logged and swallowed.
func (p *Processor) record(ctx context.Context, retentionDays int, messages ...*discordgo.Message) {
	if p.recorder == nil {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	for _, message := range messages {
		if message == nil {
			continue
		}
		if err := p.recorder.Record(recordCtx, message, retentionDays); err != nil {
			app.L().Warn("Recording Discord message failed",
				zap.String("channel_id", message.ChannelID),
				zap.String("message_id", message.ID),
				zap.Error(err))
		}
	}
}

// stampGuild fills in the guild on messages returned by the send API, which omits it.
func stampGuild(guildID string, messages []*discordgo.Message) []*discordgo.Message {
	for _, message := range messages {
		if message != nil && message.GuildID == "" {
			message.GuildID = guildID
		}
	}
	return messages
}

// stampReplyReferences links stored bot messages to the request they answer.
func stampReplyReferences(request *discordgo.Message, replies []*discordgo.Message) []*discordgo.Message {
	if request == nil {
		return replies
	}
	for _, reply := range replies {
		if reply == nil {
			continue
		}
		reply.MessageReference = &discordgo.MessageReference{
			GuildID: request.GuildID, ChannelID: request.ChannelID, MessageID: request.ID,
		}
	}
	return replies
}

// withAttachmentNote returns a copy whose blank content names its image attachments, so
// an image-only post still shows up in stored history.
func withAttachmentNote(message *discordgo.Message) *discordgo.Message {
	if strings.TrimSpace(message.Content) != "" || len(message.Attachments) == 0 {
		return message
	}
	names := make([]string, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		if attachment != nil && strings.HasPrefix(attachment.ContentType, "image/") {
			names = append(names, attachment.Filename)
		}
	}
	if len(names) == 0 {
		return message
	}
	copied := *message
	copied.Content = "[image: " + strings.Join(names, ", ") + "]"
	return &copied
}
