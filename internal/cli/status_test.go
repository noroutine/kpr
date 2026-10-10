package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// status is the ssh-and-scripts banner: redis and registry reachability
// plus counters from tracked state. If this fails, operators cannot
// tell a healthy keeper from a blind one.
func TestStatusRendersBannerAndCounters(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), liveRegistryClient(t)); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	// Exact line: "unreachable" contains "reachable", so a bare
	// substring check would pass on a red banner.
	for _, want := range []string{"registry: reachable\n", "tracked: 2", "due: 1", "performed: 1", "planned: 1", "failed: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
	// Store facts live under `store status` now — the banner must
	// not duplicate them.
	for _, gone := range []string{"store-lock:", "proof:", "identity:"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("status leaks %q, owned by store status:\n%s", gone, out.String())
		}
	}
	// Arming is per-invocation, not a posture: status owns none of
	// it, so it advertises neither armed nor dry-run. If this
	// fails, the dead serve loop's wording crept back into a banner
	// that reports on nothing armable.
	for _, gone := range []string{"armed", "dry-run", "sweeper:"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("status advertises %q (arming the banner doesn't own):\n%s", gone, out.String())
		}
	}
	if strings.Contains(out.String(), "redis: reachable") {
		t.Errorf("status names redis on a mem store:\n%s", out.String())
	}
}

// A down registry reddens the banner but still reports: status is a
// reader, not a health gate. Redis down, in contrast, fails fast —
// without state every number would be a lie. If this fails, status
// either hides a down registry or invents numbers with no backend.
func TestStatusDegradesAndFailsFast(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), registry.NewClient("http://127.0.0.1:1")); err != nil {
		t.Fatalf("registry down must not fail status: %v", err)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("status hides dead registry:\n%s", out.String())
	}
	if err := runStatus(cliCtx(), io.Discard, deadStore{}, liveRegistryClient(t)); err == nil {
		t.Error("redis down succeeded, want a fast clear error")
	} else if !strings.Contains(err.Error(), "redis") {
		t.Errorf("error = %q, want it to name redis", err.Error())
	}
}

// status without a registry client still reports (red banner), since
// the probe is optional — only state is mandatory. If this fails, a
// nil client panics the status path instead of reddening it.
func TestStatusNilRegistry(t *testing.T) {
	var out bytes.Buffer
	if err := runStatus(cliCtx(), &out, cliStore(), nil); err != nil {
		t.Fatalf("runStatus with nil registry: %v", err)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("status hides missing registry:\n%s", out.String())
	}
}

// An unreadable marker voices unknown, never locked: the operator
// must not revoke on a read error. If this fails, an outage reads
// as intent.
func TestLockStateUnknownOnOutage(t *testing.T) {
	if got := lockState(cliCtx(), deadStore{}); got != "unknown" {
		t.Errorf("lockState on outage = %q, want unknown", got)
	}
	if got := lockState(cliCtx(), store.NewMemStore()); got != "locked" {
		t.Errorf("fresh lockState = %q, want locked (born locked)", got)
	}
}

// An unreadable proof voices unproven, and an undated generation
// voices its name without an age: presence without timing is still
// presence. If this fails, outages read as proofs (or proofs hide).
func TestProofStateVoicesUnproven(t *testing.T) {
	if got := proofState(cliCtx(), nil); got != "unproven" {
		t.Errorf("proofState(nil) = %q, want unproven", got)
	}
	broken := stubProofAPI{err: errors.New("connection refused")}
	if got := proofState(cliCtx(), broken); got != "unproven" {
		t.Errorf("proofState(outage) = %q, want unproven", got)
	}
	undated := stubProofAPI{ts: "not-a-time", id: "id-1"}
	if got := proofState(cliCtx(), undated); got != "019-proof" {
		t.Errorf("proofState(undated) = %q, want the gen without age", got)
	}
}

// describeStore voices the backend in one line: file with its dir,
// redis with addr and DB. If this fails, the store card names the
// wrong backend — the operator fixes the wrong state.
func TestDescribeStoreNamesBackend(t *testing.T) {
	cfg := config.NewBuilder().WithRedisAddr("r:6379").WithRedisDB(4).Build()
	if got := describeStore(store.NewMemStore(), cfg); got != "redis (r:6379 db 4)" {
		t.Errorf("mem store described as %q, want the redis line", got)
	}
	fs := store.NewFileStore(t.TempDir())
	if got := describeStore(fs, cfg); got != "file ("+fs.Dir()+")" {
		t.Errorf("file store described as %q, want file with dir", got)
	}
}
