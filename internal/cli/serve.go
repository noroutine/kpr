package cli

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/app"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/console"
	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/fence"
	"nrtn.dev/catalyst/kpr/internal/otel"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start management console, application server, and registry edge",
	Long:  `Start the kpr servers: console, application, and the transparent registry edge proxy. The edge opens only on a RelativeURLs proof; KPR_EDGE=false opts out, and a failed proof closes the edge loudly while the other servers keep running.`,
	Run: func(cmd *cobra.Command, args []string) {
		logLicenseBanner()
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
		// Services keep the text leg: docker logs stay readable.
		// CLI one-shots never call this — their stdout belongs
		// to the live UI.
		otel.AttachStdout()
		defer func() {
			if err := shutdown(context.Background()); err != nil {
				log.Printf("Error shutting down OpenTelemetry: %v", err)
			}
		}()

		// Setup graceful shutdown context
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Shared keeper state (lazy — a down backend degrades
		// banner/receiver instead of blocking boot) and the registry
		// client. Backend derives like every command; a conflict
		// refuses boot (guessing state wrong is worse than not
		// booting).
		backend, storeDir, err := deps.ResolveStoreBackend()
		if err != nil {
			log.Fatalf("state backend: %v", err)
		}
		keeperStore := deps.BuildStore(backend, storeDir, cfg)
		defer func() { _ = keeperStore.Close() }()
		if perr := keeperStore.Ping(ctx); perr != nil {
			log.Printf("Warning: state backend unreachable, keeper sections degrade: %v", perr)
		}
		regClient := registry.NewClient(cfg.RegistryURL)
		// Informational only, never a gate: async so a slow/down
		// registry can't delay the servers coming up. Joined before
		// return: a one-shot probe must not outlive the command and
		// write to a logger it no longer owns.
		probeDone := make(chan struct{})
		go func() {
			defer close(probeDone)
			logProofAge(ctx, regClient)
		}()
		// No sweeper here: serve serves endpoints (receiver, console,
		// edge), it never sweeps. Passes run in the CLI.
		// AirPlay preflight: macOS Receiver squats localhost:5000 with
		// Server: AirTunes, and only a positive fingerprint warns —
		// unreachable or remote peers stay silent.
		warnIfAirPlaySquats(cfg)

		// The edge rides inside serve, not as its own command: the
		// switch selects intent, the RelativeURLs proof selects
		// safety, and a closed edge is a loud line — never a boot
		// refusal for the servers below.
		var edgeGate *fence.Gate
		var edgeHandler http.Handler
		if cfg.EdgeEnabled {
			gate, h, gerr := assembleEdge(cfg, backend, storeDir, keeperStore, cfg.RegistryConfig, func(e event.Event) {
				log.Printf("edge fence: %s %s", e.Stage, e.Message)
			})
			if gerr != nil {
				log.Printf("edge closed: no RelativeURLs proof (%v) — pushes bypass the fence", gerr)
			} else {
				edgeGate, edgeHandler = gate, h
			}
		} else {
			log.Printf("edge disabled (KPR_EDGE=false): pushes bypass the fence")
		}

		var wg sync.WaitGroup
		errors := make(chan error, 4)

		// No sweeper here at all: kpr is not a scheduler, and the
		// sweeper lives in the CLI. Passes run only when asked
		// (`kpr sweep`).

		// Setup signal handling
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

		// Start management console server
		wg.Add(1)
		go func() {
			defer wg.Done()
			managementServer := &console.Server{
				Host:        managementHost,
				Port:        managementPort,
				OTELEnabled: otelCfg.Enabled,
				Store:       keeperStore,
				Registry:    regClient,
				// Same client speaks sentinel.API (GetManifest +
				// GetBlob): the proof card reads the live
				// generation through it.
				Sentinel: regClient,
				// Nil when the edge is disabled or unproven: the
				// console renders it closed, never 500.
				Edge: edgeGate,
				// Endpoints the cards name (edge forwards
				// EdgeAddr → RegistryURL).
				RegistryURL: cfg.RegistryURL,
				EdgeAddr:    cfg.EdgeAddr,
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

		// Start edge proxy (only when proven above)
		if edgeHandler != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv := &http.Server{Addr: cfg.EdgeAddr, Handler: edgeHandler}
				go func() {
					<-ctx.Done()
					_ = srv.Close()
				}()
				log.Printf("edge on %s -> %s", cfg.EdgeAddr, cfg.RegistryURL)
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					errors <- err
				} else {
					errors <- nil
				}
			}()
		}

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
		<-probeDone
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
