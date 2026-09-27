package config

// runtime.go holds facts about the outer environment this process runs
// inside — where its own executable lives relative to $HOME, and which
// platform it was built for — as opposed to config.go's env-var-driven
// settings. It's still config in the broad sense: neither varies within a
// run, and both exist so the rest of the app reads one resolved value
// instead of re-deriving it (os.Executable, os.UserHomeDir) or importing
// the standard "runtime" package directly at every call site.
//
// GOOS/GOARCH specifically get their own Runtime/RuntimeBuilder/
// CurrentRuntime/SetCurrentRuntime, deliberately mirroring config.go's
// Config/Builder/Current/SetCurrent rather than being plain reassignable
// package vars (the way compiletime.go's Version/Commit/BuildTime are,
// since -ldflags -X has no other option there). A bare `var GOOS string`
// is the hatchet this whole package exists to avoid: production code has
// no business ever writing to it, and nothing stops it from doing so
// accidentally. One atomic swap, restored by the func SetCurrentRuntime
// itself returns, closes that door the same way it's closed for Config.

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
)

// Runtime holds this process's build target and toolchain version, as
// reported by the standard runtime package. Read via CurrentRuntime(),
// never this type's fields directly — the indirection is what lets a
// test substitute another platform's values without an actual
// cross-compile. This does not extend to code whose branch exists
// because the real OS's syscall semantics differ: faking GOOS there
// exercises the code path but proves nothing about the real platform
// behavior it exists for.
type Runtime struct {
	GOOS   string
	GOARCH string
	// GoVersion is the toolchain version (runtime.Version()), kept
	// here so display code (the console runtime card, /metrics) never
	// imports the standard runtime package directly.
	GoVersion string
}

func defaultRuntime() Runtime {
	return Runtime{GOOS: goruntime.GOOS, GOARCH: goruntime.GOARCH, GoVersion: goruntime.Version()}
}

// RuntimeBuilder builds a Runtime, defaulting to the real platform and
// toolchain this binary was actually built with. Production never needs
// one — only CurrentRuntime() — but a test overriding a
// platform-specific branch builds one and installs it with
// SetCurrentRuntime:
//
//	t.Cleanup(config.SetCurrentRuntime(config.NewRuntimeBuilder().WithGOOS("windows").Build()))
type RuntimeBuilder struct {
	rt Runtime
}

// NewRuntimeBuilder starts from the real platform and toolchain.
func NewRuntimeBuilder() *RuntimeBuilder {
	return &RuntimeBuilder{rt: defaultRuntime()}
}

func (b *RuntimeBuilder) WithGOOS(v string) *RuntimeBuilder      { b.rt.GOOS = v; return b }
func (b *RuntimeBuilder) WithGOARCH(v string) *RuntimeBuilder    { b.rt.GOARCH = v; return b }
func (b *RuntimeBuilder) WithGoVersion(v string) *RuntimeBuilder { b.rt.GoVersion = v; return b }

// Build returns the built Runtime.
func (b *RuntimeBuilder) Build() *Runtime {
	rt := b.rt
	return &rt
}

// currentRuntime holds the active Runtime. Never nil: init seeds it with
// the real platform and toolchain so CurrentRuntime() is safe to call
// even before anything calls SetCurrentRuntime.
var currentRuntime atomic.Pointer[Runtime]

func init() {
	currentRuntime.Store(NewRuntimeBuilder().Build())
}

// CurrentRuntime returns the active Runtime. Every consumer reads
// CurrentRuntime().GOOS/.GOARCH/.GoVersion this way rather than importing
// the standard "runtime" package directly, so every OS/arch-conditional
// branch in this program has one source — and, in a test, one place to
// override.
func CurrentRuntime() *Runtime {
	return currentRuntime.Load()
}

// SetCurrentRuntime installs rt as the active Runtime and returns a
// function that restores whatever was active before, the same one-line
// override/restore pattern as SetCurrent.
func SetCurrentRuntime(rt *Runtime) (restore func()) {
	previous := currentRuntime.Swap(rt)
	return func() { currentRuntime.Store(previous) }
}

// osExecutable is a seam for tests: it resolves this process's own
// executable path. Tests replace it to exercise InstalledBinaryPath's
// fallback to Name — os.Executable practically never fails in a normal
// test environment, so there's no way to reach that branch otherwise.
var osExecutable = os.Executable

// filepathAbs is a seam for tests: it wraps filepath.Abs, used by both
// InstalledBinaryPath and HomeRelativePath. filepath.Abs only errors when
// os.Getwd fails (a deleted or unreadable current directory), which isn't
// reliably triggerable across platforms in a normal test — this lets a
// test exercise the fallback without depending on that real OS failure.
var filepathAbs = filepath.Abs

// InstalledBinaryPath resolves this process's own executable to $HOME/...
// when it lives under the user's home, so help text and install hints
// print a copy-pasteable path instead of this specific invocation's (e.g.
// go test's temp binary).
func InstalledBinaryPath() string {
	exe, err := osExecutable()
	if err != nil {
		return Name
	}
	abs, err := filepathAbs(exe)
	if err != nil {
		abs = exe
	}
	home, _ := os.UserHomeDir()
	return HomeRelativePath(abs, home)
}

// HomeRelativePath rewrites path as $HOME/... when it is inside home. $HOME
// is understood by bash and zsh; the rest of the path uses slashes so the
// same line stays copy-pasteable.
func HomeRelativePath(path, home string) string {
	if home == "" {
		return path
	}
	absPath, err := filepathAbs(path)
	if err != nil {
		absPath = filepath.Clean(path)
	}
	absHome, err := filepathAbs(home)
	if err != nil {
		absHome = filepath.Clean(home)
	}
	rel, err := filepath.Rel(absHome, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return path
	}
	if rel == "." {
		return "$HOME"
	}
	return "$HOME/" + filepath.ToSlash(rel)
}
