//go:build e2e

package e2e

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/gc"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/sweep"
)

// The lineage matrix, pinned against a live registry: every verdict
// that refuses in unit tests refuses here too, and the adopt
// ceremony heals each one. NTP points at a closed port (fast warn
// path — the sandbox has no UDP egress); hermetic clock behavior is
// pinned in unit tests.

// stageLineage boots one registry with its config, a collector stub,
// and a fresh file store: one isolated lineage per test.
func stageLineage(t *testing.T) (ctx context.Context, api *registry.Client, url, root, cfg, bin string, state *store.FileStore) {
	t.Helper()
	url, root = startMountedRegistry(t)
	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	ctx = c
	api = registry.NewClient(url)
	cfg = stageRegistryConfig(t, root)
	bin = filepath.Join(t.TempDir(), "collector-stub")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("stage collector stub: %v", err)
	}
	state = store.NewFileStore(t.TempDir())
	return ctx, api, url, root, cfg, bin, state
}

func lineageCollect(collected *[][]string) gc.Collector {
	return func(context.Context, io.Writer, string, []string, gc.Reporter) error {
		*collected = append(*collected, []string{"collected"})
		return nil
	}
}

func liveRun(t *testing.T, ctx context.Context, out *strings.Builder, collected *[][]string, api *registry.Client, url, cfg, bin string, state *store.FileStore, opts gc.Options) error {
	t.Helper()
	// Clearance mirrors the CLI: the collector is a stub (nothing
	// is really deleted) and the staged config carries no cache,
	// so armed runs prove the cache off the world and carry
	// explicit acceptance only for the unfenced gateway (no edge
	// listens here). Previews need neither.
	var fenceAccept proof.AcceptedRisk
	if !opts.DryRun {
		fenceAccept = proof.Force(proof.Arm(true, false), true)
	}
	return gc.Run(ctx, out, gc.ProbeRegistry, state, lineageCollect(collected), api, url, cfg, bin,
		state, state, state, clock.HTTPS{}, stageTimeServer(t), opts, nil, fenceAccept)
}

func freshPayload(gen, id string) sentinel.Payload {
	return sentinel.Payload{V: 1, Gen: gen, ID: id,
		TS: time.Now().UTC().Format(time.RFC3339), Writer: "e2e"}
}

func mustGen(t *testing.T) string {
	t.Helper()
	gen, err := sentinel.NewGen()
	if err != nil {
		t.Fatalf("mint generation: %v", err)
	}
	return gen
}

// Unlock on a fresh registry establishes the pairing: the store
// holds an identity, the served proof carries it, writes open.
func TestE2ELineageUnlockEstablishesLive(t *testing.T) {
	ctx, api, _, _, cfg, _, state := stageLineage(t)
	var out strings.Builder
	if err := gc.Unlock(ctx, &out, api, cfg, state, state, state, state, clock.HTTPS{}, stageTimeServer(t)); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	ident, err := state.GetIdentity(ctx)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if ident.ID == "" {
		t.Fatal("unlock succeeded but recorded no identity")
	}
	served, _, err := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	if served.ID != ident.ID {
		t.Errorf("served %q != stored %q", served.ID, ident.ID)
	}
	if ok, err := state.IsUnlocked(ctx); err != nil || !ok {
		t.Fatalf("post-unlock IsUnlocked = (%v, %v), want (true, nil)", ok, err)
	}
}

// A foreign generation served to a paired store refuses even forced:
// the served bytes read back untouched. `kpr adopt` re-pairs (and
// prunes the old epoch's rows), and the next run mints under the
// adopted lineage. If this fails, gc clobbers live registries.
func TestE2ELineageForeignRefusesThenAdoptHeals(t *testing.T) {
	ctx, api, url, root, cfg, bin, state := stageLineage(t)
	var out strings.Builder
	if err := gc.Unlock(ctx, &out, api, cfg, state, state, state, state, clock.HTTPS{}, stageTimeServer(t)); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	foreignGen, foreignID := mustGen(t), mustGen(t)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, freshPayload(foreignGen, foreignID)); err != nil {
		t.Fatalf("stage foreign generation: %v", err)
	}
	var collected [][]string
	out.Reset()
	err := liveRun(t, ctx, &out, &collected, api, url, cfg, bin, state,
		gc.Options{Force: true, Report: func(gc.Event) {}})
	if err == nil {
		t.Fatal("gc over a foreign lineage succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "foreign lineage") {
		t.Fatalf("refusal names no cause: %v", err)
	}
	back, _, err := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read back served: %v", err)
	}
	if back.Gen != foreignGen || back.ID != foreignID {
		t.Fatalf("served generation overwritten: %+v", back)
	}

	out.Reset()
	if err := gc.Adopt(ctx, &out, api, state, state, "", ""); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ident, _ := state.GetIdentity(ctx)
	if ident.ID != foreignID {
		t.Fatalf("paired to %q, serves %q", ident.ID, foreignID)
	}
	rows, err := state.All(ctx)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	for _, r := range rows {
		if r.Repo == sentinel.Repo {
			t.Fatalf("old-epoch sentinel row survived adopt: %+v", r)
		}
	}

	out.Reset()
	collected = nil
	if err := liveRun(t, ctx, &out, &collected, api, url, cfg, bin, state,
		gc.Options{Force: true, Report: func(gc.Event) {}}); err != nil {
		t.Fatalf("gc after adopt: %v", err)
	}
	served, _, err := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	if served.ID != foreignID {
		t.Errorf("post-adopt proof carries %q, adopted %q", served.ID, foreignID)
	}
	if len(collected) != 1 {
		t.Errorf("collector ran %d times, want one proof pass", len(collected))
	}
}

// A rollback warns through a preview without minting; accepting it
// via `kpr adopt --gen` lets the next armed run mint past cleanly —
// no incident language once the operator has owned it.
func TestE2ELineageRollbackAdoptGenHeals(t *testing.T) {
	ctx, api, url, root, cfg, bin, state := stageLineage(t)
	var out strings.Builder
	if err := gc.Unlock(ctx, &out, api, cfg, state, state, state, state, clock.HTTPS{}, stageTimeServer(t)); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	gen0, _, err := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var collected [][]string
	if err := liveRun(t, ctx, &out, &collected, api, url, cfg, bin, state,
		gc.Options{Force: true, Report: func(gc.Event) {}}); err != nil {
		t.Fatalf("second mint: %v", err)
	}
	gen2, _, err := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read gen2: %v", err)
	}
	if gen2.Gen == gen0.Gen {
		t.Fatalf("second mint served the same generation")
	}
	ident, _ := state.GetIdentity(ctx)

	// Restore the baseline under a tracked newer generation.
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, freshPayload(gen0.Gen, ident.ID)); err != nil {
		t.Fatalf("restore baseline: %v", err)
	}
	out.Reset()
	if err := liveRun(t, ctx, &out, &collected, api, url, cfg, bin, state,
		gc.Options{DryRun: true, Report: func(gc.Event) {}}); err != nil {
		t.Fatalf("dry-run over a rollback: %v", err)
	}
	if !strings.Contains(out.String(), "older than tracked") {
		t.Fatalf("preview warns nothing about the rollback:\n%s", out.String())
	}

	out.Reset()
	if err := gc.Adopt(ctx, &out, api, state, state, "", gen0.Gen); err != nil {
		t.Fatalf("Adopt --gen: %v", err)
	}
	if ident2, _ := state.GetIdentity(ctx); ident2.BaselineGen != gen0.Gen {
		t.Fatalf("baseline %q, accepted %q", ident2.BaselineGen, gen0.Gen)
	}

	out.Reset()
	collected = nil
	if err := liveRun(t, ctx, &out, &collected, api, url, cfg, bin, state,
		gc.Options{Force: true, Report: func(gc.Event) {}}); err != nil {
		t.Fatalf("gc after accept: %v", err)
	}
	if strings.Contains(out.String(), "older than tracked") {
		t.Errorf("accepted rollback still reads as incident:\n%s", out.String())
	}
	served, _, err := sentinel.Read(ctx, api, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served: %v", err)
	}
	if served.Gen == gen0.Gen || served.Gen == gen2.Gen {
		t.Errorf("no fresh mint past the accepted rollback: %+v", served)
	}
}

// The sweeper refuses a foreign registry in preview too (nothing
// due gets planned); after the adopt it plans normally. Dry-run
// throughout: nothing is ever deleted.
func TestE2ELineageSweeperRefusesForeignDryRun(t *testing.T) {
	ctx, api, _, root, cfg, _, state := stageLineage(t)
	var out strings.Builder
	if err := gc.Unlock(ctx, &out, api, cfg, state, state, state, state, clock.HTTPS{}, stageTimeServer(t)); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := state.Record(ctx, policy.Row{Repo: "scratch", Tag: "10m", Digest: "sha256:a",
		PushedAt: time.Now().UTC().Add(-time.Hour), Due: true, Reason: "ttl:10m elapsed"}); err != nil {
		t.Fatalf("stage due row: %v", err)
	}
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, freshPayload(mustGen(t), mustGen(t))); err != nil {
		t.Fatalf("stage foreign generation: %v", err)
	}
	sw := &sweep.Sweeper{Store: state, Registry: api, Sentinel: api, DryRun: true}
	sum := sw.RunPass(ctx, "e2e-lineage")
	if !sum.Skipped {
		t.Error("sweep over a foreign lineage was not skipped")
	}
	if len(sum.Failures) != 1 || !strings.Contains(sum.Failures[0], "foreign lineage") {
		t.Errorf("failures = %v, want the foreign-lineage refusal", sum.Failures)
	}
	if sum.Planned != 0 {
		t.Errorf("refused sweep planned %d rows", sum.Planned)
	}

	out.Reset()
	if err := gc.Adopt(ctx, &out, api, state, state, "", ""); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	sum = sw.RunPass(ctx, "e2e-lineage")
	if len(sum.Failures) != 0 {
		t.Errorf("failures after adopt = %v, want none", sum.Failures)
	}
	if sum.Planned != 1 {
		t.Errorf("planned = %d, want the due row", sum.Planned)
	}
}
