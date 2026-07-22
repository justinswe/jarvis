package genai

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/justinswe/std/app"
	"github.com/spf13/cobra"
)

var manualTestOptions struct {
	evalProjectID, evalLocation, evalSubset, evalOutputDirectory           string
	evalPrimaryModelProfile, evalFallbackModelProfile                      string
	evalWebSearchProviders, serperAPIKey, firecrawlAPIKey, tavilyAPIKey    string
	evalModelProfiles                                                      []string
	evalRuns                                                               int
	googleAIAPIKey, nvidiaAPIKey                                           string
	openRouterAPIKey, openRouterBaseURL, openRouterModels, openRouterModel string
	exitCode                                                               int
}

// TestMain binds manual-test configuration as environment-backed command flags.
func TestMain(m *testing.M) {
	command := &cobra.Command{
		Use:          "genai-manual-tests",
		SilenceUsage: true,
		Run:          func(*cobra.Command, []string) { manualTestOptions.exitCode = m.Run() },
	}
	flags := command.Flags()
	flags.StringVar(&manualTestOptions.evalProjectID, "jarvis-eval-project-id", "", "GCP project for live presentation evaluation")
	flags.StringVar(&manualTestOptions.evalLocation, "jarvis-eval-location", "global", "Vertex location for live presentation evaluation")
	flags.StringVar(&manualTestOptions.evalSubset, "jarvis-eval-subset", "development", "Evaluation corpus subset")
	flags.StringVar(&manualTestOptions.evalOutputDirectory, "jarvis-eval-output-directory", "", "Optional evaluation output directory")
	flags.StringSliceVar(&manualTestOptions.evalModelProfiles, "jarvis-eval-model-profile", nil, "Named evaluation model profiles: name=provider:model-id")
	flags.StringVar(&manualTestOptions.evalPrimaryModelProfile, "jarvis-eval-primary-model-profile", "", "Tool-capable evaluation primary profile")
	flags.StringVar(&manualTestOptions.evalFallbackModelProfile, "jarvis-eval-fallback-model-profile", "", "Optional evaluation presentation fallback profile")
	flags.IntVar(&manualTestOptions.evalRuns, "jarvis-eval-runs", 1, "Number of evaluation passes")
	flags.StringVar(&manualTestOptions.evalWebSearchProviders, "jarvis-eval-web-search-providers", "", "Comma-separated Search providers using newly rotated credentials")
	flags.StringVar(&manualTestOptions.serperAPIKey, "serper-api-key", "", "Serper API key")
	flags.StringVar(&manualTestOptions.firecrawlAPIKey, "firecrawl-api-key", "", "Firecrawl API key")
	flags.StringVar(&manualTestOptions.tavilyAPIKey, "tavily-api-key", "", "Tavily API key")
	flags.StringVar(&manualTestOptions.googleAIAPIKey, "google-ai-api-key", "", "Google AI Studio API key")
	flags.StringVar(&manualTestOptions.nvidiaAPIKey, "nvidia-api-key", "", "NVIDIA hosted NIM API key")
	flags.StringVar(&manualTestOptions.openRouterAPIKey, "openrouter-api-key", "", "OpenRouter API key")
	flags.StringVar(&manualTestOptions.openRouterBaseURL, "openrouter-base-url", "", "Optional OpenRouter API base URL")
	flags.StringVar(&manualTestOptions.openRouterModels, "openrouter-conformance-models", "", "Comma-separated OpenRouter conformance models")
	flags.StringVar(&manualTestOptions.openRouterModel, "openrouter-conformance-model", "", "Compatibility OpenRouter conformance model")
	command.SetArgs([]string{})
	if err := app.RunCobraCommand(context.Background(), command); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "manual test configuration failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(manualTestOptions.exitCode)
}
