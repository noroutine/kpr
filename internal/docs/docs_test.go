package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every top-level docs page links from the index: an
// unlinked page is invisible to anyone browsing from the
// README, which is how DURABILITY and PROFILING sat
// orphaned. The index itself is the only exclusion —
// subdirectories (attic, agents) keep their own order.
// If this fails, link the new page from docs/README.md.
func TestEveryPageLinksFromIndex(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	index := string(raw)
	pages, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("no docs pages found; the tree moved out from under this test")
	}
	for _, page := range pages {
		name := filepath.Base(page)
		if name == "README.md" {
			continue
		}
		if !strings.Contains(index, "("+name) {
			t.Errorf("%s links from nowhere; add it to docs/README.md", name)
		}
	}
}
