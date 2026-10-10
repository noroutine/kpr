package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A lease that cannot land refuses the take: gc must hear the
// failure instead of collecting unfenced. If this fails, a gc
// collects while believing pushes are held.
func TestHoldRefusesBadDir(t *testing.T) {
	h := HoldFile{Dir: filepath.Join(t.TempDir(), "no-such-dir")}
	if _, err := h.Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Error("Hold into a missing dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "write hold lease") {
		t.Errorf("refusal = %q, want the write named", err.Error())
	}
}
