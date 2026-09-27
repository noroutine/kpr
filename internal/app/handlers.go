package app

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"
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
		Version:   "1.0.0",
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding JSON: %v", err)
	}
}

// DataHandler handles GET /api/data
func DataHandler(w http.ResponseWriter, r *http.Request) {
	atomic.AddUint64(&apiRequestCount, 1)

	items := []string{
		"Item 1: Example data",
		"Item 2: More sample data",
		"Item 3: Blueprint structure",
		"Item 4: Embedded assets",
		"Item 5: API endpoints",
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
