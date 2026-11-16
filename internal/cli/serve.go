package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
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
	"nrtn.dev/catalyst/kpr/internal/receiver"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/resources"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start management console, application server, receiver, and registry edge",
	Long:  `Start the kpr servers: console, application, the registry receiver (/events on its own port), and the transparent registry edge proxy. The edge opens only on a RelativeURLs proof; KPR_EDGE=false opts out, and a failed proof closes the edge loudly while the other servers keep running.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Identity first, then the grant: the version line opens the
		// boot log so a scrolled docker log names the build before
		// anything else speaks (the server goroutines below race
		// past anything logged after their launch).
		log.Printf("Starting %s", config.VersionString())
		logLicenseBanner()
		// Resolve configuration once: environment first, explicit flags
		// win (an unset flag already carries the env value as its
		// default, so layering flag values on top is exact).
		cfg := config.NewBuilder().FromEnv().
			WithManagementHost(managementHost).
			WithManagementPort(managementPort).
			WithAppHost(appHost).
			WithAppPort(appPort).
			WithReceiverHost(receiverHost).
			WithReceiverPort(receiverPort).
			Build()
		defer config.SetCurrent(cfg)()
		if !cmd.Flags().Changed("management-port") && cfg.ManagementPortWarning != nil {
			log.Printf("Warning: %v", cfg.ManagementPortWarning)
		}
		if !cmd.Flags().Changed("app-port") && cfg.AppPortWarning != nil {
			log.Printf("Warning: %v", cfg.AppPortWarning)
		}
		if !cmd.Flags().Changed("receiver-port") && cfg.ReceiverPortWarning != nil {
			log.Printf("Warning: %v", cfg.ReceiverPortWarning)
		}
		if cfg.RedisDBWarning != nil {
			log.Printf("Warning: %v", cfg.RedisDBWarning)
		}
		if cfg.TimeMethodWarning != nil {
			log.Printf("Warning: %v", cfg.TimeMethodWarning)
		}

		// Config source, dir or unconfigured: the dir wins when
		// set, and neither means the env-only tree. A broken
		// load refuses the boot — serve must never run on half
		// config.
		var loadSnapshot func() (*resources.Snapshot, error)
		source := "unconfigured"
		switch {
		case cfg.ConfigDir != "":
			source = "KPR_CONFIG_DIR=" + cfg.ConfigDir
			loadSnapshot = func() (*resources.Snapshot, error) { return resources.Load(cfg.ConfigDir) }
		default:
			loadSnapshot = func() (*resources.Snapshot, error) { return resources.Load("") }
		}
		snap, err := loadSnapshot()
		if err != nil {
			log.Fatalf("load resources: %v", err)
		}
		holder := resources.NewHolder(snap, loadSnapshot)
		log.Printf("config: %s: %d bundles", source, len(snap.Bundles))
		// Serving TLS resolves from the named bundle before
		// any bind: one identity filed once, served on every
		// listener. Empty serves plaintext; an unknown, parked,
		// or identity-less name refuses the boot — a listener
		// that names a bundle with no identity serves nothing.
		servingTLS, servingFloor, err := bundleTLSConfig(snap, cfg.TLSBundle)
		if err != nil {
			log.Fatal(err)
		}
		if servingTLS != nil {
			floor := resources.MinTLSName(servingFloor)
			if floor == "" {
				floor = "Go default"
			}
			log.Printf("tls: serving bundle %q (min TLS %s) on every listener", cfg.TLSBundle, floor)
		}
		// Console auth resolves before any bind: "true" without
		// a token, and any misspelling, refuses the boot —
		// opting into operator access must never land open.
		consoleAuth, err := cfg.ConsoleAuthEnabled()
		if err != nil {
			log.Fatal(err)
		}

		// Pprof is a debug listener, off unless asked: it can
		// dump heap contents, so anything but loopback refuses
		// the boot, and there is no flag — env only, like the
		// backend secret. See docs/PROFILING.md for the workflow.
		if cfg.PprofAddr != "" {
			if err := checkPprofAddr(cfg.PprofAddr); err != nil {
				log.Fatal(err)
			}
			ln, err := net.Listen("tcp", cfg.PprofAddr)
			if err != nil {
				log.Fatalf("pprof listen %s: %v (free the port or unset %s)", cfg.PprofAddr, err, config.EnvPprofAddr)
			}
			// No graceful stop and deliberately not in wg: the
			// process exit takes it (http.Serve never returns,
			// so waiting on it would hang shutdown).
			go func() {
				log.Printf("Starting pprof on %s", cfg.PprofAddr)
				if err := http.Serve(ln, nil); err != nil {
					log.Printf("pprof: %v", err)
				}
			}()
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
			gate, h, gerr := assembleEdge(cfg, backend, keeperStore, cfg.RegistryConfig, func(e event.Event) {
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

		// Start management console server, unless switched off.
		if cfg.ConsoleEnabled {
			wg.Add(1)
			go func() {
				defer wg.Done()
				managementServer := &console.Server{
					Host:        managementHost,
					Port:        managementPort,
					OTELEnabled: otelCfg.Enabled,
					TLSConfig:   servingTLS,
					Token:       cfg.ConsoleToken,
					AuthEnabled: consoleAuth,
					Resources:   holder,
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
		} else {
			log.Print("Management console disabled by KPR_MANAGEMENT_ENABLED=false")
		}

		// Start application server, unless switched off.
		if cfg.AppEnabled {
			wg.Add(1)
			go func() {
				defer wg.Done()
				appServer := &app.Server{
					Host:        appHost,
					Port:        appPort,
					OTELEnabled: otelCfg.Enabled,
					TLSConfig:   servingTLS,
					Store:       keeperStore,
				}
				addr := net.JoinHostPort(appHost, fmt.Sprintf("%d", appPort))
				log.Printf("Starting application server on %s", addr)
				errors <- appServer.Start(ctx)
			}()
		} else {
			log.Print("Application server disabled by KPR_APP_ENABLED=false")
		}

		// Start registry receiver: /events on its own port, so
		// topologies that must not expose registry coupling alongside
		// user- or management-facing ports simply do not publish it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			receiverServer := &receiver.Server{
				Host:        receiverHost,
				Port:        receiverPort,
				OTELEnabled: otelCfg.Enabled,
				TLSConfig:   servingTLS,
				Store:       keeperStore,
			}
			addr := net.JoinHostPort(receiverHost, fmt.Sprintf("%d", receiverPort))
			log.Printf("Starting registry receiver on %s", addr)
			errors <- receiverServer.Start(ctx)
		}()

		// Start edge proxy (only when proven above)
		if edgeHandler != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv := &http.Server{Addr: cfg.EdgeAddr, Handler: edgeHandler, TLSConfig: servingTLS}
				go func() {
					<-ctx.Done()
					_ = srv.Close()
				}()
				log.Printf("edge on %s -> %s", cfg.EdgeAddr, cfg.RegistryURL)
				serveEdge := srv.ListenAndServe
				if servingTLS != nil {
					serveEdge = func() error { return srv.ListenAndServeTLS("", "") }
				}
				if err := serveEdge(); err != nil && err != http.ErrServerClosed {
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
	// NOTE(mutants): >= is equivalent — staleness at the exact
	// age boundary is unhittable outside the clock (age lands
	// exactly on the constant only by measure).
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

// bundleTLSConfig resolves the serving TLS from the named
// bundle: its materialized cert plus its resolved floor.
// Empty names plaintext. Unknown names, parked bundles, and
// bundles with no identity refuse — a listener that names a
// bundle with no identity serves nothing. Pure so the matrix
// is testable without binding.
func bundleTLSConfig(snap *resources.Snapshot, name string) (*tls.Config, uint16, error) {
	if name == "" {
		return nil, 0, nil
	}
	b, ok := snap.Bundles[name]
	if !ok {
		var have []string
		for _, e := range resources.SortedEntries(snap.Bundles) {
			have = append(have, e.Name)
		}
		return nil, 0, fmt.Errorf("KPR_TLS_BUNDLE %q: no such bundle (have %v)", name, have)
	}
	if !resources.Enabled(b.Spec.Enabled) {
		return nil, 0, fmt.Errorf("KPR_TLS_BUNDLE %q: bundle is parked", name)
	}
	cert, ok := snap.Certs[name]
	if !ok {
		return nil, 0, fmt.Errorf("KPR_TLS_BUNDLE %q: bundle has no identity (trust-only)", name)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: snap.Floors[name]}, snap.Floors[name], nil
}

// checkPprofAddr validates an explicitly requested pprof bind: only
// a loopback host:port is accepted, everything else (including an
// unparseable address) refuses the boot. Pure so the refusal matrix
// is testable without booting servers.
func checkPprofAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("%s %q: refusing non-loopback pprof bind (heap contents must not leave this machine)", config.EnvPprofAddr, addr)
	}
	return nil
}
