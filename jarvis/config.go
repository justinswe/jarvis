package main

import (
	"time"

	"github.com/justinswe/jarvis/mq"
	"github.com/spf13/cobra"
)

type supervisorConfig struct {
	port, workerPort                    string
	mqDriver                            string
	natsPort, natsMonitorPort           string
	natsBinary, natsConfig              string
	ingestorBinary, workerBinary        string
	valkeyEnabled                       bool
	valkeyBinary, valkeyPort            string
	valkeyAddress                       string
	natsStartTimeout                    time.Duration
	valkeyStartTimeout                  time.Duration
	workerStartTimeout, shutdownTimeout time.Duration
}

// defaultConfig is the supervisor configuration before flags and environment.
func defaultConfig() supervisorConfig {
	return supervisorConfig{
		port:               "8080",
		mqDriver:           string(mq.DriverNATS),
		workerPort:         "8081",
		natsPort:           "4222",
		natsMonitorPort:    "8222",
		natsBinary:         natsBinary,
		natsConfig:         natsConfig,
		ingestorBinary:     ingestorBinary,
		workerBinary:       workerBinary,
		valkeyBinary:       valkeyBinary,
		valkeyPort:         "6379",
		natsStartTimeout:   15 * time.Second,
		valkeyStartTimeout: 15 * time.Second,
		workerStartTimeout: 30 * time.Second,
		// Must exceed the worker's --mq-drain-timeout (20s), or the supervisor
		// SIGKILLs the worker part-way through draining the replies it is finishing.
		shutdownTimeout: 30 * time.Second,
	}
}

func newRootCommand() *cobra.Command {
	cfg := defaultConfig()
	command := &cobra.Command{
		Use:   "jarvis",
		Short: "Starts the combined Jarvis deployment",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJarvis(cmd.Context(), cfg)
		},
	}
	flags := command.Flags()
	flags.StringVar(&cfg.port, "port", cfg.port, "Ingestor HTTP health server port")
	flags.StringVar(&cfg.workerPort, "worker-port", cfg.workerPort, "Internal worker health server port")
	flags.StringVar(&cfg.natsPort, "nats-port", cfg.natsPort, "Internal NATS client port")
	flags.StringVar(&cfg.natsMonitorPort, "nats-monitor-port", cfg.natsMonitorPort, "Internal NATS monitoring port")
	flags.StringVar(&cfg.natsBinary, "nats-binary", cfg.natsBinary, "Path to the nats-server binary")
	flags.StringVar(&cfg.natsConfig, "nats-config", cfg.natsConfig, "Path to the nats-server configuration file")
	// Bound so the supervisor can see the children's MQ_DRIVER without reading the
	// environment behind the flag machinery, which AGENTS.md forbids. The supervisor never
	// speaks to a broker itself; it only needs to know whether to start one.
	flags.StringVar(&cfg.mqDriver, "mq-driver", cfg.mqDriver, "Message queue broker the children use: nats starts a supervised server, pubsub starts none")
	flags.BoolVar(&cfg.valkeyEnabled, "valkey-enabled", cfg.valkeyEnabled, "Use Valkey; a supervised one is started unless --valkey-address names a host other than loopback")
	flags.StringVar(&cfg.valkeyBinary, "valkey-binary", cfg.valkeyBinary, "Path to the valkey-server binary")
	flags.StringVar(&cfg.valkeyPort, "valkey-port", cfg.valkeyPort, "Internal port for the supervised Valkey when --valkey-address names none")
	// Bound so the supervisor can see the worker's VALKEY_ADDRESS rather than reading the
	// environment behind the flag machinery, which AGENTS.md forbids. The supervisor never
	// connects to Valkey itself; it only needs to know whether an external one was named.
	flags.StringVar(&cfg.valkeyAddress, "valkey-address", cfg.valkeyAddress, "Valkey host:port the worker uses; loopback is supervised on the port it names, anything else is used as-is and nothing is started")
	flags.DurationVar(&cfg.natsStartTimeout, "nats-start-timeout", cfg.natsStartTimeout, "Maximum time to wait for NATS readiness")
	flags.DurationVar(&cfg.valkeyStartTimeout, "valkey-start-timeout", cfg.valkeyStartTimeout, "Maximum time to wait for Valkey readiness")
	flags.DurationVar(&cfg.workerStartTimeout, "worker-start-timeout", cfg.workerStartTimeout, "Maximum time to wait for worker readiness")
	flags.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", cfg.shutdownTimeout, "Maximum time to wait for child shutdown")
	return command
}
