package proof

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

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
