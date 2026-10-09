package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fmt"

	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// The backfill command exposes its contract on the help screen:
// the glob positional, preview-by-default arming, the config it
// resolves the mount from, and the one accepted risk. If this
// fails, the flags drifted from backfill.Run.
func TestStoreBackfillHelpNamesContract(t *testing.T) {
	var buf bytes.Buffer
	RootCmd.SetOut(&buf)
	defer RootCmd.SetOut(nil)
	RootCmd.SetArgs([]string{"store", "backfill", "--help"})
	defer RootCmd.SetArgs(nil)
	// Cobra keeps parsed flag values on the shared command: without
	// this, every later `store backfill` invocation in the package
	// reprints help and succeeds.
	defer func() { _ = storeBackfillCmd.Flags().Set("help", "false") }()
	Execute()
	out := buf.String()
	for _, want := range []string{"repo-glob", "no-dry-run", "accept-rollback", "output"} {
		if !strings.Contains(out, want) {
			t.Errorf("backfill help omits %q", want)
		}
	}
}

// The backfill wrapper refuses like the other one-shots: no
// backend, no run. If this fails, the command invents rows with
// nothing behind it.
func TestStoreBackfillRefusesWithoutBackend(t *testing.T) {
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	RootCmd.SetArgs([]string{"store", "backfill"})
	defer RootCmd.SetArgs(nil)
	if err := RootCmd.Execute(); err == nil {
		t.Error("store backfill without redis succeeded, want a fast error")
	} else if !strings.Contains(err.Error(), "redis") {
		t.Errorf("store backfill error = %q, want it to name redis", err.Error())
	}
}

// An unreadable registry config fails the run before any proof:
// the mount path comes from it, so guessing is refusing.
func TestStoreBackfillRefusesWithoutRegistryConfig(t *testing.T) {
	clearStoreEnv(t)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	missing := filepath.Join(t.TempDir(), "nope.yml")
	t.Setenv(config.EnvRegistryConfig, missing)
	RootCmd.SetArgs([]string{"store", "backfill"})
	defer RootCmd.SetArgs(nil)
	if err := RootCmd.Execute(); err == nil {
		t.Error("store backfill without registry config succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "nope.yml") {
		t.Errorf("store backfill error = %q, want it to name the config", err.Error())
	}
}

// --output to an uncreatable path refuses before any walk: the sink
// must exist before the run spends API calls. If this fails, the
// flag is declared but unwired.
func TestStoreBackfillOutputBadPathRefuses(t *testing.T) {
	clearStoreEnv(t)
	dir := t.TempDir()
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, dir)
	root := t.TempDir()
	cfg := "storage:\n  filesystem:\n    rootdirectory: " + root + "\n"
	cfgPath := filepath.Join(t.TempDir(), "registry.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("stage registry config: %v", err)
	}
	if err := store.NewFileStore(dir).SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock file store: %v", err)
	}
	t.Setenv(config.EnvRegistryConfig, cfgPath)
	RootCmd.SetArgs([]string{"store", "backfill", "--output", filepath.Join(t.TempDir(), "gone", "stream.log")})
	defer RootCmd.SetArgs(nil)
	// Cobra keeps parsed flag values on the shared command: restore
	// the default so later backfill runs don't inherit the bad path.
	defer func() { _ = storeBackfillCmd.Flags().Set("output", "") }()
	if err := RootCmd.Execute(); err == nil {
		t.Error("store backfill --output into missing dir succeeded, want refusal")
	}
}

// --output routes the per-tag stream: stdout carries it for -,
// a file carries it for a path, silence by default, and a bad
// path refuses. If this fails, the stream lands in the wrong
// sink (or none).
func TestResolveBackfillSinkRoutesStream(t *testing.T) {
	var out bytes.Buffer
	live := newLiveLines(&out)
	dash, closeDash, err := resolveBackfillSink("-", &out, live)
	if err != nil {
		t.Fatalf("resolve -: %v", err)
	}
	defer closeDash()
	if dash.Progress != nil {
		t.Error("resolve - sets Progress, want the stream instead of repaint")
	}
	if _, err := fmt.Fprint(dash.Log, "app:v1"); err != nil {
		t.Fatalf("write - stream: %v", err)
	}
	if out.String() != "app:v1" {
		t.Errorf("stdout = %q, want the stream", out.String())
	}
	quiet, _, err := resolveBackfillSink("", &out, live)
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if quiet.Log != nil {
		t.Error("resolve default sets Log, want it discarded")
	}
	if quiet.Progress == nil {
		t.Error("resolve default drops Progress, want live repaint")
	}
	stream := filepath.Join(t.TempDir(), "stream.log")
	filed, closeFile, err := resolveBackfillSink(stream, &out, live)
	if err != nil {
		t.Fatalf("resolve file: %v", err)
	}
	if _, err := fmt.Fprint(filed.Log, "app:v1"); err != nil {
		t.Fatalf("write file stream: %v", err)
	}
	closeFile()
	raw, err := os.ReadFile(stream)
	if err != nil {
		t.Fatalf("read stream file: %v", err)
	}
	if string(raw) != "app:v1" {
		t.Errorf("stream file = %q, want the stream", raw)
	}
	if _, _, err := resolveBackfillSink(filepath.Join(t.TempDir(), "gone", "stream.log"), &out, live); err == nil {
		t.Error("resolve bad path succeeded, want refusal before any walk")
	}
}

// The display is three copypastable lines: what the catalog
// names, what the store holds (tracked over everything, like
// analyze counts it, sentinels as a memo), and the run's own
// verdicts. Only the last line moves per verdict; the preview
// notice prints up front, never as a trailing suffix. If this
// fails, backfill miscounts or misrenders.
func TestStoreBackfillRendersBlock(t *testing.T) {
	for _, c := range []struct {
		sum  backfill.Summary
		want []string
	}{
		{
			backfill.Summary{Tracked: 0, Sentinels: 1, Repos: 599, Tags: 17047, Recorded: 17047},
			[]string{
				"catalog : 599 repos, 17047 tags",
				"store   : 1 tracked, 1 sentinel",
				"backfill: 17047 recorded, 0 skipped, 0 husks, 0 failed",
			},
		},
		{
			backfill.Summary{Tracked: 17144, Sentinels: 3, Repos: 600, Tags: 17050, Recorded: 16933, Skipped: 114, Husks: 150},
			[]string{
				"catalog : 600 repos, 17050 tags",
				"store   : 17147 tracked, 3 sentinels",
				"backfill: 16933 recorded, 114 skipped, 150 husks, 0 failed",
			},
		},
	} {
		lines := backfillLines(c.sum)
		if len(lines) != len(c.want) {
			t.Fatalf("backfill block has %d lines, want %d", len(lines), len(c.want))
		}
		for i, w := range c.want {
			if lines[i] != w {
				t.Errorf("line %d = %q, want %q", i, lines[i], w)
			}
			if len(lines[i]) < 11 || lines[i][8] != ':' || lines[i][9] != ' ' {
				t.Errorf("line misaligned: %q", lines[i])
			}
		}
	}
}

// The preview announces itself up front through the command: a dry
// run over an empty catalog prints the banner and records nothing.
// The banner line executes here and nowhere else, so silence means
// the preview path moved. If this fails, previews run quiet (or
// record).
func TestStoreBackfillDryRunAnnouncesPreview(t *testing.T) {
	clearStoreEnv(t)
	data := t.TempDir()
	gen, gerr := sentinel.NewGen()
	if gerr != nil {
		t.Fatalf("mint gen: %v", gerr)
	}
	id, ierr := sentinel.NewGen()
	if ierr != nil {
		t.Fatalf("mint id: %v", ierr)
	}
	if _, err := sentinel.Write(data, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: id, TS: time.Now().UTC().Format(time.RFC3339), Writer: "kpr-gc"}); err != nil {
		t.Fatalf("stage served generation: %v", err)
	}
	srv := serveDiskRegistry(t, data, `{"repositories":[]}`)
	defer srv.Close()
	cfgPath := filepath.Join(t.TempDir(), "registry.yml")
	if err := os.WriteFile(cfgPath, []byte("storage:\n  filesystem:\n    rootdirectory: "+data+"\n"), 0o644); err != nil {
		t.Fatalf("stage registry config: %v", err)
	}
	dir := t.TempDir()
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, dir)
	t.Setenv(config.EnvRegistryURL, srv.URL)
	t.Setenv(config.EnvRegistryConfig, cfgPath)
	backend, storeDir := resolveTestBackend(t)
	s, err := deps.OpenStore(config.NewBuilder().FromEnv().Build(), backend, storeDir)
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	defer func() { _ = s.Close() }()
	c := context.Background()
	if err := s.SetUnlocked(c, true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	if err := s.SetIdentity(c, store.Identity{ID: id, BaselineGen: gen}); err != nil {
		t.Fatalf("pair store: %v", err)
	}
	// Cobra keeps parsed flag values on the shared command: start
	// from defaults so no earlier invocation leaks in.
	for _, f := range [][2]string{{"output", ""}, {"no-dry-run", "false"}, {"accept-rollback", "false"}} {
		if err := storeBackfillCmd.Flags().Set(f[0], f[1]); err != nil {
			t.Fatalf("reset --%s: %v", f[0], err)
		}
	}
	var out bytes.Buffer
	RootCmd.SetOut(&out)
	defer RootCmd.SetOut(nil)
	RootCmd.SetArgs([]string{"store", "backfill"})
	defer RootCmd.SetArgs(nil)
	if err := RootCmd.Execute(); err != nil {
		t.Fatalf("store backfill dry run: %v", err)
	}
	if !strings.Contains(out.String(), "dry run — preview only, nothing recorded") {
		t.Errorf("preview lacks its banner:\n%s", out.String())
	}
}

// The glob positional reaches the run: bare and scoped invocations
// both get past the plumbing to the registry refusal behind it
// (no registry here). If this fails, the positional never arrives.
func TestStoreBackfillPassesGlobToRun(t *testing.T) {
	clearStoreEnv(t)
	dir := t.TempDir()
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, dir)
	root := t.TempDir()
	cfg := "storage:\n  filesystem:\n    rootdirectory: " + root + "\n"
	cfgPath := filepath.Join(t.TempDir(), "registry.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("stage registry config: %v", err)
	}
	if err := store.NewFileStore(dir).SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("unlock file store: %v", err)
	}
	t.Setenv(config.EnvRegistryConfig, cfgPath)
	for _, args := range [][]string{
		{"store", "backfill"},
		{"store", "backfill", "test/*"},
	} {
		RootCmd.SetArgs(args)
		defer RootCmd.SetArgs(nil)
		if err := RootCmd.Execute(); err == nil {
			t.Errorf("store backfill %q without registry succeeded, want refusal", args)
		}
	}
}
