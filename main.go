package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/justinswe/jarvis/pkg/discord"
	llm "github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

type flags struct {
	port, projectID, location, modelName, defaultPrompt, discordBotToken string
	threadMessages, parentMessages, channelMessages, historyRunes        int
	maxOutputTokens                                                      int
	temperature                                                          float64
	messageTimeout                                                       time.Duration
}

const serviceName = "jarvis"

var (
	rootCmd = &cobra.Command{
		Use:   serviceName,
		Short: "Starts the Jarvis Discord bot",
		Long:  "Connects Jarvis to Discord and generates responses with Vertex AI",
		RunE:  run,
	}
	cfg flags
)

func init() {
	rootCmd.Flags().StringVar(&cfg.port, "port", "8080", "HTTP server port")
	rootCmd.Flags().StringVar(&cfg.projectID, "project-id", "", "GCP project ID")
	rootCmd.Flags().StringVar(&cfg.location, "location", "global", "Vertex AI location")
	rootCmd.Flags().StringVar(&cfg.modelName, "model-name", llm.DefaultModel, "Vertex AI model")
	rootCmd.Flags().StringVar(&cfg.defaultPrompt, "default-prompt", llm.DefaultPrompt, "Bot identity and personality prompt")
	rootCmd.Flags().IntVar(&cfg.threadMessages, "thread-context-window", 15, "Prior thread messages")
	rootCmd.Flags().IntVar(&cfg.parentMessages, "parent-context-window", 10, "Prior parent-channel messages")
	rootCmd.Flags().IntVar(&cfg.channelMessages, "channel-context-window", 4, "Prior ordinary channel messages")
	rootCmd.Flags().IntVar(&cfg.historyRunes, "history-runes", 4000, "Combined context history rune budget")
	rootCmd.Flags().IntVar(&cfg.maxOutputTokens, "max-output-tokens", llm.DefaultMaxOutputTokens, "Maximum model output tokens (maximum 512)")
	rootCmd.Flags().Float64Var(&cfg.temperature, "temperature", 1.4, "Model temperature")
	rootCmd.Flags().StringVar(&cfg.discordBotToken, "discord-bot-token", "", "Discord bot token")
	rootCmd.Flags().DurationVar(&cfg.messageTimeout, "message-timeout", 60*time.Second, "Overall message processing timeout")

	// Mark the flags as required
	for _, flag := range []string{"project-id", "discord-bot-token"} {
		err := rootCmd.MarkFlagRequired(flag)
		if err != nil {
			app.L().Fatal("Error marking flag as required", zap.Error(err))
		}
	}
}

func run(cmd *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app.L().Info("Initializing service", zap.String("service", serviceName))

	generator, err := llm.New(ctx, llm.Config{
		ProjectID:       cfg.projectID,
		Location:        cfg.location,
		Model:           cfg.modelName,
		DefaultPrompt:   cfg.defaultPrompt,
		MaxOutputTokens: cfg.maxOutputTokens,
		Temperature:     float32(cfg.temperature),
	})
	if err != nil {
		return errors.Wrap(err, "initialize Gemini client")
	}
	defer generator.Close()

	bot, err := discord.NewBot(discord.Config{
		Token:           cfg.discordBotToken,
		ThreadMessages:  cfg.threadMessages,
		ParentMessages:  cfg.parentMessages,
		ChannelMessages: cfg.channelMessages,
		HistoryRunes:    cfg.historyRunes,
		MessageTimeout:  cfg.messageTimeout,
	}, generator)
	if err != nil {
		return errors.Wrap(err, "initialize Discord bot")
	}

	app.L().Info("Starting service", zap.String("service", serviceName), zap.String("port", cfg.port))
	return serve(ctx, cfg.port, bot)
}

type discordService interface {
	Start(context.Context) error
	Ready() bool
}

func serve(parent context.Context, port string, bot discordService) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	srv := &http.Server{Addr: ":" + port, Handler: newHTTPHandler(bot), ReadHeaderTimeout: 5 * time.Second}
	errs := make(chan error, 2)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()
	go func() { errs <- bot.Start(ctx) }()

	var result error
	completed := 0
	select {
	case <-parent.Done():
		result = nil
	case result = <-errs:
		completed++
		if result != nil {
			result = errors.Wrap(result, "service failed")
		}
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && result == nil {
		result = err
	}
	for completed < 2 {
		select {
		case err := <-errs:
			completed++
			if err != nil && result == nil {
				result = err
			}
		case <-shutdownCtx.Done():
			if result == nil {
				result = shutdownCtx.Err()
			}
			return result
		}
	}
	return result
}

func newHTTPHandler(bot interface{ Ready() bool }) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !bot.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func main() {
	if err := app.RunCobraCommand(context.Background(), rootCmd); err != nil {
		app.L().Error("Jarvis stopped", zap.Error(err))
		os.Exit(1)
	}
}
