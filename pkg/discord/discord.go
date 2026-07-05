package discord

import (
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"go.uber.org/zap"
)

// Generator defines the minimal interface required by the bot to produce responses.
type Generator interface {
	Generate(ctx context.Context, req genai.GenerateRequest) (string, error)
}

const (
	defaultWebIndicator                          = "#web"
	defaultMessageTimeout                        = 30 * time.Second
	defaultWebGroundingMessageTimeout            = 75 * time.Second
	defaultWebGroundingTimeout                   = 45 * time.Second
	defaultWebGroundingAttemptTimeout            = 20 * time.Second
	defaultWebGroundingAddendumMaxTokens         = 512
	defaultWebGroundingAPIVersion                = "v1"
	defaultWebGroundingMaxResults                = 5
	defaultWebGroundingBudgetRatio       float32 = 0.30
	defaultWebUserRPM                            = 5
	defaultWebGlobalRPM                          = 40
	discordMessageMaxLength                      = 2000
	threadGroundingRetention                     = 24 * time.Hour
	threadGroundingMemoryMaxRunes                = 6000
	threadGroundingPromptMaxRunes                = 4000
	threadGroundingHistoryScanLimit              = 250
	webGroundingAddendumMarker                   = "Web-grounded addendum ("
	threadHistoryContextMaxRunes                 = 7000
	parentHistoryContextMaxRunes                 = 4000
	channelHistoryContextMaxRunes                = 7000
)

const threadContextInstructions = "CONTEXT PRIORITY RULES:\n1) CURRENT REQUEST is the primary task.\n2) THREAD WEB GROUNDING CONTEXT contains verified web facts for this thread.\n3) THREAD HISTORY CONTEXT is prior conversation in this same thread.\n4) PARENT CHANNEL CONTEXT is background from the parent channel and can be stale.\n5) When context conflicts, prioritize in this order: CURRENT REQUEST > THREAD WEB GROUNDING CONTEXT > THREAD HISTORY CONTEXT > PARENT CHANNEL CONTEXT.\n6) If context lacks information, answer from your own knowledge. NEVER say the provided context doesn't have enough info."

const channelContextInstructions = "CONTEXT PRIORITY RULES:\n1) CURRENT REQUEST is the primary task.\n2) CHANNEL HISTORY CONTEXT is nearby background from this channel.\n3) If context conflicts with CURRENT REQUEST, prioritize CURRENT REQUEST.\n4) If context lacks information, answer from your own knowledge. NEVER say the provided context doesn't have enough info."

type threadGroundingMemory struct {
	content   string
	updatedAt time.Time
}

// WebGroundingConfig captures all #web runtime controls.
type WebGroundingConfig struct {
	Enabled                 bool
	Indicator               string
	MessageTimeout          time.Duration
	Timeout                 time.Duration
	AttemptTimeout          time.Duration
	AddendumMaxOutputTokens int
	APIVersion              string
	MaxResults              int
	BudgetRatio             float32
	RequireCitations        bool
	DisableAllowlists       bool
	ChannelAllowlist        []string
	RoleAllowlist           []string
	UserRPM                 int
	GlobalRPM               int
}

// Config captures runtime settings for the Discord bot.
type Config struct {
	Token                string
	OutsideContextWindow int
	ThreadContextWindow  int
	WebGrounding         WebGroundingConfig
}

type webGroundingState struct {
	enabled                 bool
	indicator               string
	messageTimeout          time.Duration
	timeout                 time.Duration
	attemptTimeout          time.Duration
	addendumMaxOutputTokens int
	apiVersion              string
	maxResults              int
	budgetRatio             float32
	requireCitations        bool
	disableAllowlists       bool
	channelAllowlist        map[string]struct{}
	roleAllowlist           map[string]struct{}
	limiter                 *webRateLimiter
}

// Bot wraps Discord session handling and LLM orchestration.
type Bot struct {
	session       *discordgo.Session
	botID         string
	generator     Generator
	outsideWindow int
	threadWindow  int
	fetchMessages func(channelID string, limit int, beforeID string) ([]*discordgo.Message, error)
	sendMessage   func(channelID, content string) (*discordgo.Message, error)
	startThread   func(channelID, messageID, name string, autoArchiveDuration int) (*discordgo.Channel, error)
	web           webGroundingState

	mu        sync.Mutex
	processed map[string]time.Time
	grounding map[string]threadGroundingMemory
}

// errEmptyMessageContent is returned when a message contains no content after sanitization.
var errEmptyMessageContent = errors.New("empty message content")

// errEmptyWebGroundingQuery is returned when #web is present without a query.
var errEmptyWebGroundingQuery = errors.New("empty web grounding query")

type webGroundingDeniedError struct {
	reason string
}

func (e webGroundingDeniedError) Error() string {
	return e.reason
}

// NewBot constructs a Bot with the provided generator.
func NewBot(cfg Config, gen Generator) (*Bot, error) {
	if cfg.Token == "" {
		return nil, errors.New("discord bot token is required")
	}
	outside := cfg.OutsideContextWindow
	if outside <= 0 {
		outside = 10
	}
	thread := cfg.ThreadContextWindow
	if thread <= 0 {
		thread = 20
	}

	dg, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("error creating Discord session: %w", err)
	}

	u, err := dg.User("@me")
	if err != nil {
		return nil, fmt.Errorf("error obtaining bot account details: %w", err)
	}

	web := normalizeWebGroundingState(cfg.WebGrounding)

	return &Bot{
		session:       dg,
		botID:         u.ID,
		generator:     gen,
		outsideWindow: outside,
		threadWindow:  thread,
		web:           web,
		fetchMessages: func(channelID string, limit int, beforeID string) ([]*discordgo.Message, error) {
			return dg.ChannelMessages(channelID, limit, beforeID, "", "")
		},
		sendMessage: func(channelID, content string) (*discordgo.Message, error) {
			return dg.ChannelMessageSend(channelID, content)
		},
		startThread: func(channelID, messageID, name string, autoArchiveDuration int) (*discordgo.Channel, error) {
			return dg.MessageThreadStart(channelID, messageID, name, autoArchiveDuration)
		},
		processed: make(map[string]time.Time),
		grounding: make(map[string]threadGroundingMemory),
	}, nil
}

// Start connects to Discord and blocks until the provided context is canceled.
func (b *Bot) Start(ctx context.Context) error {
	b.session.AddHandler(b.messageCreate)
	b.session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent

	if err := b.session.Open(); err != nil {
		return fmt.Errorf("error opening Discord connection: %w", err)
	}

	app.L().Info("Discord bot connected", zap.String("bot_id", b.botID))

	go b.janitor(ctx)

	go func() {
		<-ctx.Done()
		app.L().Info("Shutting down Discord session")
		_ = b.session.Close()
	}()

	<-ctx.Done()
	return nil
}

// janitor periodically cleans up the deduplication cache.
func (b *Bot) janitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.cleanup()
		}
	}
}

// cleanup removes expired entries from the processed map.
func (b *Bot) cleanup() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	for id, timestamp := range b.processed {
		if now.Sub(timestamp) > 10*time.Minute {
			delete(b.processed, id)
		}
	}
	for threadID, state := range b.grounding {
		if now.Sub(state.updatedAt) > threadGroundingRetention {
			delete(b.grounding, threadID)
		}
	}
}

// messageCreate handles incoming Discord messages.
func (b *Bot) messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if b.shouldIgnore(m) {
		return
	}

	channel, err := s.Channel(m.ChannelID)
	if err != nil {
		app.L().Warn("Failed to fetch channel", zap.Error(err), zap.String("channel_id", m.ChannelID))
		return
	}

	if !b.isTargeted(s, m, channel) {
		return
	}

	if !b.markProcessed(m.ID) {
		app.L().Debug("Ignoring duplicate message", zap.String("message_id", m.ID))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.messageTimeoutFor(m))
	defer cancel()

	b.processMessage(ctx, s, channel, m)
}

func (b *Bot) messageTimeoutFor(m *discordgo.MessageCreate) time.Duration {
	if b.web.enabled {
		content := ""
		if m != nil && m.Message != nil {
			content = sanitizeContent(m.Content, b.botID)
		}
		useWebGrounding, _ := parseWebInvocation(content, b.web.indicator)
		if useWebGrounding {
			if b.web.messageTimeout > 0 {
				return b.web.messageTimeout
			}
			return defaultWebGroundingMessageTimeout
		}
	}
	return defaultMessageTimeout
}

// processMessage orchestrates the generation and reply logic.
func (b *Bot) processMessage(ctx context.Context, s *discordgo.Session, channel *discordgo.Channel, m *discordgo.MessageCreate) {
	req, err := b.prepareGenerateRequest(channel, m)
	if err != nil {
		var deniedErr webGroundingDeniedError
		switch {
		case errors.Is(err, errEmptyMessageContent):
			app.L().Debug("Ignoring empty message content", zap.String("channel_id", m.ChannelID))
			b.sendEmptyMentionReply(s, m)
		case errors.Is(err, errEmptyWebGroundingQuery):
			b.sendWebUsageReply(s, m.ChannelID)
		case errors.As(err, &deniedErr):
			b.sendWebDeniedReply(s, m.ChannelID, deniedErr.reason)
		default:
			app.L().Warn("Failed to prepare generation request", zap.Error(err), zap.String("channel_id", m.ChannelID))
			b.sendErrorReply(s, m.ChannelID)
		}
		return
	}

	reply, err := b.generator.Generate(ctx, req)
	if err != nil {
		app.L().Warn("LLM generation failed", zap.Error(err))
		b.sendErrorReply(s, m.ChannelID)
		return
	}

	reply = stripBotPrefix(html.UnescapeString(reply))
	if strings.TrimSpace(reply) == "" {
		app.L().Warn("Received empty response from LLM")
		b.sendErrorReply(s, m.ChannelID)
		return
	}

	threadID, err := b.sendReply(s, channel, m, reply)
	if err != nil {
		app.L().Warn("Failed to post reply", zap.Error(err), zap.String("channel_id", m.ChannelID))
		return
	}
	if req.Options.UseWebGrounding && threadID != "" {
		b.rememberThreadGrounding(threadID, reply)
	}
}

func (b *Bot) prepareGenerateRequest(channel *discordgo.Channel, m *discordgo.MessageCreate) (genai.GenerateRequest, error) {
	var req genai.GenerateRequest

	currentContent := sanitizeContent(m.Content, b.botID)
	if currentContent == "" {
		return req, errEmptyMessageContent
	}

	useWebGrounding, query := parseWebInvocation(currentContent, b.web.indicator)
	if useWebGrounding {
		if query == "" {
			return req, errEmptyWebGroundingQuery
		}
		if b.threadAlreadyGrounded(channel, m) {
			return req, webGroundingDeniedError{
				reason: fmt.Sprintf("this thread already used `%s` once; ask follow-up questions without `%s` and I will reuse that grounded context", b.web.indicator, b.web.indicator),
			}
		}

		allowed, reason := b.canUseWebGrounding(channel, m)
		if !allowed {
			return req, webGroundingDeniedError{reason: reason}
		}
		currentContent = query
		req.Options = genai.GenerateOptions{
			UseWebGrounding:                     true,
			GroundingBudgetRatio:                b.web.budgetRatio,
			RequireCitations:                    b.web.requireCitations,
			WebGroundingTimeout:                 b.web.timeout,
			WebGroundingAttemptTimeout:          b.web.attemptTimeout,
			WebGroundingAddendumMaxOutputTokens: b.web.addendumMaxOutputTokens,
			WebGroundingAPIVersion:              b.web.apiVersion,
			WebGroundingMaxResults:              b.web.maxResults,
		}
	}

	prompt, err := b.buildPromptFromCurrent(channel, m, currentContent)
	if err != nil {
		return req, err
	}
	req.Messages = prompt

	return req, nil
}

// shouldIgnore filters out messages that the bot should not process.
func (b *Bot) shouldIgnore(m *discordgo.MessageCreate) bool {
	if m.Author == nil {
		return true
	}
	if m.Author.Bot {
		return true
	}
	if m.Author.ID == b.botID {
		return true
	}
	if m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply {
		return true
	}
	return false
}

// markProcessed attempts to add a message ID to the cache. Returns false if already present.
func (b *Bot) markProcessed(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.processed[id]; exists {
		return false
	}

	b.processed[id] = time.Now()
	return true
}

// sendErrorReply sends a generic error message to the channel.
func (b *Bot) sendErrorReply(s *discordgo.Session, channelID string) {
	if err := b.sendMessageChunks(channelID, "Sorry, I ran into an error while generating a response."); err != nil {
		app.L().Warn("Failed sending error reply", zap.Error(err), zap.String("channel_id", channelID))
	}
}

// sendEmptyMentionReply sends a helpful message when user mentions bot without content.
func (b *Bot) sendEmptyMentionReply(s *discordgo.Session, m *discordgo.MessageCreate) {
	reply := "Hey! It looks like you mentioned me but didn't include a question. Try something like: `@jarvis what's the weather like?`"
	if err := b.sendMessageChunks(m.ChannelID, reply); err != nil {
		app.L().Warn("Failed sending empty mention reply", zap.Error(err), zap.String("channel_id", m.ChannelID))
	}
}

// sendWebUsageReply explains how to use #web mode.
func (b *Bot) sendWebUsageReply(s *discordgo.Session, channelID string) {
	reply := fmt.Sprintf("To use web grounding, start your message with `%s`, for example: `%s latest Go 1.24 release notes`.", b.web.indicator, b.web.indicator)
	if err := b.sendMessageChunks(channelID, reply); err != nil {
		app.L().Warn("Failed sending web usage reply", zap.Error(err), zap.String("channel_id", channelID))
	}
}

// sendWebDeniedReply reports why #web mode was not allowed.
func (b *Bot) sendWebDeniedReply(s *discordgo.Session, channelID, reason string) {
	reply := fmt.Sprintf("I could not run `%s` for this message: %s", b.web.indicator, reason)
	if err := b.sendMessageChunks(channelID, reply); err != nil {
		app.L().Warn("Failed sending web denied reply", zap.Error(err), zap.String("channel_id", channelID))
	}
}

// sendReply routes the response to the appropriate location (thread or channel).
func (b *Bot) sendReply(s *discordgo.Session, channel *discordgo.Channel, m *discordgo.MessageCreate, reply string) (string, error) {
	if isThreadChannel(channel) {
		if err := b.sendMessageChunks(m.ChannelID, reply); err != nil {
			app.L().Warn("Failed sending thread reply", zap.Error(err), zap.String("channel_id", m.ChannelID))
			return "", err
		}
		return m.ChannelID, nil
	}

	threadName := fmt.Sprintf("AI Thread - %s", safeThreadName(m.Author.Username, m.Author.GlobalName))
	startThread := b.startThread
	if startThread == nil && b.session != nil {
		startThread = func(channelID, messageID, name string, autoArchiveDuration int) (*discordgo.Channel, error) {
			return b.session.MessageThreadStart(channelID, messageID, name, autoArchiveDuration)
		}
	}
	if startThread == nil {
		app.L().Warn("Thread creation function not configured; sending reply in channel",
			zap.String("channel_id", m.ChannelID),
		)
		if err := b.sendMessageChunks(m.ChannelID, reply); err != nil {
			app.L().Warn("Failed sending channel reply", zap.Error(err), zap.String("channel_id", m.ChannelID))
			return "", err
		}
		return "", nil
	}

	thread, err := startThread(m.ChannelID, m.Message.ID, threadName, 60)
	if err != nil {
		app.L().Warn("Failed to create thread; sending reply in channel instead",
			zap.Error(err),
			zap.String("channel_id", m.ChannelID),
		)
		if sendErr := b.sendMessageChunks(m.ChannelID, reply); sendErr != nil {
			app.L().Warn("Failed sending fallback channel reply",
				zap.Error(sendErr),
				zap.String("channel_id", m.ChannelID),
			)
			return "", sendErr
		}
		return "", nil
	}

	if err := b.sendMessageChunks(thread.ID, reply); err != nil {
		app.L().Warn("Failed sending thread message; attempting fallback channel send",
			zap.Error(err),
			zap.String("thread_id", thread.ID),
			zap.String("channel_id", m.ChannelID),
		)
		if sendErr := b.sendMessageChunks(m.ChannelID, reply); sendErr != nil {
			app.L().Warn("Failed sending channel fallback after thread send failure",
				zap.Error(sendErr),
				zap.String("channel_id", m.ChannelID),
			)
			return "", sendErr
		}
		return "", nil
	}

	return thread.ID, nil
}

// isTargeted determines if the bot should respond to the message.
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
	if err != nil || referenced.Author == nil {
		return false
	}

	return referenced.Author.ID == b.botID
}

// buildPrompt constructs the prompt for the LLM, separating context from the current request.
func (b *Bot) buildPrompt(channel *discordgo.Channel, m *discordgo.MessageCreate) ([]genai.Message, error) {
	currentContent := sanitizeContent(m.Content, b.botID)
	if currentContent == "" {
		return nil, errEmptyMessageContent
	}

	return b.buildPromptFromCurrent(channel, m, currentContent)
}

func (b *Bot) buildPromptFromCurrent(channel *discordgo.Channel, m *discordgo.MessageCreate, currentContent string) ([]genai.Message, error) {
	if strings.TrimSpace(currentContent) == "" {
		return nil, errEmptyMessageContent
	}

	var sections []string
	if isThreadChannel(channel) {
		threadMsgs, parentMsgs := b.fetchThreadAndParentContext(channel, m)
		threadTranscript := trimToMaxRunes(b.formatTranscript(threadMsgs), threadHistoryContextMaxRunes)
		parentTranscript := trimToMaxRunes(b.formatTranscript(parentMsgs), parentHistoryContextMaxRunes)
		if strings.TrimSpace(threadTranscript) == "" {
			threadTranscript = "[none]"
		}
		if strings.TrimSpace(parentTranscript) == "" {
			parentTranscript = "[none]"
		}

		sections = append(sections, threadContextInstructions)
		if grounding := b.threadGroundingContext(channel, m.ChannelID); grounding != "" {
			sections = append(sections, fmt.Sprintf("THREAD WEB GROUNDING CONTEXT:\n%s", grounding))
		}
		sections = append(sections, fmt.Sprintf("THREAD HISTORY CONTEXT:\n%s", threadTranscript))
		sections = append(sections, fmt.Sprintf("PARENT CHANNEL CONTEXT:\n%s", parentTranscript))
		sections = append(sections, fmt.Sprintf("CURRENT REQUEST (PRIMARY TASK):\n%s", currentContent))
	} else {
		channelMsgs := b.fetchChannelHistory(m.ChannelID, b.outsideWindow, m.ID)
		channelTranscript := trimToMaxRunes(b.formatTranscript(channelMsgs), channelHistoryContextMaxRunes)
		if strings.TrimSpace(channelTranscript) == "" {
			channelTranscript = "[none]"
		}
		sections = append(sections, channelContextInstructions)
		sections = append(sections, fmt.Sprintf("CHANNEL HISTORY CONTEXT:\n%s", channelTranscript))
		sections = append(sections, fmt.Sprintf("CURRENT REQUEST (PRIMARY TASK):\n%s", currentContent))
	}

	fullContent := strings.Join(sections, "\n\n")

	return []genai.Message{
		{
			Role:    "user",
			Content: fullContent,
		},
	}, nil
}

// fetchContext retrieves relevant messages for context.
func (b *Bot) fetchContext(channel *discordgo.Channel, m *discordgo.MessageCreate) []*discordgo.Message {
	var allMessages []*discordgo.Message

	// Guard: Message fetcher must be configured.
	if b.fetchMessages == nil {
		return nil
	}

	// Scenario 1: Non-thread channel.
	if !isThreadChannel(channel) {
		msgs, err := b.fetchMessages(m.ChannelID, b.outsideWindow, m.ID)
		if err == nil {
			slices.Reverse(msgs)
			allMessages = append(allMessages, msgs...)
		}
		return allMessages
	}

	// Scenario 2: Thread channel.
	// Step A: Parent channel context.
	if channel.ParentID != "" {
		parentMsgs, err := b.fetchMessages(channel.ParentID, b.outsideWindow, "")
		if err == nil {
			slices.Reverse(parentMsgs)
			allMessages = append(allMessages, parentMsgs...)
		}
	}

	// Step B: Thread history context.
	threadMsgs, err := b.fetchMessages(m.ChannelID, b.threadWindow, m.ID)
	if err == nil {
		slices.Reverse(threadMsgs)
		allMessages = append(allMessages, threadMsgs...)
	}

	return allMessages
}

func (b *Bot) fetchChannelHistory(channelID string, limit int, beforeID string) []*discordgo.Message {
	if b.fetchMessages == nil {
		return nil
	}

	msgs, err := b.fetchMessages(channelID, limit, beforeID)
	if err != nil {
		return nil
	}
	slices.Reverse(msgs)
	return msgs
}

func (b *Bot) fetchThreadAndParentContext(channel *discordgo.Channel, m *discordgo.MessageCreate) ([]*discordgo.Message, []*discordgo.Message) {
	threadHistory := b.fetchChannelHistory(m.ChannelID, b.threadWindow, m.ID)

	var parentHistory []*discordgo.Message
	if channel != nil && channel.ParentID != "" {
		parentHistory = b.fetchChannelHistory(channel.ParentID, b.outsideWindow, "")
	}

	return threadHistory, parentHistory
}

// formatTranscript converts a list of Discord messages into a text transcript.
func (b *Bot) formatTranscript(messages []*discordgo.Message) string {
	var sb strings.Builder
	for _, msg := range messages {
		if msg == nil || msg.Author == nil {
			continue
		}

		content := sanitizeContent(msg.Content, b.botID)
		if content == "" {
			continue
		}

		name := displayName(msg.Author)
		if msg.Author.ID == b.botID {
			name = "Jarvis"
		}

		sb.WriteString(fmt.Sprintf("%s: %s\n", name, content))
	}
	return sb.String()
}

// sanitizeContent cleans up message content by removing mentions and sanitizing HTML.
func sanitizeContent(content, botID string) string {
	content = strings.ReplaceAll(content, fmt.Sprintf("<@%s>", botID), "")
	content = strings.ReplaceAll(content, fmt.Sprintf("<@!%s>", botID), "")

	channelMention := regexp.MustCompile(`<#[0-9]+>`)
	content = channelMention.ReplaceAllString(content, "this channel")

	content = html.UnescapeString(content)
	content = strings.TrimSpace(content)
	fields := strings.Fields(content)
	return strings.Join(fields, " ")
}

// mentionsBot checks if the bot is mentioned in the message.
func mentionsBot(mentions []*discordgo.User, botID string) bool {
	for _, mention := range mentions {
		if mention != nil && mention.ID == botID {
			return true
		}
	}
	return false
}

// isThreadChannel checks if the channel is a thread.
func isThreadChannel(ch *discordgo.Channel) bool {
	if ch == nil {
		return false
	}
	return ch.Type == discordgo.ChannelTypeGuildPublicThread ||
		ch.Type == discordgo.ChannelTypeGuildPrivateThread ||
		ch.Type == discordgo.ChannelTypeGuildNewsThread
}

// safeThreadName generates a safe name for a new thread.
func safeThreadName(username, globalName string) string {
	name := strings.TrimSpace(globalName)
	if name == "" {
		name = strings.TrimSpace(username)
	}
	if name == "" {
		return "AI Thread"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// displayName returns the display name of a user.
func displayName(user *discordgo.User) string {
	if user == nil {
		return ""
	}
	if user.GlobalName != "" {
		return user.GlobalName
	}
	return user.Username
}

func (b *Bot) sendMessageChunks(channelID, content string) error {
	if strings.TrimSpace(content) == "" {
		return errors.New("cannot send empty message content")
	}

	chunks := splitMessageForDiscord(content, discordMessageMaxLength)
	if len(chunks) == 0 {
		return errors.New("message chunking produced no output")
	}

	send := b.sendMessage
	if send == nil && b.session != nil {
		send = func(channelID, content string) (*discordgo.Message, error) {
			return b.session.ChannelMessageSend(channelID, content)
		}
	}
	if send == nil {
		return errors.New("discord send function is not configured")
	}

	for idx, chunk := range chunks {
		if _, err := send(channelID, chunk); err != nil {
			return fmt.Errorf("failed to send chunk %d of %d: %w", idx+1, len(chunks), err)
		}
	}

	return nil
}

func splitMessageForDiscord(content string, maxLength int) []string {
	if content == "" {
		return nil
	}
	if maxLength <= 0 {
		maxLength = discordMessageMaxLength
	}

	runes := []rune(content)
	if len(runes) <= maxLength {
		return []string{content}
	}

	chunks := make([]string, 0, len(runes)/maxLength+1)
	for len(runes) > maxLength {
		splitAt := maxLength
		for i := maxLength; i > maxLength/2; i-- {
			if runes[i-1] == '\n' {
				splitAt = i
				break
			}
		}
		chunks = append(chunks, string(runes[:splitAt]))
		runes = runes[splitAt:]
	}

	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}

	return chunks
}

func (b *Bot) threadAlreadyGrounded(channel *discordgo.Channel, m *discordgo.MessageCreate) bool {
	if !isThreadChannel(channel) {
		return false
	}

	threadID := channel.ID
	if threadID == "" {
		threadID = m.ChannelID
	}
	if _, ok := b.getThreadGrounding(threadID); ok {
		return true
	}

	if b.fetchMessages == nil {
		return false
	}

	remaining := threadGroundingHistoryScanLimit
	beforeID := m.ID
	for remaining > 0 {
		limit := 100
		if remaining < limit {
			limit = remaining
		}

		msgs, err := b.fetchMessages(m.ChannelID, limit, beforeID)
		if err != nil || len(msgs) == 0 {
			return false
		}

		for _, msg := range msgs {
			if msg == nil || msg.Author == nil {
				continue
			}
			if msg.Author.ID != b.botID {
				continue
			}
			if strings.Contains(html.UnescapeString(msg.Content), webGroundingAddendumMarker) {
				b.rememberThreadGrounding(threadID, msg.Content)
				return true
			}
		}

		remaining -= len(msgs)
		beforeID = msgs[len(msgs)-1].ID
		if beforeID == "" {
			break
		}
	}

	return false
}

func (b *Bot) threadGroundingContext(channel *discordgo.Channel, channelID string) string {
	if !isThreadChannel(channel) {
		return ""
	}

	threadID := channel.ID
	if threadID == "" {
		threadID = channelID
	}
	content, ok := b.getThreadGrounding(threadID)
	if !ok {
		return ""
	}
	return trimToMaxRunes(content, threadGroundingPromptMaxRunes)
}

func (b *Bot) rememberThreadGrounding(threadID, reply string) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return
	}

	content := extractThreadGroundingMemory(reply)
	if content == "" {
		return
	}

	content = trimToMaxRunes(content, threadGroundingMemoryMaxRunes)
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.grounding == nil {
		b.grounding = make(map[string]threadGroundingMemory)
	}
	b.grounding[threadID] = threadGroundingMemory{
		content:   content,
		updatedAt: now,
	}
}

func (b *Bot) getThreadGrounding(threadID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.grounding == nil {
		return "", false
	}
	state, ok := b.grounding[threadID]
	if !ok {
		return "", false
	}
	if strings.TrimSpace(state.content) == "" {
		return "", false
	}
	return state.content, true
}

func extractThreadGroundingMemory(reply string) string {
	cleaned := strings.TrimSpace(reply)
	if cleaned == "" {
		return ""
	}

	markerIndex := strings.Index(cleaned, webGroundingAddendumMarker)
	if markerIndex >= 0 {
		cleaned = cleaned[markerIndex:]
	}

	return strings.TrimSpace(cleaned)
}

func trimToMaxRunes(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return strings.TrimSpace(text)
	}

	trimmed := strings.TrimSpace(text)
	runes := []rune(trimmed)
	if len(runes) <= maxRunes {
		return trimmed
	}
	if maxRunes <= 24 {
		return string(runes[:maxRunes])
	}

	ellipsis := "\n...[truncated to fit context]"
	ellipsisRunes := []rune(ellipsis)
	available := maxRunes - len(ellipsisRunes)
	if available <= 0 {
		return string(runes[:maxRunes])
	}

	return string(runes[:available]) + ellipsis
}

func (b *Bot) canUseWebGrounding(channel *discordgo.Channel, m *discordgo.MessageCreate) (bool, string) {
	if !b.web.enabled {
		return false, "web grounding is currently disabled"
	}
	if !b.web.disableAllowlists {
		if !b.isWebChannelAllowed(channel) {
			return false, "this channel is not allowlisted for #web requests"
		}
		if !b.isWebRoleAllowed(m) {
			return false, "your role is not allowlisted for #web requests"
		}
	}
	if b.web.limiter != nil {
		allowed, reason := b.web.limiter.Allow(m.Author.ID)
		if !allowed {
			return false, reason
		}
	}
	return true, ""
}

func (b *Bot) isWebChannelAllowed(channel *discordgo.Channel) bool {
	if len(b.web.channelAllowlist) == 0 {
		return true
	}
	if channel == nil {
		return false
	}
	if _, exists := b.web.channelAllowlist[channel.ID]; exists {
		return true
	}
	if channel.ParentID == "" {
		return false
	}
	_, exists := b.web.channelAllowlist[channel.ParentID]
	return exists
}

func (b *Bot) isWebRoleAllowed(m *discordgo.MessageCreate) bool {
	if len(b.web.roleAllowlist) == 0 {
		return true
	}
	if m == nil || m.Message == nil || m.Message.Member == nil {
		return false
	}
	for _, roleID := range m.Message.Member.Roles {
		if _, exists := b.web.roleAllowlist[roleID]; exists {
			return true
		}
	}
	return false
}

func parseWebInvocation(content, indicator string) (bool, string) {
	trimmedIndicator := strings.TrimSpace(indicator)
	if trimmedIndicator == "" {
		trimmedIndicator = defaultWebIndicator
	}

	trimmed := strings.TrimLeft(content, " \t\r\n")
	if !strings.HasPrefix(trimmed, trimmedIndicator) {
		return false, content
	}
	if len(trimmed) > len(trimmedIndicator) {
		next := trimmed[len(trimmedIndicator)]
		if next != ' ' && next != '\t' && next != '\n' && next != '\r' {
			return false, content
		}
	}
	return true, strings.TrimSpace(trimmed[len(trimmedIndicator):])
}

func normalizeWebGroundingState(cfg WebGroundingConfig) webGroundingState {
	indicator := strings.TrimSpace(cfg.Indicator)
	if indicator == "" {
		indicator = defaultWebIndicator
	}

	messageTimeout := cfg.MessageTimeout
	if messageTimeout <= 0 {
		messageTimeout = defaultWebGroundingMessageTimeout
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultWebGroundingTimeout
	}

	attemptTimeout := cfg.AttemptTimeout
	if attemptTimeout <= 0 {
		attemptTimeout = defaultWebGroundingAttemptTimeout
	}

	addendumMaxOutputTokens := cfg.AddendumMaxOutputTokens
	if addendumMaxOutputTokens <= 0 {
		addendumMaxOutputTokens = defaultWebGroundingAddendumMaxTokens
	}

	apiVersion := strings.TrimSpace(cfg.APIVersion)
	if apiVersion == "" {
		apiVersion = defaultWebGroundingAPIVersion
	}

	maxResults := cfg.MaxResults
	if maxResults <= 0 {
		maxResults = defaultWebGroundingMaxResults
	}

	budgetRatio := cfg.BudgetRatio
	if budgetRatio <= 0 || budgetRatio >= 1 {
		budgetRatio = defaultWebGroundingBudgetRatio
	}

	userRPM := cfg.UserRPM
	if userRPM <= 0 {
		userRPM = defaultWebUserRPM
	}

	globalRPM := cfg.GlobalRPM
	if globalRPM <= 0 {
		globalRPM = defaultWebGlobalRPM
	}

	return webGroundingState{
		enabled:                 cfg.Enabled,
		indicator:               indicator,
		messageTimeout:          messageTimeout,
		timeout:                 timeout,
		attemptTimeout:          attemptTimeout,
		addendumMaxOutputTokens: addendumMaxOutputTokens,
		apiVersion:              apiVersion,
		maxResults:              maxResults,
		budgetRatio:             budgetRatio,
		requireCitations:        cfg.RequireCitations,
		disableAllowlists:       cfg.DisableAllowlists,
		channelAllowlist:        normalizeIDSet(cfg.ChannelAllowlist),
		roleAllowlist:           normalizeIDSet(cfg.RoleAllowlist),
		limiter:                 newWebRateLimiter(userRPM, globalRPM, time.Now),
	}
}

func normalizeIDSet(ids []string) map[string]struct{} {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		out[trimmed] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type rateWindow struct {
	start time.Time
	count int
}

type webRateLimiter struct {
	mu          sync.Mutex
	userLimit   int
	globalLimit int
	userWindows map[string]rateWindow
	global      rateWindow
	now         func() time.Time
}

func newWebRateLimiter(userLimit, globalLimit int, nowFn func() time.Time) *webRateLimiter {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &webRateLimiter{
		userLimit:   userLimit,
		globalLimit: globalLimit,
		userWindows: make(map[string]rateWindow),
		now:         nowFn,
	}
}

func (l *webRateLimiter) Allow(userID string) (bool, string) {
	if l == nil {
		return true, ""
	}

	currentWindow := l.now().UTC().Truncate(time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.global.start.Equal(currentWindow) {
		l.global = rateWindow{start: currentWindow}
	}
	if l.globalLimit > 0 && l.global.count >= l.globalLimit {
		return false, "the #web request limit for this minute has been reached"
	}

	userWindow := l.userWindows[userID]
	if !userWindow.start.Equal(currentWindow) {
		userWindow = rateWindow{start: currentWindow}
	}
	if l.userLimit > 0 && userWindow.count >= l.userLimit {
		return false, fmt.Sprintf("you have hit the #web limit of %d requests per minute", l.userLimit)
	}

	l.global.count++
	userWindow.count++
	l.userWindows[userID] = userWindow
	l.cleanup(currentWindow)

	return true, ""
}

func (l *webRateLimiter) cleanup(currentWindow time.Time) {
	for userID, window := range l.userWindows {
		if window.start.Before(currentWindow) {
			delete(l.userWindows, userID)
		}
	}
}

var botPrefixPattern = regexp.MustCompile(`(?i)^(?:\s*(?:jarvis|jarvischat)\s*[:\-]\s*)+`)

// stripBotPrefix removes common bot prefixes from the response.
func stripBotPrefix(text string) string {
	clean := strings.TrimSpace(text)
	clean = botPrefixPattern.ReplaceAllString(clean, "")
	return strings.TrimSpace(clean)
}
