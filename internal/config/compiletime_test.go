package config

import (
	"bytes"
	"strings"
	"testing"
)

// The identity string is what operators paste into bug reports and what
// the console renders; it must name the binary and never come out empty,
// even from an ad hoc `go build` with no ldflags. If this fails, reports
// and dashboards can't say which build they came from.
func TestVersionStringNamesBinary(t *testing.T) {
	s := VersionString()
	if !strings.HasPrefix(s, Name+" ") {
		t.Errorf("VersionString = %q, want prefix %q", s, Name+" ")
	}
	var buf bytes.Buffer
	PrintVersion(&buf)
	if buf.String() != s+"\n" {
		t.Errorf("PrintVersion = %q, want %q", buf.String(), s+"\n")
	}
}
