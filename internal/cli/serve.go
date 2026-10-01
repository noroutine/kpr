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
	"nrtn.dev/catalyst/kpr/internal/sentinel"
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
		if cfg.RedisDBWarning != nil {
			log.Printf("Warning: %v", cfg.RedisDBWarning)
		}
		if cfg.TimeMethodWarning != nil {
			log.Printf("Warning: %v", cfg.TimeMethodWarning)
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

		// Shared keeper state (lazy — a down backend degrades
		// banner/receiver/sweeper instead of blocking boot), the registry
		// client, and the single-owner sweeper. Backend derives like
		// every command; a conflict refuses boot (guessing state wrong
		// is worse than not booting).
		backend, storeDir, err := resolveStoreBackend()
		if err != nil {
			log.Fatalf("state backend: %v", err)
		}
		keeperStore := buildStore(backend, storeDir, cfg)
		defer func() { _ = keeperStore.Close() }()
		if perr := keeperStore.Ping(ctx); perr != nil {
			log.Printf("Warning: state backend unreachable, keeper sections degrade: %v", perr)
		}
		regClient := registry.NewClient(cfg.RegistryURL)
		// Informational only, never a gate: async so a slow/down
		// registry can't delay the servers coming up.
		go logProofAge(ctx, regClient)
		sweeper := &sweep.Sweeper{
			Store:    keeperStore,
			Registry: regClient,
			Sentinel: regClient,
			DryRun:   !cfg.SweeperNoDryRun,
		}
		if cfg.SweeperNoDryRun {
			log.Printf("Sweeper armed: deletes are real")
		} else {
			log.Printf("Sweeper dry-run: deletes only planned (arm with KPR_SWEEPER_NO_DRY_RUN=true)")
		}
		// AirPlay preflight: macOS Receiver squats localhost:5000 with
		// Server: AirTunes, and only a positive fingerprint warns —
		// unreachable or remote peers stay silent.
		warnIfAirPlaySquats(cfg)

		var wg sync.WaitGroup
		errors := make(chan error, 3)

		// Sweeper loop: sweep-on-start (a restart doesn't wait a full
		// interval) plus the tick backstop.
		wg.Add(1)
		go func() {
			defer wg.Done()
			startSweeperLoop(ctx, sweeper, sweep.TickInterval, regClient)
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
				// Same client speaks sentinel.API (GetManifest +
				// GetBlob): the proof card reads the live
				// generation through it.
				Sentinel: regClient,
				Sweeper:  sweeper,
				Armed:    cfg.SweeperNoDryRun,
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

// logProofAge voices which column this serve lives in: a fresh
// generation means something with the store proved recently (blob
// reclamation handled); none served means tag lifecycle only —
// sweeps delete names, layers still need a volume-side gc. Loud at
// boot, never fatal: staleness degrades, it doesn't gate.
func logProofAge(ctx context.Context, api sentinel.API) {
	if api == nil {
		return
	}
	current, note := sweepProofFresh(ctx, api)
	if current {
		log.Printf("Same-store proof: %s", note)
		return
	}
	log.Printf("Warning: same-store proof %s", note)
}

// startSweeperLoop runs the sweep-on-start pass (a restart doesn't wait
// a full interval) then the tick backstop until ctx ends. Triggers are
// exactly these two plus the console POST — no queue, no backlog.
// The startup pass always voices the proof age; ticks only on
// fresh↔stale transitions, so a steady state costs one registry
// read per tick and zero log lines. Nil api skips the proof lines
// (tests, exotic wiring) without touching the passes.
func startSweeperLoop(ctx context.Context, sw *sweep.Sweeper, interval time.Duration, api sentinel.API) {
	fresh := logSweepProof(ctx, api, true, true)
	sw.RunPass(ctx, "startup")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fresh = logSweepProof(ctx, api, fresh, false)
			sw.RunPass(ctx, "tick")
		}
	}
}

// logSweepProof reads the live generation and voices whether the
// store counts as recently proven, returning the current freshness.
// Always logs when forced (startup); otherwise only on fresh↔stale
// transitions, so a steady state costs one registry read per tick
// and zero log lines. A read failure counts as stale (nothing
// demonstrated); nil api skips the lines entirely.
func logSweepProof(ctx context.Context, api sentinel.API, prev, force bool) bool {
	if api == nil {
		return true
	}
	current, note := sweepProofFresh(ctx, api)
	if force || current != prev {
		log.Printf("Sweep: same-store proof %s", note)
	}
	return current
}

// sweepProofFresh reads the live generation once and reports whether
// the store counts as recently proven, plus the log fragment voicing
// it. A read failure counts as stale (nothing demonstrated) —
// callers never distinguish "unproven" from "unreadable" for gating,
// because neither unlocks anything.
func sweepProofFresh(ctx context.Context, api sentinel.API) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := sentinel.LastProof(ctx, api)
	if err != nil {
		return false, "unserved — tag lifecycle only, layers accumulate until a volume-side gc"
	}
	age, err := p.Age(time.Now().UTC())
	if err != nil {
		return false, fmt.Sprintf("generation %s served but its timestamp is unreadable", p.Gen)
	}
	if age > sentinel.ProofStaleAfter {
		return false, fmt.Sprintf("generation %s %s old, stale — layers accumulate until a volume-side gc", p.Gen, age.Round(time.Second))
	}
	return true, fmt.Sprintf("generation %s %s old, fresh", p.Gen, age.Round(time.Second))
}
