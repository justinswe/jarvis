package consumer

import (
	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/std/errors"
)

// unhandledMessageType marks an event whose kind the transport does not model.
//
// It deliberately matches no discordgo message type, because the worker acts only on
// MessageTypeDefault and MessageTypeReply (see the guard in the discord package). An
// event the ingestor could not classify must therefore land on neither, and mapping it
// to the zero value would silently make it look like a default message.
const unhandledMessageType = discordgo.MessageType(-1)

// discordMessage converts a normalized transport event into a Gateway message.
func discordMessage(req *discordv1.IngestMessageRequest) (*discordgo.MessageCreate, error) {
	if req == nil || req.Event == nil {
		return nil, errors.New("event is required")
	}
	event := req.Event
	if event.MessageId == "" {
		return nil, errors.New("message_id is required")
	}
	if event.ChannelId == "" {
		return nil, errors.New("channel_id is required")
	}
	if event.Author == nil || event.Author.Id == "" {
		return nil, errors.New("author.id is required")
	}

	messageType := unhandledMessageType
	switch event.Kind {
	case discordv1.MessageKind_MESSAGE_KIND_DEFAULT:
		messageType = discordgo.MessageTypeDefault
	case discordv1.MessageKind_MESSAGE_KIND_REPLY:
		messageType = discordgo.MessageTypeReply
	}
	message := &discordgo.Message{
		ID:        event.MessageId,
		GuildID:   event.GuildId,
		ChannelID: event.ChannelId,
		Content:   event.Content,
		Type:      messageType,
		Author: &discordgo.User{
			ID:         event.Author.Id,
			Username:   event.Author.Username,
			GlobalName: event.Author.GlobalName,
			Bot:        event.Author.Bot,
		},
	}
	for _, userID := range event.MentionedUserIds {
		if userID != "" {
			message.Mentions = append(message.Mentions, &discordgo.User{ID: userID})
		}
	}
	for _, attachment := range event.Attachments {
		if attachment == nil {
			continue
		}
		message.Attachments = append(message.Attachments, &discordgo.MessageAttachment{
			ID: attachment.Id, Filename: attachment.Filename, ContentType: attachment.ContentType,
			Size: int(attachment.Size), URL: attachment.Url, ProxyURL: attachment.ProxyUrl,
			Width: int(attachment.Width), Height: int(attachment.Height),
		})
	}
	if event.Reference != nil {
		message.MessageReference = &discordgo.MessageReference{
			MessageID: event.Reference.MessageId,
			ChannelID: event.Reference.ChannelId,
			GuildID:   event.GuildId,
		}
	}
	return &discordgo.MessageCreate{Message: message}, nil
}
