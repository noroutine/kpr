package cli

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/app"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/otel"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
	"nrtn.dev/catalyst/kpr/internal/web"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start both management console and application servers",
	Long:  `Start the kpr servers: management console on port 9300 and application server on port 8080.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Resolve configuration once: environment first, explicit flags
		// win (an unset flag already carries the env value as its
		// default, so layering flag values on top is exact).
		cfg := config.NewBuilder().FromEnv().
			WithManagementHost(managementHost).
			WithManagementPort(managementPort).
			WithAppHost(appHost).
			WithAppPort(appPort).
			Build()
		defer config.SetCurrent(cfg)()
		if !cmd.Flags().Changed("management-port") && cfg.ManagementPortWarning != nil {
			log.Printf("Warning: %v", cfg.ManagementPortWarning)
		}
		if !cmd.Flags().Changed("app-port") && cfg.AppPortWarning != nil {
			log.Printf("Warning: %v", cfg.AppPortWarning)
		}

		// Initialize OpenTelemetry
		otelCfg := otel.LoadConfig()
		shutdown, err := otel.Init(otelCfg)
		if err != nil {
			log.Fatalf("Failed to initialize OpenTelemetry: %v", err)
		}
		defer func() {
			if err := shutdown(context.Background()); err != nil {
				log.Printf("Error shutting down OpenTelemetry: %v", err)
			}
		}()

		// Setup graceful shutdown context
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Shared keeper state: the redis store (lazy — a down redis
		// degrades banner/receiver/sweeper instead of blocking boot),
		// the registry client, and the single-owner sweeper.
		keeperStore := store.NewRedisStore(cfg.RedisAddr)
		defer func() { _ = keeperStore.Close() }()
		if perr := keeperStore.Ping(ctx); perr != nil {
			log.Printf("Warning: redis at %s unreachable, keeper sections degrade: %v", cfg.RedisAddr, perr)
		}
		regClient := registry.NewClient(cfg.RegistryURL)
		sweeper := &sweep.Sweeper{
			Store:    keeperStore,
			Registry: regClient,
			DryRun:   !cfg.NoDryRun,
		}
		if cfg.NoDryRun {
			log.Printf("Sweeper armed: deletes are real")
		} else {
			log.Printf("Sweeper dry-run: deletes only planned (arm with --no-dry-run or KPR_NO_DRY_RUN=true)")
		}

		var wg sync.WaitGroup
		errors := make(chan error, 3)

		// Sweeper loop: sweep-on-start (a restart doesn't wait a full
		// interval) plus the tick backstop.
		wg.Add(1)
		go func() {
			defer wg.Done()
			startSweeperLoop(ctx, sweeper, sweep.TickInterval)
		}()

		// Setup signal handling
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		// Start management console server
		wg.Add(1)
		go func() {
			defer wg.Done()
			managementServer := &web.Server{
				Host:        managementHost,
				Port:        managementPort,
				OTELEnabled: otelCfg.Enabled,
				Store:       keeperStore,
				Registry:    regClient,
				Sweeper:     sweeper,
				Armed:       cfg.NoDryRun,
			}
			addr := net.JoinHostPort(managementHost, fmt.Sprintf("%d", managementPort))
			log.Printf("Starting management console on %s", addr)
			// Always forward the result, including a nil graceful stop:
			// the select below filters, so a dropped-nil versus
			// sent-nil difference can't hide here.
			errors <- managementServer.Start(ctx)
		}()

		// Start application server
		wg.Add(1)
		go func() {
			defer wg.Done()
			appServer := &app.Server{
				Host:        appHost,
				Port:        appPort,
				OTELEnabled: otelCfg.Enabled,
				Store:       keeperStore,
			}
			addr := net.JoinHostPort(appHost, fmt.Sprintf("%d", appPort))
			log.Printf("Starting application server on %s", addr)
			errors <- appServer.Start(ctx)
		}()

		// Wait for shutdown signal or error
		select {
		case <-sigChan:
			log.Println("Shutdown signal received, stopping servers...")
			cancel() // Cancel context to trigger graceful shutdown
		case err := <-errors:
			if err != nil {
				log.Printf("Server error: %v", err)
				cancel()
			}
		}

		wg.Wait()
		log.Println("Servers stopped")
	},
}

func init() {
	RootCmd.AddCommand(serveCmd)
}

// startSweeperLoop runs the sweep-on-start pass (a restart doesn't wait
// a full interval) then the tick backstop until ctx ends. Triggers are
// exactly these two plus the console POST — no queue, no backlog.
func startSweeperLoop(ctx context.Context, sw *sweep.Sweeper, interval time.Duration) {
	sw.RunPass(ctx, "startup")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sw.RunPass(ctx, "tick")
		}
	}
}
