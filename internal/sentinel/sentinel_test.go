package sentinel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NewGen mints ordered generations: UUIDv7 strings sort by wall
// time, so two observed generations compare without parsing
// timestamps. If this fails, recency needs the payload clock.
func TestNewGenOrdersByTime(t *testing.T) {
	a, err := NewGen()
	if err != nil {
		t.Fatalf("NewGen: %v", err)
	}
	if len(a) != 36 || a[14] != '7' {
		t.Errorf("generation = %q, want a UUIDv7 string", a)
	}
}

// Writing a generation must lay out exactly the files distribution
// serves: two global blobs (payload as config, manifest) plus the
// layer, revision, and tag links — nothing else. If this fails, the
// API serves 404s or the collector trips over the residue.
func TestWriteLaysOutExactFiles(t *testing.T) {
	root := t.TempDir()
	md, err := Write(root, "kpr-sentinel", "live", Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000007", TS: "2026-09-30T11:00:00Z", Writer: "test"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasPrefix(md, "sha256:") || len(md) != 7+64 {
		t.Fatalf("manifest digest = %q, want sha256:<64hex>", md)
	}
	mhex := strings.TrimPrefix(md, "sha256:")
	want := map[string]string{
		filepath.Join("docker", "registry", "v2", "repositories", "kpr-sentinel", "_manifests", "revisions", "sha256", mhex, "link"):             md,
		filepath.Join("docker", "registry", "v2", "repositories", "kpr-sentinel", "_manifests", "tags", "live", "current", "link"):               md,
		filepath.Join("docker", "registry", "v2", "repositories", "kpr-sentinel", "_manifests", "tags", "live", "index", "sha256", mhex, "link"): md,
	}
	var payloadDigest string
	for path, wantBody := range want {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(raw) != wantBody {
			t.Errorf("%s = %q, want %q (exact, no trailing newline)", path, raw, wantBody)
		}
	}
	// The manifest blob must digest-match its own filename and carry
	// the payload digest as config; the payload blob must carry the
	// generation. Both keyed by content hash, like the store does.
	manRaw, err := os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", mhex[:2], mhex, "data"))
	if err != nil {
		t.Fatalf("read manifest blob: %v", err)
	}
	if got := digestOf(manRaw); got != md {
		t.Errorf("manifest blob digests to %q, want filename %q", got, md)
	}
	var man struct {
		SchemaVersion int `json:"schemaVersion"`
		Config        struct {
			Digest string `json:"digest"`
			Size   int    `json:"size"`
		} `json:"config"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(manRaw, &man); err != nil {
		t.Fatalf("manifest not JSON: %v", err)
	}
	if man.SchemaVersion != 2 {
		t.Errorf("schemaVersion = %d, want 2 (else GET 500s and gc aborts)", man.SchemaVersion)
	}
	if man.Annotations["kpr.gen"] != "0193abcd-0000-7000-8000-000000000007" {
		t.Errorf("annotations[kpr.gen] = %q, want the generation", man.Annotations["kpr.gen"])
	}
	payloadDigest = man.Config.Digest
	phex := strings.TrimPrefix(payloadDigest, "sha256:")
	payRaw, err := os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", phex[:2], phex, "data"))
	if err != nil {
		t.Fatalf("read payload blob: %v", err)
	}
	var pay Payload
	if err := json.Unmarshal(payRaw, &pay); err != nil || pay.Gen != "0193abcd-0000-7000-8000-000000000007" || pay.V != 1 {
		t.Errorf("payload = %q, want generation 7 as JSON", payRaw)
	}
	if man.Config.Size != len(payRaw) {
		t.Errorf("config.size = %d, want payload bytes %d", man.Config.Size, len(payRaw))
	}
	linkRaw, err := os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "repositories", "kpr-sentinel", "_layers", "sha256", phex, "link"))
	if err != nil {
		t.Fatalf("read layer link: %v", err)
	}
	if string(linkRaw) != payloadDigest {
		t.Errorf("layer link = %q, want %q", linkRaw, payloadDigest)
	}
}

// A second generation repoints the tag and leaves the old revision
// behind (untagged — the collector's --delete-untagged run sweeps
// it). If this fails, updates pile up tags or orphan the old link,
// and the proof reads a stale generation.
func TestWriteRepointMovesTag(t *testing.T) {
	root := t.TempDir()
	md1, err := Write(root, "kpr-sentinel", "live", Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000001"})
	if err != nil {
		t.Fatalf("Write gen1: %v", err)
	}
	md2, err := Write(root, "kpr-sentinel", "live", Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000002"})
	if err != nil {
		t.Fatalf("Write gen2: %v", err)
	}
	if md1 == md2 {
		t.Fatalf("generations share digest %q, want distinct", md1)
	}
	cur, err := os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "repositories", "kpr-sentinel", "_manifests", "tags", "live", "current", "link"))
	if err != nil {
		t.Fatalf("read current link: %v", err)
	}
	if string(cur) != md2 {
		t.Errorf("current link = %q, want gen2 %q", cur, md2)
	}
	// Old revision stays (untagged, collectable), new index entry added.
	for _, md := range []string{md1, md2} {
		mhex := strings.TrimPrefix(md, "sha256:")
		if _, err := os.Stat(filepath.Join(root, "docker", "registry", "v2", "repositories", "kpr-sentinel", "_manifests", "revisions", "sha256", mhex, "link")); err != nil {
			t.Errorf("revision %q missing: %v", md, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docker", "registry", "v2", "repositories", "kpr-sentinel", "_manifests", "tags", "live", "index", "sha256", strings.TrimPrefix(md1, "sha256:"), "link")); err != nil {
		t.Errorf("gen1 index entry missing: %v", err)
	}
}

// Repo and tag become directory names: empty or escaping values
// must refuse before touching the store. If this fails, a crafted
// name writes outside the root.
func TestWriteRejectsBadNames(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ repo, tag string }{
		{"", "live"},
		{"kpr-sentinel", ""},
		{"../escape", "live"},
		{"kpr-sentinel", "../escape"},
		{"kpr-sentinel", "a/b"},
	} {
		if _, err := Write(root, tc.repo, tc.tag, Payload{V: 1}); err == nil {
			t.Errorf("Write(%q, %q) accepted, want refusal", tc.repo, tc.tag)
		}
	}
}
