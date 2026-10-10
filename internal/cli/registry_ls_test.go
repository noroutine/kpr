package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// mountAPI serves sentinel manifests straight off a staged mount:
// the same shared layout production reads through the registry.
// Staging uses sentinel.Write, so payloads are real.
type mountAPI struct{ root string }

func stageSentinelTag(t *testing.T, root, tag string, p sentinel.Payload) {
	t.Helper()
	if _, err := sentinel.Write(root, sentinel.Repo, tag, p); err != nil {
		t.Fatalf("stage sentinel tag %s: %v", tag, err)
	}
}

func (f mountAPI) Catalog(_ context.Context, repo string) ([]string, error) {
	dir := filepath.Join(f.root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags")
	kids, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, k := range kids {
		if k.IsDir() {
			tags = append(tags, k.Name())
		}
	}
	return tags, nil
}

func (f mountAPI) GetManifest(_ context.Context, repo, tag string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2",
		"repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return nil, err
	}
	hex := strings.TrimSpace(string(raw))
	hex = strings.TrimPrefix(hex, "sha256:")
	return os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2",
		"blobs", "sha256", hex[:2], hex, "data"))
}

func (f mountAPI) GetBlob(_ context.Context, repo, digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	return os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2",
		"blobs", "sha256", hex[:2], hex, "data"))
}

// serveDiskRegistry serves a staged data dir as a registry:
// sentinel manifests and blobs off the mount, a fixed _catalog
// body, 404 everywhere else. Command-level procurement (prove, fs
// view, catalog) runs against bytes instead of mocks.
func serveDiskRegistry(t *testing.T, data, catalog string) *httptest.Server {
	t.Helper()
	disk := mountAPI{root: data}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" {
			_, _ = w.Write([]byte(catalog))
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v2/"+sentinel.Repo)
		var raw []byte
		var err error
		switch {
		case strings.HasPrefix(p, "/manifests/"):
			raw, err = disk.GetManifest(r.Context(), sentinel.Repo, strings.TrimPrefix(p, "/manifests/"))
		case strings.HasPrefix(p, "/blobs/"):
			raw, err = disk.GetBlob(r.Context(), sentinel.Repo, strings.TrimPrefix(p, "/blobs/"))
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(raw)
	}))
}

// Registry sentinels list evaluated: every tag the registry names
// with age and writer — the generation duplicates the tag, so the
// short view drops it (the store view shape, writer for due). A
// tag whose manifest won't parse warns past on stderr and skips,
// so one dangling tag never vetoes the listing. If this fails,
// the registry view of machinery lies or refuses.
func TestRegistryLsSentinels(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)
	ts := now.Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	stageSentinelTag(t, root, "gen-1", sentinel.Payload{V: 1, Gen: "gen-1", ID: "id-1", TS: ts, Writer: "kpr-unlock"})
	stageSentinelTag(t, root, "gen-2", sentinel.Payload{V: 1, Gen: "gen-2", ID: "id-1", TS: ts, Writer: "kpr-gc"})
	stageSentinelTag(t, root, "latest", sentinel.Payload{V: 1, Gen: "gen-2", ID: "id-1", TS: ts, Writer: "kpr-gc"})
	if err := os.MkdirAll(filepath.Join(root, "docker", "registry", "v2",
		"repositories", sentinel.Repo, "_manifests", "tags", "broken"), 0o755); err != nil {
		t.Fatalf("stage broken tag dir: %v", err)
	}
	var out, errW bytes.Buffer
	if err := runRegistryLs(context.Background(), &out, &errW, mountAPI{root}, "sentinels", now, false, false); err != nil {
		t.Fatalf("ls sentinels = %v, want listing", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("ls has %d lines, want header + 3:\n%s", len(lines), out.String())
	}
	// tabwriter aligns: compare fields, not spacing.
	fields := func(l string) []string { return strings.Fields(l) }
	join := func(f []string) string { return strings.Join(f, " ") }
	if join(fields(lines[0])) != "REPO:TAG AGE WRITER" {
		t.Errorf("header = %q, want tag/age/writer columns", lines[0])
	}
	if got := join(fields(lines[1])); got != "noroutine/kpr-sentinel:gen-1 1h30m ago kpr-unlock" {
		t.Errorf("gen-1 = %q, want evaluated age and writer", got)
	}
	if got := join(fields(lines[3])); got != "noroutine/kpr-sentinel:latest 1h30m ago kpr-gc" {
		t.Errorf("latest = %q, want floater listed like any tag", got)
	}
	if !strings.Contains(errW.String(), "broken") {
		t.Errorf("stderr = %q, want the dangling tag warned past", errW.String())
	}
}

// The help names the store-side twin: operators diffing views must
// find `store ls sentinels` from `registry ls --help`. If this
// fails, the help points at one view while the other moved.
// The long view names every column and row: the header prints,
// every row prints, and the buffer flushes — an early return would
// hand back empty output. If this fails, --long lists nothing
// while reporting success.
func TestRegistryLsSentinelsLong(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)
	ts := now.Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	stageSentinelTag(t, root, "gen-1", sentinel.Payload{V: 1, Gen: "gen-1", ID: "id-1", TS: ts, Writer: "kpr-unlock"})
	var out, errW bytes.Buffer
	if err := runRegistryLs(context.Background(), &out, &errW, mountAPI{root}, "sentinels", now, false, true); err != nil {
		t.Fatalf("ls sentinels --long = %v, want listing", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("ls --long has %d lines, want header + 1:\n%s", len(lines), out.String())
	}
	fields := func(l string) []string { return strings.Fields(l) }
	join := func(f []string) string { return strings.Join(f, " ") }
	if join(fields(lines[0])) != "REPO:TAG GEN ID DIGEST PUSHED WRITER" {
		t.Errorf("header = %q, want the long columns", lines[0])
	}
	if got := join(fields(lines[1])); !strings.HasPrefix(got, "noroutine/kpr-sentinel:gen-1 gen-1 id-1") || !strings.Contains(got, "kpr-unlock") {
		t.Errorf("row = %q, want gen/id/writer named", got)
	}
}

// The listing speaks JSON for diffing against `store ls --json`:
// one object per tag with the evaluated identity. If this fails,
// registry-vs-store joins stay manual.
func TestRegistryLsSentinelsJSON(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)
	ts := now.Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	stageSentinelTag(t, root, "gen-1", sentinel.Payload{V: 1, Gen: "gen-1", ID: "id-9", TS: ts, Writer: "kpr-unlock"})
	var out, errW bytes.Buffer
	if err := runRegistryLs(context.Background(), &out, &errW, mountAPI{root}, "sentinels", now, true, false); err != nil {
		t.Fatalf("ls --json = %v, want listing", err)
	}
	var got []map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("ls --json is not JSON: %v:\n%s", err, out.String())
	}
	if len(got) != 1 {
		t.Fatalf("ls --json has %d objects, want 1", len(got))
	}
	for _, k := range []string{"repo", "tag", "gen", "id", "ts", "writer", "digest"} {
		if _, ok := got[0][k]; !ok {
			t.Errorf("ls --json omits %q: %v", k, got[0])
		}
	}
	if got[0]["gen"] != "gen-1" || got[0]["writer"] != "kpr-unlock" {
		t.Errorf("ls --json = %v, want evaluated identity", got[0])
	}
}

// Anything but sentinels refuses naming the supported target: tag
// payloads only mean something for machinery. If this fails, the
// command pretends to evaluate what it cannot.
func TestRegistryLsRefusesOtherTargets(t *testing.T) {
	var out, errW bytes.Buffer
	err := runRegistryLs(context.Background(), &out, &errW, mountAPI{t.TempDir()}, "app", time.Now().UTC(), false, false)
	if err == nil || !strings.Contains(err.Error(), "sentinels") {
		t.Errorf("ls app = %v, want the supported target named", err)
	}
}

// An unreadable catalog refuses the listing instead of printing
// an empty table: no tags is data, unreadable tags is failure. If
// this fails, a down registry reads as an empty one.
func TestRegistryLsCatalogFailureRefuses(t *testing.T) {
	var out, errW bytes.Buffer
	if err := runRegistryLs(context.Background(), &out, &errW, mountAPI{}, "sentinels", time.Now().UTC(), false, false); err == nil {
		t.Error("ls sentinels off an empty mount succeeded, want refusal")
	}
}

// Every write in the listing surfaces: short, long, and JSON
// paths each fail loud at every failing prefix instead of
// truncating quiet. If this fails, a broken pipe reads as a
// complete listing.
func TestRegistryLsWriteFailuresSurface(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)
	ts := now.Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	stageSentinelTag(t, root, "gen-1", sentinel.Payload{V: 1, Gen: "gen-1", ID: "id-1", TS: ts, Writer: "kpr-unlock"})
	for _, tc := range []struct {
		name   string
		asJSON bool
		long   bool
	}{
		{"short", false, false},
		{"long", false, true},
		{"json", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var good bytes.Buffer
			if err := runRegistryLs(context.Background(), &good, io.Discard, mountAPI{root}, "sentinels", now, tc.asJSON, tc.long); err != nil {
				t.Fatalf("stage success: %v", err)
			}
			sawNil := false
			for n := 0; n < 100; n++ {
				w := &failAfterWriter{n: n}
				err := runRegistryLs(context.Background(), w, io.Discard, mountAPI{root}, "sentinels", now, tc.asJSON, tc.long)
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

// An unreadable tag warns past on stderr — and a stderr that
// won't take the warning fails the listing instead of swallowing
// it. If this fails, a broken errW hides skipped tags.
func TestRegistryLsErrWriterFailureRefuses(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docker", "registry", "v2",
		"repositories", sentinel.Repo, "_manifests", "tags", "broken"), 0o755); err != nil {
		t.Fatalf("stage broken tag dir: %v", err)
	}
	var out bytes.Buffer
	if err := runRegistryLs(context.Background(), &out, &failAfterWriter{}, mountAPI{root}, "sentinels", time.Now().UTC(), false, false); err == nil {
		t.Error("ls with failing stderr succeeded, want refusal")
	}
}

// A bad registry config refuses before any walk: the husk
// listing proves its filesystem view first, never a bare path.
// If this fails, a typo'd config walks the wrong tree.
func TestRegistryLsHusksBadConfigRefuses(t *testing.T) {
	if err := runRegistryLsHusks(io.Discard, filepath.Join(t.TempDir(), "missing.yml"), false, false); err == nil {
		t.Error("ls husks off a missing config succeeded, want refusal")
	}
}

// An unreadable mount fails the husk listing instead of
// listing blind: the walk names its outage. Root reads through
// permissions, so it sits this one out. If this fails, a
// blinded registry lists partial husks as all.
func TestRegistryLsHusksUnreadableRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	root := t.TempDir()
	repos := filepath.Join(root, "docker", "registry", "v2", "repositories", "app")
	if err := os.MkdirAll(filepath.Join(repos, "_manifests"), 0o755); err != nil {
		t.Fatalf("stage repo: %v", err)
	}
	if err := os.Chmod(repos, 0o000); err != nil {
		t.Fatalf("blind repo: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(repos, 0o755) })
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := runRegistryLsHusks(io.Discard, cfg, false, false); err == nil {
		t.Error("ls husks over blinded mount succeeded, want refusal")
	}
}

// Empty husks encode as an empty array, never null: scripts
// parsing the listing must not branch on null-vs-[]. If this
// fails, clean roots emit null.
func TestRegistryLsHusksEmptyIsArray(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docker", "registry", "v2", "repositories"), 0o755); err != nil {
		t.Fatalf("stage empty repos: %v", err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	var out bytes.Buffer
	if err := runRegistryLsHusks(&out, cfg, true, false); err != nil {
		t.Fatalf("ls husks --json = %v, want listing", err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Errorf("empty husks = %q, want []", got)
	}
}

// A failing stdout fails the husk listing, plain or JSON: names
// stream, so any write can be the one that breaks. If this
// fails, a truncated listing reads complete.
func TestRegistryLsHusksWriteFailureRefuses(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	p := filepath.Join(v2, "repositories", "bare", "_manifests", "revisions", "sha256", "bbb", "link")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("stage husk dir: %v", err)
	}
	if err := os.WriteFile(p, []byte("sha256:bbb"), 0o644); err != nil {
		t.Fatalf("stage husk link: %v", err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := runRegistryLsHusks(&failAfterWriter{}, cfg, false, false); err == nil {
		t.Error("ls husks with failing stdout succeeded, want refusal")
	}
	if err := runRegistryLsHusks(&failAfterWriter{}, cfg, true, false); err == nil {
		t.Error("ls husks --json with failing stdout succeeded, want refusal")
	}
}

// The husks target dispatches through the command: `registry ls
// husks` lists tagless repos off the mount, not the sentinel view.
// If this fails, the target dispatch drifted from the listing.
func TestRegistryLsHusksCommandDispatches(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "docker", "registry", "v2",
		"repositories", "bare", "_manifests", "revisions", "sha256", "bbb", "link")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.WriteFile(p, []byte("sha256:bbb"), 0o644); err != nil {
		t.Fatalf("stage link: %v", err)
	}
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	t.Setenv(config.EnvRegistryConfig, cfg)
	// Cobra keeps parsed flag values on the shared command: start
	// from defaults so no earlier invocation leaks in.
	for _, f := range [][2]string{{"long", "false"}, {"json", "false"}} {
		if err := registryLsCmd.Flags().Set(f[0], f[1]); err != nil {
			t.Fatalf("reset --%s: %v", f[0], err)
		}
	}
	out, err := runCmdWithArgs(t, t.TempDir(), "http://registry:5000", registryLsCmd, []string{"husks"})
	if err != nil {
		t.Fatalf("ls husks: %v", err)
	}
	if out != "bare\n" {
		t.Errorf("ls husks = %q, want the tagless repo", out)
	}
}

// An unparseable payload stamp reads unknown, never a
// million-hour age. If this fails, torn stamps age absurdly.
func TestLsAgeUnknownOnBadStamp(t *testing.T) {
	if got := lsAge(time.Now().UTC(), "not-a-time"); got != "unknown ts" {
		t.Errorf("lsAge(bogus) = %q, want unknown ts", got)
	}
}

// Husk listing names tagless repos straight off the mount: one per
// line, sorted, sentinel machinery excluded — the fast answer when
// analyze only counts them. --json emits the array for scripts;
// --long has no meaning here and refuses loud. If this fails, the
// listing disagrees with the walk or invents detail.
func TestRegistryLsHusks(t *testing.T) {
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	for rel, body := range map[string]string{
		"repositories/live/_manifests/tags/v1/current/link":                    "sha256:aaa",
		"repositories/bare/_manifests/revisions/sha256/bbb/link":               "sha256:bbb",
		"repositories/nest/husk/_manifests/revisions/sha256/c/link":            "sha256:ccc",
		"repositories/noroutine/kpr-shadow/_manifests/revisions/sha256/d/link": "sha256:ddd",
	} {
		p := filepath.Join(v2, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage dir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage file: %v", err)
		}
	}
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	var out bytes.Buffer
	if err := runRegistryLsHusks(&out, cfg, false, false); err != nil {
		t.Fatalf("ls husks = %v, want listing", err)
	}
	if got := out.String(); got != "bare\nnest/husk\n" {
		t.Errorf("ls husks = %q, want sorted tagless repos", got)
	}
	var jout bytes.Buffer
	if err := runRegistryLsHusks(&jout, cfg, true, false); err != nil {
		t.Fatalf("ls husks --json = %v, want listing", err)
	}
	var got []string
	if err := json.Unmarshal(jout.Bytes(), &got); err != nil {
		t.Fatalf("ls husks --json is not JSON: %v:\n%s", err, jout.String())
	}
	if len(got) != 2 || got[0] != "bare" || got[1] != "nest/husk" {
		t.Errorf("ls husks --json = %v, want [bare nest/husk]", got)
	}
	if err := runRegistryLsHusks(&bytes.Buffer{}, cfg, false, true); err == nil {
		t.Error("ls husks --long succeeded, want refusal (names only)")
	}
}

// The sentinels target dispatches through the command: `registry
// ls sentinels` evaluates tags off the API behind the configured
// URL, not the mount. If this fails, the target dispatch drifted
// from the listing.
func TestRegistryLsSentinelsCommandDispatches(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)
	ts := now.Add(-90 * time.Minute).UTC().Format(time.RFC3339)
	stageSentinelTag(t, root, "gen-1", sentinel.Payload{V: 1, Gen: "gen-1",
		ID: "id-1", TS: ts, Writer: "kpr-gc"})
	disk := mountAPI{root: root}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v2/"+sentinel.Repo)
		switch {
		case p == "/tags/list":
			_, _ = w.Write([]byte(`{"tags":["gen-1"]}`))
		case strings.HasPrefix(p, "/manifests/"):
			raw, err := disk.GetManifest(r.Context(), sentinel.Repo, strings.TrimPrefix(p, "/manifests/"))
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(raw)
		case strings.HasPrefix(p, "/blobs/"):
			raw, err := disk.GetBlob(r.Context(), sentinel.Repo, strings.TrimPrefix(p, "/blobs/"))
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(raw)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	// Cobra keeps parsed flag values on the shared command: start
	// from defaults so no earlier invocation leaks in.
	for _, f := range [][2]string{{"long", "false"}, {"json", "false"}} {
		if err := registryLsCmd.Flags().Set(f[0], f[1]); err != nil {
			t.Fatalf("reset --%s: %v", f[0], err)
		}
	}
	out, err := runCmdWithArgs(t, t.TempDir(), srv.URL, registryLsCmd, []string{"sentinels"})
	if err != nil {
		t.Fatalf("ls sentinels: %v", err)
	}
	if !strings.Contains(out, "noroutine/kpr-sentinel:gen-1") || !strings.Contains(out, "kpr-gc") {
		t.Errorf("ls sentinels = %q, want the staged tag with its writer", out)
	}
}
