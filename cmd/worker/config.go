package main

import (
	"strings"
	"time"

	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/jarvis/pkg/websearch"
	"github.com/justinswe/std/errors"
	"github.com/spf13/cobra"
)

type workerConfig struct {
	host, port, projectID, location, defaultPrompt, discordBotToken string
	openRouterAPIKey, googleAIAPIKey, nvidiaAPIKey                  string
	primaryModelProfile, fallbackModelProfile                       string
	serperAPIKey, firecrawlAPIKey, tavilyAPIKey                     string
	modelProfiles, webSearchProviders                               []string
	dynamodbTable, awsRoleARN, awsWebIdentityAudience               string
	rootUserIDs                                                     []string
	dynamodbEnabled                                                 bool
	threadMessages, parentMessages, channelMessages, historyRunes   int
	maxOutputTokens                                                 int
	messageRetentionDays                                            int
	messageTimeout                                                  time.Duration
}

func newRootCommand() *cobra.Command {
	cfg := workerConfig{
		port:                 "8080",
		location:             "global",
		defaultPrompt:        genai.DefaultPrompt,
		threadMessages:       15,
		parentMessages:       10,
		channelMessages:      4,
		historyRunes:         4000,
		maxOutputTokens:      genai.DefaultMaxOutputTokens,
		messageRetentionDays: config.DefaultMessageRetentionDays,
		messageTimeout:       time.Minute,
		dynamodbTable:        "jarvis",
	}
	command := &cobra.Command{
		Use:   "worker",
		Short: "Starts the stateless Jarvis HTTP worker",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runWorker(cmd.Context(), cfg) },
	}
	flags := command.Flags()
	flags.StringVar(&cfg.host, "host", cfg.host, "HTTP worker bind host")
	flags.StringVar(&cfg.port, "port", cfg.port, "HTTP worker port")
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
	flags.StringVar(&cfg.discordBotToken, "discord-bot-token", cfg.discordBotToken, "Discord bot token")
	flags.DurationVar(&cfg.messageTimeout, "message-timeout", cfg.messageTimeout, "Overall message processing timeout")
	flags.IntVar(&cfg.messageRetentionDays, "message-retention-days", cfg.messageRetentionDays, "Default message retention in days")
	flags.BoolVar(&cfg.dynamodbEnabled, "dynamodb-enabled", cfg.dynamodbEnabled, "Enable DynamoDB message history and server configuration")
	flags.StringVar(&cfg.dynamodbTable, "dynamodb-table", cfg.dynamodbTable, "DynamoDB table name")
	flags.StringVar(&cfg.awsRoleARN, "aws-role-arn", cfg.awsRoleARN, "AWS IAM role assumed through Google workload identity")
	flags.StringVar(&cfg.awsWebIdentityAudience, "aws-web-identity-audience", cfg.awsWebIdentityAudience, "Audience for the Google identity token exchanged with AWS")
	flags.StringSliceVar(&cfg.rootUserIDs, "root-user-ids", cfg.rootUserIDs, "Discord user IDs with cross-server root access")
	return command
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
