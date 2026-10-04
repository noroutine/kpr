//go:build e2e

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"nrtn.dev/catalyst/kpr/internal/backfill"
	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// e2eTimeout bounds every backend round-trip: a hung container fails
// the scenario instead of the suite.
const e2eTimeout = 30 * time.Second

// Scenario is the e2e DSL: Push a real image, Reap the policies the
// CLI marks from, Sweep the pass serve runs, and Expect each stage's
// outcome. Methods fatal on backend errors (fixtures, not assertions)
// and return assertion-friendly values where the test decides the
// verdict.
type Scenario struct {
	t       *testing.T
	fx      *Fixture
	store   *store.RedisStore
	reg     *registry.Client
	sweeper *sweep.Sweeper
	lined   bool
}

// New wires a scenario to a fixture: the redis store on kpr's DB, the
// registry client, and an armed sweeper — the same trio serve runs.
func New(t *testing.T, fx *Fixture) *Scenario {
	t.Helper()
	s := store.NewRedisStore(fx.RedisAddr(), fx.RedisPassword(), fx.RedisDB())
	ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("e2e redis unreachable at %s: %v", fx.RedisAddr(), err)
	}
	// The scenarios exercise sweep policies, not the lock: open the
	// store the way a paired deployment holds it (locked refusal
	// itself is covered in lock_test).
	if err := s.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("e2e unlock: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	reg := registry.NewClient(fx.RegistryURL())
	return &Scenario{
		t:       t,
		fx:      fx,
		store:   s,
		reg:     reg,
		sweeper: &sweep.Sweeper{Store: s, Registry: reg, Sentinel: reg, DryRun: false},
	}
}

func (s *Scenario) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), e2eTimeout)
}

// buildImage assembles the scenario's single-layer image: distinct
// bytes per repo:tag so every push mints a distinct digest.
func buildImage(repo, tag string) (v1.Image, error) {
	layer := static.NewLayer([]byte("kpr-e2e:"+repo+":"+tag), types.MediaType("application/octet-stream"))
	return mutate.AppendLayers(empty.Image, layer)
}

// buildArchImage assembles a single-layer image stamped for one
// platform: a genuinely gzipped layer under the compressed OCI type,
// a config carrying both the architecture and the layer's DiffID in
// its rootfs, and the OCI manifest envelope — the way real multi-arch
// children look. The rootfs entry matters: ggcr derives Layers() from
// the config's DiffIDs, so a bare Architecture/OS config silently
// empties Layers() while the manifest still names the layer, and the
// registry answers BLOB_UNKNOWN. (The untyped builder above suffices
// for single images and stays untouched.)
func buildArchImage(repo, tag, arch, os string) (v1.Image, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte("kpr-e2e:" + repo + ":" + tag + "\n")); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	layer := static.NewLayer(buf.Bytes(), types.OCILayer)
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, err
	}
	diffID, err := layer.DiffID()
	if err != nil {
		return nil, err
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		Architecture: arch,
		OS:           os,
		RootFS:       v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{diffID}},
	})
	if err != nil {
		return nil, err
	}
	return mutate.MediaType(img, types.OCIManifestSchema1), nil
}

// imageRef parses a tag ref against the fixture registry (plain HTTP,
// test-only).
func (s *Scenario) imageRef(repo, tag string) name.Tag {
	s.t.Helper()
	ref, err := name.NewTag(s.fx.RegistryHostPort()+"/"+repo+":"+tag, name.Insecure)
	if err != nil {
		s.t.Fatalf("e2e image ref: %v", err)
	}
	return ref
}

// RecordRow writes the row the receiver would track for a push
// (repo/tag/digest, push time backdated by pushedAgo so scenarios
// expire rows without sleeping on a clock) without pushing anything:
// backfilled pre-kpr tags and digest-less twins go through here, and
// so does every PushWithClient after its image lands.
func (s *Scenario) RecordRow(repo, tag, digest string, pushedAgo time.Duration) {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	row := policy.Row{
		Repo:     repo,
		Tag:      tag,
		Digest:   digest,
		PushedAt: time.Now().Add(-pushedAgo),
		Actor:    "e2e",
	}
	if err := s.store.Record(ctx, row); err != nil {
		s.t.Fatalf("record e2e row: %v", err)
	}
}

// Push writes a real single-layer image via the default client and
// records its row. It returns the manifest digest.
func (s *Scenario) Push(repo, tag string, pushedAgo time.Duration) string {
	s.t.Helper()
	return s.PushWithClient(ClientGGCR, repo, tag, pushedAgo)
}

// PushWithClient pushes through the named client, then records the
// row — the same composite as Push, with the producer swapped.
func (s *Scenario) PushWithClient(client PushClient, repo, tag string, pushedAgo time.Duration) string {
	s.t.Helper()
	digest := pushImage(s.t, s.fx, client, repo, tag)
	s.RecordRow(repo, tag, digest, pushedAgo)
	return digest
}

// PushUntracked pushes a real image and records nothing — the
// pre-kpr tag: on disk, invisible to kpr until backfill adopts it.
// It returns the manifest digest.
func (s *Scenario) PushUntracked(repo, tag string) string {
	s.t.Helper()
	return pushImage(s.t, s.fx, ClientGGCR, repo, tag)
}

// PushUntrackedList pushes a docker manifest list (the shape real
// multi-arch publishers ship) and records nothing — the pre-kpr
// list tag. The docker list type is the point: registry:3 400s a
// bare */* HEAD on it, so backfill must ask with an explicit
// manifest Accept list. It returns the list digest.
func (s *Scenario) PushUntrackedList(repo, tag string) string {
	s.t.Helper()
	var adds []mutate.IndexAddendum
	for _, arch := range []string{"amd64", "arm64"} {
		img, err := buildImage(repo, tag+"-"+arch)
		if err != nil {
			s.t.Fatalf("build e2e image for %s: %v", arch, err)
		}
		child := s.imageRef(repo, tag+"-"+arch)
		if err := remote.Write(child, img); err != nil {
			s.t.Fatalf("push e2e child for %s: %v", arch, err)
		}
		adds = append(adds, mutate.IndexAddendum{Add: img})
	}
	idx := mutate.IndexMediaType(mutate.AppendManifests(empty.Index, adds...), types.DockerManifestList)
	ref := s.imageRef(repo, tag)
	if err := remote.WriteIndex(ref, idx); err != nil {
		s.t.Fatalf("push e2e list: %v", err)
	}
	d, err := idx.Digest()
	if err != nil {
		s.t.Fatalf("e2e list digest: %v", err)
	}
	return d.String()
}

// BackfillArmed runs one armed backfill pass — the import tick's
// work, on demand — and returns its summary for the test's verdict.
// Fatal on a plain fixture: backfill stats tag links off the mount,
// it cannot prove anything blind.
func (s *Scenario) BackfillArmed() backfill.Summary {
	s.t.Helper()
	if s.fx.StorageDir() == "" {
		s.t.Fatal("backfill needs a mount fixture (NewStorageFixture)")
	}
	s.establishLineage()
	ctx, cancel := s.ctx()
	defer cancel()
	sum, err := backfill.Run(ctx, io.Discard,
		s.reg, s.reg, s.store, s.store, s.store, s.store,
		s.fx.StorageDir(), backfill.Options{}, nil)
	if err != nil {
		s.t.Fatalf("backfill: %v", err)
	}
	return sum
}

// ExpectRow returns the tracked row — the recorded half of an
// adoption verdict; the test asserts its fields.
func (s *Scenario) ExpectRow(repo, tag string) policy.Row {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.store.All(ctx)
	if err != nil {
		s.t.Fatalf("read e2e rows: %v", err)
	}
	for _, r := range rows {
		if r.Repo == repo && r.Tag == tag {
			return r
		}
	}
	s.t.Fatalf("row %s:%s not tracked", repo, tag)
	return policy.Row{}
}

// PushIndex writes a real multi-arch index (one child per arch) and
// records the row against the index digest — what the receiver tracks
// when a platform manifest list is pushed. It returns the index digest.
func (s *Scenario) PushIndex(repo, tag string, pushedAgo time.Duration, arches ...string) string {
	s.t.Helper()
	var adds []mutate.IndexAddendum
	for _, arch := range arches {
		img, err := buildArchImage(repo, tag+"-"+arch, arch, "linux")
		if err != nil {
			s.t.Fatalf("build e2e image for %s: %v", arch, err)
		}
		// Children first, like every real client: blobs and child
		// manifests must exist before the index referencing them
		// lands (WriteIndex alone uploads only the index). The
		// per-arch tags are scaffolding — verdicts only name the
		// index tag.
		child := s.imageRef(repo, tag+"-"+arch)
		if err := remote.Write(child, img); err != nil {
			s.t.Fatalf("push e2e child for %s: %v", arch, err)
		}
		adds = append(adds, mutate.IndexAddendum{Add: img})
	}
	idx := mutate.IndexMediaType(mutate.AppendManifests(empty.Index, adds...), types.OCIImageIndex)
	ref := s.imageRef(repo, tag)
	if err := remote.WriteIndex(ref, idx); err != nil {
		s.t.Fatalf("push e2e index: %v", err)
	}
	d, err := idx.Digest()
	if err != nil {
		s.t.Fatalf("e2e index digest: %v", err)
	}
	s.RecordRow(repo, tag, d.String(), pushedAgo)
	return d.String()
}

// DeleteManifest removes a manifest upstream, out from under kpr —
// the external deletion the untagged policy exists for.
func (s *Scenario) DeleteManifest(repo, ref string) {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := s.reg.DeleteManifest(ctx, repo, ref); err != nil {
		s.t.Fatalf("external delete %s@%s: %v", repo, ref, err)
	}
}

// AttachArtifact pins a typed OCI artifact onto a subject manifest,
// tags it for retention, and records the row — the signed-artifact
// shape: the artifact expires on its own TTL while the subject lives
// on. It returns the artifact digest.
func (s *Scenario) AttachArtifact(repo, subjectDigest, tag, artifactType, payload string, pushedAgo time.Duration) string {
	s.t.Helper()
	digest := orasAttach(s.t, s.fx, repo, subjectDigest, tag, artifactType, payload)
	s.RecordRow(repo, tag, digest, pushedAgo)
	return digest
}

// ExpectTagPresent asserts the tag is still listed — the survivor
// half of a precision verdict (the subject outlives its artifact).
func (s *Scenario) ExpectTagPresent(repo, tag string) {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	tags, err := s.reg.Catalog(ctx, repo)
	if err != nil {
		s.t.Fatalf("read e2e catalog: %v", err)
	}
	for _, t := range tags {
		if t == tag {
			return
		}
	}
	s.t.Fatalf("tag %s:%s missing from catalog %v", repo, tag, tags)
}

// ExpectManifestFetchable asserts the digest still resolves upstream
// (a HEAD, never a DELETE) — removing the artifact must not take the
// subject with it.
func (s *Scenario) ExpectManifestFetchable(repo, digest string) {
	s.t.Helper()
	ref, err := name.NewDigest(s.fx.RegistryHostPort()+"/"+repo+"@"+digest, name.Insecure)
	if err != nil {
		s.t.Fatalf("e2e fetch ref: %v", err)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := remote.Head(ref, remote.WithContext(ctx)); err != nil {
		s.t.Fatalf("fetch %s@%s: %v", repo, digest, err)
	}
}

// ReapArmed evaluates the keeper policies and marks every due row —
// the armed `reap` branch, minus its printed report.
func (s *Scenario) ReapArmed() {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	if _, err := keeper.Reap(ctx, s.store, s.reg, nil, time.Now(), nil, "all", true); err != nil {
		s.t.Fatalf("reap policies: %v", err)
	}
}

// ExpectDue asserts the row is marked due with a reason containing
// want — the reap verdict, before any sweep runs.
func (s *Scenario) ExpectDue(repo, tag, want string) {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.store.All(ctx)
	if err != nil {
		s.t.Fatalf("read e2e rows: %v", err)
	}
	for _, r := range rows {
		if r.Repo == repo && r.Tag == tag {
			if !r.Due {
				s.t.Fatalf("row %s:%s not due after reap", repo, tag)
			}
			if !strings.Contains(r.Reason, want) {
				s.t.Fatalf("row %s:%s reason = %q, want substring %q", repo, tag, r.Reason, want)
			}
			return
		}
	}
	s.t.Fatalf("row %s:%s not tracked", repo, tag)
}

// ExpectNotDue asserts the row is tracked but not marked — the
// survivor half of a retention verdict.
func (s *Scenario) ExpectNotDue(repo, tag string) {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.store.All(ctx)
	if err != nil {
		s.t.Fatalf("read e2e rows: %v", err)
	}
	for _, r := range rows {
		if r.Repo == repo && r.Tag == tag {
			if r.Due {
				s.t.Fatalf("row %s:%s due (%q), want survivor", repo, tag, r.Reason)
			}
			return
		}
	}
	s.t.Fatalf("row %s:%s not tracked", repo, tag)
}

// ExpectDueCount asserts exactly n rows are marked — no silent
// over- or under-marking around the named verdicts.
func (s *Scenario) ExpectDueCount(n int) {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.store.All(ctx)
	if err != nil {
		s.t.Fatalf("read e2e rows: %v", err)
	}
	var due int
	for _, r := range rows {
		if r.Due {
			due++
		}
	}
	if due != n {
		s.t.Fatalf("due rows = %d, want %d", due, n)
	}
}

// SweepArmed runs one armed pass — the tick's work, on demand — and
// returns its summary for the test's verdict. The first sweep
// establishes the lineage (push a sentinel through the API, pair
// the store): scenario registries start unproven, and serve only
// ticks proven ground.
func (s *Scenario) SweepArmed() sweep.Summary {
	s.t.Helper()
	s.establishLineage()
	ctx, cancel := s.ctx()
	defer cancel()
	return s.sweeper.RunPass(ctx, "e2e")
}

// establishLineage pushes one sentinel generation through the API
// and pairs the scenario store to it, once per scenario. The config
// blob IS the payload (oras --config), so the served manifest reads
// as a genuine generation — no fixture backdoor.
func (s *Scenario) establishLineage() {
	s.t.Helper()
	if s.lined {
		return
	}
	s.lined = true
	ctx, cancel := s.ctx()
	defer cancel()
	gen := fmt.Sprintf("e2e-%d", time.Now().UTC().UnixNano())
	payload := fmt.Sprintf(`{"v":1,"gen":%q,"id":"e2e-lineage","ts":%q,"writer":"e2e"}`,
		gen, time.Now().UTC().Format(time.RFC3339))
	file := toolboxFile(s.t, ctx, sentinel.Repo, gen, payload)
	dst := s.fx.RegistryDirect() + "/" + sentinel.Repo + ":latest"
	toolbox.Exec(s.t, ctx, "oras", "push", "--plain-http", "--disable-path-validation",
		"--config", file+":application/json", dst, file+":application/octet-stream")
	if _, _, err := sentinel.Read(ctx, s.reg, sentinel.Repo, sentinel.Tag); err != nil {
		s.t.Fatalf("establish lineage: pushed sentinel unreadable: %v", err)
	}
	if err := s.store.SetIdentity(ctx, store.Identity{ID: "e2e-lineage", BaselineGen: gen}); err != nil {
		s.t.Fatalf("establish lineage: pair store: %v", err)
	}
}

// ExpectAbsentFromCatalog asserts the tag left the registry catalog —
// the delete really landed upstream, not just in kpr's books.
func (s *Scenario) ExpectAbsentFromCatalog(repo, tag string) {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	tags, err := s.reg.Catalog(ctx, repo)
	if err != nil {
		s.t.Fatalf("read e2e catalog: %v", err)
	}
	for _, t := range tags {
		if t == tag {
			s.t.Fatalf("tag %s:%s still in catalog %v after sweep", repo, tag, tags)
		}
	}
}

// ExpectRowGone asserts the swept row left the store — no residue for
// the next pass to trip on.
func (s *Scenario) ExpectRowGone(repo, tag string) {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	rows, err := s.store.All(ctx)
	if err != nil {
		s.t.Fatalf("read e2e rows: %v", err)
	}
	for _, r := range rows {
		if r.Repo == repo && r.Tag == tag {
			s.t.Fatalf("row %s:%s still tracked after sweep", repo, tag)
		}
	}
}
