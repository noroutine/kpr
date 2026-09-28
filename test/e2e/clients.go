//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/ref"
)

// PushClient names one image producer for the client matrix. Every
// producer must yield a registry-accepted manifest the sweeper deletes
// by digest — that interchange is the whole point of the matrix.
type PushClient int

const (
	// ClientGGCR pushes via go-containerregistry's remote.Write.
	ClientGGCR PushClient = iota
	// ClientCrane pushes via the crane package (same module, its own
	// push path).
	ClientCrane
	// ClientDocker pushes via the docker daemon (independent producer,
	// schema-2 manifests).
	ClientDocker
	// ClientRegclient copies via the regclient library (independent Go
	// implementation, its own manifest serialization).
	ClientRegclient
	// ClientOras pushes a typed OCI artifact via the oras CLI
	// (artifactType manifests, not images).
	ClientOras
)

func (c PushClient) String() string {
	switch c {
	case ClientGGCR:
		return "ggcr"
	case ClientCrane:
		return "crane"
	case ClientDocker:
		return "docker"
	case ClientRegclient:
		return "regclient"
	case ClientOras:
		return "oras"
	}
	return "unknown"
}

// digestRe extracts the artifact digest from oras attach output: the
// `Digest:` line, never the subject ref in the `Attached to` line
// above it (tagging the subject would retarget retention onto the
// signed image itself).
var digestRe = regexp.MustCompile(`(?m)^Digest:\s*(sha256:[0-9a-f]{64})\s*$`)

// pushImage lands repo:tag on the fixture registry through the named
// client and returns the manifest digest. Fatal on failure: a client
// that cannot push is a broken scenario, never a skip.
func pushImage(t *testing.T, fx *Fixture, client PushClient, repo, tag string) string {
	t.Helper()
	switch client {
	case ClientGGCR:
		return pushImageGGCR(t, fx, repo, tag)
	case ClientCrane:
		return pushImageCrane(t, fx, repo, tag)
	case ClientDocker:
		return pushImageDocker(t, fx, repo, tag)
	case ClientRegclient:
		return pushImageRegclient(t, fx, repo, tag)
	case ClientOras:
		return pushImageOras(t, fx, repo, tag, e2eArtifactType)
	}
	t.Fatalf("unknown push client %d", int(client))
	return ""
}

// e2eArtifactType is the typed-blob artifact the oras matrix row
// pushes: an OCI artifact, not an image.
const e2eArtifactType = "application/vnd.kpr.e2e.artifact"

// requireOras skips when the oras CLI is absent — artifact flows need
// the binary, and its absence proves nothing about kpr.
func requireOras(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("oras"); err != nil {
		t.Skipf("oras CLI not installed, skipping artifact flow: %v", err)
	}
}

func pushImageOras(t *testing.T, fx *Fixture, repo, tag, artifactType string) string {
	t.Helper()
	requireOras(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "artifact"), []byte("kpr-e2e:"+repo+":"+tag+"\n"), 0o644); err != nil {
		t.Fatalf("oras context: %v", err)
	}
	dst := fx.RegistryHostPort() + "/" + repo + ":" + tag
	runIn(t, ctx, dir, "oras", "push", "--plain-http", "--artifact-type", artifactType, dst, "artifact:application/octet-stream")
	return headDigest(t, dst)
}

// orasAttach pins a typed artifact onto a subject manifest and tags
// the result for retention. It returns the artifact digest, read back
// uniformly (not trusted from CLI output).
func orasAttach(t *testing.T, fx *Fixture, repo, subjectDigest, tag, artifactType, payload string) string {
	t.Helper()
	requireOras(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	host := fx.RegistryHostPort()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "artifact"), []byte(payload+"\n"), 0o644); err != nil {
		t.Fatalf("oras context: %v", err)
	}
	out := runIn(t, ctx, dir, "oras", "attach", "--plain-http", "--artifact-type", artifactType,
		host+"/"+repo+"@"+subjectDigest, "artifact:application/octet-stream")
	digest := parseDigest(t, out)
	run(t, ctx, "oras", "tag", "--plain-http", host+"/"+repo+"@"+digest, tag)
	return headDigest(t, host+"/"+repo+":"+tag)
}

// parseDigest extracts the artifact digest from attach output — and
// the tag readback confirms it actually landed under that digest.
func parseDigest(t *testing.T, out string) string {
	t.Helper()
	m := digestRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no Digest: line in output:\n%s", out)
	}
	return m[1]
}

func pushImageGGCR(t *testing.T, fx *Fixture, repo, tag string) string {
	t.Helper()
	img, err := buildImage(repo, tag)
	if err != nil {
		t.Fatalf("ggcr build: %v", err)
	}
	ref, err := name.NewTag(fx.RegistryHostPort()+"/"+repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatalf("ggcr ref: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("ggcr push: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("ggcr digest: %v", err)
	}
	return d.String()
}

func pushImageCrane(t *testing.T, fx *Fixture, repo, tag string) string {
	t.Helper()
	img, err := buildImage(repo, tag)
	if err != nil {
		t.Fatalf("crane build: %v", err)
	}
	dst := fx.RegistryHostPort() + "/" + repo + ":" + tag
	if err := crane.Push(img, dst, crane.Insecure); err != nil {
		t.Fatalf("crane push: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("crane digest: %v", err)
	}
	return d.String()
}

// run executes a CLI and fails the scenario on error, with combined
// output — daemon CLIs fail opaquely otherwise.
func run(t *testing.T, ctx context.Context, bin string, args ...string) string {
	t.Helper()
	return runIn(t, ctx, "", bin, args...)
}

// runIn is run with a working directory — oras rejects absolute file
// paths, so artifact files go by relative name from their temp dir.
func runIn(t *testing.T, ctx context.Context, dir, bin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
	return string(out)
}

func pushImageDocker(t *testing.T, fx *Fixture, repo, tag string) string {
	t.Helper()
	// The push originates inside the docker daemon, not the test
	// process: on Docker Desktop (macOS/Windows) the daemon lives in
	// a VM whose localhost never reaches the mac-side port forward
	// testcontainers relies on, so the push blackholes. Native Linux
	// daemons (CI included) share the host loopback and push fine.
	// Skip with a reason instead of hanging: daemon pushes are proven
	// where the daemon and the fixture share a network.
	out, err := exec.Command("docker", "info", "--format", "{{.OperatingSystem}}").Output()
	if err != nil {
		t.Fatalf("docker info: %v", err)
	}
	if strings.Contains(string(out), "Desktop") {
		t.Skipf("docker daemon is %s: fixture ports unreachable from the VM, skipping daemon push", strings.TrimSpace(string(out)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A scratch image builds offline: no base pull, daemon-produced
	// schema-2 manifest.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY hello /hello\n"), 0o644); err != nil {
		t.Fatalf("docker context: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("kpr-e2e:"+repo+":"+tag+"\n"), 0o644); err != nil {
		t.Fatalf("docker context: %v", err)
	}
	local := "kpr-e2e-client:latest"
	run(t, ctx, "docker", "build", "-q", "-t", local, dir)
	// Loopback: the daemon reaches the fixture registry over HTTP the
	// same way it reaches any local registry.
	dst := fx.RegistryLoopback() + "/" + repo + ":" + tag
	run(t, ctx, "docker", "tag", local, dst)
	run(t, ctx, "docker", "push", dst)
	run(t, ctx, "docker", "rmi", local, dst)

	return headDigest(t, fx.RegistryHostPort()+"/"+repo+":"+tag)
}

func pushImageRegclient(t *testing.T, fx *Fixture, repo, tag string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Seed a source tag through ggcr, then copy it with regclient: the
	// copy re-serializes the manifest through regclient's own PUT
	// path, which is what the matrix is proving.
	srcRepo := "test/e2e-src"
	pushImageGGCR(t, fx, srcRepo, "base")

	host := fx.RegistryHostPort()
	rc := regclient.New(regclient.WithConfigHost(config.Host{Name: host, TLS: config.TLSDisabled}))
	src, err := ref.New(host + "/" + srcRepo + ":base")
	if err != nil {
		t.Fatalf("regclient src ref: %v", err)
	}
	dst, err := ref.New(host + "/" + repo + ":" + tag)
	if err != nil {
		t.Fatalf("regclient dst ref: %v", err)
	}
	if err := rc.ImageCopy(ctx, src, dst); err != nil {
		t.Fatalf("regclient copy: %v", err)
	}
	return headDigest(t, host+"/"+repo+":"+tag)
}

// headDigest reads a pushed manifest's digest back with a HEAD — one
// uniform readback for every producer, independent of how it pushed.
func headDigest(t *testing.T, reference string) string {
	t.Helper()
	ref, err := name.NewTag(reference, name.Insecure)
	if err != nil {
		t.Fatalf("digest ref: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	desc, err := remote.Head(ref, remote.WithContext(ctx))
	if err != nil {
		t.Fatalf("HEAD %s: %v", reference, err)
	}
	return desc.Digest.String()
}
