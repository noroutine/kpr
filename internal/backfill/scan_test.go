package backfill

import (
	"context"
	"errors"
	"io"
	"testing"
)

// Scan counts what the catalog names: repos, tags, and what failed.
// Machinery counts too — comparison with the fs walk only holds
// when both sides see everything. If this fails, the API side of
// the comparison lies.
func TestScanCatalogCounts(t *testing.T) {
	reg := &stubRegistry{
		repos: []string{"app", "noroutine/kpr-sentinel", "gone"},
		tags: map[string][]string{
			"app":                    {"v1", "v2"},
			"noroutine/kpr-sentinel": {"gen"},
		},
		tagsErr: map[string]error{"gone": &stubStatus{code: 404}},
	}
	var last CatalogReport
	n := 0
	got, err := ScanCatalog(context.Background(), io.Discard, reg, func(rep CatalogReport) {
		last, n = rep, n+1
	})
	if err != nil {
		t.Fatalf("ScanCatalog = %v, want counts", err)
	}
	want := CatalogReport{Repos: 2, Tags: 3, Sentinels: 1, FailedRepos: 1}
	if got != want {
		t.Errorf("ScanCatalog = %+v, want %+v", got, want)
	}
	if n == 0 {
		t.Fatal("no progress reported")
	}
	if last != want {
		t.Errorf("last progress = %+v, want %+v", last, want)
	}
}

// A dead catalog refuses the scan: counting nothing as something
// is the lie. If this fails, blind runs report zeros.
func TestScanCatalogRefusesDeadCatalog(t *testing.T) {
	reg := &stubRegistry{reposErr: errors.New("down")}
	if _, err := ScanCatalog(context.Background(), io.Discard, reg, nil); err == nil {
		t.Error("ScanCatalog on dead catalog succeeded, want refusal")
	}
}

// A non-404 tag-list failure refuses (not skips): weather is not
// absence. If this fails, blips read as empty repos.
func TestScanCatalogRefusesTagFailure(t *testing.T) {
	reg := &stubRegistry{
		repos:   []string{"app"},
		tagsErr: map[string]error{"app": &stubStatus{code: 500}},
	}
	if _, err := ScanCatalog(context.Background(), io.Discard, reg, nil); err == nil {
		t.Error("ScanCatalog on 500 tags succeeded, want refusal")
	}
}
