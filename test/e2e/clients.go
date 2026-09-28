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
	// ClientSkopeo copies via the skopeo CLI (containers/image stack,
	// its own manifest handling).
	ClientSkopeo
	// ClientPodman pushes via the podman CLI (daemonless: the push
	// originates in the local process, unlike docker's in-daemon
	// push — the control for the Desktop reachability question).
	ClientPodman
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
	case ClientSkopeo:
		return "skopeo"
	case ClientPodman:
		return "podman"
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
	case ClientSkopeo:
		return pushImageSkopeo(t, fx, repo, tag)
	case ClientPodman:
		return pushImagePodman(t, fx, repo, tag)
	}
	t.Fatalf("unknown push client %d", int(client))
	return ""
}

// e2eArtifactType is the typed-blob artifact the oras matrix row
// pushes: an OCI artifact, not an image.
const e2eArtifactType = "application/vnd.kpr.e2e.artifact"

// toolboxFile stages an artifact payload inside the toolbox for a
// later exec, under a per-repo name so sequential pushes never share
// one.
func toolboxFile(t *testing.T, ctx context.Context, repo, tag, payload string) string {
	t.Helper()
	name := strings.ReplaceAll(repo, "/", "_") + "-" + tag
	return toolbox.WriteFile(t, ctx, name, []byte(payload+"\n"))
}

func pushImageOras(t *testing.T, fx *Fixture, repo, tag, artifactType string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	file := toolboxFile(t, ctx, repo, tag, "kpr-e2e:"+repo+":"+tag)
	dst := fx.RegistryDirect() + "/" + repo + ":" + tag
	toolbox.Exec(t, ctx, "oras", "push", "--plain-http", "--disable-path-validation",
		"--artifact-type", artifactType, dst, file+":application/octet-stream")
	return headDigest(t, fx.RegistryHostPort()+"/"+repo+":"+tag)
}

// orasAttach pins a typed artifact onto a subject manifest and tags
// the result for retention. It returns the artifact digest, read back
// uniformly (not trusted from CLI output).
func orasAttach(t *testing.T, fx *Fixture, repo, subjectDigest, tag, artifactType, payload string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	direct := fx.RegistryDirect()
	file := toolboxFile(t, ctx, repo, tag, payload)
	out := toolbox.Exec(t, ctx, "oras", "attach", "--plain-http", "--disable-path-validation",
		"--artifact-type", artifactType, direct+"/"+repo+"@"+subjectDigest, file+":application/octet-stream")
	digest := parseDigest(t, out)
	toolbox.Exec(t, ctx, "oras", "tag", "--plain-http", direct+"/"+repo+"@"+digest, tag)
	return headDigest(t, fx.RegistryHostPort()+"/"+repo+":"+tag)
}

func pushImageSkopeo(t *testing.T, fx *Fixture, repo, tag string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Seed a source tag in-process, then copy it with skopeo: the copy
	// re-serializes the manifest through the containers/image stack,
	// which is what the matrix is proving. The seed is a fully typed
	// image — skopeo's docker transport rejects the untyped
	// octet-stream layers the plain builder emits.
	direct := fx.RegistryDirect()
	srcRepo := "test/e2e-src"
	seed, err := buildArchImage(srcRepo, "base", "amd64", "linux")
	if err != nil {
		t.Fatalf("skopeo seed build: %v", err)
	}
	srcRef, err := name.NewTag(fx.RegistryHostPort()+"/"+srcRepo+":base", name.Insecure)
	if err != nil {
		t.Fatalf("skopeo seed ref: %v", err)
	}
	if err := remote.Write(srcRef, seed); err != nil {
		t.Fatalf("skopeo seed push: %v", err)
	}
	toolbox.Exec(t, ctx, "skopeo", "copy",
		"--src-tls-verify=false", "--dest-tls-verify=false",
		"docker://"+direct+"/"+srcRepo+":base",
		"docker://"+direct+"/"+repo+":"+tag)
	return headDigest(t, fx.RegistryHostPort()+"/"+repo+":"+tag)
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

// run executes a host CLI and fails the scenario on error, with
// combined output — daemon CLIs fail opaquely otherwise. Toolbox
// clients go through Toolbox.Exec instead.
func run(t *testing.T, ctx context.Context, bin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, bin, args...)
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

	dir := scratchContext(t, repo, tag)
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

func pushImagePodman(t *testing.T, fx *Fixture, repo, tag string) string {
	t.Helper()
	// Podman is daemonless: build and push run in the local process,
	// so unlike docker the push shares the test's viewpoint and
	// reaches fixture ports from macOS too. Absent or unready podman
	// proves nothing — skip; a present one reports real verdicts.
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skipf("podman not installed, skipping podman push: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "podman", "info", "--format", "{{.Host.OS}}").CombinedOutput(); err != nil {
		t.Skipf("podman not ready, skipping podman push: %v\n%s", err, out)
	}

	dir := scratchContext(t, repo, tag)
	local := "kpr-e2e-client:latest"
	run(t, ctx, "podman", "build", "-q", "-t", local, dir)
	dst := fx.RegistryHostPort() + "/" + repo + ":" + tag
	run(t, ctx, "podman", "tag", local, dst)
	run(t, ctx, "podman", "push", "--tls-verify=false", dst)
	run(t, ctx, "podman", "rmi", local, dst)

	return headDigest(t, dst)
}

// scratchContext writes an offline scratch-image build context: no
// base pull, daemon-produced schema-2 manifests.
func scratchContext(t *testing.T, repo, tag string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY hello /hello\n"), 0o644); err != nil {
		t.Fatalf("build context: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("kpr-e2e:"+repo+":"+tag+"\n"), 0o644); err != nil {
		t.Fatalf("build context: %v", err)
	}
	return dir
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
