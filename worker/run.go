package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode"

	"github.com/justinswe/jarvis/mq"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/cache"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/consumer"
	"github.com/justinswe/jarvis/worker/pkg/discord"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/jarvis/worker/pkg/server"
	"github.com/justinswe/jarvis/worker/pkg/usage"
	"github.com/justinswe/jarvis/worker/pkg/valkeyconn"
	"github.com/justinswe/jarvis/worker/pkg/version"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/valkey-io/valkey-go"
	"go.uber.org/zap"
)

func runWorker(parent context.Context, cfg workerConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	webSearchClients, err := cfg.webSearchClients()
	if err != nil {
		return errors.Wrap(err, "initialize web search providers")
	}
	guildTiers, err := cfg.guildTiers()
	if err != nil {
		return errors.Wrap(err, "initialize guild subscription tiers")
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	staticProvider, err := config.NewStaticProvider(cfg.serverSettings())
	if err != nil {
		return errors.Wrap(err, "initialize configuration provider")
	}
	var provider config.Provider = staticProvider
	var history discord.History
	var manager config.Manager
	var recorder discord.Recorder
	var claimer discord.ReplyClaimer
	if cfg.storeEnabled() {
		persistent, storeErr := store.Open(ctx, cfg.storeConfig())
		if storeErr != nil {
			return errors.Wrap(storeErr, "open message store")
		}
		defer func() { _ = persistent.Close() }()
		provider, history, manager, recorder = persistent, persistent, persistent, persistent
		// A claim must lapse before the broker redelivers, or a worker that dies
		// mid-generation strands every retry of the message it was holding.
		persistent.SetReplyClaimTTL(cfg.mqAckWait)
		claimer = persistent
		app.L().Info("Message store initialized", zap.String("driver", cfg.storeDriver))
	}
	var limiter discord.Limiter
	var usageRecorder genai.UsageRecorder
	if cfg.valkeyEnabled {
		stack, stackErr := cfg.newValkeyStack(ctx, guildTiers)
		if stackErr != nil {
			return stackErr
		}
		defer stack.close()
		limiter, usageRecorder = stack.usage, stack.usage

		// Only worth a cache when there is a slow source of truth behind it: with the
		// store off, configuration comes from a static in-process provider.
		if cfg.storeEnabled() {
			cacheClient := cache.New(stack.client, cfg.valkeyKeyPrefix, cfg.valkeyTimeout)
			provider = config.NewCachedProvider(provider, cacheClient, cfg.valkeyConfigCacheTTL)
			manager = config.NewCachedManager(manager, cacheClient)
			app.L().Info("Valkey guild-configuration cache initialized",
				zap.Duration("ttl", cfg.valkeyConfigCacheTTL))
		}
	}
	generator, err := genai.New(ctx, genai.Config{
		ProjectID:            cfg.projectID,
		Location:             cfg.location,
		DefaultPrompt:        cfg.defaultPrompt,
		MaxOutputTokens:      cfg.maxOutputTokens,
		OpenRouterAPIKey:     cfg.openRouterAPIKey,
		GoogleAIAPIKey:       cfg.googleAIAPIKey,
		NVIDIAAPIKey:         cfg.nvidiaAPIKey,
		ModelProfiles:        cfg.modelProfiles,
		PrimaryModelProfile:  cfg.primaryModelProfile,
		FallbackModelProfile: cfg.fallbackModelProfile,
		WebSearchClients:     webSearchClients,
		MutableConfiguration: cfg.storeEnabled(),
		UsageRecorder:        usageRecorder,
	})
	if err != nil {
		return errors.Wrap(err, "initialize model orchestration")
	}
	defer generator.Close()
	webSearchProviders := make([]string, 0, len(generator.WebSearchProviders()))
	for _, provider := range generator.WebSearchProviders() {
		webSearchProviders = append(webSearchProviders, string(provider))
	}
	processor, err := discord.NewProcessorWithConfig(ctx, discord.ProcessorConfig{
		DiscordBotToken:    cfg.discordBotToken,
		Configs:            provider,
		Generator:          generator,
		History:            history,
		ConfigManager:      manager,
		ModelRegistry:      generator.Registry(),
		WebSearchProviders: webSearchProviders,
		RootUserIDs:        cfg.rootUserIDs,
		Version:            version.Value,
		Limiter:            limiter,
		Recorder:           recorder,
		ReplyClaimer:       claimer,
	})
	if err != nil {
		return errors.Wrap(err, "initialize Discord processor")
	}

	queue := cfg.queueConfig()
	subscription, err := consumer.Start(ctx, queue, processor)
	if err != nil {
		return errors.Wrap(err, "start message consumer")
	}
	defer subscription.Stop()

	address := cfg.address()
	app.L().Info("Starting worker",
		zap.String("address", address),
		zap.String("mq_driver", cfg.mqDriver),
		zap.String("topic", queue.Topic),
	)
	return server.Serve(ctx, address)
}

// valkeyStack is the Valkey-backed layer: one connection shared by usage metering and,
// when there is a repository to cache, the guild-configuration cache.
type valkeyStack struct {
	client valkey.Client
	usage  *usage.Client
}

// close releases the usage client before the connection it borrows, so the final flush
// still has somewhere to write.
func (s valkeyStack) close() {
	_ = s.usage.Close()
	s.client.Close()
}

// newValkeyStack dials Valkey and starts usage metering over that one connection.
func (cfg workerConfig) newValkeyStack(ctx context.Context, guildTiers map[string]usage.Limits) (valkeyStack, error) {
	client, err := valkeyconn.Dial(ctx, valkeyconn.Config{
		Addresses: cfg.valkeyAddresses,
		Username:  cfg.valkeyUsername,
		Password:  cfg.valkeyPassword,
		SelectDB:  cfg.valkeySelectDB,
		TLS:       cfg.valkeyTLSEnabled,
		// Not valkeyTimeout: that one bounds the inline admission check, and spending
		// 50ms on a TLS handshake plus authentication would turn a slow-but-healthy
		// server into a failed startup.
		Timeout: cfg.valkeyDialTimeout,
	})
	if err != nil {
		return valkeyStack{}, errors.Wrap(err, "connect to Valkey")
	}
	usageClient, err := usage.NewWithClient(client, usage.Config{
		KeyPrefix:     cfg.valkeyKeyPrefix,
		Timeout:       cfg.valkeyTimeout,
		FlushInterval: cfg.valkeyFlushInterval,
		RequestTTL:    cfg.valkeyRequestRetention,
		TokenTTL:      cfg.valkeyTokenRetention,
		Tiers:         guildTiers,
		DefaultTier:   cfg.defaultGuildTier,
		WarnThreshold: cfg.valkeyWarnThreshold,
	})
	if err != nil {
		client.Close()
		return valkeyStack{}, errors.Wrap(err, "initialize Valkey usage client")
	}
	app.L().Info("Valkey usage metering initialized",
		zap.Strings("addresses", cfg.valkeyAddresses),
		zap.Strings("guild_tiers", guildTierNames(guildTiers)),
		zap.String("default_guild_tier", cfg.defaultGuildTier),
	)
	return valkeyStack{client: client, usage: usageClient}, nil
}

// validate reports whether the worker can start, before anything dials or authenticates.
func (cfg workerConfig) validate() error {
	if cfg.port == "" {
		return errors.New("worker port is required")
	}
	if cfg.discordBotToken == "" {
		return errors.New("discord bot token is required")
	}
	switch store.Driver(cfg.storeDriver) {
	case store.DriverNone:
	case store.DriverPostgres:
		if strings.TrimSpace(cfg.postgresDSN) == "" {
			return errors.New("PostgreSQL DSN is required when the store driver is postgres")
		}
	case store.DriverSQLite:
		if strings.TrimSpace(cfg.sqlitePath) == "" {
			return errors.New("SQLite path is required when the store driver is sqlite")
		}
	default:
		return errors.Errorf("unsupported store driver %q", cfg.storeDriver)
	}
	if cfg.storeEnabled() && cfg.storeSweepInterval <= 0 {
		return errors.New("store sweep interval must be positive")
	}
	if !mq.Driver(cfg.mqDriver).Valid() {
		return errors.Errorf("unsupported message queue driver %q", cfg.mqDriver)
	}
	// A message may not be released before its first keepalive is even due, or every
	// message would be redelivered once while the first delivery was still working.
	if cfg.mqMaxProcessingTime < cfg.mqAckWait {
		return errors.New("max processing time must be at least the acknowledgement wait")
	}
	for _, userID := range cfg.rootUserIDs {
		if !validRootUserID(userID) {
			return errors.Errorf("root user ID %q must be a 17-20 digit Discord user ID", userID)
		}
	}
	if !cfg.valkeyEnabled {
		return nil
	}
	if len(cfg.valkeyAddresses) == 0 {
		return errors.New("at least one Valkey address is required when Valkey is enabled")
	}
	if cfg.valkeyDialTimeout <= 0 {
		return errors.New("Valkey dial timeout must be positive when Valkey is enabled")
	}
	// A non-positive TTL would not mean "no caching": cache.Set omits the expiry
	// entirely, so every guild configuration would be cached forever and a missed
	// invalidation could never age out.
	if cfg.valkeyConfigCacheTTL <= 0 {
		return errors.New("Valkey configuration cache TTL must be positive when Valkey is enabled")
	}
	return nil
}

func (cfg workerConfig) address() string {
	return net.JoinHostPort(cfg.host, cfg.port)
}

func (cfg workerConfig) serverSettings() config.ServerSettings {
	primary := strings.TrimSpace(cfg.primaryModelProfile)
	if primary == "" && len(cfg.modelProfiles) == 0 {
		primary = "default"
	}
	return config.ServerSettings{
		Prompt:               cfg.defaultPrompt,
		ThreadMessages:       cfg.threadMessages,
		ParentMessages:       cfg.parentMessages,
		ChannelMessages:      cfg.channelMessages,
		HistoryRunes:         cfg.historyRunes,
		MaxOutputTokens:      cfg.maxOutputTokens,
		MessageTimeout:       cfg.messageTimeout,
		MessageRetentionDays: cfg.messageRetentionDays,
		WebSearchEnabled:     true,
		ChannelSearchEnabled: true,
		ReasoningEffort:      llm.ReasoningEffort(strings.TrimSpace(cfg.reasoningEffort)),
		PrimaryModelProfile:  primary,
		FallbackModelProfile: strings.TrimSpace(cfg.fallbackModelProfile),
	}
}

func validRootUserID(userID string) bool {
	userID = strings.TrimSpace(userID)
	if len(userID) < 17 || len(userID) > 20 {
		return false
	}
	return !strings.ContainsFunc(userID, func(r rune) bool { return !unicode.IsDigit(r) })
}
