package memory

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/justinswe/std/app"
	"github.com/spf13/cobra"
)

var manualTestOptions struct {
	postgresDSN                                    string
	evalProjectID, evalLocation                    string
	evalModelProfiles                              []string
	evalPrimaryModelProfile, evalEmbeddingProfile  string
	googleAIAPIKey, openRouterAPIKey, nvidiaAPIKey string
	evalOutputDirectory                            string
	evalTurns                                      int
	exitCode                                       int
}

// TestMain binds manual-eval configuration as environment-backed command flags, the
// same pattern as the store and genai manual suites.
func TestMain(m *testing.M) {
	command := &cobra.Command{
		Use:          "memory-manual-tests",
		SilenceUsage: true,
		Run:          func(*cobra.Command, []string) { manualTestOptions.exitCode = m.Run() },
	}
	flags := command.Flags()
	flags.StringVar(&manualTestOptions.postgresDSN, "postgres-dsn", "", "PostgreSQL DSN with pgvector, for the live memory evaluation")
	flags.StringVar(&manualTestOptions.evalProjectID, "jarvis-eval-project-id", "", "GCP project for Vertex profiles")
	flags.StringVar(&manualTestOptions.evalLocation, "jarvis-eval-location", "global", "Vertex location")
	flags.StringSliceVar(&manualTestOptions.evalModelProfiles, "jarvis-eval-model-profile", nil, "Named evaluation model profiles: name=provider:model-id")
	flags.StringVar(&manualTestOptions.evalPrimaryModelProfile, "jarvis-eval-primary-model-profile", "", "Extraction model profile name")
	flags.StringVar(&manualTestOptions.evalEmbeddingProfile, "jarvis-eval-embedding-model-profile", "", "Embedding profile: name=provider:model-id (vertex or google-ai)")
	flags.StringVar(&manualTestOptions.googleAIAPIKey, "google-ai-api-key", "", "Google AI Studio API key")
	flags.StringVar(&manualTestOptions.openRouterAPIKey, "openrouter-api-key", "", "OpenRouter API key")
	flags.StringVar(&manualTestOptions.nvidiaAPIKey, "nvidia-api-key", "", "NVIDIA hosted NIM API key")
	flags.StringVar(&manualTestOptions.evalOutputDirectory, "jarvis-eval-output-directory", "", "Optional evaluation output directory")
	flags.IntVar(&manualTestOptions.evalTurns, "jarvis-eval-turns", 500, "Scripted conversation length")
	command.SetArgs([]string{})
	if err := app.RunCobraCommand(context.Background(), command); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "manual test configuration failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(manualTestOptions.exitCode)
}
