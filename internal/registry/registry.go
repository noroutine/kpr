// Package registry speaks the plain distribution API: kpr stays a
// dumb-registry companion (see docs/ARCHITECTURE.md) — anonymous or
// basic (one env-supplied pair): DELETE manifests, list tags,
// enumerate the catalog, HEAD digests, probe.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Delete outcomes. Gone (already deleted upstream) counts as success;
// Held (manifest owned by an index) untracks instead of retrying.
const (
	OutcomeDeleted = "deleted"
	OutcomeGone    = "gone"
	OutcomeHeld    = "held"
)

// Client is a minimal OCI distribution client. Timeouts bound every
// call so a wedged registry fails into the next asked pass's retry,
// never a hang.
type Client struct {
	base   string
	client *http.Client
	user   string
	pass   string
}

// SetBasicAuth presents the pair on every call (docs/ARCHITECTURE.md,
// Registry auth scope): open and htpasswd registries, which is all
// kpr claims. Empty user means anonymous — the default.
func (c *Client) SetBasicAuth(user, pass string) {
	c.user, c.pass = user, pass
}

// authorize stamps the pair when configured; anonymous registries
// travel headerless.
func (c *Client) authorize(req *http.Request) {
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
}

// NewClient builds a client for a registry base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		base: strings.TrimSuffix(baseURL, "/"),
		// NOTE(mutants): the 10s bound is liveness, not logic — no
		// test observes it (a hung peer is indistinguishable from a
		// slow one inside a unit run). Arithmetic here only moves the
		// deadline; every response classification is pinned by the
		// DeleteManifest table tests.
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) get(ctx context.Context, path string) (int, []byte, error) {
	return c.getAccept(ctx, path, "")
}

func (c *Client) getAccept(ctx context.Context, path, accept string) (int, []byte, error) {
	return c.getURL(ctx, c.base+path, accept)
}

func (c *Client) getURL(ctx context.Context, url, accept string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// apiCode pulls the first errors[].code from a distribution error body.
func apiCode(body []byte) string {
	var env struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Errors) == 0 {
		return ""
	}
	return env.Errors[0].Code
}

// DeleteManifest deletes one manifest by repo and reference (tag or
// digest). Gone and Held are successes with different row resolutions;
// any other failure is a retryable error and the row stays due.
func (c *Client) DeleteManifest(ctx context.Context, repo, ref string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		fmt.Sprintf("%s/v2/%s/manifests/%s", c.base, repo, ref), nil)
	if err != nil {
		return "", err
	}
	// Deleting by digest needs the manifest media type; accept anything.
	req.Header.Set("Accept", "*/*")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
		return OutcomeDeleted, nil
	case resp.StatusCode == http.StatusNotFound &&
		(apiCode(body) == "MANIFEST_UNKNOWN" || apiCode(body) == "NAME_UNKNOWN"):
		return OutcomeGone, nil
	case resp.StatusCode == http.StatusMethodNotAllowed && apiCode(body) == "DENIED":
		return OutcomeHeld, nil
	default:
		return "", fmt.Errorf("delete %s/%s: registry status %d", repo, ref, resp.StatusCode)
	}
}

// tagList is the /v2/<name>/tags/list body.
type tagList struct {
	Tags []string `json:"tags"`
}

// Catalog returns the live tag list for one repo (keep-N input).
func (c *Client) Catalog(ctx context.Context, repo string) ([]string, error) {
	status, body, err := c.get(ctx, "/v2/"+repo+"/tags/list")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("catalog %s: registry status %d", repo, status)
	}
	var list tagList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list.Tags, nil
}

// catalogPage is the /v2/_catalog body.
type catalogPage struct {
	Repositories []string `json:"repositories"`
}

// CatalogAll enumerates every repository through _catalog,
// following rel="next" links until a page arrives without one.
// A 401 names the credential vars — rejected creds refuse the
// run up front, never a silent empty enumeration. Any other
// non-200 is a StatusError for the caller to classify; an
// unreachable peer passes its error through. A repeated page
// errors: paging must progress.
func (c *Client) CatalogAll(ctx context.Context) ([]string, error) {
	var repos []string
	seen := map[string]bool{}
	next := "/v2/_catalog?n=100"
	for next != "" {
		if seen[next] {
			return nil, fmt.Errorf("catalog: paging looped on %q", next)
		}
		seen[next] = true
		url := next
		if strings.HasPrefix(url, "/") {
			url = c.base + url
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		c.authorize(req)
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("catalog: registry refused credentials (401) — check KPR_REGISTRY_USER / KPR_REGISTRY_PASSWORD")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &StatusError{Op: "catalog", Status: resp.StatusCode}
		}
		var page catalogPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("catalog: bad page: %w", err)
		}
		repos = append(repos, page.Repositories...)
		next = nextLink(resp.Header.Get("Link"))
	}
	return repos, nil
}

// nextLink pulls the rel="next" target out of a Link header:
// comma-separated <uri>; rel="next" segments, everything else
// ignored. Empty when the page is the last.
func nextLink(header string) string {
	for _, seg := range strings.Split(header, ",") {
		parts := strings.Split(seg, ";")
		if len(parts) < 2 {
			continue
		}
		uri := strings.Trim(strings.TrimSpace(parts[0]), "<>")
		for _, p := range parts[1:] {
			if strings.TrimSpace(p) == `rel="next"` {
				return uri
			}
		}
	}
	return ""
}

// ManifestDigest HEADs one tag's manifest, returning the digest the
// registry serves (Docker-Content-Digest) with the served media
// type. Headers only — backfill stamps rows without pulling
// layers. A non-200 is a StatusError for the caller to classify
// (404 skips by count); a 200 without a digest header errors — a
// digest backfill cannot stamp is not a row.
func (c *Client) ManifestDigest(ctx context.Context, repo, tag string) (digest, mediaType string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		fmt.Sprintf("%s/v2/%s/manifests/%s", c.base, repo, tag), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "*/*")
	c.authorize(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", &StatusError{Op: fmt.Sprintf("manifest %s:%s", repo, tag), Status: resp.StatusCode}
	}
	digest = resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", "", fmt.Errorf("manifest %s:%s: no Docker-Content-Digest served", repo, tag)
	}
	return digest, resp.Header.Get("Content-Type"), nil
}

// ociManifestType is the media type the sentinel reader asks for:
// without an Accept the registry 406s a manifest GET.
const ociManifestType = "application/vnd.oci.image.manifest.v1+json"

// GetManifest returns the exact manifest bytes a tag serves. Any
// non-200 is an error with the registry's status — absence of proof
// is never an empty manifest.
// StatusError reports a non-200 registry answer with its status:
// callers classify absence (404) without parsing messages. The text
// keeps the historical shape so logs and asserted outputs don't move.
type StatusError struct {
	Op     string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: registry status %d", e.Op, e.Status)
}

// StatusCode exposes the answer for classifier interfaces: callers
// match `interface{ StatusCode() int }` instead of importing the
// client for one type.
func (e *StatusError) StatusCode() int { return e.Status }

func (c *Client) GetManifest(ctx context.Context, repo, ref string) ([]byte, error) {
	status, body, err := c.getAccept(ctx, "/v2/"+repo+"/manifests/"+ref, ociManifestType)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &StatusError{Op: fmt.Sprintf("manifest %s:%s", repo, ref), Status: status}
	}
	return body, nil
}

// GetBlob returns the exact blob bytes a digest serves under repo.
// Any non-200 is an error — a missing blob is never empty bytes.
func (c *Client) GetBlob(ctx context.Context, repo, digest string) ([]byte, error) {
	status, body, err := c.get(ctx, "/v2/"+repo+"/blobs/"+digest)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &StatusError{Op: fmt.Sprintf("blob %s@%s", repo, digest), Status: status}
	}
	return body, nil
}

// Reachable probes the registry base for the console banner.
func (c *Client) Reachable(ctx context.Context) error {
	status, _, err := c.get(ctx, "/v2/")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("registry status %d", status)
	}
	return nil
}
