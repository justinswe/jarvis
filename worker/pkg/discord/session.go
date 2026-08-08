package discord

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	discordmcp "github.com/justinswe/discord-mcp"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/jarvis/worker/pkg/mcpx"
	"github.com/justinswe/jarvis/worker/pkg/version"
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

// Recorder persists one Discord message for history and search, expiring it after the
// guild's retention. Only bot-involved conversation is recorded — the targeted messages
// the processor answers and the replies it posts — never the surrounding channel traffic.
type Recorder interface {
	Record(ctx context.Context, message *discordgo.Message, retentionDays int) error
}

// Admission is the outcome of one guild rate-limit check.
type Admission struct {
	Allowed    bool
	NearLimit  bool
	RetryAfter time.Duration
}

// Limiter reports whether one guild may start a new generation.
type Limiter interface {
	Allow(ctx context.Context, guildID, tier string) (Admission, error)
}

// ReplyClaimer reserves the right to answer one Discord message.
//
// Delivery is at-least-once, and an active-active deployment keeps a Gateway connection at
// every site permanently, so the same message reaches more than one worker as a matter of
// course rather than only during a handover. Only the caller that wins the claim answers.
//
// The two calls divide one guarantee. ClaimReply is short-lived so a worker that dies
// mid-generation lets its redelivery through; HoldReply extends the winner's claim once
// the reply exists, past any copy that could still arrive.
type ReplyClaimer interface {
	ClaimReply(ctx context.Context, channelID, messageID string) (bool, error)
	HoldReply(ctx context.Context, channelID, messageID string) error
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
	client             Client
	botID              string
	generator          Generator
	configs            config.Provider
	history            History
	manager            config.Manager
	models             *llm.Registry
	webSearchProviders []string
	rootUsers          map[string]struct{}
	version            string
	imageClient        *http.Client
	threadQueue        threadRequestQueue
	limiter            Limiter
	// recorder is nil when no store is configured; messages then simply are not kept.
	recorder Recorder
	// replies is nil when no shared store is configured. Nothing then deduplicates
	// replies, which is why more than one Gateway connection requires a store driver.
	replies ReplyClaimer
	// mcp is nil when MCP is not wired; tools then stay native, exactly as before.
	mcp               *mcpx.Connector
	defaultMCPServers []config.MCPServer
	// dm shares the processor's discordgo session (one REST rate-limit bucket) and is
	// pinned per message to the requesting guild via its Guild view.
	dm *discordmcp.Client
}

// ProcessorConfig contains the worker-owned dependencies for Discord request processing.
type ProcessorConfig struct {
	DiscordBotToken    string
	Configs            config.Provider
	Generator          Generator
	History            History
	ConfigManager      config.Manager
	ModelRegistry      *llm.Registry
	WebSearchProviders []string
	RootUserIDs        []string
	Version            string
	ImageHTTPClient    *http.Client
	Limiter            Limiter
	Recorder           Recorder
	ReplyClaimer       ReplyClaimer
	// MCP enables the MCP tool path: built-ins served in-process plus the guild's
	// remote servers. Nil keeps native tools only.
	MCP *mcpx.Connector
	// DefaultMCPServers are deployment-wide remote servers offered to every guild; a
	// guild's own attachment overrides a default of the same name.
	DefaultMCPServers []config.MCPServer
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
	processor := &Processor{
		client: restClient{session: session}, botID: user.ID, generator: cfg.Generator, configs: cfg.Configs,
		history: cfg.History, manager: cfg.ConfigManager, models: cfg.ModelRegistry, webSearchProviders: append([]string(nil), cfg.WebSearchProviders...),
		rootUsers: rootUsers, version: cfg.Version, imageClient: imageClient,
		limiter: cfg.Limiter, recorder: cfg.Recorder, replies: cfg.ReplyClaimer,
		mcp: cfg.MCP, defaultMCPServers: append([]config.MCPServer(nil), cfg.DefaultMCPServers...),
	}
	if cfg.MCP != nil {
		processor.dm, err = discordmcp.New(discordmcp.Options{Session: session})
		if err != nil {
			return nil, errors.Wrap(err, "create Discord MCP client")
		}
	}
	return processor, nil
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
