package discord

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	googlegenai "google.golang.org/genai"
)

var errEmptyMessageContent = errors.New("empty message content")

var addAdminCommand = regexp.MustCompile(`(?i)^(?:please )?add <@!?(\d{17,20})> as (?:a )?(?:jarvis )?admin(?:istrator)?\.?$`)

const reactionCleanupTimeout = 5 * time.Second

// Process handles one message event and returns after all Discord side effects finish.
func (p *Processor) Process(ctx context.Context, m *discordgo.MessageCreate) error {
	if p.shouldIgnore(m) {
		return nil
	}
	channel, err := p.client.Channel(ctx, m.ChannelID)
	if err != nil {
		return errors.Wrap(err, "fetch Discord channel")
	}
	if !p.isTargeted(ctx, m, channel) {
		return nil
	}
	if handled := p.handleAddAdminCommand(ctx, m); handled {
		return nil
	}

	guildConfig, err := p.configs.Get(ctx, m.GuildID)
	if err != nil {
		return errors.Wrap(err, "resolve server configuration")
	}
	if err := guildConfig.Validate(); err != nil {
		return errors.Wrap(err, "validate server configuration")
	}
	settings := guildConfig.Settings

	started := time.Now()
	fields := discordRequestFields(channel, m)
	app.L().Info("Discord AI request received", fields...)
	if err := p.client.AddReaction(ctx, m.ChannelID, m.ID, processingReaction); err != nil {
		app.L().Debug("Failed to add processing reaction", zap.Error(err))
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), reactionCleanupTimeout)
		defer cleanupCancel()
		if err := p.client.RemoveReaction(cleanupCtx, m.ChannelID, m.ID, processingReaction, p.botID); err != nil {
			app.L().Debug("Failed to remove processing reaction", zap.Error(err))
		}
	}()

	processCtx, cancel := context.WithTimeout(ctx, settings.MessageTimeout)
	defer cancel()
	return p.processMessage(processCtx, ctx, channel, m, guildConfig, started)
}

func (p *Processor) handleAddAdminCommand(ctx context.Context, m *discordgo.MessageCreate) bool {
	command := strings.TrimSpace(sanitizeContent(m.Content, p.botID))
	match := addAdminCommand.FindStringSubmatch(command)
	if match == nil {
		return false
	}
	if _, root := p.rootUsers[m.Author.ID]; !root {
		_ = p.sendMessageChunks(ctx, m.ChannelID, "Only a Jarvis root user can add administrators.")
		return true
	}
	var targets []string
	for _, mention := range m.Mentions {
		if mention != nil && mention.ID != "" && mention.ID != p.botID {
			targets = append(targets, mention.ID)
		}
	}
	if len(targets) != 1 || targets[0] != match[1] {
		_ = p.sendMessageChunks(ctx, m.ChannelID, "Please mention exactly one Discord user to add as a Jarvis administrator.")
		return true
	}
	if p.manager == nil {
		_ = p.sendMessageChunks(ctx, m.ChannelID, "Administrator persistence is disabled, so no change was made.")
		return true
	}
	updated, err := p.manager.AddAdmin(ctx, m.GuildID, m.Author.ID, targets[0])
	if err != nil || !slices.Contains(updated.AdminUserIDs, targets[0]) {
		_ = p.sendMessageChunks(ctx, m.ChannelID, "I could not persist that administrator change, so no success is being reported.")
		return true
	}
	_ = p.sendMessageChunks(ctx, m.ChannelID, fmt.Sprintf("<@%s> is now a Jarvis administrator.", targets[0]))
	return true
}

func (p *Processor) processMessage(ctx, replyCtx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate, guildConfig config.GuildConfig, started time.Time) error {
	fields := discordRequestFields(channel, m)
	settings := guildConfig.Settings
	messages, err := p.buildPrompt(ctx, channel, m, settings)
	if err != nil {
		app.L().Warn("Failed to build AI request", append(fields, zap.Error(err))...)
		if errors.Is(err, errEmptyMessageContent) {
			p.sendEmptyMentionReply(replyCtx, m.ChannelID)
			return nil
		}
		p.sendErrorReply(replyCtx, m.ChannelID)
		return err
	}
	app.L().Info("Sending request to Gemini", append(fields, zap.Int("context_message_count", len(messages)))...)
	request := genai.GenerateRequest{
		Messages:  messages,
		RequestID: m.ID,
		CallerID:  m.Author.ID,
		ChannelID: m.ChannelID,
		Config: &genai.RequestConfig{
			Prompt:           settings.EffectivePrompt(),
			MaxOutputTokens:  settings.MaxOutputTokens,
			WebSearchEnabled: settings.WebSearchEnabled,
			ThinkingLevel:    googlegenai.ThinkingLevelHigh,
			AccuracyPolicy:   genai.ClassifyAccuracyPolicy(sanitizeContent(m.Content, p.botID)),
		},
	}
	request.Tools = append(request.Tools, p.runtimeContext())
	request.Tools = append(request.Tools, p.reactToMessage(m.ChannelID, m.ID))
	if settings.ChannelSearchEnabled {
		request.Tools = append(request.Tools, p.searchCurrentChannel(m.GuildID, m.ChannelID, m.ID))
	}
	if tools, authorized := p.configurationTools(ctx, m, guildConfig); authorized {
		request.Tools = append(request.Tools, tools...)
	}
	response, err := p.generator.Generate(ctx, request)
	if err != nil {
		app.L().Warn("Gemini generation failed", append(fields,
			zap.Duration("duration", time.Since(started)),
			zap.Error(err),
		)...)
		p.sendErrorReply(replyCtx, m.ChannelID)
		return errors.Wrap(err, "generate response")
	}
	reply := stripBotPrefix(html.UnescapeString(response.Text))
	if strings.TrimSpace(reply) == "" {
		err := errors.New("Gemini returned an empty response")
		app.L().Warn(err.Error(), append(fields,
			zap.Duration("duration", time.Since(started)),
			zap.Bool("grounded", response.Grounded),
			zap.Int("source_count", len(response.Sources)),
		)...)
		p.sendErrorReply(replyCtx, m.ChannelID)
		return err
	}
	if response.Grounded && len(response.Sources) > 0 {
		reply = appendSources(reply, response.Sources)
	}
	reply = appendEvidence(reply, response.Evidence)
	if err := p.sendReply(replyCtx, channel, m, reply); err != nil {
		app.L().Warn("Failed to post Discord reply", append(fields,
			zap.Duration("duration", time.Since(started)),
			zap.Bool("grounded", response.Grounded),
			zap.Int("source_count", len(response.Sources)),
			zap.Error(err),
		)...)
		return err
	}
	app.L().Info("Discord AI request completed", append(fields,
		zap.Duration("duration", time.Since(started)),
		zap.Bool("grounded", response.Grounded),
		zap.Int("source_count", len(response.Sources)),
		zap.Int("evidence_count", len(response.Evidence)),
		zap.Int("response_runes", len([]rune(reply))),
	)...)
	return nil
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

func (p *Processor) shouldIgnore(m *discordgo.MessageCreate) bool {
	return m == nil || m.Message == nil || m.Author == nil || m.Author.Bot || m.Author.ID == p.botID ||
		(m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply)
}

func (p *Processor) isTargeted(ctx context.Context, m *discordgo.MessageCreate, channel *discordgo.Channel) bool {
	if mentionsBot(m.Mentions, p.botID) {
		return true
	}
	if ref := m.MessageReference; ref != nil && ref.ChannelID == m.ChannelID {
		referenced, err := p.client.Message(ctx, m.ChannelID, ref.MessageID)
		if err == nil && referenced.Author != nil && referenced.Author.ID == p.botID {
			return true
		}
	}
	if !isThreadChannel(channel) {
		return false
	}
	if channel.OwnerID == p.botID {
		return true
	}
	messages, err := p.client.Messages(ctx, m.ChannelID, 100, m.ID)
	if err != nil {
		app.L().Debug("Failed to inspect thread activation history", zap.String("guild_id", m.GuildID),
			zap.String("channel_id", m.ChannelID), zap.String("message_id", m.ID), zap.Error(err))
		return false
	}
	for _, message := range messages {
		if message != nil && ((message.Author != nil && message.Author.ID == p.botID) || mentionsBot(message.Mentions, p.botID)) {
			return true
		}
	}
	return false
}

func (p *Processor) sendReply(ctx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate, reply string) error {
	if isThreadChannel(channel) {
		return p.sendMessageChunks(ctx, m.ChannelID, reply)
	}
	thread, err := p.client.StartThread(ctx, m.ChannelID, m.ID, fmt.Sprintf("AI Thread - %s", safeThreadName(m.Author.Username, m.Author.GlobalName)), 60)
	if err != nil {
		return p.sendMessageChunks(ctx, m.ChannelID, reply)
	}
	if err := p.sendMessageChunks(ctx, thread.ID, reply); err != nil {
		return p.sendMessageChunks(ctx, m.ChannelID, reply)
	}
	return nil
}

func (p *Processor) sendErrorReply(ctx context.Context, channelID string) {
	_ = p.sendMessageChunks(ctx, channelID, "Sorry, I ran into an error while generating a response.")
}

func (p *Processor) sendEmptyMentionReply(ctx context.Context, channelID string) {
	_ = p.sendMessageChunks(ctx, channelID, "Please include a question with your mention.")
}
