// Package gc is becoming the garbage-collection use case: today it
// owns the write sentinel (probe the registry writable/readonly);
// the same-store proofs, the collector run, and the lock handling
// follow in later slices. Driving adapters (cli) parse flags, call
// in, and render the event stream.
package gc

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProbeRepo is the throwaway repo the gc sentinel uploads under. A
// cancelled initiate leaves no blob, no manifest, no residue.
const ProbeRepo = "kpr-gc-probe"

// Mode is what the write sentinel found: the registry takes writes,
// refuses them (v3 maintenance readonly), or answered something the
// probe cannot classify.
type Mode int

const (
	ModeUnknown Mode = iota
	ModeWritable
	ModeReadonly
)

// ModeName renders the sentinel verdict for messages.
func ModeName(m Mode) string {
	switch m {
	case ModeWritable:
		return "writable"
	case ModeReadonly:
		return "readonly"
	default:
		return "unknown"
	}
}

// ProbeRegistryMode reports the sentinel verdict for baseURL as
// writable, readonly, or unknown (with the error). Exported so the
// e2e suite drives the same probe collection runs — one path, never
// a copy.
func ProbeRegistryMode(ctx context.Context, baseURL string) (string, error) {
	mode, _, err := ProbeRegistry(ctx, baseURL)
	if err != nil {
		return "unknown", err
	}
	if mode == ModeUnknown {
		return "unknown", fmt.Errorf("sentinel inconclusive for %s", baseURL)
	}
	return ModeName(mode), nil
}

// ProbeRegistry initiates a blob upload under the probe repo: 202
// means writable (the upload is cancelled at once, leaving nothing),
// 405 means maintenance readonly. Anything else is inconclusive and
// an error — gc fails closed rather than collecting blind. The upload
// id returns with the writable verdict for the same-store proof.
func ProbeRegistry(ctx context.Context, baseURL string) (Mode, string, error) {
	endpoint := strings.TrimSuffix(baseURL, "/") + "/v2/" + ProbeRepo + "/blobs/uploads/"
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return ModeUnknown, "", err
	}
	resp, err := client.Do(req) //nolint:gosec // operator-configured registry peer, no body
	if err != nil {
		return ModeUnknown, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusAccepted:
		// Best effort: the upload never held content, and abandoned
		// uploads purge server-side — but cancel it anyway. Location
		// is usually absolute; resolve a relative one (tests, some
		// frontings) against the peer.
		uuid := uploadUUID(resp.Header.Get("Location"))
		if loc := resp.Header.Get("Location"); loc != "" {
			if !strings.HasPrefix(loc, "http://") && !strings.HasPrefix(loc, "https://") {
				loc = strings.TrimSuffix(baseURL, "/") + "/" + strings.TrimPrefix(loc, "/")
			}
			del, derr := http.NewRequestWithContext(ctx, http.MethodDelete, loc, nil)
			if derr == nil {
				dresp, derr := client.Do(del) //nolint:gosec // cancel of our own probe upload
				if derr == nil {
					_ = dresp.Body.Close()
				}
			}
		}
		return ModeWritable, uuid, nil
	case http.StatusMethodNotAllowed:
		return ModeReadonly, "", nil
	default:
		return ModeUnknown, "", fmt.Errorf("sentinel POST %s: status %d, want 202 (writable) or 405 (readonly)", endpoint, resp.StatusCode)
	}
}

// uploadUUID extracts the upload id from a blobs/uploads Location
// (absolute or relative, query stripped): the handle the same-store
// proof keys on. Unparseable locations prove nothing.
func uploadUUID(loc string) string {
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, s := range segs {
		if s == "uploads" && i+1 < len(segs) {
			return segs[i+1]
		}
	}
	return ""
}
