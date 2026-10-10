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

	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/registryfs"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// stageAnalyzeStore builds a one-repo v2 layout and points the
// command at it through the environment: the registry config path
// is env, never a flag. One tag, one revision, one 8-byte blob,
// one upload session, one layer link — every nonzero column shows.
func stageAnalyzeStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	v2 := filepath.Join(root, "docker", "registry", "v2")
	files := map[string]string{
		"repositories/app/_manifests/tags/v1/current/link":      "sha256:aaa",
		"repositories/app/_manifests/revisions/sha256/aaa/link": "sha256:aaa",
		"repositories/app/_layers/sha256/bbb/link":              "sha256:bbb",
		"repositories/app/_uploads/uuid-1/startedAt":            "x",
		"blobs/sha256/bb/bbb/data":                              "12345678",
	}
	for rel, body := range files {
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
	t.Setenv(config.EnvRegistryConfig, cfg)
	return cfg
}

// stageCatalogServer serves one repo with two tags: the API sees
// more than the fs walk staged, so the comparison has a delta.
func stageCatalogServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/_catalog":
			_, _ = io.WriteString(w, `{"repositories":["app"]}`)
		case "/v2/app/tags/list":
			_, _ = io.WriteString(w, `{"name":"app","tags":["v1","v2"]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runAnalyzeCmd(t *testing.T, cfgPath string, reg backfill.Registry, args ...string) (string, error) {
	t.Helper()
	// The store view reads ambient config: pin an empty file
	// store so the store line is deterministic, never whatever
	// the developer's shell points at.
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	var buf bytes.Buffer
	registryAnalyzeCmd.SetOut(&buf)
	defer registryAnalyzeCmd.SetOut(nil)
	registryAnalyzeCmd.SetContext(context.Background())
	if err := registryAnalyzeCmd.Flags().Set("json", "false"); err != nil {
		t.Fatalf("reset --json: %v", err)
	}
	t.Cleanup(func() { _ = registryAnalyzeCmd.Flags().Set("json", "false") })
	for _, a := range args {
		if a == "--json" {
			if err := registryAnalyzeCmd.Flags().Set("json", "true"); err != nil {
				t.Fatalf("set --json: %v", err)
			}
		}
	}
	err := runRegistryAnalyze(context.Background(), &buf, cfgPath, reg, registryAnalyzeJSON)
	return buf.String(), err
}

// The prime rides the catalog line as its own field, like the
// store status: present is structure, anything else calls for
// investigation. If this fails, the pointer is back inside the
// count with no annotation.
func TestCatalogLineShowsPrimeStatus(t *testing.T) {
	for _, tc := range []struct {
		prime sentinel.Prime
		want  string
	}{
		{sentinel.PrimePresent, "catalog: 0 repos, 0 tags, 0 sentinels, prime status: present"},
		{sentinel.PrimeMissing, "catalog: 0 repos, 0 tags, 0 sentinels, prime status: missing"},
		{sentinel.PrimeCorrupt, "catalog: 0 repos, 0 tags, 0 sentinels, prime status: corrupt"},
	} {
		if got := catalogLine(backfill.CatalogReport{Prime: tc.prime}); got != tc.want {
			t.Errorf("catalogLine(%q) = %q, want %q", tc.prime, got, tc.want)
		}
	}
}

// Analyze reports the staged magnitude as five copypastable
// lines, grouped by sense: catalog shape, fs-vs-catalog shape,
// manifests, blobs, bytes. The API sees the same repo but two
// tags, so the fs delta reads -1 tag (fs minus API, converging
// from below as the walk counts up); one revision is tag-named,
// so untagged is 0; the staged links hold 30 bytes of metadata.
// Values start in the same column under every label. If this
// fails, the command miscounts or misrenders.
func TestRegistryAnalyzeReportsCounters(t *testing.T) {
	cfgPath := stageAnalyzeStore(t)
	srv := stageCatalogServer(t)
	reg := registry.NewClient(srv.URL)
	out, err := runAnalyzeCmd(t, cfgPath, reg)
	if err != nil {
		t.Fatalf("analyze = %v, want report", err)
	}
	want := []string{
		"catalog: 1 repo, 2 tags, 0 sentinels, prime status: missing",
		"store  : 0 repos, 0 tags, 0 sentinels, store status: unpaired",
		"store Δ: -1 repo, -2 tags, +0 sentinels",
		"fs     : 1 repo, 1 tag, 0 sentinels, 0 husks",
		"fs Δ   : +0 repos, -1 tag, +0 sentinels",
		"revs   : 1 revision, 0 untagged",
		"blobs  : 1 blob, 1 layer link, 1 upload",
		"size   : 8 B blobs",
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(want) {
		t.Fatalf("analyze has %d lines, want %d:\n%s", len(lines), len(want), out)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i, lines[i], w)
		}
	}
}

// All eight lines align: values start in the same column under
// every label, and each byte scale sits last on its line. If this
// fails, a label width drifted and the block stops scanning.
func TestRegistryAnalyzeLinesAlign(t *testing.T) {
	fsRep := registryfs.Report{Repos: 600, Tags: 17050, Revisions: 24993, Blobs: 55077,
		BlobBytes: 679001899008, Uploads: 1, LayerLinks: 101173}
	api := backfill.CatalogReport{Repos: 600, Tags: 17050, Prime: sentinel.PrimePresent}
	view := storeView{ok: true, repos: 599, tags: 17048, sentinels: 1}
	lines := analyzeLines(api, fsRep, view, true)
	if len(lines) != 8 {
		t.Fatalf("analyzeLines has %d lines, want 8", len(lines))
	}
	for _, l := range lines {
		r := []rune(l)
		if len(r) < 10 || r[7] != ':' || r[8] != ' ' {
			t.Errorf("line misaligned: %q", l)
		}
	}
	if !strings.HasPrefix(lines[1], "store  : ") || !strings.HasPrefix(lines[2], "store Δ: ") ||
		!strings.HasPrefix(lines[4], "fs Δ   : ") || !strings.HasPrefix(lines[6], "blobs  : ") || !strings.HasPrefix(lines[7], "size   : ") {
		t.Errorf("lines = %q, want store/delta, fs/delta, blobs-then-size last", lines)
	}
	if want := "store  : 599 repos, 17048 tags, 1 sentinel"; lines[1] != want {
		t.Errorf("store line = %q, want %q", lines[1], want)
	}
	if want := "store Δ: -1 repo, -2 tags, +1 sentinel"; lines[2] != want {
		t.Errorf("store delta = %q, want %q", lines[2], want)
	}
	single := analyzeLines(backfill.CatalogReport{Repos: 1, Tags: 1, Sentinels: 1},
		registryfs.Report{Repos: 1, Tags: 1, Sentinels: 1},
		storeView{ok: true, repos: 1, tags: 1, sentinels: 1}, true)
	if !strings.Contains(single[0], "1 repo, 1 tag, 1 sentinel") {
		t.Errorf("catalog line = %q, want singular nouns", single[0])
	}
	if !strings.Contains(single[4], "+0 repos, +0 tags, +0 sentinels") {
		t.Errorf("fs delta = %q, want signed singular nouns", single[4])
	}
}

// Each byte figure picks its own unit for its scale: bytes stay
// bytes, gibibytes stay gibibytes. If this fails, a magnitude
// reads in the wrong unit.
// Analyze speaks full JSON for scripts: the exact key set, exact
// values. If this fails, piping breaks.
func TestRegistryAnalyzeJSON(t *testing.T) {
	cfgPath := stageAnalyzeStore(t)
	srv := stageCatalogServer(t)
	reg := registry.NewClient(srv.URL)
	out, err := runAnalyzeCmd(t, cfgPath, reg, "--json")
	if err != nil {
		t.Fatalf("analyze --json = %v, want report", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("analyze --json is not JSON: %v:\n%s", err, out)
	}
	want := map[string]any{
		"repos": float64(1), "tags": float64(1), "revisions": float64(1),
		"blobs": float64(1), "blob_bytes": float64(8),
		"uploads": float64(1), "layer_links": float64(1),
		"sentinels":   float64(0),
		"store_repos": float64(0), "store_tags": float64(0),
		"store_sentinels": float64(0), "store_ok": true, "store_note": "unpaired",
		"api_repos": float64(1), "api_tags": float64(2), "api_sentinels": float64(0),
		"api_prime":     "missing",
		"dangling_tags": float64(0), "dangling_layers": float64(0),
		"husks": float64(0), "husk_repos": nil,
	}
	if len(got) != len(want) {
		t.Fatalf("analyze --json has %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("analyze --json %q = %v, want %v", k, got[k], w)
		}
	}
}

// The --json spelling parses through argv: registration, name,
// and effect all arrive. If this fails, the flag is declared but
// unwired.
func TestRegistryAnalyzeJSONViaArgv(t *testing.T) {
	stageAnalyzeStore(t)
	srv := stageCatalogServer(t)
	t.Setenv(config.EnvRegistryURL, srv.URL)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	RootCmd.SetArgs([]string{"registry", "analyze", "--json"})
	defer RootCmd.SetArgs(nil)
	var buf bytes.Buffer
	registryAnalyzeCmd.SetOut(&buf)
	defer registryAnalyzeCmd.SetOut(nil)
	if err := RootCmd.Execute(); err != nil {
		t.Fatalf("registry analyze --json = %v, want report", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("argv --json is not JSON: %v:\n%s", err, buf.String())
	}
	if got["repos"] != float64(1) || got["api_tags"] != float64(2) {
		t.Errorf("argv --json = %v, want fs repos 1 + api tags 2", got)
	}
}

// blindRegistry enumerates but serves no blobs: a client shaped
// like the backfill port without the proof port. readStoreView
// must still snapshot rows and name the proof unproven, never
// refuse the walk. If this fails, a limited client vetoes
// registry insight.
type blindRegistry struct{}

func (blindRegistry) CatalogAll(context.Context) ([]string, error) { return nil, nil }
func (blindRegistry) Catalog(context.Context, string) ([]string, error) {
	return nil, nil
}
func (blindRegistry) ManifestDigest(context.Context, string, string) (string, string, error) {
	return "", "", nil
}

func TestReadStoreViewWithoutProofPort(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	view := readStoreView(context.Background(), blindRegistry{})
	if !view.ok || view.note != "unproven" {
		t.Errorf("view = %+v, want ok with unproven note", view)
	}
}

// A long row name clamps its pad instead of running the gutter
// negative: columns align, never overlap. If this fails, wide
// names glue into their bodies.
func TestAnalyzeRowClampsLongName(t *testing.T) {
	if got := analyzeRow("averylongname", "b"); got != "averylongname: b" {
		t.Errorf("analyzeRow(long) = %q, want zero pad", got)
	}
}

// An unreadable store degrades the view instead of failing it:
// no rows readable means an empty view, never an error. If this
// fails, a blinded disk errors the analyze header.
func TestReadStoreViewUnreadableStoreDegrades(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through file permissions")
	}
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	if err := s.Record(context.Background(), policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:a", PushedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("stage row: %v", err)
	}
	rows := filepath.Join(dir, "rows")
	if err := os.Chmod(rows, 0o000); err != nil {
		t.Fatalf("blind rows: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(rows, 0o755) })
	t.Setenv(config.EnvStoreDir, dir)
	if view := readStoreView(context.Background(), blindRegistry{}); view.ok {
		t.Errorf("view over unreadable store = %+v, want not-ok", view)
	}
}

// A torn identity degrades the proof note instead of the rows:
// the inventory reads, the lineage doesn't prove. If this fails,
// an unparseable identity hides the whole store.
func TestReadStoreViewTornIdentityDegrades(t *testing.T) {
	dir := t.TempDir()
	s := store.NewFileStore(dir)
	if err := s.Record(context.Background(), policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:a", PushedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("stage row: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte(`{"half`), 0o644); err != nil {
		t.Fatalf("stage torn identity: %v", err)
	}
	t.Setenv(config.EnvStoreDir, dir)
	view := readStoreView(context.Background(), blindRegistry{})
	if !view.ok || view.note != "unproven" {
		t.Errorf("view over torn identity = %+v, want ok with unproven note", view)
	}
}

// Dead pointers tail the revs and blobs lines only when present:
// a clean walk reads exactly as before, a dirty one names its
// count (singular included). If this fails, dead links hide or
// clean blocks grow noise.
func TestAnalyzeLinesNamesDangling(t *testing.T) {
	fsRep := registryfs.Report{Repos: 2, Tags: 3, Revisions: 2, Blobs: 2,
		LayerLinks: 2, DanglingTags: 2, DanglingLayers: 1}
	lines := analyzeLines(backfill.CatalogReport{}, fsRep, storeView{}, true)
	if got := lines[5]; got != "revs   : 2 revisions, 0 untagged, 2 dangling tag links" {
		t.Errorf("revs line = %q, want dangling tail", got)
	}
	if got := lines[6]; !strings.HasSuffix(got, "1 dangling layer link") {
		t.Errorf("blobs line = %q, want dangling tail", got)
	}
	clean := analyzeLines(backfill.CatalogReport{}, registryfs.Report{}, storeView{}, true)
	if strings.Contains(clean[3], "dangling") || strings.Contains(clean[4], "dangling") {
		t.Errorf("clean lines = %q, %q, want no dangling tails", clean[3], clean[4])
	}
}

// The fs line always carries its husk count: 0 husks on a clean
// walk reads as a verdict, not a missing tail. Names ride --json
// only — 150 rows never fit a block. If this fails, husks hide.
func TestAnalyzeLinesNamesHusks(t *testing.T) {
	fsRep := registryfs.Report{Repos: 3, Tags: 1, HuskRepos: []string{"bare", "nest/husk"}, Husks: 2}
	lines := analyzeLines(backfill.CatalogReport{}, fsRep, storeView{}, true)
	if got := lines[3]; got != "fs     : 1 repo, 1 tag, 0 sentinels, 2 husks" {
		t.Errorf("fs line = %q, want repos-minus-husks with husk tail", got)
	}
	clean := analyzeLines(backfill.CatalogReport{}, registryfs.Report{}, storeView{}, true)
	if got := clean[3]; got != "fs     : 0 repos, 0 tags, 0 sentinels, 0 husks" {
		t.Errorf("clean fs line = %q, want zero husks shown", got)
	}
}

// summarizeRows groups tracked rows the store line's way:
// distinct repos over everything, tags over everything (like the
// catalog and fs views count them), sentinel rows split out as a
// memo. If this fails, the store Δ tags compares scoped counts
// against unscoped ones.
func TestSummarizeRows(t *testing.T) {
	rows := []policy.Row{
		{Repo: "app", Tag: "v1"},
		{Repo: "app", Tag: "v2"},
		{Repo: "noroutine/kpr-sentinel", Tag: "gen"},
	}
	repos, tags, sentinels := summarizeRows(rows)
	if repos != 2 || tags != 3 || sentinels != 1 {
		t.Errorf("summarizeRows = %d repos, %d tags, %d sentinels, want 2, 3, 1",
			repos, tags, sentinels)
	}
}

// The store line reads tracked rows against the catalog: distinct
// repos over everything, tags over everything, sentinel rows as a
// memo, deltas store-minus-API. If this fails, the store view lies
// about what `store ls` holds.
func TestRegistryAnalyzeStoreLine(t *testing.T) {
	cfgPath := stageAnalyzeStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/_catalog":
			_, _ = io.WriteString(w, `{"repositories":["app","noroutine/kpr-sentinel"]}`)
		case "/v2/app/tags/list":
			_, _ = io.WriteString(w, `{"name":"app","tags":["v1","v2"]}`)
		case "/v2/noroutine/kpr-sentinel/tags/list":
			_, _ = io.WriteString(w, `{"name":"noroutine/kpr-sentinel","tags":["gen"]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, dir)
	st := store.NewFileStore(dir)
	for _, row := range []policy.Row{
		{Repo: "app", Tag: "v1", Digest: "sha256:aaa", PushedAt: time.Now().UTC()},
		{Repo: "noroutine/kpr-sentinel", Tag: "gen", Digest: "sha256:bbb", PushedAt: time.Now().UTC()},
	} {
		if err := st.Record(context.Background(), row); err != nil {
			t.Fatalf("record %s:%s: %v", row.Repo, row.Tag, err)
		}
	}
	var buf bytes.Buffer
	err := runRegistryAnalyze(context.Background(), &buf, cfgPath, registry.NewClient(srv.URL), false)
	if err != nil {
		t.Fatalf("analyze = %v, want report", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 8 {
		t.Fatalf("analyze has %d lines, want 8:\n%s", len(lines), buf.String())
	}
	if want := "store  : 2 repos, 2 tags, 1 sentinel, store status: unpaired"; lines[1] != want {
		t.Errorf("store line = %q, want %q", lines[1], want)
	}
	if want := "store Δ: +0 repos, -1 tag, +0 sentinels"; lines[2] != want {
		t.Errorf("store delta = %q, want %q", lines[2], want)
	}
}

// A dead backend degrades the store line instead of refusing the
// walk: a read-only magnitude stays available when redis is
// down. If this fails, store trouble vetoes registry insight.
func TestRegistryAnalyzeStoreUnavailable(t *testing.T) {
	cfgPath := stageAnalyzeStore(t)
	srv := stageCatalogServer(t)
	reg := registry.NewClient(srv.URL)
	t.Setenv(config.EnvStore, "redis")
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	var buf bytes.Buffer
	registryAnalyzeCmd.SetOut(&buf)
	defer registryAnalyzeCmd.SetOut(nil)
	err := runRegistryAnalyze(context.Background(), &buf, cfgPath, reg, false)
	if err != nil {
		t.Fatalf("analyze with dead store = %v, want degraded walk", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 8 {
		t.Fatalf("analyze has %d lines, want 8:\n%s", len(lines), buf.String())
	}
	if lines[1] != "store  : unavailable" {
		t.Errorf("store line = %q, want honest unavailable", lines[1])
	}
	if lines[2] != "store Δ: unavailable" {
		t.Errorf("store delta = %q, want honest unavailable", lines[2])
	}
}

// Analyze refuses before walking when the config names no
// filesystem store, naming the refusal. If this fails, non-local
// stores analyze or refuse namelessly.
func TestRegistryAnalyzeRefusesS3(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  s3:\n    bucket: blobs\n"), 0o600); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	srv := stageCatalogServer(t)
	reg := registry.NewClient(srv.URL)
	_, err := runAnalyzeCmd(t, cfg, reg)
	if err == nil {
		t.Fatal("analyze on s3 config succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "filesystem") {
		t.Errorf("refusal = %q, want it to name the filesystem demand", err.Error())
	}
}
