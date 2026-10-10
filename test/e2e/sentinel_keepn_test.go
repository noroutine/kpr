//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// Twelve minted generations, oldest first: keep-N must mark only the
// two oldest with keep-n:exceeds 10, the sweep must delete their docs
// by digest, and the floater must keep serving the newest throughout.
// Reaped gen tags stay listed (dead links for the future dead-link
// pass); their docs 404. If this fails, retention either keeps
// everything (litter returns) or eats the proof.
func TestSentinelGenerationsKeepNReaped(t *testing.T) {
	url, root := startMountedRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	api := registry.NewClient(url)
	st := store.NewMemStore()
	now := time.Now().UTC()

	const lineageID = "0193abcd-0000-7000-8000-000000000099"
	if err := st.SetIdentity(ctx, store.Identity{ID: lineageID, BaselineGen: "0193abcd-0000-7000-8000-000000000012"}); err != nil {
		t.Fatalf("pair store: %v", err)
	}
	// The subject here is keep-n sweeping, not the lock: open the
	// store the way a paired deployment holds it (locked refusal
	// itself is covered in lock_test).
	if err := st.SetUnlocked(ctx, true); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	gens := make([]string, 0, 12)
	digests := map[string]string{}
	for k := 1; k <= 12; k++ {
		gen := fmt.Sprintf("0193abcd-0000-7000-8000-%012d", k)
		gens = append(gens, gen)
		md, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, sentinel.Payload{
			V: 1, Gen: gen, ID: lineageID, TS: now.Format(time.RFC3339), Writer: "e2e",
		})
		if err != nil {
			t.Fatalf("Write gen %d: %v", k, err)
		}
		digests[gen] = md
		if err := st.Record(ctx, policy.Row{
			Repo: sentinel.Repo, Tag: gen, Digest: md,
			MediaType: sentinel.ManifestMediaType,
			PushedAt:  now.Add(time.Duration(k) * time.Second), Actor: "e2e",
		}); err != nil {
			t.Fatalf("Record gen %d: %v", k, err)
		}
	}
	if err := sentinel.Verify(ctx, api, sentinel.Repo, sentinel.Tag, gens[11]); err != nil {
		t.Fatalf("floater serves %v, want newest gen", err)
	}

	marked, err := keeper.Reap(ctx, st, api, now.Add(time.Hour), nil, "keep-n", proof.Arm(true, false))
	if err != nil {
		t.Fatalf("Reap keep-n: %v", err)
	}
	if len(marked) != 2 {
		t.Fatalf("marked = %d, want the 2 oldest generations", len(marked))
	}
	for i, gen := range gens[:2] {
		found := false
		for _, m := range marked {
			if m.Repo == sentinel.Repo && m.Tag == gen && strings.Contains(m.Reason, "keep-n:exceeds 10") {
				found = true
			}
		}
		if !found {
			t.Errorf("gen %d (%s) not marked keep-n:exceeds 10", i+1, gen)
		}
	}

	sum := (&sweep.Sweeper{Store: st, Registry: api, Sentinel: api, Armed: proof.Arm(true, false)}).RunPass(ctx, "e2e")
	if sum.Performed != 2 || sum.Failed != 0 {
		t.Fatalf("sweep = %+v, want 2 performed, 0 failed", sum)
	}

	for _, gen := range gens[:2] {
		if _, err := api.GetManifest(ctx, sentinel.Repo, digests[gen]); err == nil {
			t.Errorf("reaped doc %s still fetchable, want gone", digests[gen])
		}
	}
	for _, gen := range gens[2:] {
		if _, err := api.GetManifest(ctx, sentinel.Repo, digests[gen]); err != nil {
			t.Errorf("survivor doc %s unfetchable: %v", digests[gen], err)
		}
	}
	tags, err := api.Catalog(ctx, sentinel.Repo)
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	listed := map[string]bool{}
	for _, tg := range tags {
		listed[tg] = true
	}
	// The digest delete unlinks referencing tags server-side: reaped
	// gens leave neither doc nor tag link. (Dangling links from
	// crashed deletes are the dead-link pass's job, not this path's.)
	for _, gen := range gens[:2] {
		if listed[gen] {
			t.Errorf("reaped gen tag %s still listed, want gone with its doc", gen)
		}
	}
	if !listed[sentinel.Tag] {
		t.Errorf("floater %q unlisted, want serving", sentinel.Tag)
	}
	if err := sentinel.Verify(ctx, api, sentinel.Repo, sentinel.Tag, gens[11]); err != nil {
		t.Errorf("floater after sweep: %v, want newest gen", err)
	}
	rows, err := st.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(rows) != 10 {
		t.Errorf("tracked rows = %d, want the 10 survivors", len(rows))
	}
	for _, r := range rows {
		if r.Tag == sentinel.Tag {
			t.Errorf("floater tracked, want never (this test pins absence-of-row; the name spare is unit-pinned)")
		}
	}
}
