package app

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"

	"nrtn.dev/catalyst/kpr/internal/store"
)

var (
	// apiRequestCount is every request the app server serves (status
	// page, receiver) — the console's request metric. The
	// template's per-mock-endpoint counting is gone with the mocks.
	apiRequestCount uint64
)

// StatusResponse is the GET /api/status body: the receiver's
// expected state as the app server sees it. "ok" serves fully,
// "degraded" serves with a blind receiver (store unreachable —
// boot degrades the same way), and anything else is NOK (the
// server itself unreachable). Machines read this; the landing
// page carries no state.
type StatusResponse struct {
	Status string `json:"status"`
	Store  string `json:"store"`
}

// StatusHandler serves GET /api/status for the given receiver
// store. A nil store means the receiver is disabled; a Ping failure
// means the store is unreachable. Both stay 200: the state rides
// the body, not the code.
func StatusHandler(st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		response := StatusResponse{Status: "ok", Store: "disabled"}
		if st != nil {
			response.Store = "reachable"
			if err := st.Ping(r.Context()); err != nil {
				response.Status = "degraded"
				response.Store = "unreachable"
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
