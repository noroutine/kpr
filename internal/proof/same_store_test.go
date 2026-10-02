package proof

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var errProofDown = errors.New("redis: connection refused")

// stubSentinel serves one staged generation over the read port.
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

var proofNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func pairGround(s *store.MemStore, id, gen string) stubSentinel {
	if err := s.SetIdentity(context.Background(), store.Identity{ID: id, BaselineGen: gen}); err != nil {
		panic(err)
	}
	return stubSentinel{pay: sentinel.Payload{
		V: 1, Gen: gen, ID: id,
		TS: proofNow.Format(time.RFC3339), Writer: "kpr-gc",
	}}
}

// A stage takes its evidence as an argument: no proof value, no
// delete. If this fails, the signature stopped meaning it.
func ExampleProver_Prove() {
	s := store.NewMemStore()
	api := pairGround(s, "id-example", "gen-example")
	_ = s.Record(context.Background(), policy.Row{Repo: "app", Tag: "v1",
		Digest: "sha256:aaa", PushedAt: proofNow.Add(-time.Hour), Actor: "kpr-receiver"})

	p, err := (Prover{Sentinel: api, Store: s, Now: func() time.Time { return proofNow }}).Prove(context.Background())
	if err != nil {
		fmt.Println("refused:", err)
		return
	}
	fmt.Printf("proven %s for %s\n", p.Generation(), p.Identity())
	// Output: proven gen-example for id-example
}

// Foreign ground refuses with the ceremony named: a misconfigured
// registry URL plus a valid digest deletes nobody's tags. If this
// fails, the prover trusts strangers.
func TestProveRefusesForeign(t *testing.T) {
	s := store.NewMemStore()
	api := pairGround(s, "id-example", "gen-example")
	if err := s.SetIdentity(context.Background(), store.Identity{ID: "elsewhere"}); err != nil {
		t.Fatalf("stage pairing: %v", err)
	}
	if _, err := (Prover{Sentinel: api, Store: s}).Prove(context.Background()); err == nil {
		t.Fatal("foreign store proved, want refusal")
	} else if !strings.Contains(err.Error(), "adopt") {
		t.Errorf("refusal = %q, want the ceremony named", err.Error())
	}
}

// errProofStore fails the paired side per scenario: the
// backend-outage stand-in for the lineage record and the rows.
type errProofStore struct {
	*store.MemStore
	identErr error
	rowsErr  error
}

func (s errProofStore) GetIdentity(ctx context.Context) (store.Identity, error) {
	if s.identErr != nil {
		return store.Identity{}, s.identErr
	}
	return s.MemStore.GetIdentity(ctx)
}

func (s errProofStore) All(ctx context.Context) ([]policy.Row, error) {
	if s.rowsErr != nil {
		return nil, s.rowsErr
	}
	return s.MemStore.All(ctx)
}

// A miswired prover refuses instead of proving blind: no sentinel
// reader means no evidence, never a default. If this fails, a nil
// port proves the store it never read.
func TestProveRefusesMiswired(t *testing.T) {
	s := store.NewMemStore()
	if _, err := (Prover{Store: s}).Prove(context.Background()); err == nil {
		t.Fatal("prover without sentinel proved, want refusal")
	} else if !strings.Contains(err.Error(), "prover miswired") {
		t.Errorf("refusal = %q, want the wiring named", err.Error())
	}
}

// Backend outages on the paired side refuse with the side named:
// an unreadable lineage is not an unpaired one, untracked rows are
// not clean rows. If this fails, an outage proves.
func TestProveRefusesUnreadableGround(t *testing.T) {
	s := store.NewMemStore()
	api := pairGround(s, "id-example", "gen-example")
	if _, err := (Prover{Sentinel: api,
		Store: errProofStore{MemStore: s, identErr: errProofDown},
	}).Prove(context.Background()); err == nil {
		t.Fatal("prove over unreadable lineage succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unreadable") {
		t.Errorf("refusal = %q, want the lineage named", err.Error())
	}
	if _, err := (Prover{Sentinel: api,
		Store: errProofStore{MemStore: s, rowsErr: errProofDown},
	}).Prove(context.Background()); err == nil {
		t.Fatal("prove over unreadable rows succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "tracked state unreadable") {
		t.Errorf("refusal = %q, want the rows named", err.Error())
	}
}

// A stale preview still proves, carrying the staleness: dry-run
// proves presence over an older generation, and the proof says so.
// If this fails, previews either refuse what they should describe
// or hide the rollback they saw.
func TestProveStalePreviewCarriesStaleness(t *testing.T) {
	s := store.NewMemStore()
	api := pairGround(s, "id-example", "gen-example")
	ctx := context.Background()
	// No accepted baseline: the served gen is tracked but older,
	// which is a rollback until adopted.
	if err := s.SetIdentity(ctx, store.Identity{ID: "id-example"}); err != nil {
		t.Fatalf("clear baseline: %v", err)
	}
	if err := s.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: "gen-example",
		Digest: "sha256:aaa", PushedAt: proofNow.Add(-time.Hour), Actor: "kpr-gc"}); err != nil {
		t.Fatalf("track served generation: %v", err)
	}
	if err := s.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: "gen-newer",
		Digest: "sha256:bbb", PushedAt: proofNow, Actor: "kpr-gc"}); err != nil {
		t.Fatalf("track newer generation: %v", err)
	}
	p, err := (Prover{Sentinel: api, Store: s, DryRun: true,
		Now: func() time.Time { return proofNow }}).Prove(ctx)
	if err != nil {
		t.Fatalf("stale preview refused: %v", err)
	}
	if !p.Stale() {
		t.Error("stale preview proof reports fresh, want Stale() true")
	}
	if p.Generation() != "gen-example" || p.Identity() != "id-example" {
		t.Errorf("proof = (%q, %q), want the served generation and identity",
			p.Generation(), p.Identity())
	}
}
