package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"
)

// Absent reports read silence: the tag serves nothing — a 404 from
// the registry, or a missing file behind a fake. Silence means
// establish-or-refuse-dry, never corruption (unparseable bytes) and
// never transport failure: both refuse everywhere. A nil error is
// never absence.
func Absent(err error) bool {
	if err == nil {
		return false
	}
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) {
		return sc.StatusCode() == http.StatusNotFound
	}
	return errors.Is(err, fs.ErrNotExist)
}

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

// ProofStaleAfter is the age past which a served generation stops
// meaning "recently proven": tag lifecycle keeps working, but blob
// reclamation hasn't been demonstrated within the window. A loud
// line, never a refusal — staleness degrades, it doesn't gate.
// NOTE(mutants): only the collapse is observable — zero makes
// every proof stale and the 3-day-fresh test goes red; the
// 3-day/8-day literals pin days-against-hours, and finer steps
// change nothing a test should observe.
const ProofStaleAfter = 7 * 24 * time.Hour

// LastProof reads the live generation at the fixed sentinel address:
// what the registry serves right now, whoever proved it. Absence
// refuses (never proven, or nothing served) — callers render that
// as "unproven", never as generation zero.
func LastProof(ctx context.Context, api API) (Payload, error) {
	p, _, err := Read(ctx, api, Repo, Tag)
	if err != nil {
		return Payload{}, err
	}
	return p, nil
}

// Age reports how long ago p was proven, from its wall timestamp.
// gc writes RFC3339; anything unparseable (or unwritten) refuses —
// an age nobody can compute is not zero.
func (p Payload) Age(now time.Time) (time.Duration, error) {
	ts, err := time.Parse(time.RFC3339, p.TS)
	if err != nil {
		return 0, fmt.Errorf("sentinel: generation %q has unparseable timestamp %q", p.Gen, p.TS)
	}
	return now.Sub(ts), nil
}
