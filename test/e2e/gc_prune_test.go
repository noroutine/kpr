//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// A stock collect must survive our prune. Live origin: staging grew
// repos whose _layers went missing (observed: infra-dev/rabbits), and
// the first stock walk to reach one aborts the whole mark phase with
// zero deletions — every gc after that is dead. So: push two tags
// for real, untag one for real, collect orphans with the real stock
// binary (which drains the doomed tag's links while the live tag
// keeps the repo out of the gc's own husk pass), prune with the real
// product code through an armed run, collect again. The second
// collect must exit 0. If it fails, prune ate skeleton the stock
// walker stats.
func TestGcSurvivesPrune(t *testing.T) {
	fx := NewStorageFixture(t)
	s := New(t, fx)

	// v2 stays live so the repo survives the gc's own husk pass;
	// v1 is the doomed tag whose drained links the prune must
	// spare the skeleton around.
	v1 := s.PushUntracked("probe/prune", "v1")
	s.PushUntracked("probe/prune", "v2")
	s.DeleteManifest("probe/prune", v1)

	// Orphan collect: v1's manifest is already gone, so its blobs
	// go and their layer links with them — v1 drains for real
	// while v2 keeps the repo alive.
	if code := runStockCollect(t, fx.StorageDir(), true); code != 0 {
		t.Fatalf("first stock collect exit %d, want 0", code)
	}
	// Layer links key on the raw layer bytes' sha256, not the
	// manifest digest: the same "kpr-e2e:repo:tag" format
	// buildImage pushes, so a format change here fails loud
	// instead of asserting on strangers.
	link := func(tag string) string {
		hex := fmt.Sprintf("%x", sha256.Sum256([]byte("kpr-e2e:probe/prune:"+tag)))
		return filepath.Join(fx.StorageDir(), "docker", "registry", "v2",
			"repositories", "probe", "prune", "_layers", "sha256", hex, "link")
	}
	if _, err := os.Lstat(link("v1")); !os.IsNotExist(err) {
		t.Fatalf("v1 link survives collect: want drained, else this test proves nothing")
	}
	if _, err := os.Lstat(link("v2")); err != nil {
		t.Fatalf("v2 link unreadable after collect, want kept: %v", err)
	}

	// The product prune under test, driven through the real run:
	// gc self-establishes pairing (nothing served yet), collects
	// via stub, then prunes husks and empty dirs for real.
	st := store.NewFileStore(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := st.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("unlock gc store: %v", err)
	}
	bin, _ := stageCollectorStub(t, "")
	cfg := stageRegistryConfig(t, fx.StorageDir())
	restore := stageRunConfig(t, fx.RegistryURL(), cfg, bin, stageTimeServer(t), "")
	defer restore()
	armed := proof.Arm(true, false)
	force := proof.Force(armed, true)
	var out strings.Builder
	if err := gc.Run(ctx, &out, gc.Deps{
		Lock: st, Rec: st, Ids: st, Rows: st,
		API: registry.NewClient(fx.RegistryURL()), Clock: clock.HTTPS{},
		Report: func(event.Event) {}, Store: st,
	}, gc.Options{Armed: armed}, gc.Accepts{Cache: force, Fence: force}); err != nil {
		t.Fatalf("armed gc: %v", err)
	}
	layers := filepath.Join(fx.StorageDir(), "docker", "registry", "v2",
		"repositories", "probe", "prune", "_layers")
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
