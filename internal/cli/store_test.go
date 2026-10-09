package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

func seedRows(s *store.MemStore) {
	c := cliCtx()
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:aaa",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-2 * time.Hour), Actor: "receiver"})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:bbb",
		PushedAt: cliNow.Add(-time.Hour), Actor: "receiver",
		Due: true, Reason: "ttl:10m elapsed"})
}

func seedSentinel(s *store.MemStore) {
	_ = s.Record(cliCtx(), policy.Row{Repo: "noroutine/kpr-sentinel", Tag: "019-gen",
		Digest: "sha256:ccc", PushedAt: cliNow.Add(-30 * time.Minute), Actor: "kpr-unlock"})
}

// mustUnlock opens the marker: rm tests stage unlocked ground so
// the intent gate (tested here, not there) stays green.
func mustUnlock(t *testing.T, s *store.MemStore) {
	t.Helper()
	if err := s.SetUnlocked(cliCtx(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
}

// unlockedProof reads the marker the way the rm command does.
func unlockedProof(t *testing.T, s *store.MemStore) proof.UnlockedStore {
	t.Helper()
	unlocked, err := proof.ProveUnlockedStore(cliCtx(), s)
	if err != nil {
		t.Fatalf("prove unlocked ground: %v", err)
	}
	return unlocked
}

// untagProof mints the way the rm command does: paired store,
// served generation of our lineage. If minting fails here, the
// test ground (not the rm path) is broken.
func untagProof(t *testing.T, s *store.MemStore) proof.SameStore {
	t.Helper()
	if err := s.SetIdentity(cliCtx(), store.Identity{ID: "test-id"}); err != nil {
		t.Fatalf("stage identity: %v", err)
	}
	mustUnlock(t, s)
	same, err := proof.Prover{
		Sentinel: stubProofAPI{id: "test-id", ts: cliNow.Format(time.RFC3339)},
		Store:    s, DryRun: false,
		Now: func() time.Time { return cliNow },
	}.Prove(cliCtx())
	if err != nil {
		t.Fatalf("prove on paired ground: %v", err)
	}
	return same
}

// ghosts is the read-only convergence view: rows both witnesses
// agree are gone, each with its evidence. Live rows stay out,
// conflicts surface apart (catalog lists, fs absent — never
// ghosts), unreadable repos degrade to a footer (never to ghosts),
// machinery never lists, and an unproven store or fs refuses
// instead of guessing. --long names the footers, --json pipes
// ghosts plus both degraded buckets. If this fails, operators act
// blind on the store-vs-registry delta.
// seedGhostStore stages the convergence fixture: one agreed-gone
// row, one split witness, one live row, one unreadable repo, one
// machinery row. Shared by the evidence test and the write-failure
// sweep so both fail over the same layout.
func seedGhostStore(t *testing.T) (*store.MemStore, ghostReg, map[string]bool, proof.SameStore) {
	t.Helper()
	s := store.NewMemStore()
	c := cliCtx()
	old := cliNow.Add(-200 * 24 * time.Hour)
	for _, r := range []policy.Row{
		{Repo: "gone", Tag: "v1", Digest: "sha256:a", PushedAt: old},
		{Repo: "split", Tag: "v1", Digest: "sha256:b", PushedAt: old},
		{Repo: "live", Tag: "v1", Digest: "sha256:c", PushedAt: old},
		{Repo: "flaky", Tag: "v1", Digest: "sha256:d", PushedAt: old},
		{Repo: "noroutine/kpr-sentinel", Tag: "v1", Digest: "sha256:e", PushedAt: old},
	} {
		_ = s.Record(c, r)
	}
	reg := ghostReg{
		tags: map[string][]string{"live": {"v1"}, "split": {"other"}},
		gone: map[string]bool{"gone": true, "noroutine/kpr-sentinel": true},
	}
	return s, reg, map[string]bool{"live": true}, untagProof(t, s)
}

// ghostReg 404s gone repos and fails everything else it does not
// list — the two catalog answers the ghosts view joins.
type ghostReg struct {
	tags map[string][]string
	gone map[string]bool
}

func (g ghostReg) Catalog(_ context.Context, repo string) ([]string, error) {
	if g.gone[repo] {
		return nil, &registry.StatusError{Op: "catalog " + repo, Status: 404}
	}
	if tags, ok := g.tags[repo]; ok {
		return tags, nil
	}
	return nil, &registry.StatusError{Op: "catalog " + repo, Status: 500}
}

// untagStub is the registry half of `rm --untag`: canned outcome
// per call, with the refs it saw (untouched on refusal).
type untagStub struct {
	outcome string
	err     error
	calls   []string
}

func (s *untagStub) DeleteManifest(_ context.Context, repo, ref string) (string, error) {
	s.calls = append(s.calls, repo+":"+ref)
	return s.outcome, s.err
}

// flipStub succeeds once, then holds: partial success must still
// print the confirmed row before the error returns.
type flipStub struct {
	calls []string
	n     int
}

func (s *flipStub) DeleteManifest(_ context.Context, repo, ref string) (string, error) {
	s.calls = append(s.calls, repo+":"+ref)
	s.n++
	if s.n == 1 {
		return "deleted", nil
	}
	return "held", nil
}

// mustUnlockedMem stages unlocked mem ground for proofs that need
// a store apart from the row table.
func mustUnlockedMem(t *testing.T) *store.MemStore {
	t.Helper()
	s := store.NewMemStore()
	mustUnlock(t, s)
	return s
}

// Command tails run the real RunE against a file backend: ls
// (valid and curious spellings), inspect, rm refusals. Refusals
// are the point here — rm-proof wiring already has success paths —
// plus one rm tail past the gates. If any fail, the command is
// unwired.
func TestStoreCommandTailsRunAgainstFileBackend(t *testing.T) {
	dir := t.TempDir()
	regRoot := t.TempDir()
	srv := serveRegistry(t, regRoot, true)
	defer srv.Close()
	cfg := stageGCStore(t, regRoot)
	setUnlockConfig(t, cfg)
	if _, err := runLockCmd(t, dir, srv.URL, unlockCmd); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	for _, tc := range []struct {
		name    string
		target  *cobra.Command
		args    []string
		wantErr string
	}{
		{"ls", storeLsCmd, nil, ""},
		{"ls sentinels", storeLsCmd, []string{"sentinels"}, ""},
		{"inspect unknown", storeInspectCmd, []string{"ghost:v1"}, "no tracked row"},
		{"rm unknown", storeRmCmd, []string{"ghost:v1"}, "no tracked row"},
		{"rm untag unknown", storeRmCmd, []string{"ghost:v1"}, "no tracked row"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.args) > 0 && tc.target == storeRmCmd {
				if tc.name == "rm untag unknown" {
					if err := storeRmCmd.Flags().Set("untag", "true"); err != nil {
						t.Fatalf("set --untag: %v", err)
					}
					defer func() { _ = storeRmCmd.Flags().Set("untag", "false") }()
				}
			}
			out, err := runCmdWithArgs(t, dir, srv.URL, tc.target, tc.args)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("%s err = %v, want %q (out: %s)", tc.name, err, tc.wantErr, out)
			}
		})
	}
	if err := storeLsCmd.Args(storeLsCmd, []string{"bogus", "extra"}); err == nil {
		t.Error("ls with two args validated, want refusal")
	}
	if err := storeLsCmd.Args(storeLsCmd, []string{"bogus"}); err == nil {
		t.Error("ls with curious spelling validated, want refusal")
	}
	// The two valid spellings validate clean: bare and sentinels.
	// If this fails, a boundary tweak refuses the documented
	// invocation while misspellings still error.
	for _, args := range [][]string{nil, {"sentinels"}} {
		if err := storeLsCmd.Args(storeLsCmd, args); err != nil {
			t.Errorf("ls %v refused: %v, want acceptance", args, err)
		}
	}
}

// runCmdWithArgs drives one command RunE with args against a file
// backend: the args-aware twin of runLockCmd.
func runCmdWithArgs(t *testing.T, dir, regURL string, target *cobra.Command, args []string) (string, error) {
	t.Helper()
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, dir)
	t.Setenv(config.EnvRegistryURL, regURL)
	var buf bytes.Buffer
	target.SetOut(&buf)
	defer target.SetOut(nil)
	target.SetContext(context.Background())
	err := target.RunE(target, args)
	return buf.String(), err
}
