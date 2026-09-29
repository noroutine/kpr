package keeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var keeperNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func keeperCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = cancel
	return c
}

func untaggedStage() *store.MemStore {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "gone", Tag: "v1", Digest: "sha256:a",
		PushedAt: keeperNow.Add(-200 * time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "kept", Tag: "v9", Digest: "sha256:b",
		PushedAt: keeperNow.Add(-200 * time.Hour)})
	return s
}

// The untagged policy reads the live catalog: a tracked tag gone
// from the registry past the grace period marks, a listed tag does
// not. If this fails, evaluation reasons about rows alone and either
// never marks untagged or marks the whole world.
func TestEvaluateUntaggedUsesCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tags := []string{}
		if strings.Contains(r.URL.Path, "/kept/") {
			tags = []string{"v9"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": tags})
	}))
	defer srv.Close()
	marked, err := EvaluatePolicy(keeperCtx(), untaggedStage(), registry.NewClient(srv.URL), keeperNow, nil, "untagged")
	if err != nil {
		t.Fatalf("EvaluatePolicy untagged: %v", err)
	}
	if len(marked) != 1 || marked[0].Repo != "gone" {
		t.Errorf("untagged = %v, want only gone:v1", marked)
	}
}

// stubCatalog answers the catalog probe without HTTP: evaluation must
// consume the registry through its port, not a concrete client. If
// this fails, the use case is still coupled to the transport.
type stubCatalog struct {
	tags map[string][]string
	err  error
}

func (f stubCatalog) Catalog(ctx context.Context, repo string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tags[repo], nil
}

// The untagged selector marks only tags absent from the live catalog
// past grace: the stub lists v9 for kept and nothing for gone, so
// gone:v1 marks and kept:v9 stays. If this fails, evaluation reads
// the transport instead of its port — or the selector misfires.
func TestEvaluateUntaggedUsesStubCatalog(t *testing.T) {
	stub := stubCatalog{tags: map[string][]string{"gone": {}, "kept": {"v9"}}}
	marked, err := EvaluatePolicy(keeperCtx(), untaggedStage(), stub, keeperNow, nil, "untagged")
	if err != nil {
		t.Fatalf("EvaluatePolicy untagged: %v", err)
	}
	if len(marked) != 1 || marked[0].Repo != "gone" {
		t.Errorf("untagged = %v, want only gone:v1", marked)
	}
}
