package discord

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

const (
	defaultThreadMessages   = 12
	defaultParentMessages   = 4
	defaultChannelMessages  = 8
	defaultHistoryRunes     = 6000
	defaultMessageTimeout   = 45 * time.Second
	discordMessageMaxLength = 2000
)

type Generator interface {
	Generate(context.Context, genai.GenerateRequest) (genai.GenerateResponse, error)
}

type Config struct {
	Token           string
	ThreadMessages  int
	ParentMessages  int
	ChannelMessages int
	HistoryRunes    int
	MessageTimeout  time.Duration
}

type Bot struct {
	session        *discordgo.Session
	botID          string
	generator      Generator
	threadLimit    int
	parentLimit    int
	channelLimit   int
	historyRunes   int
	messageTimeout time.Duration
	ready          atomic.Bool

	fetchMessages  func(string, int, string) ([]*discordgo.Message, error)
	sendMessage    func(string, string) (*discordgo.Message, error)
	startThread    func(string, string, string, int) (*discordgo.Channel, error)
	addReaction    func(string, string, string) error
	removeReaction func(string, string, string, string) error
}

func NewBot(cfg Config, generator Generator) (*Bot, error) {
	if cfg.Token == "" {
		return nil, errors.New("discord bot token is required")
	}
	if generator == nil {
		return nil, errors.New("generator is required")
	}
	if cfg.ThreadMessages <= 0 {
		cfg.ThreadMessages = defaultThreadMessages
	}
	if cfg.ParentMessages <= 0 {
		cfg.ParentMessages = defaultParentMessages
	}
	if cfg.ChannelMessages <= 0 {
		cfg.ChannelMessages = defaultChannelMessages
	}
	if cfg.HistoryRunes <= 0 {
		cfg.HistoryRunes = defaultHistoryRunes
	}
	if cfg.MessageTimeout <= 0 {
		cfg.MessageTimeout = defaultMessageTimeout
	}

	s, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, errors.Wrap(err, "create Discord session")
	}
	u, err := s.User("@me")
	if err != nil {
		return nil, errors.Wrap(err, "obtain bot account")
	}
	b := &Bot{session: s, botID: u.ID, generator: generator, threadLimit: cfg.ThreadMessages,
		parentLimit: cfg.ParentMessages, channelLimit: cfg.ChannelMessages, historyRunes: cfg.HistoryRunes,
		messageTimeout: cfg.MessageTimeout}
	b.fetchMessages = func(id string, limit int, before string) ([]*discordgo.Message, error) {
		return s.ChannelMessages(id, limit, before, "", "")
	}
	b.sendMessage = func(channelID, content string) (*discordgo.Message, error) {
		return s.ChannelMessageSendComplex(channelID, suppressedMessage(content))
	}
	b.startThread = func(channelID, messageID, name string, archiveDuration int) (*discordgo.Channel, error) {
		return s.MessageThreadStart(channelID, messageID, name, archiveDuration)
	}
	b.addReaction = func(channelID, messageID, emoji string) error {
		return s.MessageReactionAdd(channelID, messageID, emoji)
	}
	b.removeReaction = func(channelID, messageID, emoji, userID string) error {
		return s.MessageReactionRemove(channelID, messageID, emoji, userID)
	}
	return b, nil
}

func suppressedMessage(content string) *discordgo.MessageSend {
	return &discordgo.MessageSend{
		Content: content,
		Flags:   discordgo.MessageFlagsSuppressEmbeds,
	}
}

func (b *Bot) Ready() bool { return b.ready.Load() }

func (b *Bot) Start(ctx context.Context) error {
	b.session.AddHandler(b.messageCreate)
	b.session.AddHandler(func(_ *discordgo.Session, _ *discordgo.Ready) { b.ready.Store(true) })
	b.session.AddHandler(func(_ *discordgo.Session, _ *discordgo.Resumed) { b.ready.Store(true) })
	b.session.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { b.ready.Store(false) })
	b.session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent
	if err := b.session.Open(); err != nil {
		return errors.Wrap(err, "open Discord connection")
	}
	app.L().Info("Discord bot connected", zap.String("bot_id", b.botID))
	<-ctx.Done()
	b.ready.Store(false)
	if err := b.session.Close(); err != nil {
		return errors.Wrap(err, "close Discord connection")
	}
	return nil
}
