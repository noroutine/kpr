package config

// compiletime.go holds build-time metadata injected via -ldflags (see
// Makefile/justfile) — the program's own name and version/commit/build
// time — as opposed to config.go's env-driven settings and runtime.go's
// process/OS facts. All three "kinds of config" in this package answer the
// same question — what does this process know about itself and the world
// it runs in — resolved once rather than re-derived or re-described at
// every call site. All three vars default to placeholder values so
// `go run ./cmd/app` and ad-hoc `go build` (no ldflags) still print
// something sensible instead of an empty string.

import (
	"fmt"
	"io"
)

// Name is the binary / product name shown in -version, logs, and the
// management console.
const Name = "kpr"

var (
	// Version is the git tag/describe output, set at build time.
	Version = "dev"
	// Commit is the short git commit hash, set at build time.
	Commit = "unknown"
	// BuildTime is the UTC build timestamp, set at build time.
	BuildTime = "unknown"
)

// VersionString renders the single-line identity string used by the
// version flag, startup log lines, and the management console.
func VersionString() string {
	return Name + " " + Version + " (commit " + Commit + ", built " + BuildTime + ")"
}

// PrintVersion writes VersionString to w, followed by a newline.
func PrintVersion(w io.Writer) {
	_, _ = fmt.Fprintln(w, VersionString())
}

// LicenseBanner is the GPL short notice: warranty refusal plus
// redistribution grant. One source for the version screen, the root
// help, and the serve startup log, so the three can't drift apart.
func LicenseBanner() string {
	return `Copyright (C) 2026 noroutine
This program comes with ABSOLUTELY NO WARRANTY; for details run 'kpr license'.
This is free software, and you are welcome to redistribute it
under certain conditions; run 'kpr license' for details.`
}
