package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// status is the high-level posture, one line per section: the
// detail lives behind each status card. If this fails,
// operators cannot tell a healthy keeper from a blind one.
func TestStatusShowsSectionPostures(t *testing.T) {
	now := time.Now().UTC()
	st := keeper.Status{StoreOK: true, Tracked: 2, Due: 1,
		Plan: []keeper.PlanEntry{{Repo: "app", Tag: "v1"}},
		Activity: []keeper.ActivityEntry{{Repo: "app", Tag: "v1",
			Reason: "ttl:10m elapsed", Outcome: "deleted", At: now.Add(-time.Hour)}},
	}
	var out bytes.Buffer
	err := runStatus(&out,
		keeper.RegistryCard{Standing: keeper.StandingOK, URL: "http://registry:5000", Reachable: true},
		keeper.StoreCard{Standing: keeper.StandingLocked, Name: "file", Detail: "/var/lib/kpr", Healthy: true, LockNote: "locked", Identity: "019-abc"},
		st,
		keeper.EdgeCard{Standing: keeper.StandingDenied, Open: true, Deny: true},
		keeper.ClockCard{Standing: keeper.StandingOK, Method: "https", Server: "ts", Skew: -time.Second},
		keeper.ObsCard{Standing: keeper.StandingOK, Links: []keeper.ObsLink{{Name: "Grafana"}}},
	)
	if err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	for _, want := range []string{
		"registry: reachable (http://registry:5000)",
		"store: locked — writes denied, reads continue (file)",
		"edge: open, deny",
		"clock: -1s via https",
		"plan: 1 due",
		"activity: 1 recorded, last deleted",
		"observability: 1 backend",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
	// No section duplicates another's detail: counters, proof,
	// and ages live behind the cards, not here.
	for _, gone := range []string{"tracked:", "proof:", "ago", "armed", "dry-run", "sweeper:"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("status leaks %q (detail the cards own):\n%s", gone, out.String())
		}
	}
}

// Two backends read plural: the aggregate counts links, it
// never hardcodes the singular. If this fails, the second
// backend reads "2 backend".
func TestStatusCountsPluralBackends(t *testing.T) {
	var out bytes.Buffer
	err := runStatus(&out,
		keeper.RegistryCard{Standing: keeper.StandingOK, Reachable: true},
		keeper.StoreCard{Standing: keeper.StandingOK, Name: "file"},
		keeper.Status{StoreOK: true},
		keeper.EdgeCard{},
		keeper.ClockCard{Standing: keeper.StandingUnchecked, Method: "local"},
		keeper.ObsCard{Standing: keeper.StandingOK, Links: []keeper.ObsLink{{Name: "a"}, {Name: "b"}}},
	)
	if err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if want := "observability: 2 backends"; !strings.Contains(out.String(), want) {
		t.Errorf("status missing %q:\n%s", want, out.String())
	}
}

// A clock with no reading names its state, never a doubled
// word: unreachable and unchecked voice state plus transport,
// a real skew voices with its bound behind it. If this fails,
// the clock line stutters.
func TestStatusClockLineHasNoReadingWithoutOne(t *testing.T) {
	st := keeper.Status{StoreOK: true}
	for _, tc := range []struct {
		name string
		card keeper.ClockCard
		want string
	}{
		{"unreachable", keeper.ClockCard{Standing: keeper.StandingUnreachable, Method: "https"}, "clock: unreachable (https)"},
		{"unchecked", keeper.ClockCard{Standing: keeper.StandingUnchecked, Method: "local"}, "clock: unchecked (local)"},
		{"degraded", keeper.ClockCard{Standing: keeper.StandingDegraded, Method: "https", Skew: 61 * time.Second}, "clock: degraded: +1m1s via https"},
	} {
		var out bytes.Buffer
		err := runStatus(&out, keeper.RegistryCard{}, keeper.StoreCard{Standing: keeper.StandingOK},
			st, keeper.EdgeCard{}, tc.card, keeper.ObsCard{})
		if err != nil {
			t.Fatalf("%s: runStatus: %v", tc.name, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s missing %q:\n%s", tc.name, tc.want, out.String())
		}
	}
}

// A down registry reddens its line but still reports: status is a
// reader, not a health gate. A dead store, in contrast, fails
// fast — without state every number would be a lie. If this
// fails, status either hides a down registry or invents numbers
// with no backend.
func TestStatusDegradesAndFailsFast(t *testing.T) {
	st := keeper.Status{StoreOK: true}
	reg := keeper.RegistryCard{Standing: keeper.StandingUnreachable, URL: "http://127.0.0.1:1"}
	var out bytes.Buffer
	if err := runStatus(&out, reg, keeper.StoreCard{Standing: keeper.StandingOK},
		st, keeper.EdgeCard{}, keeper.ClockCard{}, keeper.ObsCard{}); err != nil {
		t.Fatalf("registry down must not fail status: %v", err)
	}
	if !strings.Contains(out.String(), "registry: unreachable") {
		t.Errorf("status hides dead registry:\n%s", out.String())
	}
	down := keeper.Status{StoreOK: false}
	if err := runStatus(io.Discard, reg,
		keeper.StoreCard{Standing: keeper.StandingUnreachable, Name: "redis"},
		down, keeper.EdgeCard{}, keeper.ClockCard{}, keeper.ObsCard{}); err == nil {
		t.Error("dead store succeeded, want a fast clear error")
	} else if !strings.Contains(err.Error(), "redis") {
		t.Errorf("error = %q, want it to name the store", err.Error())
	}
}

// status without a probed registry still reports: no prober
// reads unavailable, never a missing line. If this fails, a
// missing registry panics the status path instead of voicing
// it.
func TestStatusNilRegistry(t *testing.T) {
	st := keeper.Status{StoreOK: true}
	reg := keeper.FetchRegistryCard(context.Background(), nil, "")
	var out bytes.Buffer
	if err := runStatus(&out, reg, keeper.StoreCard{Standing: keeper.StandingOK},
		st, keeper.EdgeCard{}, keeper.ClockCard{}, keeper.ObsCard{}); err != nil {
		t.Fatalf("runStatus with no registry: %v", err)
	}
	if !strings.Contains(out.String(), "registry: unavailable") {
		t.Errorf("status hides missing registry:\n%s", out.String())
	}
}

// An unreadable proof voices unproven, and an undated generation
// voices its name without an age: presence without timing is still
// presence. If this fails, outages read as proofs (or proofs hide).
func TestProofVoicesUnproven(t *testing.T) {
	if got := voiceProof(keeper.FetchProofCard(cliCtx(), nil)); got != "unproven" {
		t.Errorf("voiceProof(nil) = %q, want unproven", got)
	}
	broken := stubProofAPI{err: errors.New("connection refused")}
	if got := voiceProof(keeper.FetchProofCard(cliCtx(), broken)); got != "unproven" {
		t.Errorf("voiceProof(outage) = %q, want unproven", got)
	}
	undated := stubProofAPI{ts: "not-a-time", id: "id-1"}
	if got := voiceProof(keeper.FetchProofCard(cliCtx(), undated)); got != "019-proof" {
		t.Errorf("voiceProof(undated) = %q, want the gen without age", got)
	}
}

// status with a stray arg refuses: an unknown card name must read
// as an error, never the banner. This pins the arg validator
// cobra runs before RunE (executing the shared command here
// would re-root onto Root with the test binary's own args, so
// the validator — the unit this owns — is asserted directly).
// If this fails, typos silently report numbers.
func TestStatusRefusesUnknownCard(t *testing.T) {
	if err := statusCmd.ValidateArgs([]string{"bullshit"}); err == nil {
		t.Error("status bullshit validated, want unknown-command refusal")
	} else if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want unknown command", err.Error())
	}
}

// Every renderer surfaces a failed write instead of swallowing
// it: each assembles its card then writes once, so a broken
// pipe errors with no partial card printed as success. One
// pass each with the shared errWriter pins the single write.
// If this fails, a renderer eats write errors.
func TestStatusRenderersSurfaceWriteErrors(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := store.NewMemStore()
	st := keeper.Status{StoreOK: true}
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"aggregate", func() error {
			return runStatus(errWriter{},
				keeper.RegistryCard{Standing: keeper.StandingOK, Reachable: true},
				keeper.StoreCard{Standing: keeper.StandingOK}, st,
				keeper.EdgeCard{}, keeper.ClockCard{}, keeper.ObsCard{})
		}},
		{"store", func() error {
			return runStatusStore(ctx, errWriter{}, s, "", 0, nil, false)
		}},
		{"edge", func() error {
			return renderEdgeCard(errWriter{},
				keeper.EdgeCard{Standing: keeper.StandingOK, Open: true}, false)
		}},
		{"registry", func() error {
			return renderRegistryCard(errWriter{},
				keeper.RegistryCard{Standing: keeper.StandingOK, Reachable: true}, false)
		}},
		{"activity", func() error {
			return renderActivityCard(errWriter{},
				keeper.FetchActivityCard(ctx, s), now, false)
		}},
		{"observability", func() error {
			return renderObsCard(errWriter{},
				keeper.ObsCard{Standing: keeper.StandingOK,
					Links: []keeper.ObsLink{{Name: "Grafana"}}}, false)
		}},
	} {
		if err := tc.run(); err == nil {
			t.Errorf("%s swallowed a write error", tc.name)
		}
	}
}

// Plan stays off status: `kpr plan` owns the human view, the
// console list reads Status.Plan — a status plan twin would
// duplicate both for no new fact (dropped as garbage). If this
// fails, the twin crept back.
func TestStatusHasNoPlanTwin(t *testing.T) {
	for _, sub := range statusCmd.Commands() {
		if sub.Name() == "plan" {
			t.Errorf("status carries a %q subcommand, want plan left to `kpr plan`", sub.Name())
		}
	}
}
