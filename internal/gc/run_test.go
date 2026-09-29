package gc

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
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
