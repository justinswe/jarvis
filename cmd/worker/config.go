package main

import (
	"time"

	"github.com/justinswe/jarvis/internal/config"
	llm "github.com/justinswe/jarvis/pkg/genai"
	"github.com/spf13/cobra"
)

type workerConfig struct {
	host, port, projectID, location, defaultPrompt, discordBotToken string
	dynamodbTable                                                   string
	rootUserIDs                                                     []string
	dynamodbEnabled                                                 bool
	threadMessages, parentMessages, channelMessages, historyRunes   int
	maxOutputTokens                                                 int
	messageRetentionDays                                            int
	temperature                                                     float64
	messageTimeout                                                  time.Duration
}

func newRootCommand() *cobra.Command {
	cfg := workerConfig{
		port:                 "8080",
		location:             "global",
		defaultPrompt:        llm.DefaultPrompt,
		threadMessages:       15,
		parentMessages:       10,
		channelMessages:      4,
		historyRunes:         4000,
		maxOutputTokens:      llm.DefaultMaxOutputTokens,
		messageRetentionDays: config.DefaultMessageRetentionDays,
		temperature:          1.4,
		messageTimeout:       time.Minute,
		dynamodbTable:        "jarvis",
	}
	command := &cobra.Command{
		Use:   "worker",
		Short: "Starts the stateless Jarvis HTTP worker",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorker(cmd.Context(), cfg)
		},
	}
	flags := command.Flags()
	flags.StringVar(&cfg.host, "host", cfg.host, "HTTP worker bind host")
	flags.StringVar(&cfg.port, "port", cfg.port, "HTTP worker port")
	flags.StringVar(&cfg.projectID, "project-id", cfg.projectID, "GCP project ID")
	flags.StringVar(&cfg.location, "location", cfg.location, "Vertex AI location")
	flags.StringVar(&cfg.defaultPrompt, "default-prompt", cfg.defaultPrompt, "Bot identity and personality prompt")
	flags.IntVar(&cfg.threadMessages, "thread-context-window", cfg.threadMessages, "Prior thread messages")
	flags.IntVar(&cfg.parentMessages, "parent-context-window", cfg.parentMessages, "Prior parent-channel messages")
	flags.IntVar(&cfg.channelMessages, "channel-context-window", cfg.channelMessages, "Prior ordinary channel messages")
	flags.IntVar(&cfg.historyRunes, "history-runes", cfg.historyRunes, "Combined context history rune budget")
	flags.IntVar(&cfg.maxOutputTokens, "max-output-tokens", cfg.maxOutputTokens, "Maximum total generated tokens, including thinking and visible text (maximum 8192)")
	flags.Float64Var(&cfg.temperature, "temperature", cfg.temperature, "Model temperature")
	flags.StringVar(&cfg.discordBotToken, "discord-bot-token", cfg.discordBotToken, "Discord bot token")
	flags.DurationVar(&cfg.messageTimeout, "message-timeout", cfg.messageTimeout, "Overall message processing timeout")
	flags.IntVar(&cfg.messageRetentionDays, "message-retention-days", cfg.messageRetentionDays, "Default message retention in days")
	flags.BoolVar(&cfg.dynamodbEnabled, "dynamodb-enabled", cfg.dynamodbEnabled, "Enable DynamoDB message history and server configuration")
	flags.StringVar(&cfg.dynamodbTable, "dynamodb-table", cfg.dynamodbTable, "DynamoDB table name")
	flags.StringSliceVar(&cfg.rootUserIDs, "root-user-ids", cfg.rootUserIDs, "Discord user IDs with cross-server root access")
	return command
}
