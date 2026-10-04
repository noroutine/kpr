package kpr

import (
	"strings"
	"testing"
)

// The embed is the whole point of this package: if LICENSE stops
// flowing into the binary, kpr license prints a stub. If this fails,
// the go:embed path broke or the file emptied.
func TestTextIsGPL(t *testing.T) {
	for _, want := range []string{"GNU GENERAL PUBLIC LICENSE", "Version 3"} {
		if !strings.Contains(Text(), want) {
			t.Errorf("Text() misses %q", want)
		}
	}
}
