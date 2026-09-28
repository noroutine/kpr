//go:build e2e

package e2e

import (
	"context"
	"io"
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

// Toolbox is the scenario binary drawer: one long-lived container
// with every CLI the scenarios shell out to (oras, skopeo), started
// once per package run by TestMain. Clients run where the fixtures
// live — container-to-container over plain HTTP — so no host binary
// is ever required and Docker Desktop port forwards never enter the
// picture. Fatal on failure: a broken drawer breaks every scenario.
type Toolbox struct {
	ctr testcontainers.Container
}

// toolbox is the package run's drawer, installed by TestMain before
// any scenario runs. Scenarios use it; they never start their own.
var toolbox *Toolbox

// startToolbox builds the toolbox image (cached after the first
// build) and starts one sleeping container for the package run.
func startToolbox(ctx context.Context) (*Toolbox, error) {
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:   "testdata/toolbox",
				Repo:      "kpr-e2e-toolbox",
				Tag:       "latest",
				KeepImage: true,
			},
			// Override the base image's entrypoint (skopeo's own):
			// Cmd alone would become its arguments and exit.
			Entrypoint: []string{"sleep", "infinity"},
		},
		Started: true,
	})
	if err != nil {
		return nil, err
	}
	return &Toolbox{ctr: ctr}, nil
}

// Terminate stops the toolbox container (the built image stays
// cached for the next run).
func (tb *Toolbox) Terminate(ctx context.Context) error {
	return tb.ctr.Terminate(ctx)
}

// Exec runs argv in the toolbox and returns combined output,
// failing the test on nonzero exit — CLIs fail opaquely otherwise.
func (tb *Toolbox) Exec(t *testing.T, ctx context.Context, argv ...string) string {
	t.Helper()
	code, out, err := tb.ctr.Exec(ctx, argv)
	if err != nil {
		t.Fatalf("toolbox exec %v: %v", argv, err)
	}
	body, rerr := io.ReadAll(out)
	if rerr != nil {
		t.Fatalf("toolbox exec %v: read output: %v", argv, rerr)
	}
	if code != 0 {
		t.Fatalf("toolbox %v: exit %d\n%s", argv, code, body)
	}
	return string(body)
}

// WriteFile stages content inside the toolbox for a later exec
// (oras artifact payloads) and returns its container path.
func (tb *Toolbox) WriteFile(t *testing.T, ctx context.Context, name string, content []byte) string {
	t.Helper()
	path := "/tmp/" + name
	if err := tb.ctr.CopyToContainer(ctx, content, path, 0o644); err != nil {
		t.Fatalf("toolbox stage %s: %v", name, err)
	}
	return path
}
