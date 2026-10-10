package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var cliNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func cliCtx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = cancel
	return c
}

func cliStore() *store.MemStore {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: cliNow.Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "latest", Digest: "sha256:b",
		PushedAt: cliNow.Add(-time.Hour)})
	_ = s.PushActivity(c, store.Outcome{Repo: "scratch", Tag: "10m",
		Reason: "ttl:10m elapsed", Outcome: "deleted", At: cliNow})
	_ = s.PushActivity(c, store.Outcome{Repo: "scratch", Tag: "9m",
		Reason: "ttl:9m elapsed", Outcome: "planned", At: cliNow})
	_ = s.PushActivity(c, store.Outcome{Repo: "app", Tag: "v1",
		Reason: "keep-n:exceeds 10", Outcome: "failed", At: cliNow})
	return s
}

func liveRegistryClient(t *testing.T) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return registry.NewClient(srv.URL)
}

// plan discard drops the whole plan (due marks) and says how many went.
// No dry-run: discarding previews nothing, it reports. If this fails,
// a stale plan survives its discard and the next sweep eats rows the
// operator already pardoned.
// reapStage builds one expired row, one partial (digest-less, stale)
// row, and an 11-tag pile whose oldest is keep-n's victim: each
// selective reap must mark only its own policy's rows.
func reapStage(s *store.MemStore) {
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: cliNow.Add(-time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "abc1234", Digest: "sha256:h",
		PushedAt: cliNow.Add(-49 * time.Hour)})
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "partial",
		PushedAt: cliNow.Add(-25 * time.Hour)})
	for i := 1; i <= 11; i++ {
		_ = s.Record(c, policy.Row{Repo: "pile", Tag: fmt.Sprintf("v%02d", i),
			Digest: "sha256:p", PushedAt: cliNow.Add(-time.Duration(12-i) * time.Minute)})
	}
}

func dueTags(due []policy.Row) []string {
	var out []string
	for _, r := range due {
		out = append(out, r.Repo+":"+r.Tag)
	}
	return out
}

// sweepStub is the registry as the direct sweeper consumes it:
// deletes plus the sentinel read port, no HTTP. The served
// generation pairs the staged store below.
type sweepStub struct {
	stubProofAPI
	outcome string
	delErr  error
}

func (s sweepStub) DeleteManifest(context.Context, string, string) (string, error) {
	return s.outcome, s.delErr
}

// pairedSweepStore stages one due row on paired, unlocked ground
// with a fresh served generation: a pass runs instead of refusing.
func pairedSweepStore(t *testing.T) (*store.MemStore, sweepStub) {
	t.Helper()
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"})
	_ = s.SetIdentity(c, store.Identity{ID: "cli-lineage", BaselineGen: "019-proof"})
	if err := s.SetUnlocked(c, true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	stub := sweepStub{
		stubProofAPI: stubProofAPI{
			ts: time.Now().UTC().Format(time.RFC3339),
			id: "cli-lineage",
		},
		outcome: registry.OutcomeDeleted,
	}
	return s, stub
}

// Every keeper command fails fast naming redis when there is no
// state: the cobra wrappers are thin, but their one branch (refuse
// without a backend) must hold. If this fails, a command invents
// numbers with redis down.
func TestKeeperCommandsRefuseWithoutRedis(t *testing.T) {
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	for _, args := range [][]string{
		{"status"}, {"plan"}, {"plan", "discard"}, {"reap"}, {"reap", "ttl"}, {"sweep"},
	} {
		RootCmd.SetArgs(args)
		defer RootCmd.SetArgs(nil)
		if err := RootCmd.Execute(); err == nil {
			t.Errorf("kpr %s without redis succeeded, want a fast error", args[0])
		} else if !strings.Contains(err.Error(), "redis") {
			t.Errorf("kpr %s error = %q, want it to name redis", args[0], err.Error())
		}
	}
}

// deadStore is a store whose redis is gone: every method reports the
// outage instead of embedding a zero store.Store whose promoted
// methods nil-deref. If this fails, the outage test panics instead
// of asserting the loud report.
type deadStore struct{}

func (deadStore) outage() error { return errors.New("redis: connection refused") }

func (d deadStore) Ping(context.Context) error { return d.outage() }
func (d deadStore) Record(context.Context, policy.Row) error {
	return d.outage()
}
func (d deadStore) All(context.Context) ([]policy.Row, error) {
	return nil, d.outage()
}
func (d deadStore) Due(context.Context) ([]policy.Row, error) {
	return nil, d.outage()
}
func (d deadStore) Get(context.Context, string, string) (policy.Row, bool, error) {
	return policy.Row{}, false, d.outage()
}
func (d deadStore) MarkDue(context.Context, string, string, string) error {
	return d.outage()
}
func (d deadStore) ClearDue(context.Context) (int, error) {
	return 0, d.outage()
}
func (d deadStore) UnmarkDue(context.Context, string, string) (bool, error) {
	return false, d.outage()
}
func (d deadStore) Delete(context.Context, string, string) error {
	return d.outage()
}
func (d deadStore) SetCurrent(context.Context, store.Current) error {
	return d.outage()
}
func (d deadStore) GetCurrent(context.Context) (store.Current, error) {
	return store.Current{}, d.outage()
}
func (d deadStore) PushActivity(context.Context, store.Outcome) error {
	return d.outage()
}
func (d deadStore) Activity(context.Context) ([]store.Outcome, error) {
	return nil, d.outage()
}
func (d deadStore) AcquireLock(context.Context, string, time.Duration) (bool, error) {
	return false, d.outage()
}
func (d deadStore) ReleaseLock(context.Context, string) error {
	return d.outage()
}
func (d deadStore) IsUnlocked(context.Context) (bool, error) {
	return false, d.outage()
}
func (d deadStore) SetUnlocked(context.Context, bool) error {
	return d.outage()
}
func (d deadStore) GetIdentity(context.Context) (store.Identity, error) {
	return store.Identity{}, d.outage()
}
func (d deadStore) SetIdentity(context.Context, store.Identity) error {
	return d.outage()
}

// A catalog read against a dead registry fails fast: reap treats it as
// "no catalog for this repo" downstream, but the client itself must
// report the error rather than empty tags (empty would read as "every
// row untagged"). If this fails, registry blips become mass untagging.
func TestCatalogOnDeadRegistryFails(t *testing.T) {
	if _, err := registry.NewClient("http://127.0.0.1:1").Catalog(cliCtx(), "app"); err == nil {
		t.Error("catalog on dead registry succeeded, want an error")
	}
}

// failAfterWriter fails every write after n successes: the flaky-pipe
// stand-in for multi-line output.
type failAfterWriter struct {
	n     int
	calls int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.n {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}

// clearStoreEnv unsets every backend signal: derivation reads
// explicitness, and a leaked CI variable would select a backend the
// case never asked for.
func clearStoreEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvStore, "")
	t.Setenv(config.EnvStoreDir, "")
	t.Setenv(config.EnvRedisAddr, "")
}

// Command tails run the real RunE against a file backend: open,
// render, close — the wiring no unit covers. Each command gets one
// hermetic pass; refusals already pin the failure paths. If any of
// these fail, the command is unwired (flags, deps, or close).
func TestCommandTailsRunAgainstFileBackend(t *testing.T) {
	dir := t.TempDir()
	srv := serveRegistry(t, t.TempDir(), false)
	defer srv.Close()
	for _, tc := range []struct {
		name   string
		target *cobra.Command
		args   []string
		want   string
	}{
		{"status", statusCmd, nil, "registry:"},
		{"plan", planCmd, nil, "nothing due"},
		{"discard", planDiscardCmd, nil, "nothing due"},
		{"sweep", sweepCmd, nil, "sweep "},
		{"store status", storeStatusCmd, nil, "status:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runLockCmd(t, dir, srv.URL, tc.target)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("%s output lacks %q:\n%s", tc.name, tc.want, out)
			}
		})
	}
}

// Every keeper command refuses without state: one bad backend
// exercises every RunE's open failure. If this fails, a command
// invents numbers without a backend.
func TestKeeperCommandsRefuseBadBackend(t *testing.T) {
	t.Setenv(config.EnvStore, "bogus-backend")
	for _, target := range []*cobra.Command{statusCmd, planCmd, planDiscardCmd, sweepCmd, Cmd, lockCmd} {
		var buf bytes.Buffer
		target.SetOut(&buf)
		defer target.SetOut(nil)
		target.SetContext(context.Background())
		if err := target.RunE(target, nil); err == nil {
			t.Errorf("%s on bad backend succeeded, want refusal", target.Use)
		}
	}
}

// Every store and plan-edit command refuses without state too:
// the open failure belongs to all RunEs, not just the keeper's.
// If this fails, one command renders without a backend.
func TestStoreCommandsRefuseBadBackend(t *testing.T) {
	t.Setenv(config.EnvStore, "bogus-backend")
	for _, target := range []*cobra.Command{storeLsCmd, storeStatusCmd, storeInspectCmd, storeRmCmd, planAddCmd, planRemoveCmd, unlockCmd, adoptCmd} {
		var buf bytes.Buffer
		target.SetOut(&buf)
		defer target.SetOut(nil)
		target.SetContext(context.Background())
		if err := target.RunE(target, nil); err == nil {
			t.Errorf("%s on bad backend succeeded, want refusal", target.Use)
		}
	}
}

// Execute exits 1 on usage failure: the production entrypoint's
// failure mode, pinned via a child process (the parent only asserts
// the exit). If this fails, CLI misuse exits 0 and scripts proceed.
func TestExecuteExitsOneOnUsageError(t *testing.T) {
	if os.Getenv("KPR_EXEC_CHILD") == "1" {
		RootCmd.SetArgs([]string{"bogus-command"})
		Execute()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestExecuteExitsOneOnUsageError")
	cmd.Env = append(os.Environ(), "KPR_EXEC_CHILD=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("bogus command exited 0, want exit 1")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("bogus command err = %v, want exit 1", err)
	}
}

// errWriter fails every write: the broken-pipe stand-in.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
