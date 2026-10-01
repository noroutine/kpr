package policy

import (
	"fmt"
	"regexp"
	"strings"
)

// regexPrefix opts a pattern into full regex (unanchored, like the
// reap --exclude expressions). Without it the pattern is a
// Kyverno-style glob: * matches any run (slashes included), ? one
// char, everything else literal.
const regexPrefix = "regex:"

// MatchImage reports whether pattern matches the qualified image
// name (repo:tag, registry stripped). Empty patterns and invalid
// regex: expressions refuse — a typo must never pattern the world.
func MatchImage(pattern, qualified string) (bool, error) {
	if pattern == "" {
		return false, fmt.Errorf("empty image pattern matches nothing")
	}
	if rest, ok := strings.CutPrefix(pattern, regexPrefix); ok {
		re, err := regexp.Compile(rest)
		if err != nil {
			return false, fmt.Errorf("invalid regex %q: %w", pattern, err)
		}
		return re.MatchString(qualified), nil
	}
	return regexp.MatchString(globToRegex(pattern), qualified)
}

// globToRegex anchors a Kyverno-style glob: * becomes .*, ? becomes
// ., every other byte literal.
func globToRegex(glob string) string {
	var b strings.Builder
	b.WriteString("^(?:")
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	b.WriteString(")$")
	return b.String()
}

// ParseExactImage splits repo:tag on the last colon (the tag
// separator; tracked repos never carry a port — the registry is
// stripped). Wildcards refuse: exact means exact.
func ParseExactImage(image string) (repo, tag string, err error) {
	if strings.ContainsAny(image, "*?") {
		return "", "", fmt.Errorf("not an exact image %q: wildcards need plan add/remove, not reap add", image)
	}
	idx := strings.LastIndex(image, ":")
	// NOTE(mutants): <= is equivalent — idx 0 (":tag") falls into
	// the identical empty-repo refusal below, same message.
	if idx < 0 {
		return "", "", fmt.Errorf("not an exact image %q: want repo:tag", image)
	}
	repo, tag = image[:idx], image[idx+1:]
	if repo == "" || tag == "" {
		return "", "", fmt.Errorf("not an exact image %q: want repo:tag", image)
	}
	return repo, tag, nil
}
