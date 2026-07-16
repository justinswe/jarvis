package discord

import (
	"context"
	"net/http"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/internal/version"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/errors"
)

const (
	defaultHistoryRunes     = 4000
	discordMessageMaxLength = 2000
)

// Generator produces a response for a normalized conversation.
type Generator interface {
	Generate(context.Context, genai.GenerateRequest) (genai.GenerateResponse, error)
}

// History reads prior Discord messages for prompt construction.
type History interface {
	Messages(context.Context, string, string, int, string) ([]*discordgo.Message, error)
}

// Client contains the Discord REST operations used while processing a message.
type Client interface {
	Channel(context.Context, string) (*discordgo.Channel, error)
	Message(context.Context, string, string) (*discordgo.Message, error)
	Messages(context.Context, string, int, string) ([]*discordgo.Message, error)
	SendMessage(context.Context, string, string) (*discordgo.Message, error)
	StartThread(context.Context, string, string, string, int) (*discordgo.Channel, error)
	AddReaction(context.Context, string, string, string) error
	RemoveReaction(context.Context, string, string, string, string) error
	UserChannelPermissions(context.Context, string, string) (int64, error)
}

// Processor handles Discord messages without owning a Gateway connection.
type Processor struct {
	client      Client
	botID       string
	generator   Generator
	configs     config.Provider
	history     History
	manager     config.Manager
	rootUsers   map[string]struct{}
	version     string
	imageClient *http.Client
}

// ProcessorConfig contains the worker-owned dependencies for Discord request processing.
type ProcessorConfig struct {
	DiscordBotToken string
	Configs         config.Provider
	Generator       Generator
	History         History
	ConfigManager   config.Manager
	RootUserIDs     []string
	Version         string
	ImageHTTPClient *http.Client
}

// NewProcessor creates a request processor backed only by Discord REST APIs.
func NewProcessor(ctx context.Context, token string, configs config.Provider, generator Generator) (*Processor, error) {
	return NewProcessorWithConfig(ctx, ProcessorConfig{DiscordBotToken: token, Configs: configs, Generator: generator})
}

// NewProcessorWithConfig creates a processor with optional database history and configuration tools.
func NewProcessorWithConfig(ctx context.Context, cfg ProcessorConfig) (*Processor, error) {
	if cfg.DiscordBotToken == "" {
		return nil, errors.New("discord bot token is required")
	}
	if cfg.Configs == nil {
		return nil, errors.New("configuration provider is required")
	}
	if cfg.Generator == nil {
		return nil, errors.New("generator is required")
	}
	if strings.TrimSpace(cfg.Version) == "" {
		cfg.Version = version.Value
	}
	session, err := discordgo.New("Bot " + cfg.DiscordBotToken)
	if err != nil {
		return nil, errors.Wrap(err, "create Discord REST client")
	}
	user, err := session.User("@me", discordgo.WithContext(ctx))
	if err != nil {
		return nil, errors.Wrap(err, "obtain bot account")
	}
	rootUsers := make(map[string]struct{}, len(cfg.RootUserIDs))
	for _, userID := range cfg.RootUserIDs {
		if userID = strings.TrimSpace(userID); userID != "" {
			rootUsers[userID] = struct{}{}
		}
	}
	imageClient := cfg.ImageHTTPClient
	if imageClient == nil {
		imageClient = newImageHTTPClient()
	}
	return &Processor{
		client: restClient{session: session}, botID: user.ID, generator: cfg.Generator, configs: cfg.Configs,
		history: cfg.History, manager: cfg.ConfigManager, rootUsers: rootUsers, version: cfg.Version, imageClient: imageClient,
	}, nil
}

type restClient struct {
	session *discordgo.Session
}

func (c restClient) Channel(ctx context.Context, channelID string) (*discordgo.Channel, error) {
	return c.session.Channel(channelID, discordgo.WithContext(ctx))
}

func (c restClient) Message(ctx context.Context, channelID, messageID string) (*discordgo.Message, error) {
	return c.session.ChannelMessage(channelID, messageID, discordgo.WithContext(ctx))
}

func (c restClient) Messages(ctx context.Context, channelID string, limit int, beforeID string) ([]*discordgo.Message, error) {
	return c.session.ChannelMessages(channelID, limit, beforeID, "", "", discordgo.WithContext(ctx))
}

func (c restClient) SendMessage(ctx context.Context, channelID, content string) (*discordgo.Message, error) {
	return c.session.ChannelMessageSendComplex(channelID, suppressedMessage(content), discordgo.WithContext(ctx))
}

func (c restClient) StartThread(ctx context.Context, channelID, messageID, name string, archiveDuration int) (*discordgo.Channel, error) {
	return c.session.MessageThreadStart(channelID, messageID, name, archiveDuration, discordgo.WithContext(ctx))
}

func (c restClient) AddReaction(ctx context.Context, channelID, messageID, emoji string) error {
	return c.session.MessageReactionAdd(channelID, messageID, emoji, discordgo.WithContext(ctx))
}

func (c restClient) RemoveReaction(ctx context.Context, channelID, messageID, emoji, userID string) error {
	return c.session.MessageReactionRemove(channelID, messageID, emoji, userID, discordgo.WithContext(ctx))
}

func (c restClient) UserChannelPermissions(ctx context.Context, userID, channelID string) (int64, error) {
	return c.session.UserChannelPermissions(userID, channelID, discordgo.WithContext(ctx))
}

func suppressedMessage(content string) *discordgo.MessageSend {
	return &discordgo.MessageSend{
		Content: content,
		Flags:   discordgo.MessageFlagsSuppressEmbeds,
	}
}
