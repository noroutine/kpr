package cli

import (
	"bytes"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// --version prints the banner: if this fails, the version template
// got rewired without carrying the notice along.
func TestVersionTemplateCarriesBanner(t *testing.T) {
	if tmpl := RootCmd.VersionTemplate(); !strings.Contains(tmpl, "ABSOLUTELY NO WARRANTY") {
		t.Errorf("version template = %q, want the license banner in it", tmpl)
	}
}

// kpr version prints what --version prints (identity plus banner):
// if this fails, the command drifted from the flag.
func TestVersionCommandMatchesFlag(t *testing.T) {
	c := newVersionCmd()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{})
	if err := c.Execute(); err != nil {
		t.Fatalf("version execute: %v", err)
	}
	for _, want := range []string{config.VersionString(), "ABSOLUTELY NO WARRANTY"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("version output misses %q", want)
		}
	}
}
