// Package sentinel crafts the live sentinel image straight onto the
// registry's filesystem store: a payload config blob carrying
// structured info (generation, timestamp, writer) plus a minimal OCI
// manifest tagging it, with the exact layout distribution serves.
// The API never sees a write — it only reads back what the files
// say. Consumed by gc (same-store proof) and backfill (snapshot
// detection); the registry's own collector sweeps stale generations.
package sentinel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Payload is the structured info in the sentinel config blob: schema
// version, writer-side generation, wall timestamp, writer name. Blobs
// are never validated on read, so any JSON serves — the version
// keeps future readers honest.
type Payload struct {
	V      int    `json:"v"`
	Gen    int    `json:"gen"`
	TS     string `json:"ts,omitempty"`
	Writer string `json:"writer,omitempty"`
}

// Manifest media types, fixed: the revision must parse as OCI (else
// GET 500s and the collector aborts its mark phase), references may
// point anywhere.
const (
	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	configMediaType   = "application/vnd.oci.image.config.v1+json"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum)
}

// layout joins the distribution filesystem layout below root.
func layout(root string, elems ...string) string {
	return filepath.Join(append([]string{root, "docker", "registry", "v2"}, elems...)...)
}

func blobFile(root, digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	return layout(root, "blobs", "sha256", hex[:2], hex, "data")
}

// validRepo allows nested names (a/b) but no escaping elements.
func validRepo(repo string) bool {
	if repo == "" {
		return false
	}
	for _, el := range strings.Split(repo, "/") {
		if el == "" || el == "." || el == ".." {
			return false
		}
	}
	return true
}

// validTag follows the OCI tag shape: word char first, then
// word/dot/dash, at most 128. Tags become directory names —
// anything else risks the layout.
func validTag(tag string) bool {
	if tag == "" || len(tag) > 128 {
		return false
	}
	for i, c := range tag {
		word := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if i == 0 {
			if !word {
				return false
			}
			continue
		}
		if !word && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

func putFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Write crafts one sentinel generation under root: payload blob,
// manifest blob, layer/revision/tag links, and returns the manifest
// digest. Order is crash-safe: blobs and side links first, the tag
// switch last via atomic rename — an interrupted write leaves
// unreferenced blobs (the collector sweeps those), never a tag
// pointing at a half-written generation. Writes are idempotent:
// same payload rewrites identical bytes.
func Write(root, repo, tag string, p Payload) (string, error) {
	if !validRepo(repo) {
		return "", fmt.Errorf("sentinel: bad repo %q", repo)
	}
	if !validTag(tag) {
		return "", fmt.Errorf("sentinel: bad tag %q", tag)
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("sentinel: marshal payload: %w", err)
	}
	pDigest := digestOf(payload)
	man, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		Config:        descriptor{MediaType: configMediaType, Digest: pDigest, Size: len(payload)},
		Layers:        []descriptor{},
		Annotations:   map[string]string{"kpr.sentinel": "1", "kpr.gen": fmt.Sprint(p.Gen)},
	})
	if err != nil {
		return "", fmt.Errorf("sentinel: marshal manifest: %w", err)
	}
	mDigest := digestOf(man)
	mHex := strings.TrimPrefix(mDigest, "sha256:")
	pHex := strings.TrimPrefix(pDigest, "sha256:")
	if err := putFile(blobFile(root, pDigest), payload); err != nil {
		return "", err
	}
	if err := putFile(blobFile(root, mDigest), man); err != nil {
		return "", err
	}
	base := layout(root, "repositories", repo)
	for _, link := range []struct {
		path string
		body string
	}{
		{filepath.Join(base, "_layers", "sha256", pHex, "link"), pDigest},
		{filepath.Join(base, "_manifests", "revisions", "sha256", mHex, "link"), mDigest},
		{filepath.Join(base, "_manifests", "tags", tag, "index", "sha256", mHex, "link"), mDigest},
	} {
		if err := putFile(link.path, []byte(link.body)); err != nil {
			return "", err
		}
	}
	current := filepath.Join(base, "_manifests", "tags", tag, "current", "link")
	if err := putFile(current+".tmp", []byte(mDigest)); err != nil {
		return "", err
	}
	if err := os.Rename(current+".tmp", current); err != nil {
		return "", err
	}
	return mDigest, nil
}
