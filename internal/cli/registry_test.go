package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// stageAnalyzeStore builds a one-repo v2 layout and points the
// command at it through the environment: the registry config path
// is env, never a flag. One tag, one revision, one 8-byte blob,
// one upload session, one layer link — every nonzero column shows.
func stageAnalyzeStore(t *testing.T) {
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
}

func runAnalyzeCmd(t *testing.T, args ...string) (string, error) {
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
	err := registryAnalyzeCmd.RunE(registryAnalyzeCmd, nil)
	return buf.String(), err
}

// Analyze reports the staged magnitude in columns, each value
// under its header: a transposed column fails. If this fails, the
// command miscounts or misrenders.
func TestRegistryAnalyzeReportsColumns(t *testing.T) {
	stageAnalyzeStore(t)
	out, err := runAnalyzeCmd(t)
	if err != nil {
		t.Fatalf("analyze = %v, want report", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("analyze has %d lines, want header + row:\n%s", len(lines), out)
	}
	header := strings.Fields(lines[0])
	row := strings.Fields(lines[1])
	if len(header) != len(row) {
		t.Fatalf("header/row width mismatch:\n%s", out)
	}
	want := map[string]string{
		"REPOS": "1", "TAGS": "1", "REVISIONS": "1", "BLOBS": "1",
		"BLOB_BYTES": "8", "UPLOADS": "1", "LAYER_LINKS": "1",
	}
	if len(header) != len(want) {
		t.Fatalf("header has %d columns, want %d:\n%s", len(header), len(want), out)
	}
	for i, h := range header {
		w, ok := want[h]
		if !ok {
			t.Errorf("unexpected column %q", h)
			continue
		}
		if row[i] != w {
			t.Errorf("column %s = %q, want %q", h, row[i], w)
		}
	}
}

// Analyze speaks full JSON for scripts: the exact key set, exact
// values. If this fails, piping breaks.
func TestRegistryAnalyzeJSON(t *testing.T) {
	stageAnalyzeStore(t)
	out, err := runAnalyzeCmd(t, "--json")
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
	if got["repos"] != float64(1) {
		t.Errorf("argv --json repos = %v, want 1", got["repos"])
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
	t.Setenv(config.EnvRegistryConfig, cfg)
	_, err := runAnalyzeCmd(t)
	if err == nil {
		t.Fatal("analyze on s3 config succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "filesystem") {
		t.Errorf("refusal = %q, want it to name the filesystem demand", err.Error())
	}
}
