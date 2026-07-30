package main

import (
	"slices"
	"strconv"
	"strings"
	"time"

	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/jarvis/worker/pkg/usage"
	"github.com/justinswe/jarvis/worker/pkg/websearch"
	"github.com/justinswe/std/errors"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

type workerConfig struct {
	host, port, projectID, location, defaultPrompt, discordBotToken string
	natsURL, natsStream, natsSubject, natsDurable                   string
	natsAckWait, natsDrainTimeout                                   time.Duration
	natsMaxDeliver, natsMaxAckPending                               int
	openRouterAPIKey, googleAIAPIKey, nvidiaAPIKey                  string
	primaryModelProfile, fallbackModelProfile                       string
	serperAPIKey, firecrawlAPIKey, tavilyAPIKey                     string
	modelProfiles, webSearchProviders                               []string
	reasoningEffort                                                 string
	dynamodbTable, awsRoleARN, awsWebIdentityAudience               string
	rootUserIDs                                                     []string
	dynamodbEnabled                                                 bool
	valkeyEnabled, valkeyTLSEnabled                                 bool
	valkeyAddresses, guildTierLimits                                []string
	valkeyUsername, valkeyPassword, valkeyKeyPrefix                 string
	defaultGuildTier                                                string
	valkeySelectDB, valkeyWarnThreshold                             int
	valkeyTimeout, valkeyDialTimeout, valkeyFlushInterval           time.Duration
	valkeyRequestRetention, valkeyTokenRetention                    time.Duration
	valkeyConfigCacheTTL                                            time.Duration
	threadMessages, parentMessages, channelMessages, historyRunes   int
	maxOutputTokens                                                 int
	messageRetentionDays                                            int
	messageTimeout                                                  time.Duration
}

// newWorkerConfig is the worker configuration before flags and environment.
func newWorkerConfig() workerConfig {
	return workerConfig{
		port:                 "8080",
		location:             "global",
		defaultPrompt:        genai.DefaultPrompt,
		threadMessages:       15,
		parentMessages:       10,
		channelMessages:      4,
		historyRunes:         4000,
		maxOutputTokens:      genai.DefaultMaxOutputTokens,
		reasoningEffort:      string(llm.ReasoningLow),
		messageRetentionDays: config.DefaultMessageRetentionDays,
		messageTimeout:       time.Minute,
		dynamodbTable:        "jarvis",

		natsURL:           nats.DefaultURL,
		natsStream:        discordv1.StreamName,
		natsSubject:       discordv1.Subject,
		natsDurable:       discordv1.DurableName,
		natsAckWait:       30 * time.Second,
		natsDrainTimeout:  20 * time.Second,
		natsMaxDeliver:    3,
		natsMaxAckPending: 4,

		valkeyKeyPrefix:        "jarvis",
		valkeyTimeout:          50 * time.Millisecond,
		valkeyDialTimeout:      5 * time.Second,
		valkeyFlushInterval:    time.Second,
		valkeyRequestRetention: 48 * time.Hour,
		valkeyTokenRetention:   720 * time.Hour,
		valkeyWarnThreshold:    95,
		valkeyConfigCacheTTL:   5 * time.Minute,
		defaultGuildTier:       "free",
	}
}

func newRootCommand() *cobra.Command {
	cfg := newWorkerConfig()
	command := &cobra.Command{
		Use:   "worker",
		Short: "Starts the stateless Jarvis message worker",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runWorker(cmd.Context(), cfg) },
	}
	flags := command.Flags()
	// Empty binds every interface, which is what a standalone deployment needs: a
	// container platform probes the pod's own address, not its loopback. The combined
	// image narrows this to 127.0.0.1 by injecting HOST, because there the only client
	// is the supervisor in the same container.
	flags.StringVar(&cfg.host, "host", cfg.host, "Health server bind host; empty binds every interface")
	flags.StringVar(&cfg.port, "port", cfg.port, "Health server port")
	flags.StringVar(&cfg.natsURL, "nats-url", cfg.natsURL, "NATS server URL")
	flags.StringVar(&cfg.natsStream, "nats-stream", cfg.natsStream, "JetStream stream holding normalized Discord events")
	flags.StringVar(&cfg.natsSubject, "nats-subject", cfg.natsSubject, "Subject normalized Discord events are published to")
	flags.StringVar(&cfg.natsDurable, "nats-durable", cfg.natsDurable, "Durable consumer name shared by every worker replica")
	flags.DurationVar(&cfg.natsAckWait, "nats-ack-wait", cfg.natsAckWait, "How long a held message may go without progress before redelivery")
	flags.DurationVar(&cfg.natsDrainTimeout, "nats-drain-timeout", cfg.natsDrainTimeout, "How long shutdown lets in-flight messages finish before cancelling them; must fit the platform's grace period")
	flags.IntVar(&cfg.natsMaxDeliver, "nats-max-deliver", cfg.natsMaxDeliver, "Maximum delivery attempts per message")
	flags.IntVar(&cfg.natsMaxAckPending, "nats-max-ack-pending", cfg.natsMaxAckPending, "Maximum messages held un-acknowledged at once")
	flags.StringVar(&cfg.projectID, "project-id", cfg.projectID, "GCP project ID")
	flags.StringVar(&cfg.location, "location", cfg.location, "Vertex AI location")
	flags.StringVar(&cfg.defaultPrompt, "default-prompt", cfg.defaultPrompt, "Root-controlled assistant customization prompt; may define the assistant name and personality")
	flags.StringVar(&cfg.openRouterAPIKey, "openrouter-api-key", cfg.openRouterAPIKey, "OpenRouter API key (required when an OpenRouter profile is configured)")
	flags.StringVar(&cfg.googleAIAPIKey, "google-ai-api-key", cfg.googleAIAPIKey, "Google AI Studio API key (required when a Google AI profile is configured)")
	flags.StringVar(&cfg.nvidiaAPIKey, "nvidia-api-key", cfg.nvidiaAPIKey, "NVIDIA hosted NIM API key")
	flags.StringSliceVar(&cfg.modelProfiles, "model-profile", cfg.modelProfiles, "Named model profiles: name=provider:model-id (comma-capable and repeatable)")
	flags.StringVar(&cfg.primaryModelProfile, "primary-model-profile", cfg.primaryModelProfile, "Default tool-capable primary model profile")
	flags.StringVar(&cfg.fallbackModelProfile, "fallback-model-profile", cfg.fallbackModelProfile, "Default tools-disabled presentation fallback profile; empty disables fallback")
	flags.StringSliceVar(&cfg.webSearchProviders, "web-search-providers", cfg.webSearchProviders, "Ordered web search providers (zero to two): serper, firecrawl, tavily")
	flags.StringVar(&cfg.serperAPIKey, "serper-api-key", cfg.serperAPIKey, "Serper API key")
	flags.StringVar(&cfg.firecrawlAPIKey, "firecrawl-api-key", cfg.firecrawlAPIKey, "Firecrawl API key")
	flags.StringVar(&cfg.tavilyAPIKey, "tavily-api-key", cfg.tavilyAPIKey, "Tavily API key")
	flags.IntVar(&cfg.threadMessages, "thread-context-window", cfg.threadMessages, "Prior thread messages")
	flags.IntVar(&cfg.parentMessages, "parent-context-window", cfg.parentMessages, "Prior parent-channel messages")
	flags.IntVar(&cfg.channelMessages, "channel-context-window", cfg.channelMessages, "Prior ordinary channel messages")
	flags.IntVar(&cfg.historyRunes, "history-runes", cfg.historyRunes, "Combined context history rune budget")
	flags.IntVar(&cfg.maxOutputTokens, "max-output-tokens", cfg.maxOutputTokens, "Maximum total generated tokens, including thinking and visible text (maximum 8192)")
	flags.StringVar(&cfg.reasoningEffort, "reasoning-effort", cfg.reasoningEffort, "Default model thinking level: low, medium, or high")
	flags.StringVar(&cfg.discordBotToken, "discord-bot-token", cfg.discordBotToken, "Discord bot token")
	flags.DurationVar(&cfg.messageTimeout, "message-timeout", cfg.messageTimeout, "Overall message processing timeout")
	flags.IntVar(&cfg.messageRetentionDays, "message-retention-days", cfg.messageRetentionDays, "Default message retention in days")
	flags.BoolVar(&cfg.dynamodbEnabled, "dynamodb-enabled", cfg.dynamodbEnabled, "Enable DynamoDB message history and server configuration")
	flags.StringVar(&cfg.dynamodbTable, "dynamodb-table", cfg.dynamodbTable, "DynamoDB table name")
	flags.StringVar(&cfg.awsRoleARN, "aws-role-arn", cfg.awsRoleARN, "AWS IAM role assumed through Google workload identity")
	flags.StringVar(&cfg.awsWebIdentityAudience, "aws-web-identity-audience", cfg.awsWebIdentityAudience, "Audience for the Google identity token exchanged with AWS")
	flags.StringSliceVar(&cfg.rootUserIDs, "root-user-ids", cfg.rootUserIDs, "Discord user IDs with cross-server root access")
	flags.BoolVar(&cfg.valkeyEnabled, "valkey-enabled", cfg.valkeyEnabled, "Enable Valkey per-guild usage metering and subscription limits")
	flags.StringSliceVar(&cfg.valkeyAddresses, "valkey-address", cfg.valkeyAddresses, "Valkey host:port addresses (comma-capable and repeatable)")
	flags.StringVar(&cfg.valkeyUsername, "valkey-username", cfg.valkeyUsername, "Valkey ACL username")
	flags.StringVar(&cfg.valkeyPassword, "valkey-password", cfg.valkeyPassword, "Valkey ACL password")
	flags.BoolVar(&cfg.valkeyTLSEnabled, "valkey-tls-enabled", cfg.valkeyTLSEnabled, "Connect to Valkey over TLS")
	flags.IntVar(&cfg.valkeySelectDB, "valkey-select-db", cfg.valkeySelectDB, "Valkey database index for non-cluster deployments")
	flags.StringVar(&cfg.valkeyKeyPrefix, "valkey-key-prefix", cfg.valkeyKeyPrefix, "Key namespace prefix for recorded usage")
	flags.DurationVar(&cfg.valkeyTimeout, "valkey-timeout", cfg.valkeyTimeout, "Deadline for the inline admission check; exceeding it allows the request")
	flags.DurationVar(&cfg.valkeyDialTimeout, "valkey-dial-timeout", cfg.valkeyDialTimeout, "Deadline for the startup connection and its verifying PING; must cover a TLS handshake and authentication, so it is far longer than --valkey-timeout")
	flags.DurationVar(&cfg.valkeyFlushInterval, "valkey-flush-interval", cfg.valkeyFlushInterval, "How often accumulated token usage is written to Valkey")
	flags.DurationVar(&cfg.valkeyRequestRetention, "valkey-request-retention", cfg.valkeyRequestRetention, "Retention for per-second request counters")
	flags.DurationVar(&cfg.valkeyTokenRetention, "valkey-token-retention", cfg.valkeyTokenRetention, "Retention for per-model token counters")
	flags.IntVar(&cfg.valkeyWarnThreshold, "valkey-warn-threshold", cfg.valkeyWarnThreshold, "Limit utilization percentage that appends a near-limit notice to replies")
	flags.DurationVar(&cfg.valkeyConfigCacheTTL, "valkey-config-cache-ttl", cfg.valkeyConfigCacheTTL, "Guild configuration cache time-to-live; bounds staleness if a write invalidation is missed")
	flags.StringSliceVar(&cfg.guildTierLimits, "guild-tier", cfg.guildTierLimits, "Subscription tier limits: name=requests-per-second:burst:tokens-per-hour (comma-capable and repeatable)")
	flags.StringVar(&cfg.defaultGuildTier, "default-guild-tier", cfg.defaultGuildTier, "Tier applied to servers with no assigned or a no longer defined tier")
	return command
}

// guildTiers parses and validates the deployment subscription tier table.
func (cfg workerConfig) guildTiers() (map[string]usage.Limits, error) {
	entries := cfg.guildTierLimits
	if len(entries) == 1 && strings.TrimSpace(entries[0]) == "" {
		entries = nil
	}
	if len(entries) == 0 {
		return nil, nil
	}
	tiers := make(map[string]usage.Limits, len(entries))
	for _, entry := range entries {
		name, limits, err := parseGuildTier(entry)
		if err != nil {
			return nil, err
		}
		if _, duplicate := tiers[name]; duplicate {
			return nil, errors.Errorf("duplicate guild tier %q", name)
		}
		tiers[name] = limits
	}
	if _, defined := tiers[strings.TrimSpace(cfg.defaultGuildTier)]; !defined {
		return nil, errors.Errorf("default-guild-tier %q is not declared by any guild-tier entry", cfg.defaultGuildTier)
	}
	return tiers, nil
}

// parseGuildTier reads one name=requests-per-second:burst:tokens-per-hour declaration.
func parseGuildTier(entry string) (string, usage.Limits, error) {
	name, spec, found := strings.Cut(strings.TrimSpace(entry), "=")
	if !found {
		return "", usage.Limits{}, errors.Errorf("guild tier %q must use name=requests-per-second:burst:tokens-per-hour", entry)
	}
	name = strings.TrimSpace(name)
	if err := config.ValidateTier(name); err != nil || name == "" {
		return "", usage.Limits{}, errors.Errorf("guild tier name %q must be lowercase alphanumeric with hyphens", name)
	}
	fields := strings.Split(strings.TrimSpace(spec), ":")
	if len(fields) != 3 {
		return "", usage.Limits{}, errors.Errorf("guild tier %q must declare requests-per-second:burst:tokens-per-hour", name)
	}
	values := make([]int, len(fields))
	for index, field := range fields {
		value, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || value < 0 {
			return "", usage.Limits{}, errors.Errorf("guild tier %q must declare non-negative integers, got %q", name, field)
		}
		values[index] = value
	}
	limits := usage.Limits{RequestsPerSecond: values[0], Burst: values[1], TokensPerHour: values[2]}
	if limits.RequestsPerSecond > 0 && limits.Burst < 1 {
		return "", usage.Limits{}, errors.Errorf("guild tier %q must allow a burst of at least one request", name)
	}
	return name, limits, nil
}

// guildTierNames returns the declared tier names in a stable order for tool schemas.
func guildTierNames(tiers map[string]usage.Limits) []string {
	names := make([]string, 0, len(tiers))
	for name := range tiers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (cfg workerConfig) webSearchClients() ([]*websearch.Client, error) {
	providerNames := cfg.webSearchProviders
	if len(providerNames) == 1 && strings.TrimSpace(providerNames[0]) == "" {
		providerNames = nil
	}
	if len(providerNames) > 2 {
		return nil, errors.New("web-search-providers accepts at most two providers")
	}
	seen := make(map[websearch.Provider]struct{}, len(providerNames))
	clients := make([]*websearch.Client, 0, len(providerNames))
	for index, name := range providerNames {
		provider := websearch.Provider(strings.TrimSpace(name))
		if !websearch.SupportedProvider(provider) {
			return nil, errors.Errorf("unsupported web search provider %q", provider)
		}
		if _, duplicate := seen[provider]; duplicate {
			return nil, errors.Errorf("duplicate web search provider %q", provider)
		}
		if provider == websearch.ProviderSerper && index != 0 {
			return nil, errors.New("serper must be the first web search provider")
		}
		seen[provider] = struct{}{}
		apiKey := cfg.webSearchAPIKey(provider)
		if strings.TrimSpace(apiKey) == "" {
			return nil, errors.Errorf("%s-api-key is required when %s is selected", provider, provider)
		}
		client, err := websearch.New(websearch.Config{Provider: provider, APIKey: apiKey})
		if err != nil {
			return nil, errors.Wrap(err, "initialize web search provider")
		}
		clients = append(clients, client)
	}
	return clients, nil
}

func (cfg workerConfig) webSearchAPIKey(provider websearch.Provider) string {
	switch provider {
	case websearch.ProviderSerper:
		return cfg.serperAPIKey
	case websearch.ProviderFirecrawl:
		return cfg.firecrawlAPIKey
	case websearch.ProviderTavily:
		return cfg.tavilyAPIKey
	default:
		return ""
	}
}
