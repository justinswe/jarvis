package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/justinswe/jarvis/internal/awsidentity"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/internal/dynamostore"
	workerservice "github.com/justinswe/jarvis/internal/worker"
	"github.com/justinswe/jarvis/pkg/discord"
	llm "github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

func runWorker(parent context.Context, cfg workerConfig) error {
	if cfg.port == "" {
		return errors.New("worker port is required")
	}
	if cfg.projectID == "" {
		return errors.New("project-id is required")
	}
	if cfg.discordBotToken == "" {
		return errors.New("discord bot token is required")
	}
	if cfg.dynamodbEnabled && strings.TrimSpace(cfg.dynamodbTable) == "" {
		return errors.New("DynamoDB table is required when DynamoDB is enabled")
	}
	for _, userID := range cfg.rootUserIDs {
		if !validRootUserID(userID) {
			return errors.Errorf("root user ID %q must be a 17-20 digit Discord user ID", userID)
		}
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
	var recorder workerservice.Recorder
	if cfg.dynamodbEnabled {
		awsCfg, loadErr := awsidentity.Load(ctx, awsidentity.Config{
			RoleARN:  cfg.awsRoleARN,
			Audience: cfg.awsWebIdentityAudience,
		})
		if loadErr != nil {
			return errors.Wrap(loadErr, "initialize DynamoDB AWS configuration")
		}
		credentials, retrieveErr := awsCfg.Credentials.Retrieve(ctx)
		if retrieveErr != nil {
			return errors.Wrap(retrieveErr, "authenticate to AWS for DynamoDB")
		}
		fields := []zap.Field{
			zap.String("credential_source", credentials.Source),
			zap.String("aws_account_id", credentials.AccountID),
		}
		if strings.TrimSpace(cfg.awsRoleARN) != "" {
			fields = append(fields,
				zap.String("authentication_mode", "gcp_web_identity"),
				zap.String("role_arn", strings.TrimSpace(cfg.awsRoleARN)),
			)
		} else {
			fields = append(fields, zap.String("authentication_mode", "default_aws_chain"))
		}
		if credentials.CanExpire {
			fields = append(fields, zap.Time("credential_expiration", credentials.Expires))
		}
		app.L().Info("DynamoDB AWS authentication initialized", fields...)

		repository, repositoryErr := dynamostore.New(
			dynamodb.NewFromConfig(awsCfg), cfg.dynamodbTable, config.GuildConfig{Settings: cfg.serverSettings()},
		)
		if repositoryErr != nil {
			return errors.Wrap(repositoryErr, "initialize DynamoDB repository")
		}
		defer repository.Close()
		provider, history, manager, recorder = repository, repository, repository, repository
	}
	generator, err := llm.New(ctx, llm.Config{
		ProjectID:       cfg.projectID,
		Location:        cfg.location,
		DefaultPrompt:   cfg.defaultPrompt,
		MaxOutputTokens: cfg.maxOutputTokens,
		Temperature:     float32(cfg.temperature),
	})
	if err != nil {
		return errors.Wrap(err, "initialize Gemini client")
	}
	defer generator.Close()
	processor, err := discord.NewProcessorWithConfig(ctx, discord.ProcessorConfig{
		DiscordBotToken: cfg.discordBotToken,
		Configs:         provider,
		Generator:       generator,
		History:         history,
		ConfigManager:   manager,
		RootUserIDs:     cfg.rootUserIDs,
	})
	if err != nil {
		return errors.Wrap(err, "initialize Discord processor")
	}

	address := cfg.address()
	app.L().Info("Starting stateless worker", zap.String("address", address))
	return workerservice.Serve(ctx, address, processor, recorder)
}

func (cfg workerConfig) address() string {
	return net.JoinHostPort(cfg.host, cfg.port)
}

func (cfg workerConfig) serverSettings() config.ServerSettings {
	return config.ServerSettings{
		Prompt:               cfg.defaultPrompt,
		ThreadMessages:       cfg.threadMessages,
		ParentMessages:       cfg.parentMessages,
		ChannelMessages:      cfg.channelMessages,
		HistoryRunes:         cfg.historyRunes,
		MaxOutputTokens:      cfg.maxOutputTokens,
		Temperature:          float32(cfg.temperature),
		MessageTimeout:       cfg.messageTimeout,
		MessageRetentionDays: cfg.messageRetentionDays,
		WebSearchEnabled:     true,
		ChannelSearchEnabled: true,
	}
}

func validRootUserID(userID string) bool {
	userID = strings.TrimSpace(userID)
	return len(userID) >= 17 && len(userID) <= 20 && strings.IndexFunc(userID, func(r rune) bool { return !unicode.IsDigit(r) }) < 0
}
