//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestMain starts the toolbox container once for the package run —
// one drawer of client binaries every scenario shares — and
// terminates it afterwards. Without docker nothing can run, so the
// package passes quietly (every scenario would skip on its own
// fixture gate anyway); a toolbox that fails to build with docker
// present is a real failure.
func TestMain(m *testing.M) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		fmt.Println("e2e: docker unavailable, skipping package")
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tb, err := startToolbox(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e toolbox:", err)
		os.Exit(1)
	}
	toolbox = tb
	code := m.Run()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Minute)
	defer cancel2()
	_ = tb.Terminate(ctx2)
	os.Exit(code)
}
