package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
)

// API is the read-back half of the sentinel: the manifest a tag
// serves, the blob a digest serves. internal/registry carries the
// production implementation; tests carry stubs. Read-only — the
// sentinel never writes through the API.
type API interface {
	GetManifest(ctx context.Context, repo, ref string) ([]byte, error)
	GetBlob(ctx context.Context, repo, digest string) ([]byte, error)
}

// readManifest is the fields Read trusts: schemaVersion 2 plus the
// config reference. Anything else is not a generation Write made.
type readManifest struct {
	SchemaVersion int `json:"schemaVersion"`
	Config        struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

// Read returns the sentinel generation the API serves for repo:tag:
// the manifest's config digest, resolved to the payload behind it,
// plus the manifest's own digest for cross-checks. Whatever the API
// cannot explain — unknown tag, unparseable manifest, missing blob —
// refuses: absence of proof is never generation 0.
func Read(ctx context.Context, api API, repo, tag string) (Payload, string, error) {
	if repo == "" || tag == "" {
		return Payload{}, "", fmt.Errorf("sentinel: read %s:%s: empty repo or tag", repo, tag)
	}
	manRaw, err := api.GetManifest(ctx, repo, tag)
	if err != nil {
		return Payload{}, "", fmt.Errorf("sentinel: read %s:%s: %w", repo, tag, err)
	}
	var man readManifest
	if err := json.Unmarshal(manRaw, &man); err != nil {
		return Payload{}, "", fmt.Errorf("sentinel: read %s:%s: manifest unparseable: %w", repo, tag, err)
	}
	if man.SchemaVersion != 2 || man.Config.Digest == "" {
		return Payload{}, "", fmt.Errorf("sentinel: read %s:%s: not a sentinel generation", repo, tag)
	}
	payRaw, err := api.GetBlob(ctx, repo, man.Config.Digest)
	if err != nil {
		return Payload{}, "", fmt.Errorf("sentinel: read %s:%s: %w", repo, tag, err)
	}
	var p Payload
	if err := json.Unmarshal(payRaw, &p); err != nil {
		return Payload{}, "", fmt.Errorf("sentinel: read %s:%s: payload unparseable: %w", repo, tag, err)
	}
	if p.V != 1 {
		return Payload{}, "", fmt.Errorf("sentinel: read %s:%s: payload version %d, want 1", repo, tag, p.V)
	}
	return p, digestOf(manRaw), nil
}

// Mismatch means the store answered the sentinel read with a
// generation this writer did not write. That is a stale snapshot of
// a once-shared store — something served sentinel content, just not
// ours — as opposed to a Read failure, which is no evidence at all
// (stranger's store, down registry, unparseable bytes). The type
// carries the distinction so callers can policy on it: gc refuses
// both, backfill (M4) treats a mismatch as snapshot age, not absence.
type Mismatch struct {
	Repo, Tag string
	Got, Want string
}

func (e *Mismatch) Error() string {
	return fmt.Sprintf("sentinel: %s:%s serves generation %s, want %s", e.Repo, e.Tag, e.Got, e.Want)
}

// Verify is the proof primitive: the served generation must equal
// the written one. A mismatch refuses typed; a read failure refuses
// plain.
func Verify(ctx context.Context, api API, repo, tag string, wantGen string) error {
	got, _, err := Read(ctx, api, repo, tag)
	if err != nil {
		return err
	}
	if got.Gen != wantGen {
		return &Mismatch{Repo: repo, Tag: tag, Got: got.Gen, Want: wantGen}
	}
	return nil
}
