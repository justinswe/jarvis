package discord

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

var errEmptyMessageContent = errors.New("empty message content")

func (b *Bot) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if b.shouldIgnore(m) {
		return
	}
	channel, err := s.Channel(m.ChannelID)
	if err != nil {
		app.L().Warn("Failed to fetch channel", zap.Error(err))
		return
	}
	if !b.isTargeted(s, m, channel) {
		return
	}
	b.handleMessage(channel, m)
}

func (b *Bot) handleMessage(channel *discordgo.Channel, m *discordgo.MessageCreate) {
	started := time.Now()
	fields := discordRequestFields(channel, m)
	app.L().Info("Discord AI request received", fields...)
	if b.addReaction != nil {
		if err := b.addReaction(m.ChannelID, m.ID, "🤔"); err != nil {
			app.L().Debug("Failed to add processing reaction", zap.Error(err))
		}
	}
	defer func() {
		if b.removeReaction != nil {
			if err := b.removeReaction(m.ChannelID, m.ID, "🤔", b.botID); err != nil {
				app.L().Debug("Failed to remove processing reaction", zap.Error(err))
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), b.messageTimeout)
	defer cancel()
	b.processMessage(ctx, channel, m, started)
}

func (b *Bot) processMessage(ctx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate, started time.Time) {
	fields := discordRequestFields(channel, m)
	messages, err := b.buildPrompt(ctx, channel, m)
	if err != nil {
		app.L().Warn("Failed to build AI request", append(fields, zap.Error(err))...)
		if errors.Is(err, errEmptyMessageContent) {
			b.sendEmptyMentionReply(m.ChannelID)
		} else {
			b.sendErrorReply(m.ChannelID)
		}
		return
	}
	app.L().Info("Sending request to Gemini", append(fields, zap.Int("context_message_count", len(messages)))...)
	response, err := b.generator.Generate(ctx, genai.GenerateRequest{
		Messages:  messages,
		RequestID: m.ID,
		CallerID:  m.Author.ID,
		ChannelID: m.ChannelID,
		Tools:     []genai.FunctionTool{b.searchCurrentChannel(m.GuildID, m.ChannelID, m.ID)},
	})
	if err != nil {
		app.L().Warn("Gemini generation failed", append(fields,
			zap.Duration("duration", time.Since(started)),
			zap.Error(err),
		)...)
		b.sendErrorReply(m.ChannelID)
		return
	}
	reply := stripBotPrefix(html.UnescapeString(response.Text))
	if strings.TrimSpace(reply) == "" {
		app.L().Warn("Gemini returned an empty response", append(fields,
			zap.Duration("duration", time.Since(started)),
			zap.Bool("grounded", response.Grounded),
			zap.Int("source_count", len(response.Sources)),
		)...)
		b.sendErrorReply(m.ChannelID)
		return
	}
	if response.Grounded && len(response.Sources) > 0 {
		reply = appendSources(reply, response.Sources)
	}
	if err := b.sendReply(channel, m, reply); err != nil {
		app.L().Warn("Failed to post Discord reply", append(fields,
			zap.Duration("duration", time.Since(started)),
			zap.Bool("grounded", response.Grounded),
			zap.Int("source_count", len(response.Sources)),
			zap.Error(err),
		)...)
		return
	}
	app.L().Info("Discord AI request completed", append(fields,
		zap.Duration("duration", time.Since(started)),
		zap.Bool("grounded", response.Grounded),
		zap.Int("source_count", len(response.Sources)),
		zap.Int("response_runes", len([]rune(reply))),
	)...)
}

func discordRequestFields(channel *discordgo.Channel, m *discordgo.MessageCreate) []zap.Field {
	fields := []zap.Field{
		zap.String("user_id", m.Author.ID),
		zap.String("username", displayName(m.Author)),
		zap.String("guild_id", m.GuildID),
		zap.String("channel_id", m.ChannelID),
		zap.String("message_id", m.ID),
		zap.Bool("thread", isThreadChannel(channel)),
	}
	if channel != nil {
		fields = append(fields, zap.String("parent_channel_id", channel.ParentID))
	}
	return fields
}

func (b *Bot) shouldIgnore(m *discordgo.MessageCreate) bool {
	return m == nil || m.Message == nil || m.Author == nil || m.Author.Bot || m.Author.ID == b.botID ||
		(m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply)
}

func (b *Bot) isTargeted(s *discordgo.Session, m *discordgo.MessageCreate, channel *discordgo.Channel) bool {
	if mentionsBot(m.Mentions, b.botID) {
		return true
	}
	if !isThreadChannel(channel) {
		return false
	}
	if channel.OwnerID == b.botID {
		return true
	}
	ref := m.MessageReference
	if ref == nil || ref.ChannelID != m.ChannelID {
		return false
	}
	referenced, err := s.ChannelMessage(m.ChannelID, ref.MessageID)
	return err == nil && referenced.Author != nil && referenced.Author.ID == b.botID
}

func (b *Bot) sendReply(channel *discordgo.Channel, m *discordgo.MessageCreate, reply string) error {
	if isThreadChannel(channel) {
		return b.sendMessageChunks(m.ChannelID, reply)
	}
	thread, err := b.startThread(m.ChannelID, m.ID, fmt.Sprintf("AI Thread - %s", safeThreadName(m.Author.Username, m.Author.GlobalName)), 60)
	if err != nil {
		return b.sendMessageChunks(m.ChannelID, reply)
	}
	if err := b.sendMessageChunks(thread.ID, reply); err != nil {
		return b.sendMessageChunks(m.ChannelID, reply)
	}
	return nil
}

func (b *Bot) sendErrorReply(channelID string) {
	_ = b.sendMessageChunks(channelID, "Sorry, I ran into an error while generating a response.")
}
func (b *Bot) sendEmptyMentionReply(channelID string) {
	_ = b.sendMessageChunks(channelID, "Please include a question with your mention.")
}
