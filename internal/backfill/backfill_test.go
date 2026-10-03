package backfill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stubStatus is the typed registry failure the client reports:
// backfill classifies 404s by count, everything else refuses.
type stubStatus struct{ code int }

func (s *stubStatus) Error() string   { return fmt.Sprintf("registry status %d", s.code) }
func (s *stubStatus) StatusCode() int { return s.code }

// fileAPI serves generations off a TempDir volume exactly like the
// shared mount: link file to manifest digest, blob under the
// content path. Same shape as gc's seam stub.
type fileAPI struct{ root string }

func (f fileAPI) blob(digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	return os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2", "blobs", "sha256", hex[:2], hex, "data"))
}

func (f fileAPI) GetManifest(_ context.Context, repo, tag string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return nil, err
	}
	return f.blob(strings.TrimSpace(string(raw)))
}

func (f fileAPI) GetBlob(_ context.Context, repo, digest string) ([]byte, error) {
	return f.blob(digest)
}

// stubRegistry is the programmable catalog: repos, per-repo tags,
// per-tag digests, each with its own failure.
type stubRegistry struct {
	repos    []string
	reposErr error
	tags     map[string][]string
	tagsErr  map[string]error
	digests  map[string]string
	digErr   map[string]error
}

func (s *stubRegistry) CatalogAll(context.Context) ([]string, error) {
	if s.reposErr != nil {
		return nil, s.reposErr
	}
	return s.repos, nil
}

func (s *stubRegistry) Catalog(_ context.Context, repo string) ([]string, error) {
	if err, ok := s.tagsErr[repo]; ok {
		return nil, err
	}
	return s.tags[repo], nil
}

func (s *stubRegistry) ManifestDigest(_ context.Context, repo, tag string) (string, string, error) {
	k := repo + "\x00" + tag
	if err, ok := s.digErr[k]; ok {
		return "", "", err
	}
	return s.digests[k], "application/vnd.oci.image.manifest.v1+json", nil
}

// stagePaired writes a served generation and pairs the store to it,
// tracking the generation like a proven run would. Returns the root
// (volume layout), the mem store (rows, identity, lock), the served
// gen, and the store identity.
func stagePaired(t *testing.T) (root string, s *store.MemStore, gen, id string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()
	s = store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock staging store: %v", err)
	}
	var gerr, ierr error
	gen, gerr = sentinel.NewGen()
	if gerr != nil {
		t.Fatalf("mint gen: %v", gerr)
	}
	id, ierr = sentinel.NewGen()
	if ierr != nil {
		t.Fatalf("mint id: %v", ierr)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: id, TS: now}); err != nil {
		t.Fatalf("stage generation: %v", err)
	}
	if err := s.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: gen}); err != nil {
		t.Fatalf("pair store: %v", err)
	}
	if err := s.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: gen, PushedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("track generation: %v", err)
	}
	return root, s, gen, id
}

// stageTagDir lays a tag link with a real mtime under root: the
// layout tagMtime stats. Returns the link's mtime.
func stageTagDir(t *testing.T, root, repo, tag string) time.Time {
	t.Helper()
	dir := filepath.Join(root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir tag dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "link"), []byte("sha256:abc"), 0o644); err != nil {
		t.Fatalf("write link: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "link"))
	if err != nil {
		t.Fatalf("stat link: %v", err)
	}
	return fi.ModTime().UTC()
}

func accept(t *testing.T) proof.AcceptedRisk {
	t.Helper()
	return proof.Force(proof.Arm(true, false), true)
}

// An absent tag records with its link mtime, signed kpr-backfill,
// not due: the row a re-push would have made, minus the push. If
// this fails, pre-kpr tags stay invisible to every selector.
func TestBackfillRecordsAbsentWithMtimes(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	mtime := stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Recorded: 1}) {
		t.Errorf("sum = %+v, want {Recorded:1}", sum)
	}
	rows, _ := s.All(ctx)
	if len(rows) != 2 {
		t.Fatalf("tracked rows = %d, want gen + app:v1", len(rows))
	}
	var got *policy.Row
	for i, r := range rows {
		if r.Repo == "app" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("no app:v1 row recorded")
	}
	if got.Digest != "sha256:abc" || got.Actor != ActorBackfill || got.Due || !got.PushedAt.Equal(mtime) {
		t.Errorf("row = %+v, want digest + kpr-backfill + not-due + link mtime", got)
	}
}

// Progress reports every verdict as it lands: the caller renders
// live counters from it. If this fails, counters lag the run.
func TestBackfillProgressReportsVerdicts(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	stageTagDir(t, root, "app", "v2")
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1", "v2"}},
		digests: map[string]string{"app\x00v1": "sha256:abc", "app\x00v2": "sha256:def"},
	}
	var last Summary
	n := 0
	opts := Options{DryRun: true, Log: io.Discard, Progress: func(sum Summary) {
		last, n = sum, n+1
	}}
	if _, err := Run(ctx, io.Discard, fileAPI{root}, reg, s, s, s, s, root, opts, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n == 0 {
		t.Fatal("no progress reported")
	}
	if last.Recorded != 2 || last.Skipped != 0 || last.Failed != 0 {
		t.Errorf("last progress = %+v, want 2 recorded", last)
	}
}

// A preview prints what it would stamp to the log sink and records
// nothing: the operator reviews before arming. No sink, no stream
// — the per-tag lines need an explicit Log. If this fails, dry
// runs lie or write.
func TestBackfillDryRunPrintsWithoutRecording(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, io.Discard, fileAPI{root}, reg, s, s, s, s, root, Options{DryRun: true, Log: &out}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "would record app:v1") {
		t.Errorf("preview prints no would-record:\n%s", out.String())
	}
	rows, _ := s.All(ctx)
	if len(rows) != 1 {
		t.Errorf("dry run recorded %d rows, want none added", len(rows)-1)
	}
	if sum.Recorded != 1 {
		t.Errorf("sum = %+v, want the would-record counted", sum)
	}
}

// Tracked tags are no-ops: absence-only means a rerun over
// receiver-known rows changes nothing. If this fails, backfill
// clobbers live tracking.
func TestBackfillSkipsTracked(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	if err := s.Record(ctx, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:old", Actor: "kpr-receiver"}); err != nil {
		t.Fatalf("pre-track: %v", err)
	}
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 0 || sum.Skipped != 1 {
		t.Errorf("sum = %+v, want {Skipped:1}", sum)
	}
	rows, _ := s.All(ctx)
	for _, r := range rows {
		if r.Repo == "app" && r.Digest != "sha256:old" {
			t.Errorf("tracked row clobbered: %+v", r)
		}
	}
}

// Sentinel machinery is inventory, not backfill: our repos never
// become rows. If this fails, generations get TTL'd like workloads.
func TestBackfillSkipsSentinelRepos(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos:   []string{sentinel.Repo, "app"},
		tags:    map[string][]string{sentinel.Repo: {"gen1"}, "app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 1 {
		t.Errorf("sum = %+v, want exactly the app row", sum)
	}
}

// A glob matching nothing refuses, like exact names in plan add:
// a typo'd scope must not read as an empty registry. If this
// fails, `store backfill typo-*` reports a clean zero.
func TestBackfillUnknownGlobRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{repos: []string{"app"}}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{RepoGlob: "nope-*"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no catalog repository matches") {
		t.Errorf("err = %v, want the no-match refusal", err)
	}
}

// Tags vanishing mid-run skip by count: a 404 on the digest or a
// gone repo warns, never refuses. If this fails, a tag deleted
// between catalog and digest aborts the whole import.
func TestBackfillMidRun404Skips(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	stageTagDir(t, root, "app", "v1")
	reg := &stubRegistry{
		repos: []string{"app", "gone"},
		tags:  map[string][]string{"app": {"v1", "rip"}},
		digests: map[string]string{
			"app\x00v1": "sha256:abc",
		},
		digErr: map[string]error{
			"app\x00rip": &stubStatus{404},
		},
		tagsErr: map[string]error{
			"gone": &stubStatus{404},
		},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 1 || sum.Failed != 2 {
		t.Errorf("sum = %+v, want {Recorded:1 Failed:2}", sum)
	}
}

// A digest backfill cannot stamp without storage: a tag whose link
// never landed skips, never a zero-time row. If this fails,
// timeless rows enter the selectors.
func TestBackfillMissingLinkSkips(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{
		repos:   []string{"app"},
		tags:    map[string][]string{"app": {"v1"}},
		digests: map[string]string{"app\x00v1": "sha256:abc"},
	}
	var out strings.Builder
	sum, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Recorded != 0 || sum.Failed != 1 {
		t.Errorf("sum = %+v, want {Failed:1}", sum)
	}
}

// A locked store refuses naming the ceremony, before any catalog
// read: backfill under a fence would bless rows the fence
// invalidates. If this fails, locked runs enumerate anyway.
func TestBackfillLockedRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	if err := s.SetUnlocked(ctx, false); err != nil {
		t.Fatalf("lock: %v", err)
	}
	reg := &stubRegistry{}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if !errors.Is(err, proof.ErrLocked) {
		t.Errorf("err = %v, want the locked refusal", err)
	}
}

// A stranger's store refuses before enumerating: backfill never
// adopts by recording. If this fails, pointing at the wrong
// volume imports someone else's tags.
func TestBackfillForeignRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	if err := s.SetIdentity(ctx, store.Identity{ID: "someone-else"}); err != nil {
		t.Fatalf("re-pair: %v", err)
	}
	reg := &stubRegistry{}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "foreign lineage") {
		t.Errorf("err = %v, want the foreign refusal", err)
	}
}

// A restored generation refuses armed unless rollback-accepted,
// warns through a preview: backfill deletes nothing, but its rows
// could resurrect blobs keep-N already ate. If this fails, a
// rollback imports silently on a live run.
func TestBackfillRollbackNeedsAcceptArmed(t *testing.T) {
	ctx := context.Background()
	root, s, _, id := stagePaired(t)
	next, nerr := sentinel.NewGen()
	if nerr != nil {
		t.Fatalf("mint next gen: %v", nerr)
	}
	// A true rollback: the store tracks newer than served, and the
	// baseline is neither — the served gen is not adopted.
	if err := s.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: next}); err != nil {
		t.Fatalf("re-baseline: %v", err)
	}
	if err := s.Record(ctx, policy.Row{Repo: sentinel.Repo, Tag: next, PushedAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("track newer: %v", err)
	}
	reg := &stubRegistry{repos: []string{}}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "--accept-rollback") {
		t.Errorf("err = %v, want the rollback refusal naming its flag", err)
	}

	var warn strings.Builder
	sum, err := Run(ctx, &warn, fileAPI{root}, reg, s, s, s, s, root, Options{}, accept(t))
	if err != nil {
		t.Fatalf("accepted Run: %v", err)
	}
	if !strings.Contains(warn.String(), "Warning:") {
		t.Errorf("accepted run warns nothing:\n%s", warn.String())
	}
	if sum.Recorded != 0 {
		t.Errorf("sum = %+v, want nothing to record", sum)
	}

	var preview strings.Builder
	if _, err := Run(ctx, &preview, fileAPI{root}, reg, s, s, s, s, root, Options{DryRun: true}, nil); err != nil {
		t.Errorf("dry run over rollback refused: %v", err)
	}
}

// An unreachable catalog refuses the whole run before any row:
// backfill against a dead registry must not report a clean zero.
// If this fails, outages read as empty registries.
func TestBackfillUnreachableCatalogRefuses(t *testing.T) {
	ctx := context.Background()
	root, s, _, _ := stagePaired(t)
	reg := &stubRegistry{reposErr: fmt.Errorf("connection refused")}
	var out strings.Builder
	_, err := Run(ctx, &out, fileAPI{root}, reg, s, s, s, s, root, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "backfill enumeration") {
		t.Errorf("err = %v, want the enumeration refusal", err)
	}
}
