package main

import (
	"testing"

	"nrtn.dev/catalyst/kpr/internal/cli"
)

// Invoking the real binary entrypoint with --help must print help and
// return instead of exiting nonzero: it proves main is wired to the root
// command. If this fails, the shipped binary can't even ask for help.
func TestMainHelp(t *testing.T) {
	cli.RootCmd.SetArgs([]string{"--help"})
	defer cli.RootCmd.SetArgs(nil)
	main()
}
