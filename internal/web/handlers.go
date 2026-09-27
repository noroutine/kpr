package web

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"nrtn.dev/catalyst/kpr/internal/app"
)

var (
	// Version is set via ldflags at build time
	Version = "dev"
	// Commit is set via ldflags at build time
	Commit = "unknown"
	// BuildTime is set via ldflags at build time
	BuildTime = "unknown"

	// Metrics
	startTime = time.Now()
)

type pageData struct {
	Hostname       string
	Version        string
	Commit         string
	BuildTime      string
	Uptime         string
	Requests       uint64
	GoVersion      string
	ManagementHost string
	ManagementPort string
	AppHost        string
	AppPort        string
}

type metricsData struct {
	Uptime        string            `json:"uptime"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Requests      uint64            `json:"requests"`
	GoVersion     string            `json:"go_version"`
	Version       string            `json:"version"`
	Commit        string            `json:"commit"`
	BuildTime     string            `json:"build_time"`
	Environment   map[string]string `json:"environment"`
}

// IndexHandler serves the main management console dashboard
func IndexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	hostname, _ := os.Hostname()
	uptime := time.Since(startTime).Round(time.Second)

	// Get default values if env vars not set
	managementHost := os.Getenv("KPR_MANAGEMENT_HOST")
	if managementHost == "" {
		managementHost = "::"
	}
	managementPort := os.Getenv("KPR_MANAGEMENT_PORT")
	if managementPort == "" {
		managementPort = "9300"
	}
	appHost := os.Getenv("KPR_APP_HOST")
	if appHost == "" {
		appHost = "::"
	}
	appPort := os.Getenv("KPR_APP_PORT")
	if appPort == "" {
		appPort = "8080"
	}

	data := pageData{
		Hostname:       hostname,
		Version:        Version,
		Commit:         Commit,
		BuildTime:      BuildTime,
		Uptime:         uptime.String(),
		Requests:       app.GetAPIRequestCount(),
		GoVersion:      runtime.Version(),
		ManagementHost: managementHost,
		ManagementPort: managementPort,
		AppHost:        appHost,
		AppPort:        appPort,
	}

	tmpl, err := template.New("index").Parse(indexTemplate)
	if err != nil {
		log.Printf("Error parsing template: %v", err)
		http.Error(w, "Template error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Error executing template: %v", err)
	}
}

// MetricsHandler returns JSON metrics
func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	uptime := time.Since(startTime)

	env := map[string]string{
		"HOSTNAME": hostname,
		"PORT":     os.Getenv("KPR_PORT"),
		"HOST":     os.Getenv("KPR_HOST"),
	}

	metrics := metricsData{
		Uptime:        uptime.Round(time.Second).String(),
		UptimeSeconds: int64(uptime.Seconds()),
		Requests:      app.GetAPIRequestCount(),
		GoVersion:     runtime.Version(),
		Version:       Version,
		Commit:        Commit,
		BuildTime:     BuildTime,
		Environment:   env,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metrics); err != nil {
		log.Printf("Error encoding JSON: %v", err)
	}
}

// HealthHandler returns 200 OK if service is healthy
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("OK\n")); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}
