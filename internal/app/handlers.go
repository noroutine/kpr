package app

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var (
	// apiRequestCount is every request the app server serves (status
	// page, health, receiver) — the console's request metric. The
	// template's per-mock-endpoint counting is gone with the mocks.
	apiRequestCount uint64
)

// HealthResponse is the GET /health body the front page's status ball
// reads: the process alive, the running binary's version, and whether
// the receiver's redis answers. Degraded (not dead) on redis outage,
// mirroring the degraded boot.
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Redis   string `json:"redis"`
}

// HealthHandler serves GET /health for the given receiver store. A nil
// store means the receiver is disabled; a Ping failure means redis is
// unreachable. Both stay 200: the ball reads the body, not the code.
func HealthHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := HealthResponse{Status: "ok", Version: config.Version, Redis: "disabled"}
		if st != nil {
			response.Redis = "reachable"
			if err := st.Ping(r.Context()); err != nil {
				response.Status = "degraded"
				response.Redis = "unreachable"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			log.Printf("Error encoding JSON: %v", err)
		}
	}
}

// CountRequests wraps h so every served request moves the counter the
// console reports.
func CountRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddUint64(&apiRequestCount, 1)
		h.ServeHTTP(w, r)
	})
}

// GetAPIRequestCount returns the current API request count.
func GetAPIRequestCount() uint64 {
	return atomic.LoadUint64(&apiRequestCount)
}
