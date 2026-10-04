package cli

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// The GPL short notice must name the warranty refusal and the
// redistribution grant: a banner with only one of the two reads as
// either proprietary or warranty-bearing.
func TestLicenseBannerContent(t *testing.T) {
	b := config.LicenseBanner()
	for _, want := range []string{"Copyright (C)", "ABSOLUTELY NO WARRANTY", "redistribute", "kpr license"} {
		if !strings.Contains(b, want) {
			t.Errorf("banner = %q, want it to contain %q", b, want)
		}
	}
}

// --version prints the banner: if this fails, the version template
// got rewired without carrying the notice along.
func TestVersionTemplateCarriesBanner(t *testing.T) {
	if tmpl := RootCmd.VersionTemplate(); !strings.Contains(tmpl, "ABSOLUTELY NO WARRANTY") {
		t.Errorf("version template = %q, want the license banner in it", tmpl)
	}
}

// kpr --help prints the banner: if this fails, the root Long got
// rewritten without carrying the notice along.
func TestRootHelpCarriesBanner(t *testing.T) {
	if !strings.Contains(RootCmd.Long, "redistribute") {
		t.Errorf("root Long = %q, want the license banner in it", RootCmd.Long)
	}
}

// kpr license prints the full GPL text: if this fails, the embed
// broke or the command stopped printing it.
func TestLicenseCommandPrintsGPL(t *testing.T) {
	c := newLicenseCmd()
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{})
	if err := c.Execute(); err != nil {
		t.Fatalf("license execute: %v", err)
	}
	for _, want := range []string{"GNU GENERAL PUBLIC LICENSE", "Version 3"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("license output misses %q", want)
		}
	}
}

// serve logs the banner at startup: if this fails, the startup path
// stopped calling logLicenseBanner.
func TestLogLicenseBanner(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	log.SetFlags(0)
	defer log.SetFlags(log.LstdFlags)
	logLicenseBanner()
	if got := buf.String(); !strings.Contains(got, "ABSOLUTELY NO WARRANTY") {
		t.Errorf("startup log = %q, want the license banner in it", got)
	}
}
