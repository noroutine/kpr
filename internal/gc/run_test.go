package gc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var (
	errReleaseFailed = errors.New("release failed")
	errProbeDead     = errors.New("probe dead")
)

// fileAPI serves sentinel generations straight from the staged root:
// tag link to revision blob, manifest to config blob, layer link
// checked like the registry checks it. Whatever Write laid down,
// this serves back — a file-backed fake registry for the proof half
// of Run. A different (or empty) root serves nothing.
type fileAPI struct{ root string }

func (f fileAPI) blob(root, digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	return os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", hex[:2], hex, "data"))
}

func (f fileAPI) GetManifest(_ context.Context, repo, tag string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return nil, err
	}
	return f.blob(f.root, strings.TrimSpace(string(raw)))
}

func (f fileAPI) GetBlob(_ context.Context, repo, digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	if _, err := os.Stat(filepath.Join(f.root, "docker", "registry", "v2", "repositories", repo, "_layers", "sha256", hex, "link")); err != nil {
		return nil, err
	}
	return f.blob(f.root, digest)
}

// frozenAPI serves one fixed generation whatever the store holds: a
// stale snapshot on demand. If Run accepts it, the proof compares
// anything but the generation it just wrote.
type frozenAPI struct {
	manifest []byte
	blob     []byte
}

func (f frozenAPI) GetManifest(context.Context, string, string) ([]byte, error) {
	return f.manifest, nil
}

func (f frozenAPI) GetBlob(context.Context, string, string) ([]byte, error) {
	return f.blob, nil
}

// stageProvenRun stages a config over an empty root and returns the
// config, the root, and a lock: the sentinel Write inside Run lays
// the proof ground itself, so tests start empty and vary one port.
func stageProvenRun(t *testing.T) (string, string, *store.MemStore) {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	// Runs prove locality per pass, but intent is the operator's:
	// staged stores arrive unlocked so tests vary the ports, not
	// the marker. Fresh-locked is pinned by the storetest contract
	// and the dedicated refusal test below.
	s := store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	return cfg, root, s
}

func okCollector(collected *[][]string) Collector {
	return func(_ context.Context, _ io.Writer, _ string, args []string, _ Reporter) error {
		*collected = append(*collected, args)
		return nil
	}
}

// A verified mint records its generation row for keep-N: repo, gen
// tag, digest, actor. If this fails, generations go untracked and
// keep-N can never reap them — the litter returns silently.
func TestRunRecordsMintedGeneration(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s,
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("stub-port run: %v", err)
	}
	rows, err := s.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("tracked rows = %d, want exactly the minted generation", len(rows))
	}
	r := rows[0]
	if r.Repo != sentinel.Repo {
		t.Errorf("row repo = %q, want %q", r.Repo, sentinel.Repo)
	}
	if r.Tag == "" || r.Tag == sentinel.Tag {
		t.Errorf("row tag = %q, want the gen tag, never the floater", r.Tag)
	}
	if !strings.HasPrefix(r.Digest, "sha256:") || len(r.Digest) != 7+64 {
		t.Errorf("row digest = %q, want sha256:<64hex>", r.Digest)
	}
	if r.Actor != "kpr-gc" {
		t.Errorf("row actor = %q, want kpr-gc", r.Actor)
	}
	if r.PushedAt.IsZero() {
		t.Error("row PushedAt is zero, keep-N orders by it")
	}
}

// The orchestration runs behind stub ports: a fixed readonly probe,
// a file-backed fake registry, and a recording collector drive a
// full pass with no network and no binary. If this fails, Run reaches
// past its ports to the concrete world.
func TestRunBehindStubPorts(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock,
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("stub-port run: %v", err)
	}
	if len(collected) != 1 || !hasArg(collected[0], "--dry-run") {
		t.Errorf("collector got %v, want one --dry-run invocation", collected)
	}
	if !strings.Contains(out.String(), "shared store proven via noroutine/kpr-sentinel:latest generation ") {
		t.Errorf("output lacks the proof line:\n%s", out.String())
	}
	if !strings.HasSuffix(strings.TrimRight(out.String(), "\n"), "dry-run complete: nothing was deleted (collect for real with --no-dry-run)") {
		t.Errorf("dry-run verdict is not the last line:\n%s", out.String())
	}
}

// A stranger's store — the fake serves a different root — refuses
// even forced: the generation just written never comes back. If this
// fails, gc collects whatever directory the mount points at.
func TestRunStrangerStoreRefuses(t *testing.T) {
	cfg, _, lock := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, lock, okCollector(&collected), fileAPI{t.TempDir()},
		"http://registry:5000", cfg, "/bin/sh", lock,
		Options{DryRun: true, Force: true, Report: func(Event) {}})
	if err == nil {
		t.Fatal("gc on a stranger's store succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "does not share") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("collector ran %v on unproven ground, want no invocation", collected)
	}
}

// A stale snapshot — the fake serves a fixed old generation —
// refuses too: same files, wrong content. If this fails, the proof
// is presence of the sentinel, not freshness, and old snapshots
// pass silently.
func TestRunStaleSnapshotRefuses(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, sentinel.Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000001"}); err != nil {
		t.Fatalf("stage old generation: %v", err)
	}
	manRaw, err := fileAPI{root}.GetManifest(context.Background(), sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read staged manifest: %v", err)
	}
	var staged struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manRaw, &staged); err != nil {
		t.Fatalf("parse staged manifest: %v", err)
	}
	payRaw, err := fileAPI{root}.GetBlob(context.Background(), sentinel.Repo, staged.Config.Digest)
	if err != nil {
		t.Fatalf("read staged blob: %v", err)
	}
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	frozen := frozenAPI{manifest: manRaw, blob: payRaw}
	var collected [][]string
	var out strings.Builder
	err = Run(context.Background(), &out, probe, lock, okCollector(&collected), frozen,
		"http://registry:5000", cfg, "/bin/sh", lock,
		Options{DryRun: true, Force: true, Report: func(Event) {}})
	if err == nil {
		t.Fatal("gc on a stale snapshot succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "does not share") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// releaseFailLocker drops the lock release: the run still passes
// (the lock expires), but warns loud instead of pretending a clean
// handoff. If this fails, a leaked lock reads as orderly.
func TestRunWarnsOnReleaseFailure(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out,
		Probe(func(context.Context, string) (Mode, string, error) { return ModeReadonly, "", nil }),
		releaseFailLocker{s}, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s,
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("release-failed run: %v", err)
	}
	if !strings.Contains(out.String(), "gc lock release failed") {
		t.Errorf("output lacks the release warning:\n%s", out.String())
	}
}

type releaseFailLocker struct {
	*store.MemStore
}

func (releaseFailLocker) ReleaseLock(context.Context, string) error {
	return errReleaseFailed
}

// A mode flip between pre- and post-probe fails the run: writes may
// have raced the mark phase, and silence would bless the collect.
// --force passes warned instead — the operator presumed to know. If
// this fails, mid-run writes go unnoticed either way.
func TestRunFlipRefusesUnlessForced(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	flipProbe := func() Probe {
		calls := 0
		return func(context.Context, string) (Mode, string, error) {
			calls++
			if calls == 1 {
				return ModeReadonly, "", nil
			}
			return ModeWritable, "", nil
		}
	}
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, flipProbe(), s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s,
		Options{DryRun: true, Report: func(Event) {}})
	if err == nil || !strings.Contains(err.Error(), "mode changed") {
		t.Fatalf("flipped run = %v, want the mode-change refusal", err)
	}
	out.Reset()
	if err := Run(context.Background(), &out, flipProbe(), s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s,
		Options{DryRun: true, Force: true, Report: func(Event) {}}); err != nil {
		t.Fatalf("forced flipped run: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("forced flip lacks the WARNING:\n%s", out.String())
	}
}

// A dead post-probe only warns: the collect already happened against
// proven ground, and refusing now would lie about work done. If this
// fails, a transient probe blip fails a good run.
func TestRunDeadPostProbeWarns(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	calls := 0
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		calls++
		if calls == 1 {
			return ModeReadonly, "", nil
		}
		return ModeUnknown, "", errProbeDead
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s,
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("dead-post-probe run: %v", err)
	}
	if !strings.Contains(out.String(), "post-run probe failed") {
		t.Errorf("output lacks the post-probe warning:\n%s", out.String())
	}
}
