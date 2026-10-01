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

// The floater tag is "latest" on purpose: every policy selector
// spares that exact name, so keep-N can run over the sentinel repo
// with no excludes and never mark the proof due. If this fails, the
// proof is one unexcluded reap away from collection.
func TestFloaterTagIsLatest(t *testing.T) {
	if Tag != "latest" {
		t.Errorf("Tag = %q, want \"latest\" (the policy-spared name)", Tag)
	}
}

// Writing a generation must lay out exactly the files distribution
// serves: two global blobs (payload as config, manifest) plus the
// layer, revision, and two tag links (floater and generation) —
// nothing else. If this fails, the API serves 404s or the collector
// trips over the residue.
// testIdentity is the lineage every test mint belongs to: Write
// carries whatever the minter sets, so tests set it like a paired
// minter would.
const testIdentity = "0193abcd-0000-7000-8000-0000000000aa"

func TestWriteLaysOutExactFiles(t *testing.T) {
	root := t.TempDir()
	md, err := Write(root, Repo, Tag, Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000007", ID: testIdentity, TS: "2026-09-30T11:00:00Z", Writer: "test"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasPrefix(md, "sha256:") || len(md) != 7+64 {
		t.Fatalf("manifest digest = %q, want sha256:<64hex>", md)
	}
	mhex := strings.TrimPrefix(md, "sha256:")
	want := map[string]string{
		filepath.Join("docker", "registry", "v2", "repositories", Repo, "_manifests", "revisions", "sha256", mhex, "link"):                                             md,
		filepath.Join("docker", "registry", "v2", "repositories", Repo, "_manifests", "tags", Tag, "current", "link"):                                                  md,
		filepath.Join("docker", "registry", "v2", "repositories", Repo, "_manifests", "tags", Tag, "index", "sha256", mhex, "link"):                                    md,
		filepath.Join("docker", "registry", "v2", "repositories", Repo, "_manifests", "tags", "0193abcd-0000-7000-8000-000000000007", "current", "link"):               md,
		filepath.Join("docker", "registry", "v2", "repositories", Repo, "_manifests", "tags", "0193abcd-0000-7000-8000-000000000007", "index", "sha256", mhex, "link"): md,
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
	if pay.ID != testIdentity {
		t.Errorf("payload id = %q, want the lineage identity the mint recorded", pay.ID)
	}
	if man.Config.Size != len(payRaw) {
		t.Errorf("config.size = %d, want payload bytes %d", man.Config.Size, len(payRaw))
	}
	linkRaw, err := os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "repositories", Repo, "_layers", "sha256", phex, "link"))
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
	md1, err := Write(root, Repo, Tag, Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000001", ID: testIdentity})
	if err != nil {
		t.Fatalf("Write gen1: %v", err)
	}
	md2, err := Write(root, Repo, Tag, Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000002", ID: testIdentity})
	if err != nil {
		t.Fatalf("Write gen2: %v", err)
	}
	if md1 == md2 {
		t.Fatalf("generations share digest %q, want distinct", md1)
	}
	cur, err := os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "repositories", Repo, "_manifests", "tags", Tag, "current", "link"))
	if err != nil {
		t.Fatalf("read current link: %v", err)
	}
	if string(cur) != md2 {
		t.Errorf("current link = %q, want gen2 %q", cur, md2)
	}
	// Old revision stays (untagged, collectable), new index entry added.
	for _, md := range []string{md1, md2} {
		mhex := strings.TrimPrefix(md, "sha256:")
		if _, err := os.Stat(filepath.Join(root, "docker", "registry", "v2", "repositories", Repo, "_manifests", "revisions", "sha256", mhex, "link")); err != nil {
			t.Errorf("revision %q missing: %v", md, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docker", "registry", "v2", "repositories", Repo, "_manifests", "tags", Tag, "index", "sha256", strings.TrimPrefix(md1, "sha256:"), "link")); err != nil {
		t.Errorf("gen1 index entry missing: %v", err)
	}
}

// Every mint links its generation as a tag beside the floater:
// <gen>/current resolves the same digest, so history is enumerable
// from tags/list and keep-N can reap it. If this fails, old
// generations go untagged and the litter is unmanageable again.
func TestWriteLinksGenerationTag(t *testing.T) {
	root := t.TempDir()
	gen := "0193abcd-0000-7000-8000-000000000007"
	md, err := Write(root, Repo, Tag, Payload{V: 1, Gen: gen, ID: testIdentity})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	mhex := strings.TrimPrefix(md, "sha256:")
	for _, path := range []string{
		filepath.Join(root, "docker", "registry", "v2", "repositories", Repo, "_manifests", "tags", gen, "current", "link"),
		filepath.Join(root, "docker", "registry", "v2", "repositories", Repo, "_manifests", "tags", gen, "index", "sha256", mhex, "link"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(raw) != md {
			t.Errorf("%s = %q, want %q", path, raw, md)
		}
	}
}

// Repo and tag become directory names: empty or escaping values
// must refuse before touching the store, and every boundary rune of
// the tag shape must decide correctly (word edges a/z/A/Z/0-9/_,
// first-char word-only, 128 max). If this fails, a crafted name
// writes outside the root — or a valid tag refuses a generation.
func TestWriteValidatesNames(t *testing.T) {
	root := t.TempDir()
	gen := Payload{V: 1, Gen: "0193abcd-0000-7000-8000-000000000007", ID: testIdentity}
	for _, tag := range []string{
		"live", "v1", "_", "a", "z", "A", "Z", "0", "9",
		"aZ09_.-b", strings.Repeat("a", 128),
	} {
		if _, err := Write(root, "kpr", tag, gen); err != nil {
			t.Errorf("Write(kpr, %q) refused, want accept", tag)
		}
	}
	for _, tc := range []struct{ repo, tag string }{
		{"", "live"},
		{"kpr", ""},
		{"../escape", "live"},
		{"kpr", "../escape"},
		{"kpr", "a/b"},
		{"kpr", ".v1"},
		{"kpr", "-v1"},
		{"kpr", "a:b"},
		{"kpr", "a b"},
		{"kpr", "a`b"},
		{"kpr", "a{b"},
		{"kpr", strings.Repeat("a", 129)},
	} {
		if _, err := Write(root, tc.repo, tc.tag, gen); err == nil {
			t.Errorf("Write(%q, %q) accepted, want refusal", tc.repo, tc.tag)
		}
	}
	// The generation is a tag too: empty or escaping gens refuse even
	// when repo and tag are fine.
	for _, bad := range []string{"", "../escape", "a b", strings.Repeat("a", 129)} {
		if _, err := Write(root, "kpr", "v1", Payload{V: 1, Gen: bad}); err == nil {
			t.Errorf("Write(gen %q) accepted, want refusal", bad)
		}
	}
}
