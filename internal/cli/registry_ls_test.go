package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Registry sentinels list evaluated: every tag the registry names
// with the identity behind it — generation, age, writer. A tag
// whose manifest won't parse warns past on stderr and skips, so
// one dangling tag never vetoes the listing. If this fails, the
// registry view of machinery lies or refuses.
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
	if join(fields(lines[0])) != "REPO:TAG GEN AGE WRITER" {
		t.Errorf("header = %q, want tag/gen/age/writer columns", lines[0])
	}
	if got := join(fields(lines[1])); got != "noroutine/kpr-sentinel:gen-1 gen-1 1h30m0s ago kpr-unlock" {
		t.Errorf("gen-1 = %q, want evaluated identity", got)
	}
	if got := join(fields(lines[3])); got != "noroutine/kpr-sentinel:latest gen-2 1h30m0s ago kpr-gc" {
		t.Errorf("latest = %q, want floater resolved to its generation", got)
	}
	if !strings.Contains(errW.String(), "broken") {
		t.Errorf("stderr = %q, want the dangling tag warned past", errW.String())
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
