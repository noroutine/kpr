package gc

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var (
	errReleaseFailed = errors.New("release failed")
	errProbeDead     = errors.New("probe dead")
)

// The orchestration runs behind stub ports: a fixed readonly probe
// and a recording collector drive a full pass with no network and no
// binary. If this fails, Run reaches past its ports to the concrete
// world.
func TestRunBehindStubPorts(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	digest := "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd"
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: digest, PushedAt: time.Now()})
	link := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_manifests", "tags", "v1", "current", "link")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("stage link dir: %v", err)
	}
	if err := os.WriteFile(link, []byte(digest+"\n"), 0o644); err != nil {
		t.Fatalf("stage link: %v", err)
	}

	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	collect := Collector(func(_ context.Context, _ io.Writer, _ string, args []string, _ Reporter) error {
		collected = append(collected, args)
		return nil
	})
	var out strings.Builder
	err := Run(c, &out, s, probe, s, collect, "http://registry:5000", cfg, "/bin/sh",
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("stub-port run: %v", err)
	}
	if len(collected) != 1 || !hasArg(collected[0], "--dry-run") {
		t.Errorf("collector got %v, want one --dry-run invocation", collected)
	}
	if !strings.Contains(out.String(), "shared store proven via app:v1") {
		t.Errorf("output lacks the proof line:\n%s", out.String())
	}
}

// stageProvenRun stages config, one digest row, and its link: every
// orchestration test below starts from proven ground and varies one
// port.
func stageProvenRun(t *testing.T) (string, *store.MemStore) {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	digest := "sha256:094354e66a2a3da4f26955a83048fb9a5b6e36e8a972a3ea3628c2fcdd09a3cd"
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: digest, PushedAt: time.Now()})
	link := filepath.Join(root, "docker", "registry", "v2", "repositories", "app", "_manifests", "tags", "v1", "current", "link")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("stage link dir: %v", err)
	}
	if err := os.WriteFile(link, []byte(digest+"\n"), 0o644); err != nil {
		t.Fatalf("stage link: %v", err)
	}
	return cfg, s
}

func okCollector(collected *[][]string) Collector {
	return func(_ context.Context, _ io.Writer, _ string, args []string, _ Reporter) error {
		*collected = append(*collected, args)
		return nil
	}
}

// releaseFailLocker drops the lock release: the run still passes
// (the lock expires), but warns loud instead of pretending a clean
// handoff. If this fails, a leaked lock reads as orderly.
func TestRunWarnsOnReleaseFailure(t *testing.T) {
	cfg, s := stageProvenRun(t)
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, s,
		Probe(func(context.Context, string) (Mode, string, error) { return ModeReadonly, "", nil }),
		releaseFailLocker{s}, okCollector(&collected),
		"http://registry:5000", cfg, "/bin/sh",
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
	cfg, s := stageProvenRun(t)
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
	err := Run(context.Background(), &out, s, flipProbe(), s, okCollector(&collected),
		"http://registry:5000", cfg, "/bin/sh",
		Options{DryRun: true, Report: func(Event) {}})
	if err == nil || !strings.Contains(err.Error(), "mode changed") {
		t.Fatalf("flipped run = %v, want the mode-change refusal", err)
	}
	out.Reset()
	if err := Run(context.Background(), &out, s, flipProbe(), s, okCollector(&collected),
		"http://registry:5000", cfg, "/bin/sh",
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
	cfg, s := stageProvenRun(t)
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
	err := Run(context.Background(), &out, s, probe, s, okCollector(&collected),
		"http://registry:5000", cfg, "/bin/sh",
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("dead-post-probe run: %v", err)
	}
	if !strings.Contains(out.String(), "post-run probe failed") {
		t.Errorf("output lacks the post-probe warning:\n%s", out.String())
	}
}
