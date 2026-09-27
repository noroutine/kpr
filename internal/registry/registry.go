// Package registry speaks the plain distribution API: kpr stays a
// dumb-registry companion (see docs/PLAN.md), so this client needs no
// auth, no catalog extensions — DELETE manifests, list tags, probe.
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
// call so a wedged registry fails into next-tick retry, never a hang.
type Client struct {
	base   string
	client *http.Client
}

// NewClient builds a client for a registry base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		base:   strings.TrimSuffix(baseURL, "/"),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) get(ctx context.Context, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
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
