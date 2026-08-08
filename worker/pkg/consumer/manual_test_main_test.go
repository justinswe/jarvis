package consumer

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/justinswe/std/app"
	"github.com/spf13/cobra"
)

var manualTestOptions struct {
	natsURL         string
	pubsubProjectID string
	exitCode        int
}

// TestMain binds manual-test configuration as environment-backed command flags.
func TestMain(m *testing.M) {
	command := &cobra.Command{
		Use:          "consumer-manual-tests",
		SilenceUsage: true,
		Run:          func(*cobra.Command, []string) { manualTestOptions.exitCode = m.Run() },
	}
	command.Flags().StringVar(&manualTestOptions.natsURL, "nats-url", "", "Live NATS URL for integration tests")
	command.Flags().StringVar(&manualTestOptions.pubsubProjectID, "pubsub-project-id", "",
		"Pub/Sub project for integration tests; point PUBSUB_EMULATOR_HOST at an emulator")
	command.SetArgs([]string{})
	if err := app.RunCobraCommand(context.Background(), command); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "manual test configuration failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(manualTestOptions.exitCode)
}
