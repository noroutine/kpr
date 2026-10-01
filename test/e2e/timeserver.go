//go:build e2e

package e2e

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stageTimeServer serves live local time over HTTPS: the hermetic
// clock for e2e. Tests exercise the real HTTPS transport code
// against it — never a third-party host, whose outage or skew
// would fail proofs unrelated to the code under test.
func stageTimeServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
