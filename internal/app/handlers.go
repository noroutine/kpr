package app

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
)

var (
	apiRequestCount uint64
)

// HelloResponse represents the response from /api/hello
type HelloResponse struct {
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Version   string    `json:"version"`
}

// DataResponse represents the response from /api/data
type DataResponse struct {
	Items     []string  `json:"items"`
	Count     int       `json:"count"`
	Timestamp time.Time `json:"timestamp"`
}

// HelloHandler handles GET /api/hello
func HelloHandler(w http.ResponseWriter, r *http.Request) {
	atomic.AddUint64(&apiRequestCount, 1)

	response := HelloResponse{
		Message:   "Hello from kpr!",
		Timestamp: time.Now(),
		Version:   config.Version,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding JSON: %v", err)
	}
}

// DataHandler handles GET /api/data
func DataHandler(w http.ResponseWriter, r *http.Request) {
	atomic.AddUint64(&apiRequestCount, 1)

	// Mock pipeline rows: the shape /api/data serves (a list with a
	// count) stays stable for the SPA callers; the content names the
	// keeper pipeline instead of the template's sample data.
	items := []string{
		"receiver tracks every push as a redis row",
		"reap marks expired rows due, with a reason",
		"sweep deletes marked manifests by digest",
		"offline GC reclaims the orphaned blobs",
		"console and Quickwit show what the sweeper did",
	}

	response := DataResponse{
		Items:     items,
		Count:     len(items),
		Timestamp: time.Now(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding JSON: %v", err)
	}
}

// GetAPIRequestCount returns the current API request count
func GetAPIRequestCount() uint64 {
	return atomic.LoadUint64(&apiRequestCount)
}
