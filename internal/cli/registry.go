package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Inspect the distribution registry itself",
}

// plural renders a counted noun: 1 repo, 2 repos. Shared across
// commands (analyze, store backfill, store status) — it lives
// with the registry parent, not with any one command.
// plural renders a counted noun: 1 repo, 2 repos. Participles
// (tracked, recorded, untagged) and substance labels (GiB blobs)
// stay invariant — only true nouns pluralize.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func init() {
	RootCmd.AddCommand(registryCmd)
}
