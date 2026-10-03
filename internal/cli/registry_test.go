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

	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/registryfs"
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
		"catalog: 1 repos, 2 tags",
		"fs     : 1 repos, 1 tags, Δ repos: +0, Δ tags: -1",
		"revs   : 1 revisions, 0 untagged",
		"blobs  : 1 blobs, 1 layer links, 1 uploads",
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

// All five lines align: values start in the same column under
// every label, and each byte scale sits last on its line. If this
// fails, a label width drifted and the block stops scanning.
func TestRegistryAnalyzeLinesAlign(t *testing.T) {
	fsRep := registryfs.Report{Repos: 600, Tags: 17050, Revisions: 24993, Blobs: 55077,
		BlobBytes: 679001899008, Uploads: 1, LayerLinks: 101173}
	api := backfill.CatalogReport{Repos: 600, Tags: 17050}
	lines := analyzeLines(api, fsRep, true)
	if len(lines) != 5 {
		t.Fatalf("analyzeLines has %d lines, want 5", len(lines))
	}
	for _, l := range lines {
		if len(l) < 10 || l[7] != ':' || l[8] != ' ' {
			t.Errorf("line misaligned: %q", l)
		}
	}
	if !strings.HasSuffix(lines[3], "uploads") || !strings.HasSuffix(lines[4], "blobs") {
		t.Errorf("trailing lines = %q, %q, want counts-then-scale order", lines[3], lines[4])
	}
}

// Each byte figure picks its own unit for its scale: bytes stay
// bytes, gibibytes stay gibibytes. If this fails, a magnitude
// reads in the wrong unit.
func TestHumanBytesScale(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{71, "71 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{9580388, "9.14 MiB"},
		{679001899008, "632.37 GiB"},
		{1 << 50, "1.00 PiB"},
	} {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

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
		"api_repos": float64(1), "api_tags": float64(2),
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
