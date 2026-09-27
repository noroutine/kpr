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

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/app"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/otel"
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

		var wg sync.WaitGroup
		errors := make(chan error, 2)

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
			}
			addr := net.JoinHostPort(managementHost, fmt.Sprintf("%d", managementPort))
			log.Printf("Starting management console on %s", addr)
			if err := managementServer.Start(ctx); err != nil {
				errors <- err
			}
		}()

		// Start application server
		wg.Add(1)
		go func() {
			defer wg.Done()
			appServer := &app.Server{
				Host:        appHost,
				Port:        appPort,
				OTELEnabled: otelCfg.Enabled,
			}
			addr := net.JoinHostPort(appHost, fmt.Sprintf("%d", appPort))
			log.Printf("Starting application server on %s", addr)
			if err := appServer.Start(ctx); err != nil {
				errors <- err
			}
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
