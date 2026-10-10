package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/policy"
)

// splitRef cuts an exact repo:tag, sharing the split with
// policy.ParseExactImage (one parser for the concept —
// plan-side stays the pattern world). The wildcard refusal keeps
// store wording: ParseExactImage's message points at plan/reap,
// which would misdirect here.
func splitRef(ref string) (repo, tag string, err error) {
	if strings.ContainsAny(ref, "*?") {
		return "", "", fmt.Errorf("not an exact image %q: globs need plan add/remove, not store", ref)
	}
	return policy.ParseExactImage(ref)
}

// rowJSON is the piped shape of a tracked row.
type rowJSON struct {
	Repo      string `json:"repo"`
	Tag       string `json:"tag"`
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	PushedAt  string `json:"pushed_at"`
	Actor     string `json:"actor"`
	Due       bool   `json:"due"`
	Reason    string `json:"reason,omitempty"`
}

func rowToJSON(r policy.Row) rowJSON {
	return rowJSON{Repo: r.Repo, Tag: r.Tag, Digest: r.Digest,
		MediaType: r.MediaType, PushedAt: r.PushedAt.UTC().Format(time.RFC3339),
		Actor: r.Actor, Due: r.Due, Reason: r.Reason}
}

var storeCmd = &cobra.Command{
	Use:   "store",
	Short: "Inspect and prune tracked store rows",
}

func init() {
	RootCmd.AddCommand(storeCmd)
}
