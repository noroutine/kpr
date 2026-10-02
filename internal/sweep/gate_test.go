package sweep

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stubSentinel serves one staged generation over the read port: real
// manifest/blob bytes so Read parses honestly, or a read error for
// silence and down-registry cases. If Sweeper has no Sentinel field,
// these tests don't compile — the gate has no port.
type stubSentinel struct {
	pay sentinel.Payload
	err error
}

func (s stubSentinel) GetManifest(context.Context, string, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []byte(`{"schemaVersion":2,"config":{"digest":"sha256:stub"}}`), nil
}

func (s stubSentinel) GetBlob(context.Context, string, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return json.Marshal(s.pay)
}

func stubDeletes(f *stubRegistry) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refs)
}

// pairGround pairs a test store to a fresh served lineage and
// returns the stub serving it: delete-behavior tests sweep paired
// ground so the lineage gate (tested here, not there) stays green.
// Paired ground is unlocked ground — the intent gate (tested here,
// not there) stays green too.
func pairGround(s store.Store) stubSentinel {
	const id, gen = "test-lineage", "gen-test"
	if err := s.SetIdentity(context.Background(), store.Identity{ID: id, BaselineGen: gen}); err != nil {
		panic(err)
	}
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		panic(err)
	}
	return stubSentinel{pay: sentinel.Payload{
		V: 1, Gen: gen, ID: id,
		TS: sweepNow.Format(time.RFC3339), Writer: "kpr-gc",
	}}
}

func pairedSweeper(s store.Store, stub *stubRegistry, id, gen string, dryRun bool) *Sweeper {
	if err := s.SetIdentity(context.Background(), store.Identity{ID: id, BaselineGen: gen}); err != nil {
		panic(err)
	}
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		panic(err)
	}
	return &Sweeper{
		Store:    s,
		Registry: stub,
		Sentinel: stubSentinel{pay: sentinel.Payload{
			V: 1, Gen: gen, ID: id,
			TS: sweepNow.Format(time.RFC3339), Writer: "kpr-gc",
		}},
		DryRun: dryRun,
		Now:    func() time.Time { return sweepNow },
	}
}

// A locked store refuses before lineage: the pass is skipped, the
// failure names the marker, and the registry sees zero DELETEs —
// previews included (a pass is a run, not a read). If this fails,
// the freeze has a hole where the loop sweeps.
func TestRunPassRefusesLockedStore(t *testing.T) {
	for _, dry := range []bool{true, false} {
		s := store.NewMemStore()
		stub := &stubRegistry{}
		sw := pairedSweeper(s, stub, "local-lineage", "gen-local", dry)
		if err := s.SetUnlocked(testCtx(), false); err != nil {
			t.Fatalf("stage lock: %v", err)
		}
		if err := s.Record(testCtx(), duerow("app", "10m", time.Hour)); err != nil {
			t.Fatalf("stage due row: %v", err)
		}
		sum := sw.RunPass(testCtx(), "test")
		if !sum.Skipped {
			t.Errorf("dry=%v locked pass was not skipped", dry)
		}
		if len(sum.Failures) != 1 || !strings.Contains(sum.Failures[0], "store is locked") {
			t.Errorf("dry=%v failures = %v, want the locked refusal", dry, sum.Failures)
		}
		if got := stubDeletes(stub); got != 0 {
			t.Errorf("dry=%v locked pass attempted %d deletes", dry, got)
		}
	}
}

// A foreign lineage refuses before any delete: the pass is skipped,
// the failure names the cause and the ceremony, and the registry
// sees zero DELETEs despite due rows. If this fails, the sweeper
// serves registries mounted by mistake.
func TestRunPassRefusesForeignLineage(t *testing.T) {
	s := store.NewMemStore()
	stub := &stubRegistry{}
	sw := pairedSweeper(s, stub, "local-lineage", "gen-local", false)
	sw.Sentinel = stubSentinel{pay: sentinel.Payload{
		V: 1, Gen: "gen-foreign", ID: "foreign-lineage",
		TS: sweepNow.Format(time.RFC3339),
	}}
	if err := s.Record(testCtx(), duerow("app", "10m", time.Hour)); err != nil {
		t.Fatalf("stage due row: %v", err)
	}
	sum := sw.RunPass(testCtx(), "test")
	if !sum.Skipped {
		t.Error("pass over a foreign lineage was not skipped")
	}
	if len(sum.Failures) != 1 || !strings.Contains(sum.Failures[0], "foreign lineage") {
		t.Errorf("failures = %v, want the foreign-lineage refusal", sum.Failures)
	} else if !strings.Contains(sum.Failures[0], "kpr store adopt") {
		t.Errorf("refusal names no ceremony: %v", sum.Failures[0])
	}
	if got := stubDeletes(stub); got != 0 {
		t.Errorf("refused pass attempted %d deletes", got)
	}
}

// The refusal is narrated where Quickwit indexes it: the pass
// summary carries the cause in failures, not just the Go value.
// If this fails, operators see a skip with no reason.
func TestRunPassRefusalLogsFailures(t *testing.T) {
	s := store.NewMemStore()
	stub := &stubRegistry{}
	sw := pairedSweeper(s, stub, "local-lineage", "gen-local", false)
	sw.Sentinel = stubSentinel{pay: sentinel.Payload{
		V: 1, Gen: "gen-foreign", ID: "foreign-lineage",
		TS: sweepNow.Format(time.RFC3339),
	}}
	sink := &recordSink{}
	sw.Log = sink.log
	sum := sw.RunPass(testCtx(), "test")
	if !sum.Skipped || len(sum.Failures) != 1 {
		t.Fatalf("summary = %+v, want one skipped failure", sum)
	}
	pass := sink.byMsg("sweep pass")
	if len(pass) != 1 {
		t.Fatalf("logged %d pass summaries, want one", len(pass))
	}
	Logged, ok := pass[0].attrs["failures"].([]string)
	if !ok || len(Logged) != 1 || !strings.Contains(Logged[0], "foreign lineage") {
		t.Errorf("logged failures = %v, want the refusal cause", pass[0].attrs["failures"])
	}
}

// Silence refuses too: with nothing served the tracked rows prove
// nothing, armed or not. The sweeper never mints, so it cannot
// establish — only unlock or gc can.
func TestRunPassRefusesSilence(t *testing.T) {
	for _, dry := range []bool{true, false} {
		s := store.NewMemStore()
		// Unlocked: this test isolates silence, not the marker.
		if err := s.SetUnlocked(testCtx(), true); err != nil {
			t.Fatalf("stage unlock: %v", err)
		}
		stub := &stubRegistry{}
		sw := &Sweeper{
			Store:    s,
			Registry: stub,
			Sentinel: stubSentinel{err: fs.ErrNotExist},
			DryRun:   dry,
			Now:      func() time.Time { return sweepNow },
		}
		if err := s.Record(testCtx(), duerow("app", "10m", time.Hour)); err != nil {
			t.Fatalf("stage due row: %v", err)
		}
		sum := sw.RunPass(testCtx(), "test")
		if !sum.Skipped {
			t.Errorf("dry=%v silent pass was not skipped", dry)
		}
		if len(sum.Failures) != 1 || !strings.Contains(sum.Failures[0], "no sentinel served") {
			t.Errorf("dry=%v failures = %v, want the silence refusal", dry, sum.Failures)
		}
		if got := stubDeletes(stub); got != 0 {
			t.Errorf("dry=%v refused pass attempted %d deletes", dry, got)
		}
	}
}

// A rollback served to an armed sweeper refuses: only --force (gc)
// or an explicit adopt accepts it. If this fails, a restored
// registry gets swept against stale evidence.
func TestRunPassRefusesStaleArmed(t *testing.T) {
	s := store.NewMemStore()
	stub := &stubRegistry{}
	ctx := context.Background()
	id := "local-lineage"
	if err := s.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: "gen-new"}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	// Unlocked: this test isolates the rollback, not the marker.
	if err := s.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	for _, row := range []policy.Row{
		{Repo: sentinel.Repo, Tag: "gen-old", Digest: "sha256:a", PushedAt: sweepNow.Add(-2 * time.Hour)},
		{Repo: sentinel.Repo, Tag: "gen-new", Digest: "sha256:b", PushedAt: sweepNow},
	} {
		if err := s.Record(ctx, row); err != nil {
			t.Fatalf("stage row: %v", err)
		}
	}
	sw := &Sweeper{
		Store:    s,
		Registry: stub,
		Sentinel: stubSentinel{pay: sentinel.Payload{
			V: 1, Gen: "gen-old", ID: id,
			TS: sweepNow.Add(-2 * time.Hour).Format(time.RFC3339),
		}},
		Now: func() time.Time { return sweepNow },
	}
	sum := sw.RunPass(ctx, "test")
	if !sum.Skipped {
		t.Error("armed pass over a rollback was not skipped")
	}
	if len(sum.Failures) != 1 || !strings.Contains(sum.Failures[0], "older than tracked") {
		t.Errorf("failures = %v, want the rollback refusal", sum.Failures)
	}
	if got := stubDeletes(stub); got != 0 {
		t.Errorf("refused pass attempted %d deletes", got)
	}
}

// A sweeper with no sentinel reader refuses loud instead of
// panicking on a nil port: miswired serve must read as broken, not
// crash the keeper loop.
func TestRunPassRefusesNilSentinel(t *testing.T) {
	s := store.NewMemStore()
	stub := &stubRegistry{}
	sw := &Sweeper{
		Store:    s,
		Registry: stub,
		Now:      func() time.Time { return sweepNow },
	}
	sum := sw.RunPass(testCtx(), "test")
	if !sum.Skipped || len(sum.Failures) != 1 {
		t.Errorf("summary = %+v, want a skipped pass with one failure", sum)
	}
}
