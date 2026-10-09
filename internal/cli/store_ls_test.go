package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// ls is the lay of the field: tracked rows, short columns, no
// machinery. Sentinels stay out by default (semantically
// different); `ls sentinels` shows only them. If this fails,
// operators cannot see what the store actually holds.
func TestStoreLsListsRowsShort(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	// Same namespace, not machinery: must stay visible.
	_ = s.Record(cliCtx(), policy.Row{Repo: "noroutine/kpr-web", Tag: "v2", Digest: "sha256:ddd",
		PushedAt: cliNow.Add(-10 * time.Minute), Actor: "receiver"})
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	for _, want := range []string{"REPO:TAG", "AGE", "DUE", "app:v1", "2h0m0s ago",
		"scratch:10m", "1h0m0s ago", "ttl:10m elapsed", "not due", "noroutine/kpr-web:v2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ls missing %q:\n%s", want, out.String())
		}
	}
	for _, gone := range []string{"kpr-sentinel", "sha256:aaa", "receiver", "pushed=", "actor="} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("ls leaks %q (sentinel/digest/actor/prefix):\n%s", gone, out.String())
		}
	}
}

// Same-repo rows sort by tag: the repo guard routes equal repos
// to the tag compare, and the tag compare orders them. Seeded
// backwards, so store order alone would fail. If this fails, ls
// lists tags in store order — the plan view scrambles.
func TestStoreLsSortsSameRepoByTag(t *testing.T) {
	s := store.NewMemStore()
	c := cliCtx()
	_ = s.Record(c, policy.Row{Repo: "zzz", Tag: "v9", Digest: "sha256:ddd",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-time.Hour), Actor: "receiver"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v2", Digest: "sha256:ddd",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-time.Hour), Actor: "receiver"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:aaa",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-2 * time.Hour), Actor: "receiver"})
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	body := out.String()
	v1, v2, z := strings.Index(body, "app:v1"), strings.Index(body, "app:v2"), strings.Index(body, "zzz:v9")
	if v1 < 0 || v2 < 0 || z < 0 || v1 > v2 || v2 > z {
		t.Errorf("rows out of repo-then-tag order:\n%s", body)
	}
}

// Repo-major beats tag-minor even when they disagree: app:z9 sorts
// before zzz:a1, so a tag-major comparator fails from any store
// order (map order is random — agreement here is luck, not proof).
// If this fails, ls lists tag-first and the plan view scrambles
// across repos.
func TestStoreLsSortsRepoBeforeTag(t *testing.T) {
	s := store.NewMemStore()
	c := cliCtx()
	_ = s.Record(c, policy.Row{Repo: "zzz", Tag: "a1", Digest: "sha256:ddd",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-time.Hour), Actor: "receiver"})
	_ = s.Record(c, policy.Row{Repo: "app", Tag: "z9", Digest: "sha256:aaa",
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		PushedAt:  cliNow.Add(-2 * time.Hour), Actor: "receiver"})
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	body := out.String()
	a, z := strings.Index(body, "app:z9"), strings.Index(body, "zzz:a1")
	if a < 0 || z < 0 || a > z {
		t.Errorf("rows out of repo-major order:\n%s", body)
	}
}

// The help names the ghosts target: operators converging the delta
// must find 'ls ghosts' from `store ls --help`. If this fails,
// the help points at one view while the other moved.
func TestStoreLsHelpNamesGhostsTwin(t *testing.T) {
	if !strings.Contains(storeLsCmd.Long, "'ls ghosts'") {
		t.Errorf("help = %q, want the ghosts target named", storeLsCmd.Long)
	}
}

// An empty ghost list still names its footers: a split witness
// degrades visibly even when no row is agreed gone. If this fails,
// a conflict reads as a clean bill.
func TestStoreLsGhostsEmptyNamesFooters(t *testing.T) {
	s := store.NewMemStore()
	for _, r := range []policy.Row{
		{Repo: "split", Tag: "v1", Digest: "sha256:b", PushedAt: cliNow.Add(-200 * 24 * time.Hour)},
		{Repo: "flaky", Tag: "v1", Digest: "sha256:c", PushedAt: cliNow.Add(-200 * 24 * time.Hour)},
	} {
		_ = s.Record(cliCtx(), r)
	}
	reg := ghostReg{tags: map[string][]string{"split": {"other"}}}
	var out bytes.Buffer
	if err := runStoreGhosts(cliCtx(), &out, s, reg,
		map[string]bool{"other": true}, untagProof(t, s), storeLsOpts{now: cliNow, long: true}); err != nil {
		t.Fatalf("runStoreGhosts: %v", err)
	}
	// Two footers: a write-error mutant returns after the first,
	// so one footer alone cannot catch it.
	for _, want := range []string{"no ghost rows", "skipped 1 repo", "conflict 1 repo", "split", "flaky"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ghosts footer missing %q:\n%s", want, out.String())
		}
	}
}

// Every write in the ghost listing surfaces: short, long, and
// JSON paths each fail loud at every failing prefix instead of
// truncating quiet. If this fails, a broken pipe reads as a
// complete convergence view.
func TestStoreLsGhostsWriteFailuresSurface(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts storeLsOpts
	}{
		{"short", storeLsOpts{now: cliNow}},
		{"long", storeLsOpts{now: cliNow, long: true}},
		{"json", storeLsOpts{now: cliNow, json: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, reg, fs, same := seedGhostStore(t)
			var good bytes.Buffer
			if err := runStoreGhosts(cliCtx(), &good, s, reg, fs, same, tc.opts); err != nil {
				t.Fatalf("stage success: %v", err)
			}
			sawNil := false
			for n := 0; n < 100; n++ {
				s, reg, fs, same := seedGhostStore(t)
				err := runStoreGhosts(cliCtx(), &failAfterWriter{n: n}, s, reg, fs, same, tc.opts)
				if err == nil {
					sawNil = true
				} else if sawNil {
					t.Fatalf("write %d failed after a success, want monotonic errors-then-clean", n)
				}
			}
			if !sawNil {
				t.Error("no write prefix succeeded, want the full run clean past its writes")
			}
		})
	}
}

func TestStoreLsGhostsNamesEvidence(t *testing.T) {
	s, reg, fs, same := seedGhostStore(t)
	var out bytes.Buffer
	if err := runStoreGhosts(cliCtx(), &out, s, reg, fs, same, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreGhosts: %v", err)
	}
	body := out.String()
	for _, want := range []string{"REPO:TAG", "EVIDENCE", "gone:v1", "catalog 404, fs absent",
		"skipped 1 repo", "conflict 1 repo"} {
		if !strings.Contains(body, want) {
			t.Errorf("ghosts missing %q:\n%s", want, body)
		}
	}
	for _, leak := range []string{"live:v1", "split:v1", "flaky:v1", "kpr-sentinel"} {
		if strings.Contains(body, leak) {
			t.Errorf("ghosts leaks %q:\n%s", leak, body)
		}
	}
	var long bytes.Buffer
	if err := runStoreGhosts(cliCtx(), &long, s, reg, fs, same, storeLsOpts{now: cliNow, long: true}); err != nil {
		t.Fatalf("runStoreGhosts long: %v", err)
	}
	for _, want := range []string{"DIGEST", "EVIDENCE", "sha256:a", "skipped 1 repo (catalog unreadable): flaky",
		"conflict 1 repo (catalog lists, fs absent): split"} {
		if !strings.Contains(long.String(), want) {
			t.Errorf("ghosts long missing %q:\n%s", want, long.String())
		}
	}
	var js bytes.Buffer
	if err := runStoreGhosts(cliCtx(), &js, s, reg, fs, same, storeLsOpts{now: cliNow, json: true}); err != nil {
		t.Fatalf("runStoreGhosts json: %v", err)
	}
	var decoded struct {
		Ghosts []struct {
			Repo     string `json:"repo"`
			Evidence string `json:"evidence"`
		} `json:"ghosts"`
		Conflicts  []string `json:"conflicts"`
		Unreadable []string `json:"unreadable"`
	}
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatalf("ghosts json unparseable: %v\n%s", err, js.String())
	}
	if len(decoded.Ghosts) != 1 || decoded.Ghosts[0].Repo != "gone" || decoded.Ghosts[0].Evidence == "" {
		t.Errorf("ghosts json = %+v, want [gone + evidence]", decoded.Ghosts)
	}
	if len(decoded.Conflicts) != 1 || len(decoded.Unreadable) != 1 {
		t.Errorf("ghosts json buckets = %v/%v, want [split]/[flaky]", decoded.Conflicts, decoded.Unreadable)
	}
	if err := runStoreGhosts(cliCtx(), io.Discard, s, reg, nil, same, storeLsOpts{now: cliNow}); err == nil {
		t.Error("runStoreGhosts(nil fs) succeeded, want refusal")
	}
	if err := runStoreGhosts(cliCtx(), io.Discard, s, reg, fs, nil, storeLsOpts{now: cliNow}); err == nil {
		t.Error("runStoreGhosts(nil proof) succeeded, want refusal")
	}
	var empty bytes.Buffer
	if err := runStoreGhosts(cliCtx(), &empty, store.NewMemStore(), reg, fs, same, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreGhosts empty: %v", err)
	}
	if !strings.Contains(empty.String(), "no ghost rows") {
		t.Errorf("ghosts empty = %q, want no-ghost verdict", empty.String())
	}
	// Empty buckets print nothing: a zero-count footer would
	// invent skips and conflicts the run never saw. If this
	// fails, clean runs read as dirty.
	for _, leak := range []string{"skipped", "conflict"} {
		if strings.Contains(empty.String(), leak) {
			t.Errorf("ghosts empty leaks %q:\n%s", leak, empty.String())
		}
	}
}

// A breaking pipe on the empty verdict still fails: the verdict
// is the output, not a courtesy. If this fails, dead pipes read
// as clean empties.
func TestStoreGhostsEmptyWriteFailureSurfaces(t *testing.T) {
	s := store.NewMemStore()
	same := untagProof(t, s)
	if err := runStoreGhosts(cliCtx(), &failAfterWriter{}, s, ghostReg{},
		map[string]bool{}, same, storeLsOpts{now: cliNow}); err == nil {
		t.Error("ghosts empty into breaking pipe succeeded, want an error")
	}
}

func TestStoreLsSentinelsOnly(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow, sentinels: true}); err != nil {
		t.Fatalf("runStoreLs sentinels: %v", err)
	}
	if !strings.Contains(out.String(), "noroutine/kpr-sentinel:019-gen") {
		t.Errorf("sentinels view missing the generation:\n%s", out.String())
	}
	for _, gone := range []string{"app:v1", "scratch:10m"} {
		if strings.Contains(out.String(), gone) {
			t.Errorf("sentinels view leaks user row %q:\n%s", gone, out.String())
		}
	}
}

func TestStoreLsLong(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow, long: true}); err != nil {
		t.Fatalf("runStoreLs long: %v", err)
	}
	for _, want := range []string{"DIGEST", "PUSHED", "ACTOR", "sha256:aaa", "receiver", "2026-09-27T10:00:00Z"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("long ls missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "kpr-sentinel") {
		t.Errorf("long ls leaks sentinels by default:\n%s", out.String())
	}
}

func TestStoreLsEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, store.NewMemStore(), storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("runStoreLs: %v", err)
	}
	if !strings.Contains(out.String(), "no tracked rows") {
		t.Errorf("empty ls should say so:\n%s", out.String())
	}
	var sout bytes.Buffer
	if err := runStoreLs(cliCtx(), &sout, store.NewMemStore(), storeLsOpts{now: cliNow, sentinels: true}); err != nil {
		t.Fatalf("runStoreLs sentinels: %v", err)
	}
	if !strings.Contains(sout.String(), "no sentinel rows") {
		t.Errorf("empty sentinels view should say so:\n%s", sout.String())
	}
}

func TestStoreLsJSON(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	var out bytes.Buffer
	if err := runStoreLs(cliCtx(), &out, s, storeLsOpts{now: cliNow, json: true}); err != nil {
		t.Fatalf("runStoreLs json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("ls json unparseable: %v\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("ls json has %d rows, want 2 (sentinels excluded)", len(rows))
	}
}

// Listing against dead state fails naming the outage: an empty
// table must mean empty, never unreadable. If this fails, an
// outage prints as no inventory.
func TestStoreLsOnDeadStoreFails(t *testing.T) {
	if err := runStoreLs(cliCtx(), io.Discard, deadStore{}, storeLsOpts{now: cliNow}); err == nil {
		t.Error("ls on dead store succeeded, want an error")
	}
}

// Every ls line is a real write: a breaking pipe surfaces the
// failure at the header or the row, long or short. If this fails,
// truncated tables read as complete inventory.
func TestStoreLsWriteFailuresSurface(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	for _, tc := range []struct {
		name string
		opts storeLsOpts
		n    int
	}{
		{"long header", storeLsOpts{now: cliNow, long: true}, 0},
		{"long row", storeLsOpts{now: cliNow, long: true}, 1},
		{"short header", storeLsOpts{now: cliNow}, 0},
		{"short row", storeLsOpts{now: cliNow}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runStoreLs(cliCtx(), &failAfterWriter{n: tc.n}, s, tc.opts); err == nil {
				t.Errorf("ls %s into breaking pipe succeeded, want an error", tc.name)
			}
		})
	}
}

// The ghosts command wires all three witnesses through real deps:
// the store pairs to the served generation, the fs view reads the
// registry's own data dir, the catalog 404s the gone repo — and the
// ghost lists. Procurement failures refuse instead of listing
// blind, so an early return with empty output fails here. If this
// fails, `store ls ghosts` never ran past unit fakes.
func TestStoreLsGhostsCommandListsAgreedGone(t *testing.T) {
	data := t.TempDir()
	gen, gerr := sentinel.NewGen()
	if gerr != nil {
		t.Fatalf("mint gen: %v", gerr)
	}
	id, ierr := sentinel.NewGen()
	if ierr != nil {
		t.Fatalf("mint id: %v", ierr)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(data, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: id, TS: now, Writer: "kpr-gc"}); err != nil {
		t.Fatalf("stage served generation: %v", err)
	}
	srv := serveDiskRegistry(t, data, `{"repositories":[]}`)
	defer srv.Close()
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte("storage:\n  filesystem:\n    rootdirectory: "+data+"\n"), 0o644); err != nil {
		t.Fatalf("stage registry config: %v", err)
	}
	t.Setenv(config.EnvRegistryConfig, cfgPath)
	dir := t.TempDir()
	func() {
		t.Setenv(config.EnvStore, "file")
		t.Setenv(config.EnvStoreDir, dir)
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
		_ = s.Record(c, policy.Row{Repo: "gone", Tag: "v1", Digest: "sha256:a",
			PushedAt: cliNow.Add(-200 * 24 * time.Hour)})
	}()
	// Cobra keeps parsed flag values on the shared command: start
	// from defaults so no earlier invocation leaks in.
	for _, f := range [][2]string{{"long", "false"}, {"json", "false"}} {
		if err := storeLsCmd.Flags().Set(f[0], f[1]); err != nil {
			t.Fatalf("reset --%s: %v", f[0], err)
		}
	}
	out, err := runCmdWithArgs(t, dir, srv.URL, storeLsCmd, []string{"ghosts"})
	if err != nil {
		t.Fatalf("ls ghosts: %v", err)
	}
	if !strings.Contains(out, "gone:v1") {
		t.Errorf("ghosts lack the agreed-gone row:\n%s", out)
	}
}

// The ls flag plumbing splits at the command, not just the unit:
// `ls sentinels` through RunE shows machinery, bare ls shows
// inventory. If this fails, the flag misroutes the view.
func TestStoreLsCommandSplitsSentinels(t *testing.T) {
	dir := t.TempDir()
	srv := serveRegistry(t, t.TempDir(), false)
	defer srv.Close()
	func() {
		t.Setenv(config.EnvStore, "file")
		t.Setenv(config.EnvStoreDir, dir)
		backend, storeDir := resolveTestBackend(t)
		s, err := deps.OpenStore(config.NewBuilder().FromEnv().Build(), backend, storeDir)
		if err != nil {
			t.Fatalf("open file store: %v", err)
		}
		defer func() { _ = s.Close() }()
		c := context.Background()
		_ = s.Record(c, policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:a", PushedAt: cliNow})
		_ = s.Record(c, policy.Row{Repo: "noroutine/kpr-sentinel", Tag: "019-gen",
			Digest: "sha256:b", PushedAt: cliNow})
	}()
	plain, err := runCmdWithArgs(t, dir, srv.URL, storeLsCmd, nil)
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(plain, "app:v1") || strings.Contains(plain, "kpr-sentinel") {
		t.Errorf("ls through the command mixes the views:\n%s", plain)
	}
	mach, err := runCmdWithArgs(t, dir, srv.URL, storeLsCmd, []string{"sentinels"})
	if err != nil {
		t.Fatalf("ls sentinels: %v", err)
	}
	if !strings.Contains(mach, "kpr-sentinel") || strings.Contains(mach, "app:v1") {
		t.Errorf("ls sentinels through the command mixes the views:\n%s", mach)
	}
}

// ls splits inventory from machinery: default shows tracked rows
// without sentinels, `ls sentinels` shows only them. If this
// fails, the split the console depends on collapses to one view.
func TestStoreLsSplitsSentinels(t *testing.T) {
	s := store.NewMemStore()
	seedRows(s)
	seedSentinel(s)
	var plain bytes.Buffer
	if err := runStoreLs(cliCtx(), &plain, s, storeLsOpts{now: cliNow}); err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(plain.String(), "app:v1") {
		t.Errorf("ls lacks the tracked row:\n%s", plain.String())
	}
	if strings.Contains(plain.String(), "kpr-sentinel") {
		t.Errorf("ls leaks machinery:\n%s", plain.String())
	}
	var mach bytes.Buffer
	if err := runStoreLs(cliCtx(), &mach, s, storeLsOpts{now: cliNow, sentinels: true}); err != nil {
		t.Fatalf("ls sentinels: %v", err)
	}
	if !strings.Contains(mach.String(), "kpr-sentinel") {
		t.Errorf("ls sentinels lacks the generation:\n%s", mach.String())
	}
	if strings.Contains(mach.String(), "app:v1") {
		t.Errorf("ls sentinels leaks inventory:\n%s", mach.String())
	}
}
