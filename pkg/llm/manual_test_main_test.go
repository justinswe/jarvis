package llm

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/justinswe/std/app"
	"github.com/spf13/cobra"
)

var manualTestOptions struct {
	googleAIAPIKey, googleAIModel                                          string
	openRouterAPIKey, openRouterBaseURL, openRouterModels, openRouterModel string
	openRouterAllTextModels                                                bool
	openRouterMaxModels                                                    int
	exitCode                                                               int
}

// TestMain binds manual-test configuration as environment-backed command flags.
func TestMain(m *testing.M) {
	command := &cobra.Command{
		Use:          "llm-manual-tests",
		SilenceUsage: true,
		Run:          func(*cobra.Command, []string) { manualTestOptions.exitCode = m.Run() },
	}
	flags := command.Flags()
	flags.StringVar(&manualTestOptions.googleAIAPIKey, "google-ai-api-key", "", "Google AI Studio API key")
	flags.StringVar(&manualTestOptions.googleAIModel, "google-ai-conformance-model", "", "Explicit Gemini 3+ Google AI conformance model")
	flags.StringVar(&manualTestOptions.openRouterAPIKey, "openrouter-api-key", "", "OpenRouter API key")
	flags.StringVar(&manualTestOptions.openRouterBaseURL, "openrouter-base-url", "", "Optional OpenRouter API base URL")
	flags.StringVar(&manualTestOptions.openRouterModels, "openrouter-conformance-models", "", "Comma-separated OpenRouter conformance models")
	flags.StringVar(&manualTestOptions.openRouterModel, "openrouter-conformance-model", "", "Compatibility OpenRouter conformance model")
	flags.BoolVar(&manualTestOptions.openRouterAllTextModels, "openrouter-conformance-all-text-models", false, "Audit every credential-visible text model without generation")
	flags.IntVar(&manualTestOptions.openRouterMaxModels, "openrouter-conformance-max-models", 0, "Optional catalog audit model limit")
	command.SetArgs([]string{})
	if err := app.RunCobraCommand(context.Background(), command); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "manual test configuration failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(manualTestOptions.exitCode)
}
