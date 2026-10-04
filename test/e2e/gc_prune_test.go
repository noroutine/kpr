//go:build e2e

package e2e

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"nrtn.dev/catalyst/kpr/internal/gc"
)

// A stock collect must survive our prune. Live origin: staging grew
// repos whose _layers went missing (observed: infra-dev/rabbits), and
// the first stock walk to reach one aborts the whole mark phase with
// zero deletions — every gc after that is dead. So: push for real,
// untag for real, collect orphans with the real stock binary (which
// empties _layers), prune with the real product code, collect again.
// The second collect must exit 0. If it fails, prune ate skeleton the
// stock walker stats.
func TestGcSurvivesPrune(t *testing.T) {
	fx := NewStorageFixture(t)
	s := New(t, fx)

	digest := s.PushUntracked("probe/prune", "v1")
	s.DeleteManifest("probe/prune", digest)

	// Orphan collect: the manifest is already gone, so its blobs go
	// and their layer links with them — _layers empties for real.
	if code := runStockCollect(t, fx.StorageDir(), true); code != 0 {
		t.Fatalf("first stock collect exit %d, want 0", code)
	}
	layers := filepath.Join(fx.StorageDir(), "docker", "registry", "v2",
		"repositories", "probe", "prune", "_layers")
	var links int
	_ = filepath.WalkDir(layers, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			links++
		}
		return nil
	})
	if links != 0 {
		t.Fatalf("_layers holds %d files: want drained by collect, else this test proves nothing", links)
	}

	// The product prune under test.
	if _, err := gc.PruneEmptyDirs(fx.StorageDir()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Lstat(layers); err != nil {
		t.Fatalf("_layers unreadable after prune, want kept: %v", err)
	}

	// The collect that died live must exit 0 on the pruned tree.
	if code := runStockCollect(t, fx.StorageDir(), true); code != 0 {
		t.Fatalf("second stock collect exit %d, want 0 (prune broke the skeleton)", code)
	}
}

// runStockCollect runs the real registry:3 collector once over a host
// storage dir and returns its exit code. The registry image ships the
// binary at /bin/registry; the entrypoint override keeps the image's
// serve wrapper out of the way. A nonzero exit is a real stock
// refusal, never a stub.
func runStockCollect(t *testing.T, storageDir string, deleteUntagged bool) int {
	t.Helper()
	cfg := "version: 0.1\nstorage:\n  filesystem:\n    rootdirectory: /var/lib/registry\n"
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("stage collect config: %v", err)
	}
	args := []string{"garbage-collect", "--delete-untagged", "/cfg.yml"}
	if !deleteUntagged {
		args = []string{"garbage-collect", "/cfg.yml"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctr, err := testcontainers.Run(ctx, fixtureRegistryImage,
		testcontainers.WithEntrypoint("/bin/registry"),
		testcontainers.WithCmd(args...),
		testcontainers.WithMounts(
			testcontainers.ContainerMount{
				Source: testcontainers.GenericBindMountSource{HostPath: storageDir},
				Target: "/var/lib/registry",
			},
			testcontainers.ContainerMount{
				Source: testcontainers.GenericBindMountSource{HostPath: cfgPath},
				Target: "/cfg.yml",
			},
		),
		testcontainers.WithWaitStrategy(wait.ForExit()),
	)
	if err != nil {
		t.Fatalf("run stock collect: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	st, err := ctr.State(ctx)
	if err != nil {
		t.Fatalf("collect state: %v", err)
	}
	if st.ExitCode != 0 {
		logs, _ := ctr.Logs(ctx)
		body, _ := io.ReadAll(logs)
		t.Logf("stock collect output:\n%s", tailLines(string(body), 8))
	}
	return st.ExitCode
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
