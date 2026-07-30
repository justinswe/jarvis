package usage

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/justinswe/std/app"
	"github.com/spf13/cobra"
)

var manualTestOptions struct {
	valkeyAddress  string
	valkeyUsername string
	valkeyPassword string
	exitCode       int
}

// TestMain binds manual-test configuration as environment-backed command flags.
func TestMain(m *testing.M) {
	command := &cobra.Command{
		Use:          "usage-manual-tests",
		SilenceUsage: true,
		Run:          func(*cobra.Command, []string) { manualTestOptions.exitCode = m.Run() },
	}
	flags := command.Flags()
	flags.StringVar(&manualTestOptions.valkeyAddress, "valkey-address", "", "Live Valkey host:port for integration tests")
	flags.StringVar(&manualTestOptions.valkeyUsername, "valkey-username", "", "Live Valkey ACL username")
	flags.StringVar(&manualTestOptions.valkeyPassword, "valkey-password", "", "Live Valkey ACL password")
	command.SetArgs([]string{})
	if err := app.RunCobraCommand(context.Background(), command); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "manual test configuration failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(manualTestOptions.exitCode)
}
