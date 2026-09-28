//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"nrtn.dev/catalyst/kpr/internal/cli"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/registry"
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
	t.Cleanup(func() { _ = s.Close() })
	reg := registry.NewClient(fx.RegistryURL())
	return &Scenario{
		t:       t,
		fx:      fx,
		store:   s,
		reg:     reg,
		sweeper: &sweep.Sweeper{Store: s, Registry: reg, DryRun: false},
	}
}

func (s *Scenario) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), e2eTimeout)
}

// Push writes a real single-layer image to the fixture registry and
// records the row the receiver would track for it (repo/tag/digest,
// push time backdated by pushedAgo so scenarios expire rows without
// sleeping on a clock). It returns the manifest digest.
func (s *Scenario) Push(repo, tag string, pushedAgo time.Duration) string {
	s.t.Helper()
	layer := static.NewLayer([]byte("kpr-e2e:"+repo+":"+tag), types.MediaType("application/octet-stream"))
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		s.t.Fatalf("build e2e image: %v", err)
	}
	ref, err := name.NewTag(s.fx.RegistryHostPort()+"/"+repo+":"+tag, name.Insecure)
	if err != nil {
		s.t.Fatalf("e2e image ref: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		s.t.Fatalf("push e2e image: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		s.t.Fatalf("e2e image digest: %v", err)
	}
	ctx, cancel := s.ctx()
	defer cancel()
	row := policy.Row{
		Repo:     repo,
		Tag:      tag,
		Digest:   d.String(),
		PushedAt: time.Now().Add(-pushedAgo),
		Actor:    "e2e",
	}
	if err := s.store.Record(ctx, row); err != nil {
		s.t.Fatalf("record e2e row: %v", err)
	}
	return d.String()
}

// ReapArmed evaluates the CLI's own policies and marks every due row —
// the armed `reap` branch, minus its printed report.
func (s *Scenario) ReapArmed() {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	marked, err := cli.EvaluatePolicies(ctx, s.store, s.reg, time.Now())
	if err != nil {
		s.t.Fatalf("evaluate policies: %v", err)
	}
	for _, r := range marked {
		if err := s.store.MarkDue(ctx, r.Repo, r.Tag, r.Reason); err != nil {
			s.t.Fatalf("mark e2e row due: %v", err)
		}
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

// SweepArmed runs one armed pass — the tick's work, on demand — and
// returns its summary for the test's verdict.
func (s *Scenario) SweepArmed() sweep.Summary {
	s.t.Helper()
	ctx, cancel := s.ctx()
	defer cancel()
	return s.sweeper.RunPass(ctx, "e2e")
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
