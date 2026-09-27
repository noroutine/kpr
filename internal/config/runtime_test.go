package config

import (
	"errors"
	"testing"
)

// Install hints print the binary's own path; outside $HOME (a system
// install, a container) the raw path must pass through untouched, inside
// $HOME it must collapse to the copy-pasteable $HOME/... form. If this
// fails, hints point at a path that doesn't exist on the operator's
// machine.
func TestHomeRelativePath(t *testing.T) {
	if got := HomeRelativePath("/home/u/.local/bin/kpr", "/home/u"); got != "$HOME/.local/bin/kpr" {
		t.Errorf("inside home = %q", got)
	}
	if got := HomeRelativePath("/usr/local/bin/kpr", "/home/u"); got != "/usr/local/bin/kpr" {
		t.Errorf("outside home = %q", got)
	}
	if got := HomeRelativePath("/home/u", "/home/u"); got != "$HOME" {
		t.Errorf("home itself = %q", got)
	}
	if got := HomeRelativePath("/x", ""); got != "/x" {
		t.Errorf("empty home = %q", got)
	}
}

// When the OS can't even report the executable path (exotic sandboxing),
// help must still print the binary name rather than an empty string. If
// this fails, -help renders a blank where the binary path belongs. Only
// the seam makes this branch reachable: real os.Executable never fails in
// a test run.
func TestInstalledBinaryPathFallsBackToName(t *testing.T) {
	old := osExecutable
	osExecutable = func() (string, error) { return "", errors.New("nope") }
	defer func() { osExecutable = old }()
	if got := InstalledBinaryPath(); got != Name {
		t.Errorf("fallback = %q, want %q", got, Name)
	}
}

// Overriding the platform for one test must not leak into the next, the
// same contract as SetCurrent. If this fails, an OS-branch test poisons
// every test after it on a different assumption about the platform.
func TestSetCurrentRuntimeRestoresPrevious(t *testing.T) {
	before := CurrentRuntime()
	restore := SetCurrentRuntime(NewRuntimeBuilder().WithGOOS("windows").WithGOARCH("arm64").Build())
	if CurrentRuntime().GOOS != "windows" || CurrentRuntime().GOARCH != "arm64" {
		t.Fatal("override not installed")
	}
	restore()
	if CurrentRuntime() != before {
		t.Error("restore did not put the previous runtime back")
	}
}

// The default builder must report the real platform: a test overriding
// only GOOS still gets a real GOARCH, and production never sees an empty
// pair. If this fails, platform branching downstream keys off blank
// values.
func TestNewRuntimeBuilderDefaultsToRealPlatform(t *testing.T) {
	rt := NewRuntimeBuilder().Build()
	if rt.GOOS == "" || rt.GOARCH == "" {
		t.Errorf("runtime = %+v, want real GOOS/GOARCH", rt)
	}
}

// The binary-path helper must always return something printable: the
// $HOME-collapsed form under home, the raw path elsewhere, and the
// binary name when the OS can't report the path at all. If this fails,
// install hints print a blank or a temp-test path instead of a
// copy-pasteable one.
func TestInstalledBinaryPath(t *testing.T) {
	if got := InstalledBinaryPath(); got == "" {
		t.Error("empty path for the real executable")
	}

	oldAbs := filepathAbs
	filepathAbs = func(string) (string, error) { return "", errors.New("nope") }
	defer func() { filepathAbs = oldAbs }()
	if got := InstalledBinaryPath(); got == "" {
		t.Error("empty path when Abs fails")
	}
	if got := HomeRelativePath("/home/u/.local/bin/kpr", "/home/u"); got != "$HOME/.local/bin/kpr" {
		t.Errorf("abs-fallback rewrite = %q", got)
	}
}
