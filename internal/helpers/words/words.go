// Package words holds the human display words every command
// shares: counted nouns, nothing else. Command-specific
// renderers stay with their commands — this package grows only
// when a second command counts the same way.
package words

import "fmt"

// Plural renders a counted noun: 1 repo, 2 repos. Participles
// (tracked, recorded, untagged) and substance labels (GiB blobs)
// stay invariant — only true nouns pluralize, and the caller
// says so by passing one word twice.
func Plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// SignedPlural signs a delta with singular nouns at ±1: +1 repo,
// -1 tag, +0 sentinels. Deltas read signed; plain counts don't.
func SignedPlural(n int, one, many string) string {
	noun := many
	if n == 1 || n == -1 {
		noun = one
	}
	return fmt.Sprintf("%+d %s", n, noun)
}
