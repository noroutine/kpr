package gc

import (
	"errors"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/fence"
)

// The file backend wires the lease dir through the injected
// factory, silently: the edge reads what gc writes. The
// factory seam is where the adapter package stays out of the
// use case — gc decides, edge builds. If this fails, the lease
// stopped reaching the edge, or gc started naming the adapter.
func TestFenceForBackendWiresFileStore(t *testing.T) {
	var out strings.Builder
	var made string
	var events []string
	fenced := FenceForBackend("file", "/state", func(dir string) fence.Controller {
		made = dir
		return stubFencer{events: &events}
	}, nil, false, &out)
	if fenced == nil {
		t.Fatal("file backend fence = nil, want a controller")
	}
	if made != "/state" {
		t.Errorf("factory dir = %q, want the shared store dir", made)
	}
	if out.Len() != 0 {
		t.Errorf("wired fence warned %q, want silence", out.String())
	}
}

// Anything else runs unfenced with the warning said out loud on
// armed runs — and silent on previews, where nothing holds. The
// factory must never fire there. If this fails, unfenced
// collects went quiet or previews warned for no reason.
func TestFenceForBackendWarnsWhenUnshared(t *testing.T) {
	unbuilt := func(string) fence.Controller {
		t.Error("factory fired on an unfenced path, want no adapter")
		return nil
	}
	var armed strings.Builder
	if fenced := FenceForBackend("redis", "", unbuilt, nil, false, &armed); fenced != nil {
		t.Errorf("redis backend fence = %v, want nil", fenced)
	}
	if !strings.Contains(armed.String(), "unfenced") {
		t.Errorf("armed unfenced warning = %q, want it said", armed.String())
	}

	var preview strings.Builder
	if fenced := FenceForBackend("redis", "", unbuilt, nil, true, &preview); fenced != nil {
		t.Errorf("preview fence = %v, want nil", fenced)
	}
	if preview.Len() != 0 {
		t.Errorf("preview warned %q, want silence", preview.String())
	}

	var broken strings.Builder
	if fenced := FenceForBackend("", "", unbuilt, errors.New("boom"), false, &broken); fenced != nil {
		t.Errorf("broken backend fence = %v, want nil", fenced)
	}
	if !strings.Contains(broken.String(), "unfenced") {
		t.Errorf("broken backend warning = %q, want it said", broken.String())
	}
}
