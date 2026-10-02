package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// A missing registry config refuses before anything listens: no
// proof, no edge. If this fails, the edge stopped gating on its
// proof.
func TestOpenEdgeRefusesWithoutConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yml")
	h, err := openEdge("http://127.0.0.1:9", missing, nil)
	if h != nil {
		t.Error("openEdge(missing) built a handler, want nothing")
	}
	if err == nil {
		t.Error("openEdge(missing) error = nil, want the read error")
	}
}

// An absolute-config registry refuses: bypass must not compile
// into a listener. If this fails, the edge opened on a bypass.
func TestOpenEdgeRefusesAbsoluteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("http:\n  addr: :5000\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	h, err := openEdge("http://127.0.0.1:9", path, nil)
	if h != nil {
		t.Error("openEdge(absolute) built a handler, want nothing")
	}
	if !errors.Is(err, proof.ErrRelativeURLsOff) {
		t.Errorf("openEdge(absolute) error = %v, want ErrRelativeURLsOff", err)
	}
}

// buildEdge relays the proof refusal: a missing config fails gate
// assembly, never a nil gate with a nil error. If this fails,
// serve boots an unfenced edge thinking it proved one.
func TestBuildEdgeRelaysProofRefusal(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yml")
	gate, h, err := buildEdge(store.NewMemStore(), "http://127.0.0.1:9", missing, "", nil)
	if err == nil {
		t.Error("buildEdge(missing) error = nil, want the read error")
	}
	if gate != nil || h != nil {
		t.Error("buildEdge(missing) built a gate, want nothing")
	}
}

// A proven config builds a handler without binding: the
// listener stays a thin tail the spike verifies live. If this
// fails, proving stopped opening.
func TestOpenEdgeBuildsWhenProven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("http:\n  relativeurls: true\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	h, err := openEdge("http://127.0.0.1:9", path, nil)
	if err != nil {
		t.Fatalf("openEdge(proven) = %v, want handler", err)
	}
	if h == nil {
		t.Fatal("openEdge(proven) built nil, want handler")
	}
}
