package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/justinswe/jarvis/pkg/discord"
	llm "github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

type flags struct {
	port                                string
	projectID                           string
	location                            string
	modelName                           string
	systemPrompt                        string
	outsideContextWindow                int
	threadContextWindow                 int
	maxOutputTokens                     int
	temperature                         float64
	discordBotToken                     string
	ginMode                             string
	webGroundingEnabled                 bool
	webIndicator                        string
	webGroundingMessageTimeout          time.Duration
	webGroundingTimeout                 time.Duration
	webGroundingAttemptTimeout          time.Duration
	webGroundingAddendumMaxOutputTokens int
	webGroundingAPIVersion              string
	webGroundingMaxResults              int
	webGroundingBudgetRatio             float64
	webGroundingRequireCitations        bool
	webGroundingRetryMaxAttempts        int
	webGroundingRetryBaseDelay          time.Duration
	webGroundingRetryMaxDelay           time.Duration
	webGroundingRetryJitter             float64
	webGroundingDisableAllowlists       bool
	webGroundingChannelAllowlist        []string
	webGroundingRoleAllowlist           []string
	webGroundingUserRPM                 int
	webGroundingGlobalRPM               int
}

var (
	command = &cobra.Command{
		Use:   "discordai",
		Short: "Discord w/ Vertex AI",
		RunE:  run,
	}
	cfg flags
)

func init() {
	command.Flags().StringVar(&cfg.port, "port", "8080", "The port of the service")
	command.Flags().StringVar(&cfg.projectID, "project-id", "", "The GCP project ID (required)")
	command.Flags().StringVar(&cfg.location, "location", "global", "The Vertex AI location")
	command.Flags().StringVar(&cfg.modelName, "model-name", llm.DefaultModel, "The Vertex AI model name")
	command.Flags().StringVar(&cfg.systemPrompt, "system-prompt", llm.DefaultSystemPrompt, "The system prompt used for responses")
	command.Flags().IntVar(&cfg.outsideContextWindow, "outside-context-window", 20, "Number of prior messages from the parent channel to include as context")
	command.Flags().IntVar(&cfg.threadContextWindow, "thread-context-window", 30, "Number of prior messages from the thread to include as context")
	command.Flags().IntVar(&cfg.maxOutputTokens, "max-output-tokens", 6144, "Maximum tokens in the model response")
	command.Flags().Float64Var(&cfg.temperature, "temperature", 1.4, "Temperature for the model")
	command.Flags().StringVar(&cfg.discordBotToken, "discord-bot-token", "", "The Discord bot token (required)")
	command.Flags().StringVar(&cfg.ginMode, "gin-mode", "debug", "The mode of the Gin server")
	command.Flags().BoolVar(&cfg.webGroundingEnabled, "web-grounding-enabled", false, "Enable optional #web grounding mode")
	command.Flags().StringVar(&cfg.webIndicator, "web-indicator", "#web", "Front-of-message token that enables web grounding")
	command.Flags().DurationVar(&cfg.webGroundingMessageTimeout, "web-grounding-message-timeout", 75*time.Second, "Discord message timeout for #web requests")
	command.Flags().DurationVar(&cfg.webGroundingTimeout, "web-grounding-timeout", llm.DefaultWebGroundingTimeout, "Total timeout for web grounding calls")
	command.Flags().DurationVar(&cfg.webGroundingAttemptTimeout, "web-grounding-attempt-timeout", llm.DefaultWebGroundingAttemptTimeout, "Timeout for one web grounding attempt")
	command.Flags().IntVar(&cfg.webGroundingAddendumMaxOutputTokens, "web-grounding-addendum-max-output-tokens", llm.DefaultWebGroundingAddendumMaxOutputTokens, "Maximum output tokens for the web-grounded addendum")
	command.Flags().StringVar(&cfg.webGroundingAPIVersion, "web-grounding-api-version", llm.DefaultWebGroundingAPIVersion, "Vertex AI API version for web-grounded calls")
	command.Flags().IntVar(&cfg.webGroundingMaxResults, "web-grounding-max-results", llm.DefaultWebGroundingMaxResults, "Maximum number of web sources to render")
	command.Flags().Float64Var(&cfg.webGroundingBudgetRatio, "web-grounding-budget-ratio", float64(llm.DefaultWebGroundingBudgetRatio), "Max output share reserved for web grounding")
	command.Flags().BoolVar(&cfg.webGroundingRequireCitations, "web-grounding-require-citations", true, "Require visible citations in web-grounded addenda")
	command.Flags().IntVar(&cfg.webGroundingRetryMaxAttempts, "web-grounding-retry-max-attempts", llm.DefaultWebGroundingRetryMaxAttempts, "Max attempts for retryable #web grounding errors")
	command.Flags().DurationVar(&cfg.webGroundingRetryBaseDelay, "web-grounding-retry-base-delay", llm.DefaultWebGroundingRetryBaseDelay, "Base backoff delay for retryable #web grounding errors")
	command.Flags().DurationVar(&cfg.webGroundingRetryMaxDelay, "web-grounding-retry-max-delay", llm.DefaultWebGroundingRetryMaxDelay, "Max backoff delay for retryable #web grounding errors")
	command.Flags().Float64Var(&cfg.webGroundingRetryJitter, "web-grounding-retry-jitter", llm.DefaultWebGroundingRetryJitter, "Jitter factor (0-1) applied to #web grounding retry delays")
	command.Flags().BoolVar(&cfg.webGroundingDisableAllowlists, "web-grounding-disable-allowlists", false, "Disable channel and role allowlist enforcement for #web requests")
	command.Flags().StringSliceVar(&cfg.webGroundingChannelAllowlist, "web-grounding-channel-allowlist", nil, "Allowlisted channel IDs for #web requests")
	command.Flags().StringSliceVar(&cfg.webGroundingRoleAllowlist, "web-grounding-role-allowlist", nil, "Allowlisted Discord role IDs for #web requests")
	command.Flags().IntVar(&cfg.webGroundingUserRPM, "web-grounding-user-rpm", 5, "Per-user #web requests per minute")
	command.Flags().IntVar(&cfg.webGroundingGlobalRPM, "web-grounding-global-rpm", 5, "Global #web requests per minute")

	viper.BindPFlags(command.Flags())
	// Mark the flags as required
	for _, flag := range []string{"discord-bot-token", "project-id"} {
		err := command.MarkFlagRequired(flag)
		if err != nil {
			app.L().Fatal("Error marking flag as required", zap.Error(err))
		}
	}
}

func run(cmd *cobra.Command, _ []string) error {
	if cfg.projectID == "" {
		return errors.New("project-id is required")
	}
	if cfg.discordBotToken == "" {
		return errors.New("discord-bot-token is required")
	}

	ctx := cmd.Context()

	llmHandler, err := llm.New(ctx, llm.Config{
		ProjectID:                           cfg.projectID,
		Location:                            cfg.location,
		Model:                               cfg.modelName,
		SystemPrompt:                        cfg.systemPrompt,
		MaxOutputTokens:                     cfg.maxOutputTokens,
		Temperature:                         float32(cfg.temperature),
		WebGroundingTimeout:                 cfg.webGroundingTimeout,
		WebGroundingAttemptTimeout:          cfg.webGroundingAttemptTimeout,
		WebGroundingAddendumMaxOutputTokens: cfg.webGroundingAddendumMaxOutputTokens,
		WebGroundingAPIVersion:              cfg.webGroundingAPIVersion,
		WebGroundingMaxResults:              cfg.webGroundingMaxResults,
		WebGroundingBudgetRatio:             float32(cfg.webGroundingBudgetRatio),
		WebGroundingRequireCitations:        cfg.webGroundingRequireCitations,
		WebGroundingRetryMaxAttempts:        cfg.webGroundingRetryMaxAttempts,
		WebGroundingRetryBaseDelay:          cfg.webGroundingRetryBaseDelay,
		WebGroundingRetryMaxDelay:           cfg.webGroundingRetryMaxDelay,
		WebGroundingRetryJitter:             cfg.webGroundingRetryJitter,
	})
	if err != nil {
		return err
	}
	defer llmHandler.Close()

	bot, err := discord.NewBot(discord.Config{
		Token:                cfg.discordBotToken,
		OutsideContextWindow: cfg.outsideContextWindow,
		ThreadContextWindow:  cfg.threadContextWindow,
		WebGrounding: discord.WebGroundingConfig{
			Enabled:                 cfg.webGroundingEnabled,
			Indicator:               cfg.webIndicator,
			MessageTimeout:          cfg.webGroundingMessageTimeout,
			Timeout:                 cfg.webGroundingTimeout,
			AttemptTimeout:          cfg.webGroundingAttemptTimeout,
			AddendumMaxOutputTokens: cfg.webGroundingAddendumMaxOutputTokens,
			APIVersion:              cfg.webGroundingAPIVersion,
			MaxResults:              cfg.webGroundingMaxResults,
			BudgetRatio:             float32(cfg.webGroundingBudgetRatio),
			RequireCitations:        cfg.webGroundingRequireCitations,
			DisableAllowlists:       cfg.webGroundingDisableAllowlists,
			ChannelAllowlist:        cfg.webGroundingChannelAllowlist,
			RoleAllowlist:           cfg.webGroundingRoleAllowlist,
			UserRPM:                 cfg.webGroundingUserRPM,
			GlobalRPM:               cfg.webGroundingGlobalRPM,
		},
	}, llmHandler)
	if err != nil {
		return err
	}

	if _, err := startHTTPServer(ctx); err != nil {
		return err
	}

	app.L().Info("Starting Discord AI bot",
		zap.String("model", cfg.modelName),
		zap.Int("outside_context_window", cfg.outsideContextWindow),
		zap.Int("thread_context_window", cfg.threadContextWindow),
		zap.Int("max_output_tokens", cfg.maxOutputTokens),
		zap.String("location", cfg.location),
		zap.Bool("web_grounding_enabled", cfg.webGroundingEnabled),
		zap.String("web_indicator", cfg.webIndicator),
		zap.Duration("web_grounding_message_timeout", cfg.webGroundingMessageTimeout),
		zap.Duration("web_grounding_timeout", cfg.webGroundingTimeout),
		zap.Duration("web_grounding_attempt_timeout", cfg.webGroundingAttemptTimeout),
		zap.Int("web_grounding_addendum_max_output_tokens", cfg.webGroundingAddendumMaxOutputTokens),
		zap.String("web_grounding_api_version", cfg.webGroundingAPIVersion),
		zap.Int("web_grounding_max_results", cfg.webGroundingMaxResults),
		zap.Float64("web_grounding_budget_ratio", cfg.webGroundingBudgetRatio),
		zap.Bool("web_grounding_require_citations", cfg.webGroundingRequireCitations),
		zap.Int("web_grounding_retry_max_attempts", cfg.webGroundingRetryMaxAttempts),
		zap.Duration("web_grounding_retry_base_delay", cfg.webGroundingRetryBaseDelay),
		zap.Duration("web_grounding_retry_max_delay", cfg.webGroundingRetryMaxDelay),
		zap.Float64("web_grounding_retry_jitter", cfg.webGroundingRetryJitter),
		zap.Bool("web_grounding_disable_allowlists", cfg.webGroundingDisableAllowlists),
		zap.Int("web_grounding_channel_allowlist_size", len(cfg.webGroundingChannelAllowlist)),
		zap.Int("web_grounding_role_allowlist_size", len(cfg.webGroundingRoleAllowlist)),
		zap.Int("web_grounding_user_rpm", cfg.webGroundingUserRPM),
		zap.Int("web_grounding_global_rpm", cfg.webGroundingGlobalRPM),
	)

	return bot.Start(ctx)
}

func main() {
	if err := app.RunCobraCommand(context.Background(), command); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func startHTTPServer(ctx context.Context) (*http.Server, error) {
	router := gin.New()
	gin.SetMode(cfg.ginMode)
	router.Use(gin.Recovery())

	router.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "Starting Discord AI bot")
	})

	srv := &http.Server{
		Addr:    ":" + cfg.port,
		Handler: router,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			app.L().Warn("HTTP server failed", zap.Error(err))
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			app.L().Warn("HTTP server shutdown error", zap.Error(err))
		}
	}()

	return srv, nil
}
