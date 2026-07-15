package main

import (
	"time"

	"github.com/spf13/cobra"
)

type supervisorConfig struct {
	port, workerPort                    string
	workerStartTimeout, shutdownTimeout time.Duration
}

func newRootCommand() *cobra.Command {
	cfg := supervisorConfig{
		port:               "8080",
		workerPort:         "8081",
		workerStartTimeout: 30 * time.Second,
		shutdownTimeout:    10 * time.Second,
	}
	command := &cobra.Command{
		Use:   "jarvis",
		Short: "Starts the combined Jarvis deployment",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJarvis(cmd.Context(), cfg)
		},
	}
	flags := command.Flags()
	flags.StringVar(&cfg.port, "port", cfg.port, "Ingestor HTTP health server port")
	flags.StringVar(&cfg.workerPort, "worker-port", cfg.workerPort, "Internal worker HTTP port")
	flags.DurationVar(&cfg.workerStartTimeout, "worker-start-timeout", cfg.workerStartTimeout, "Maximum time to wait for worker readiness")
	flags.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", cfg.shutdownTimeout, "Maximum time to wait for child shutdown")
	return command
}
