package gc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var (
	errReleaseFailed = errors.New("release failed")
	errProbeDead     = errors.New("probe dead")
)

// fileAPI serves sentinel generations straight from the staged root:
// tag link to revision blob, manifest to config blob, layer link
// checked like the registry checks it. Whatever Write laid down,
// this serves back — a file-backed fake registry for the proof half
// of Run. A different (or empty) root serves nothing.
type fileAPI struct{ root string }

func (f fileAPI) blob(root, digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	return os.ReadFile(filepath.Join(root, "docker", "registry", "v2", "blobs", "sha256", hex[:2], hex, "data"))
}

func (f fileAPI) GetManifest(_ context.Context, repo, tag string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(f.root, "docker", "registry", "v2", "repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return nil, err
	}
	return f.blob(f.root, strings.TrimSpace(string(raw)))
}

func (f fileAPI) GetBlob(_ context.Context, repo, digest string) ([]byte, error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	if _, err := os.Stat(filepath.Join(f.root, "docker", "registry", "v2", "repositories", repo, "_layers", "sha256", hex, "link")); err != nil {
		return nil, err
	}
	return f.blob(f.root, digest)
}

// frozenAPI serves one fixed generation whatever the store holds: a
// stale snapshot on demand. If Run accepts it, the proof compares
// anything but the generation it just wrote.
type frozenAPI struct {
	manifest []byte
	blob     []byte
}

func (f frozenAPI) GetManifest(context.Context, string, string) ([]byte, error) {
	return f.manifest, nil
}

func (f frozenAPI) GetBlob(context.Context, string, string) ([]byte, error) {
	return f.blob, nil
}

// stageProvenRun stages a config over an empty root and returns the
// config, the root, and a lock: the sentinel Write inside Run lays
// the proof ground itself, so tests start empty and vary one port.
// stubClock answers a fixed offset (or error): the verdict-on-offset
// wiring without the network. Zero value is a healthy clock.
type stubClock struct {
	off time.Duration
	err error
}

func (s stubClock) Offset(context.Context, string) (time.Duration, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.off, nil
}

var errClockUnreachable = errors.New("no route to time source")

func stageProvenRun(t *testing.T) (string, string, *store.MemStore) {
	t.Helper()
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+"\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	// Runs prove locality per pass, but intent is the operator's:
	// staged stores arrive unlocked so tests vary the ports, not
	// the marker. Fresh-locked is pinned by the storetest contract
	// and the dedicated refusal test below.
	s := store.NewMemStore()
	if err := s.SetUnlocked(context.Background(), true); err != nil {
		t.Fatalf("stage unlock: %v", err)
	}
	return cfg, root, s
}

func okCollector(collected *[][]string) Collector {
	return func(_ context.Context, _ io.Writer, _ string, args []string, _ Reporter) error {
		*collected = append(*collected, args)
		return nil
	}
}

// A verified mint records its generation row for keep-N: repo, gen
// tag, digest, actor. If this fails, generations go untracked and
// keep-N can never reap them — the litter returns silently.
func TestRunRecordsMintedGeneration(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: false, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("stub-port run: %v", err)
	}
	rows, err := s.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("tracked rows = %d, want exactly the minted generation", len(rows))
	}
	r := rows[0]
	if r.Repo != sentinel.Repo {
		t.Errorf("row repo = %q, want %q", r.Repo, sentinel.Repo)
	}
	if r.Tag == "" || r.Tag == sentinel.Tag {
		t.Errorf("row tag = %q, want the gen tag, never the floater", r.Tag)
	}
	if !strings.HasPrefix(r.Digest, "sha256:") || len(r.Digest) != 7+64 {
		t.Errorf("row digest = %q, want sha256:<64hex>", r.Digest)
	}
	if r.Actor != "kpr-gc" {
		t.Errorf("row actor = %q, want kpr-gc", r.Actor)
	}
	if r.PushedAt.IsZero() {
		t.Error("row PushedAt is zero, keep-N orders by it")
	}
}

// The orchestration runs behind stub ports: a fixed readonly probe,
// a file-backed fake registry, and a recording collector drive a
// full pass with no network and no binary. If this fails, Run reaches
// past its ports to the concrete world.
func TestRunBehindStubPorts(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	stagePairedGen(t, lock, root)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("stub-port run: %v", err)
	}
	if len(collected) != 1 || !hasArg(collected[0], "--dry-run") {
		t.Errorf("collector got %v, want one --dry-run invocation", collected)
	}
	if strings.Contains(out.String(), "shared store proven via") {
		t.Errorf("dry-run names a proof it never minted:\n%s", out.String())
	}
	if !strings.HasSuffix(strings.TrimRight(out.String(), "\n"), "dry-run complete: nothing was deleted (collect for real with --no-dry-run)") {
		t.Errorf("dry-run verdict is not the last line:\n%s", out.String())
	}
}

// A stranger's store — the fake serves a different root — refuses
// in preview too: the read finds nothing to serve, armed or not.
// If this fails, gc previews whatever directory the mount points
// at.
func TestRunStrangerStoreRefuses(t *testing.T) {
	cfg, _, lock := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	for _, dry := range []bool{true, false} {
		var collected [][]string
		var out strings.Builder
		err := Run(context.Background(), &out, probe, lock, okCollector(&collected), fileAPI{t.TempDir()},
			"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
			Options{DryRun: dry, Report: func(Event) {}})
		want := "does not share"
		if dry {
			// A preview cannot establish pairing: nothing served
			// refuses at the read, before any proof language.
			want = "no sentinel served"
		}
		if err == nil {
			t.Fatalf("dry=%v gc on a stranger's store succeeded, want refusal", dry)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("dry=%v refusal names no cause: %v", dry, err)
		}
		if len(collected) != 0 {
			t.Errorf("dry=%v collector ran %v on unproven ground, want no invocation", dry, collected)
		}
	}
}

// The read gate refuses what it cannot attribute, armed: an
// identity-less served generation (wipe, then re-establish) and a
// served lineage the store never paired to (`kpr adopt` first).
// The matrix pins these verdicts pure; this pins them through the
// wiring that enforces them. If this fails, gc mints over
// evidence it cannot own.
func TestRunRefusesUnattributableLineage(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name    string
		payload sentinel.Payload
		want    string
	}{
		{name: "identity-less", payload: sentinel.Payload{V: 1, Gen: newGenID(t), TS: now}, want: "identity-less"},
		{name: "unpaired", payload: sentinel.Payload{V: 1, Gen: newGenID(t), ID: newGenID(t), TS: now}, want: "unpaired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, root, lock := stageProvenRun(t)
			if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, tc.payload); err != nil {
				t.Fatalf("stage served: %v", err)
			}
			var collected [][]string
			var out strings.Builder
			err := Run(ctx, &out, readonlyProbe(), lock, okCollector(&collected), fileAPI{root},
				"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
				Options{Report: func(Event) {}})
			if err == nil {
				t.Fatalf("gc over %s lineage succeeded, want refusal", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal names no cause: %v", err)
			}
			if len(collected) != 0 {
				t.Errorf("refused run collected %d times, want none", len(collected))
			}
		})
	}
}

// A stale snapshot — the fake serves a fixed old generation —
// refuses too: same files, wrong content. Freshness is armed-only
// (only a fresh mint distinguishes stale from shared); dry-run
// reads presence, and the frozen fake answers. If this fails, the
// proof is presence of the sentinel, not freshness, and old
// snapshots pass silently.
func TestRunStaleSnapshotRefuses(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	stagePairedGen(t, lock, root)
	manRaw, err := fileAPI{root}.GetManifest(context.Background(), sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read staged manifest: %v", err)
	}
	var staged struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manRaw, &staged); err != nil {
		t.Fatalf("parse staged manifest: %v", err)
	}
	payRaw, err := fileAPI{root}.GetBlob(context.Background(), sentinel.Repo, staged.Config.Digest)
	if err != nil {
		t.Fatalf("read staged blob: %v", err)
	}
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	frozen := frozenAPI{manifest: manRaw, blob: payRaw}
	var collected [][]string
	var out strings.Builder
	err = Run(context.Background(), &out, probe, lock, okCollector(&collected), frozen,
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{DryRun: false, Force: true, Report: func(Event) {}})
	if err == nil {
		t.Fatal("gc on a stale snapshot succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "does not share") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// releaseFailLocker drops the lock release: the run still passes
// (the lock expires), but warns loud instead of pretending a clean
// handoff. If this fails, a leaked lock reads as orderly.
func TestRunWarnsOnReleaseFailure(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	stagePairedGen(t, s, root)
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out,
		Probe(func(context.Context, string) (Mode, string, error) { return ModeReadonly, "", nil }),
		releaseFailLocker{s}, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("release-failed run: %v", err)
	}
	if !strings.Contains(out.String(), "gc lock release failed") {
		t.Errorf("output lacks the release warning:\n%s", out.String())
	}
}

type releaseFailLocker struct {
	*store.MemStore
}

func (releaseFailLocker) ReleaseLock(context.Context, string) error {
	return errReleaseFailed
}

// A mode flip between pre- and post-probe fails the run: writes may
// have raced the mark phase, and silence would bless the collect.
// --force passes warned instead — the operator presumed to know. If
// this fails, mid-run writes go unnoticed either way.
func TestRunFlipRefusesUnlessForced(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	stagePairedGen(t, s, root)
	flipProbe := func() Probe {
		calls := 0
		return func(context.Context, string) (Mode, string, error) {
			calls++
			if calls == 1 {
				return ModeReadonly, "", nil
			}
			return ModeWritable, "", nil
		}
	}
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, flipProbe(), s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}})
	if err == nil || !strings.Contains(err.Error(), "mode changed") {
		t.Fatalf("flipped run = %v, want the mode-change refusal", err)
	}
	out.Reset()
	if err := Run(context.Background(), &out, flipProbe(), s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Force: true, Report: func(Event) {}}); err != nil {
		t.Fatalf("forced flipped run: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("forced flip lacks the WARNING:\n%s", out.String())
	}
}

// A dry-run against a store serving no sentinel refuses: the read
// is the whole gate — presence, not freshness — and silence means
// stranger, empty, or down. If this fails, previews describe stores
// kpr never shared.
func TestRunDryRunRefusesWithoutSentinel(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}})
	if err == nil || !strings.Contains(err.Error(), "no sentinel served") {
		t.Fatalf("dry-run on silence = %v, want the no-shared-store refusal", err)
	}
	if len(collected) != 0 {
		t.Fatalf("refused preview reached the collector")
	}
}

// A dry-run previews without minting: no generation is written, no
// row recorded — a preview changes nothing, so it litters nothing.
// The read gate above still applies: this stages a servable
// generation first. If this fails, every default gc invocation
// costs a generation.
func TestRunDryRunSkipsProof(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	stagePairedGen(t, s, root)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if strings.Contains(out.String(), "shared store proven via") {
		t.Errorf("dry-run names a proof it never minted:\n%s", out.String())
	}
	if len(collected) != 1 || !hasArg(collected[0], "--dry-run") {
		t.Errorf("collector got %v, want one --dry-run invocation", collected)
	}
	rows, rerr := s.All(context.Background())
	if rerr != nil {
		t.Fatalf("All: %v", rerr)
	}
	if len(rows) != 0 {
		t.Errorf("dry-run recorded %d rows, want none", len(rows))
	}
	// The staged generation stays the only revision: the preview
	// minted nothing beside it.
	revs, rverr := filepath.Glob(filepath.Join(root, "docker", "registry", "v2", "repositories", sentinel.Repo, "_manifests", "revisions", "sha256", "*", "link"))
	if rverr != nil {
		t.Fatalf("glob revisions: %v", rverr)
	}
	if len(revs) != 1 {
		t.Errorf("revisions after dry-run = %d, want the 1 staged", len(revs))
	}
}

// A writable run without --force refuses before the proof: no
// generation is minted, no row recorded — refusing work must not
// leave the litter it refused to collect. If this fails, every
// refused run costs a tracked generation.
func TestRunWritableRefusalMintsNothing(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeWritable, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: false, Report: func(Event) {}})
	if err == nil || !strings.Contains(err.Error(), "registry is writable") {
		t.Fatalf("writable run = %v, want the writable refusal", err)
	}
	if len(collected) != 0 {
		t.Fatalf("refused run reached the collector")
	}
	rows, rerr := s.All(context.Background())
	if rerr != nil {
		t.Fatalf("All: %v", rerr)
	}
	if len(rows) != 0 {
		t.Errorf("refused run recorded %d rows, want none", len(rows))
	}
	if _, serr := os.Stat(filepath.Join(root, "docker", "registry", "v2", "repositories", sentinel.Repo)); !os.IsNotExist(serr) {
		t.Errorf("refused run laid the sentinel repo, want untouched root")
	}
}

// A dead post-probe only warns: the collect already happened against
// proven ground, and refusing now would lie about work done. If this
// fails, a transient probe blip fails a good run.
func TestRunDeadPostProbeWarns(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	stagePairedGen(t, s, root)
	calls := 0
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		calls++
		if calls == 1 {
			return ModeReadonly, "", nil
		}
		return ModeUnknown, "", errProbeDead
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("dead-post-probe run: %v", err)
	}
	if !strings.Contains(out.String(), "post-run probe failed") {
		t.Errorf("output lacks the post-probe warning:\n%s", out.String())
	}
}

// stagePairedRun stages a fresh ID-bearing generation and pairs the
// store to it: the served lineage matches, so only the clock or the
// mode gates can refuse. If this fails, the pairing itself is wrong,
// not the gate under test.
func stagePairedRun(t *testing.T) (cfg, root, localID, gen string, lock *store.MemStore) {
	t.Helper()
	cfg, root, lock = stageProvenRun(t)
	gen = stagePairedGen(t, lock, root)
	ident, err := lock.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read paired identity: %v", err)
	}
	localID = ident.ID
	return cfg, root, localID, gen, lock
}

// stagePairedGen writes a fresh ID-bearing generation and pairs the
// store to it: downstream-gate tests need the verdict to pass so the
// gate under test fires. It returns the served generation.
func stagePairedGen(t *testing.T, lock *store.MemStore, root string) string {
	t.Helper()
	gen := newGenID(t)
	id := newGenID(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: id, TS: now}); err != nil {
		t.Fatalf("stage paired generation: %v", err)
	}
	if err := lock.SetIdentity(context.Background(), store.Identity{
		ID: id, BaselineGen: gen, AdoptedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("pair store: %v", err)
	}
	return gen
}

func newGenID(t *testing.T) string {
	t.Helper()
	id, err := sentinel.NewGen()
	if err != nil {
		t.Fatalf("mint generation id: %v", err)
	}
	return id
}

func readonlyProbe() Probe {
	return Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
}

// A foreign lineage — the registry serves an ID the store never
// paired to — refuses even forced: --force accepts clock skew and a
// writable registry, never a stranger's store. Nothing mints (the
// served generation still reads back) and nothing records. If this
// fails, gc clobbers registries mounted by mistake.
func TestRunForeignLineageRefusesBeforeMint(t *testing.T) {
	cfg, root, _, gen, lock := stagePairedRun(t)
	foreign := newGenID(t)
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag,
		sentinel.Payload{V: 1, Gen: gen, ID: foreign}); err != nil {
		t.Fatalf("stage foreign generation: %v", err)
	}
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{Force: true, Report: func(Event) {}})
	if err == nil {
		t.Fatal("gc over a foreign lineage succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "foreign lineage") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("refused run collected %d times, want none", len(collected))
	}
	rows, rerr := lock.All(context.Background())
	if rerr != nil {
		t.Fatalf("read rows: %v", rerr)
	}
	if len(rows) != 0 {
		t.Errorf("refused run recorded %d rows, want none", len(rows))
	}
	back, _, rerr := sentinel.Read(context.Background(), fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if rerr != nil {
		t.Fatalf("read back served: %v", rerr)
	}
	if back.Gen != gen {
		t.Errorf("served generation overwritten: got %s want %s", back.Gen, gen)
	}
}

// Silence with an unpaired store establishes the pairing in-band:
// the first armed run against a fresh registry mints its baseline
// and records the store's ID from the served payload. If this fails,
// every new registry needs a manual adopt before its first gc.
func TestRunSilenceEstablishesPairing(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}})
	if err != nil {
		t.Fatalf("first run on silence: %v", err)
	}
	ident, err := lock.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if ident.ID == "" {
		t.Error("run succeeded but recorded no identity")
	}
	if len(collected) != 1 {
		t.Errorf("collected %d times, want one proof pass", len(collected))
	}
	served, _, err := sentinel.Read(context.Background(), fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served baseline: %v", err)
	}
	if served.ID != ident.ID {
		t.Errorf("served ID %q != stored ID %q", served.ID, ident.ID)
	}
}

// Skew beyond tolerance refuses without --force and warns through
// it: mint timestamps come from a checked clock. If this fails, a
// host with a drifting clock mints generations that sort wrong.
func TestRunClockSkewRefusesUnlessForced(t *testing.T) {
	cfg, root, _, _, lock := stagePairedRun(t)
	var out strings.Builder
	var refused [][]string
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(&refused), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{off: time.Hour}, "time.example.com",
		Options{Report: func(Event) {}})
	if err == nil {
		t.Fatal("skewed clock run succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "clock skew") {
		t.Errorf("refusal names no cause: %v", err)
	}
	var forced [][]string
	out.Reset()
	err = Run(context.Background(), &out, readonlyProbe(), lock, okCollector(&forced), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{off: time.Hour}, "time.example.com",
		Options{Force: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("forced skewed run: %v", err)
	}
	if !strings.Contains(out.String(), "clock skew") {
		t.Errorf("forced run warns nothing about the skew it accepted:\n%s", out.String())
	}
	if len(forced) != 1 {
		t.Errorf("forced run collected %d times, want one", len(forced))
	}
}

// A served generation older than tracked refuses armed without
// --force: the registry may have been restored, and minting past a
// rollback without an explicit accept hides the incident. The live
// matrix proves the ceremony around this; here the refusal itself
// is pinned (live registries are writable, which would refuse
// first). If this fails, gc mints over rollbacks silently.
// newRow tracks a generation at a fixed age: stale verdicts compare
// served gens against the newest tracked push.
func newRow(repo, tag string, at time.Time) policy.Row {
	return policy.Row{Repo: repo, Tag: tag, Digest: "sha256:" + tag,
		MediaType: sentinel.ManifestMediaType, PushedAt: at, Actor: "test"}
}

func TestRunStaleRollbackRefusesArmed(t *testing.T) {
	cfg, root, localID, gen, lock := stagePairedRun(t)
	ctx := context.Background()
	// Post-mint identities carry no baseline (only `adopt --gen`
	// records one): the served gen must not come pre-accepted.
	if err := lock.SetIdentity(ctx, store.Identity{ID: localID}); err != nil {
		t.Fatalf("clear baseline: %v", err)
	}
	now := time.Now().UTC()
	if err := lock.Record(ctx, newRow(sentinel.Repo, gen, now.Add(-time.Hour))); err != nil {
		t.Fatalf("track served generation: %v", err)
	}
	newer := newGenID(t)
	if err := lock.Record(ctx, newRow(sentinel.Repo, newer, now)); err != nil {
		t.Fatalf("track newer generation: %v", err)
	}
	var collected [][]string
	var out strings.Builder
	err := Run(ctx, &out, readonlyProbe(), lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}})
	if err == nil {
		t.Fatal("gc over a rollback succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "older than tracked") {
		t.Errorf("refusal names no cause: %v", err)
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal names no override: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("refused run collected %d times, want none", len(collected))
	}
}

// A forced run over a rollback warns the incident away instead of
// hiding it: the mint proceeds, but the output keeps the evidence.
// If this fails, --force also silences the audit trail.
func TestRunStaleForceWarns(t *testing.T) {
	cfg, root, localID, gen, lock := stagePairedRun(t)
	ctx := context.Background()
	if err := lock.SetIdentity(ctx, store.Identity{ID: localID}); err != nil {
		t.Fatalf("clear baseline: %v", err)
	}
	now := time.Now().UTC()
	if err := lock.Record(ctx, newRow(sentinel.Repo, gen, now.Add(-time.Hour))); err != nil {
		t.Fatalf("track served generation: %v", err)
	}
	if err := lock.Record(ctx, newRow(sentinel.Repo, newGenID(t), now)); err != nil {
		t.Fatalf("track newer generation: %v", err)
	}
	var collected [][]string
	var out strings.Builder
	err := Run(ctx, &out, readonlyProbe(), lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{Force: true, Report: func(Event) {}})
	if err != nil {
		t.Fatalf("forced run over a rollback: %v", err)
	}
	if !strings.Contains(out.String(), "older than tracked") {
		t.Errorf("forced run warns nothing about the rollback it accepted:\n%s", out.String())
	}
	if len(collected) != 1 {
		t.Errorf("forced run collected %d times, want one", len(collected))
	}
}

// Establishing under an already-paired store warns and preserves
// the pairing: the accepted baseline and adopted-at survive the
// re-mint (a wipe revokes no acceptance). If this fails, a wiped
// tag reads as a fresh deploy and revokes the operator's accept.
func TestRunEstablishPairedWarns(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	ctx := context.Background()
	id := newGenID(t)
	adopted := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := lock.SetIdentity(ctx, store.Identity{ID: id, BaselineGen: "accepted-gen", AdoptedAt: adopted}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	var collected [][]string
	var out strings.Builder
	err := Run(ctx, &out, readonlyProbe(), lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}})
	if err != nil {
		t.Fatalf("re-establish: %v", err)
	}
	if !strings.Contains(out.String(), "re-minting the baseline") {
		t.Errorf("re-establish warns nothing about the missing tag:\n%s", out.String())
	}
	ident, err := lock.GetIdentity(ctx)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if ident.ID != id || ident.BaselineGen != "accepted-gen" {
		t.Errorf("re-establish rewrote the pairing: %+v", ident)
	}
	if !ident.AdoptedAt.Equal(adopted) {
		t.Errorf("re-establish moved adopted-at to %v", ident.AdoptedAt)
	}
	// The warning is not the outcome: the proof still mints past it.
	if len(collected) != 1 {
		t.Errorf("warned run collected %d times, want one proof pass", len(collected))
	}
	served, _, err := sentinel.Read(ctx, fileAPI{root}, sentinel.Repo, sentinel.Tag)
	if err != nil {
		t.Fatalf("read served baseline: %v", err)
	}
	if served.ID != id {
		t.Errorf("re-minted baseline carries %q, paired to %q", served.ID, id)
	}
}

// An unreachable NTP warns and proceeds on local time: air-gapped
// sites stay working, and the warning says which clock to distrust.
// If this fails, a firewall becomes a gc outage.
func TestRunNTPUnreachableWarnsProceeds(t *testing.T) {
	cfg, root, _, _, lock := stagePairedRun(t)
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{err: errClockUnreachable}, "time.example.com",
		Options{Report: func(Event) {}})
	if err != nil {
		t.Fatalf("run with unreachable NTP: %v", err)
	}
	if !strings.Contains(out.String(), "proceeding with local clock") {
		t.Errorf("run warns nothing about the NTP it could not reach:\n%s", out.String())
	}
	if len(collected) != 1 {
		t.Errorf("collected %d times, want one", len(collected))
	}
}
