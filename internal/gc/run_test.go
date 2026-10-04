package gc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var (
	errReleaseFailed = errors.New("release failed")
	errProbeDead     = errors.New("probe dead")
)

// acceptRisk mints one acceptance the way the command does for an
// --accept-* flag on an armed run. Each test holds only the token
// its gate needs — a fully-accepted run spells all five fields,
// never a shared umbrella.
func acceptRisk() proof.AcceptedRisk {
	return proof.Force(proof.Arm(true, false), true)
}

// acceptAll spells every acceptance: for runs that must refuse (or
// pass) with nothing left unaccepted, so the verdict pins the
// gate under test, not a missing token.
func acceptAll() Accepts {
	return Accepts{
		Cache:     acceptRisk(),
		Fence:     acceptRisk(),
		ClockSkew: acceptRisk(),
		Rollback:  acceptRisk(),
		ModeFlip:  acceptRisk(),
	}
}

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
		Options{DryRun: false, Report: func(Event) {}}, Accepts{})
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
		Options{DryRun: true, Report: func(Event) {}}, Accepts{})
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

// The husk verdict prints only when husks were removed: a clean
// root narrates the walks but invents no removals, a husked root
// names its count. If this fails, empty runs invent removals (or
// real ones go unannounced).
func TestRunHuskVerdictNamesRemovals(t *testing.T) {
	runArmed := func(t *testing.T, stage func(v2 string)) string {
		t.Helper()
		cfg, root, s := stageProvenRun(t)
		v2 := filepath.Join(root, "docker", "registry", "v2", "repositories")
		if err := os.MkdirAll(v2, 0o755); err != nil {
			t.Fatalf("stage v2: %v", err)
		}
		if stage != nil {
			stage(v2)
		}
		stagePairedGen(t, s, root)
		probe := Probe(func(context.Context, string) (Mode, string, error) {
			return ModeReadonly, "", nil
		})
		var collected [][]string
		var out strings.Builder
		err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
			"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
			Options{DryRun: false, Report: func(Event) {}}, Accepts{})
		if err != nil {
			t.Fatalf("stub-port armed run: %v", err)
		}
		return out.String()
	}
	if clean := runArmed(t, nil); strings.Contains(clean, "removed") {
		t.Errorf("clean root announces removals:\n%s", clean)
	}
	dirty := runArmed(t, func(v2 string) {
		p := filepath.Join(v2, "husk", "_manifests", "revisions", "sha256", "bbb", "link")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("stage husk dir: %v", err)
		}
		if err := os.WriteFile(p, []byte("sha256:bbb"), 0o644); err != nil {
			t.Fatalf("stage husk link: %v", err)
		}
	})
	// The verdict counts, never names: a 169-husk run must not
	// flood the log with inventory. If this fails, the names
	// are back on the line.
	if !strings.Contains(dirty, "pruned 1 husks\n") {
		t.Errorf("husked root hides its removal:\n%s", dirty)
	}
	if strings.Contains(dirty, "(husk)") {
		t.Errorf("husk verdict names its repos:\n%s", dirty)
	}
	// The silent walks narrate their start: the past-tense count
	// must read as after-the-fact, never dropped from nowhere
	// after minutes of quiet. If this fails, a start line moved
	// after its work (or vanished).
	if strings.Index(dirty, "pruning husks...\n") > strings.Index(dirty, "pruned 1 husks\n") ||
		!strings.Contains(dirty, "pruning husks...\n") {
		t.Errorf("husk start does not precede its verdict:\n%s", dirty)
	}
	if strings.Index(dirty, "pruning empty directories...\n") > strings.Index(dirty, " empty directories\n") ||
		!strings.Contains(dirty, "pruning empty directories...\n") {
		t.Errorf("dir-prune start does not precede its verdict:\n%s", dirty)
	}
}

// A failing armed collect fails the run: the readonly-armed branch
// surfaces the collector error like the preview does. If this fails,
// real-run collection errors vanish into a nil return.
func TestRunArmedCollectFailureSurfaces(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	stagePairedGen(t, lock, root)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	failCollector := func(context.Context, io.Writer, string, []string, Reporter) error {
		return errors.New("collector exploded")
	}
	var out strings.Builder
	err := Run(context.Background(), &out, probe, lock, failCollector, fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{DryRun: false, Report: func(Event) {}}, Accepts{})
	if err == nil || !strings.Contains(err.Error(), "collector exploded") {
		t.Fatalf("armed collect failure = %v, want the collector error surfaced", err)
	}
}

// tailFailWriter passes every write to the buffer except the final
// verdict line, which fails: the dry-run tail is a real write, and
// its failure must surface, not vanish into a nil return.
type tailFailWriter struct {
	buf strings.Builder
	err error
}

func (w *tailFailWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "dry-run complete") {
		if w.err == nil {
			w.err = errors.New("tail write failed")
		}
		return 0, w.err
	}
	return w.buf.Write(p)
}

// A failing final verdict write fails the preview: swallowing it
// would report success while the operator never saw the verdict.
// If this fails, output errors below the event stream go quiet.
func TestRunDryRunTailWriteFailureSurfaces(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	stagePairedGen(t, lock, root)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	out := &tailFailWriter{}
	err := Run(context.Background(), out, probe, lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("dry-run with failing verdict write succeeded, want the write error")
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
			Options{DryRun: dry, Report: func(Event) {}}, Accepts{})
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
				Options{Report: func(Event) {}}, Accepts{})
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
		Options{DryRun: false, Report: func(Event) {}}, Accepts{})
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
		Options{DryRun: true, Report: func(Event) {}}, Accepts{})
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
// --accept-mode-flip passes warned instead — the operator presumed
// to know. If this fails, mid-run writes go unnoticed either way.
func TestRunFlipRefusesUnlessModeFlipAccepted(t *testing.T) {
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
		Options{DryRun: true, Report: func(Event) {}}, Accepts{})
	if err == nil || !strings.Contains(err.Error(), "mode changed") {
		t.Fatalf("flipped run = %v, want the mode-change refusal", err)
	} else if !strings.Contains(err.Error(), "--accept-mode-flip") {
		t.Errorf("refusal names no override: %v", err)
	}
	out.Reset()
	if err := Run(context.Background(), &out, flipProbe(), s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}}, Accepts{ModeFlip: acceptRisk()}); err != nil {
		t.Fatalf("mode-flip-accepted run: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("accepted flip lacks the WARNING:\n%s", out.String())
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
		Options{DryRun: true, Report: func(Event) {}}, Accepts{})
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
		Options{DryRun: true, Report: func(Event) {}}, Accepts{})
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

// A writable run without online clearance refuses before the proof:
// no generation is minted, no row recorded — refusing work must not
// leave the litter it refused to collect. If this fails, every
// refused run costs a tracked generation.
// An armed run against a serving registry clears the online
// preflight first: the staged config proves nothing (no
// relativeurls), no edge listens, no fence is configured — so the
// refusal names the gateway miss with its override, and mints
// nothing. If this fails, armed gc collects against a live
// registry believing it fenced, or mints for a run that refuses.
func TestRunWritableRefusalMintsNothing(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeWritable, "", nil
	})
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: false, Report: func(Event) {}}, Accepts{})
	if err == nil || !strings.Contains(err.Error(), "gateway") || !strings.Contains(err.Error(), "--accept-unfenced") {
		t.Fatalf("writable run = %v, want the gateway refusal with override", err)
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
		Options{DryRun: true, Report: func(Event) {}}, Accepts{})
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
// paired to — refuses even fully accepted: no acceptance names a
// stranger's store, so all five minted still refuse. Nothing mints
// (the served generation still reads back) and nothing records. If
// this fails, gc clobbers registries mounted by mistake.
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
		Options{Report: func(Event) {}}, acceptAll())
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
		Options{Report: func(Event) {}}, Accepts{})
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

// Skew beyond tolerance refuses without --accept-clock-skew and
// warns through it: mint timestamps come from a checked clock. If
// this fails, a host with a drifting clock mints generations that
// sort wrong.
func TestRunClockSkewRefusesUnlessAccepted(t *testing.T) {
	cfg, root, _, _, lock := stagePairedRun(t)
	var out strings.Builder
	var refused [][]string
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(&refused), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{off: time.Hour}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("skewed clock run succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "clock skew") {
		t.Errorf("refusal names no cause: %v", err)
	} else if !strings.Contains(err.Error(), "--accept-clock-skew") {
		t.Errorf("refusal names no override: %v", err)
	}
	var accepted [][]string
	out.Reset()
	err = Run(context.Background(), &out, readonlyProbe(), lock, okCollector(&accepted), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{off: time.Hour}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{ClockSkew: acceptRisk()})
	if err != nil {
		t.Fatalf("skew-accepted run: %v", err)
	}
	if !strings.Contains(out.String(), "clock skew") {
		t.Errorf("accepted run warns nothing about the skew it accepted:\n%s", out.String())
	}
	if len(accepted) != 1 {
		t.Errorf("accepted run collected %d times, want one", len(accepted))
	}
}

// A served generation older than tracked refuses armed without
// --accept-rollback: the registry may have been restored, and
// minting past a rollback without an explicit accept hides the
// incident. The live
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
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("gc over a rollback succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "older than tracked") {
		t.Errorf("refusal names no cause: %v", err)
	} else if !strings.Contains(err.Error(), "--accept-rollback") {
		t.Errorf("refusal names no override: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("refused run collected %d times, want none", len(collected))
	}
}

// An accepted run over a rollback warns the incident away instead
// of hiding it: the mint proceeds, but the output keeps the
// evidence. If this fails, acceptance also silences the audit
// trail.
func TestRunRollbackAcceptWarns(t *testing.T) {
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
		Options{Report: func(Event) {}}, Accepts{Rollback: acceptRisk()})
	if err != nil {
		t.Fatalf("rollback-accepted run: %v", err)
	}
	if !strings.Contains(out.String(), "older than tracked") {
		t.Errorf("accepted run warns nothing about the rollback it accepted:\n%s", out.String())
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
		Options{Report: func(Event) {}}, Accepts{})
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
		Options{Report: func(Event) {}}, Accepts{})
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

// scriptLocker varies the lock port per refusal scenario: an
// unreadable marker, a dead backend, a held lock. The zero value
// holds nothing, so each test names what it stages.
type scriptLocker struct {
	*store.MemStore
	unlockErr  error
	held       bool
	acquireErr error
}

func (s scriptLocker) IsUnlocked(ctx context.Context) (bool, error) {
	if s.unlockErr != nil {
		return false, s.unlockErr
	}
	return s.MemStore.IsUnlocked(ctx)
}

func (s scriptLocker) AcquireLock(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	if s.acquireErr != nil {
		return false, s.acquireErr
	}
	return s.held, nil
}

// errIdentityStore fails the lineage read: the backend-outage
// stand-in for the pairing record.
type errIdentityStore struct{ err error }

func (e errIdentityStore) GetIdentity(context.Context) (store.Identity, error) {
	return store.Identity{}, e.err
}

func (e errIdentityStore) SetIdentity(context.Context, store.Identity) error { return e.err }

// errRecorder fails the keep-N write: the backend-outage stand-in
// for the generation log.
type errRecorder struct{ err error }

func (e errRecorder) Record(context.Context, policy.Row) error { return e.err }

// An unreadable lock marker refuses before anything mints: unknown
// intent is not unlocked intent. If this fails, a backend outage at
// the marker reads as permission.
func TestRunLockUnreadableRefuses(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	lock := scriptLocker{MemStore: s, unlockErr: errTestStoreDown, held: true}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with unreadable lock succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "store lock unreadable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A missing registry binary refuses at readiness: no mint, no
// probe, no lock held. If this fails, a bare image mints a proof
// no collector can redeem.
func TestRunMissingBinaryRefuses(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	lock := scriptLocker{MemStore: s, held: true}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/nonexistent-registry", s, s, s, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with missing binary succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "registry binary not found") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A config with no filesystem root refuses before the mint: local
// collection only understands the shared directory layout. If this
// fails, gc mints against a store it cannot prove local.
func TestRunS3ConfigRefuses(t *testing.T) {
	_, _, s := stageProvenRun(t)
	s3cfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(s3cfg, []byte("storage:\n  s3:\n    bucket: blobs\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	lock := scriptLocker{MemStore: s, held: true}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{t.TempDir()},
		"http://registry:5000", s3cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with s3 config succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "no filesystem storage root") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A dead lock backend refuses with the remedy: the operator learns
// redis is down, not that gc is broken. If this fails, a redis
// outage reports a mystery error.
func TestRunAcquireFailureRefuses(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	lock := scriptLocker{MemStore: s, held: true, acquireErr: errTestStoreDown}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with dead lock backend succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "redis unreachable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A held lock refuses without minting: two collectors never mark
// together. If this fails, concurrent runs double-collect.
func TestRunContendedLockRefuses(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	lock := scriptLocker{MemStore: s, held: false}
	var collected [][]string
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run under a held lock succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "another gc run holds the lock") {
		t.Errorf("refusal names no cause: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("refused run collected %d times, want none", len(collected))
	}
}

// A dead pre-probe refuses with the probe error: the mode is input,
// not a default. If this fails, an unreachable registry collects
// under an assumed mode.
func TestRunDeadPreProbeRefuses(t *testing.T) {
	cfg, root, s := stageProvenRun(t)
	lock := scriptLocker{MemStore: s, held: true}
	dead := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeUnknown, "", errProbeDead
	})
	var out strings.Builder
	err := Run(context.Background(), &out, dead, lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if !errors.Is(err, errProbeDead) {
		t.Fatalf("dead-probe run = %v, want the probe error surfaced", err)
	}
}

// A dead blobdescriptor cache refuses the readonly path: without
// the cache the run would delete what it must keep. If this fails,
// gc collects blind on a broken cache connection.
func TestRunReadonlyCacheOutageRefuses(t *testing.T) {
	_, root, s := stageProvenRun(t)
	redisCfg := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(redisCfg, []byte("storage:\n  filesystem:\n    rootdirectory: "+root+
		"\nredis:\n  addr: 127.0.0.1:1\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	lock := scriptLocker{MemStore: s, held: true}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", redisCfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with dead cache succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "blobdescriptor cache unreachable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// An accepted skew warning that cannot print fails the run: the
// acceptance audit trail is mandatory, not best-effort. If this
// fails, an accepted run over a skewed clock leaves no trace it
// did.
func TestRunSkewWarnWriteFailureSurfaces(t *testing.T) {
	cfg, root, _, _, lock := stagePairedRun(t)
	w := errWriter{errTestStoreDown}
	err := Run(context.Background(), w, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{off: time.Hour}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{ClockSkew: acceptRisk()})
	if err == nil {
		t.Fatal("skewed accepted run with dead output succeeded, want failure")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("failure names no output cause: %v", err)
	}
}

// An NTP warning that cannot print fails the run for the same
// reason: air-gapped sites stay working, but never silently.
func TestRunUnreachableWarnWriteFailureSurfaces(t *testing.T) {
	cfg, root, _, _, lock := stagePairedRun(t)
	w := errWriter{errTestStoreDown}
	err := Run(context.Background(), w, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{err: errClockUnreachable}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with dead output succeeded, want failure")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("failure names no output cause: %v", err)
	}
}

// Untracked state refuses before the verdict: silence means
// stranger, empty, or down — never proceed. If this fails, a
// backend outage at the rows reads as a clean store.
func TestRunTrackedStateFailureRefuses(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	stagePairedGen(t, lock, root)
	rows := failRows{MemStore: lock, allErr: errTestStoreDown}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, rows, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with unreadable rows succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "tracked state unreadable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// An unreadable lineage refuses before the verdict for the same
// reason: unknown pairing is not pairing. If this fails, a backend
// outage at the lineage reads as unpaired.
func TestRunLineageFailureRefuses(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	stagePairedGen(t, lock, root)
	ids := errIdentityStore{errTestStoreDown}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, ids, lock, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with unreadable lineage succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unreadable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// An unrecordable pairing refuses after the proof window: the
// generation is served but the lineage did not record — proceeding
// would orphan it. If this fails, a backend outage mid-ceremony
// reads as paired.
func TestRunLineageWriteFailureRefuses(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	ids := &failIdentityStore{MemStore: store.NewMemStore(), armed: true}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, ids, lock, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with unrecordable lineage succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "lineage unrecordable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}

// A generation the keep-N log cannot take fails the run loudly:
// the proof is held but untracked, and silence would litter. If
// this fails, a backend outage at the log reads as collected.
func TestRunMintRecordFailureRefuses(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	rec := errRecorder{errTestStoreDown}
	var out strings.Builder
	err := Run(context.Background(), &out, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", rec, lock, lock, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("run with failing keep-N log succeeded, want failure")
	} else if !strings.Contains(err.Error(), "generation went untracked") {
		t.Errorf("failure names no cause: %v", err)
	}
}

// A stale warning that cannot print fails the armed run: the
// rollback evidence must land before the mint. If this fails, a
// forced run over a rollback leaves no audit trail.
func TestRunStaleWarnWriteFailureSurfaces(t *testing.T) {
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
	w := errWriter{errTestStoreDown}
	err := Run(ctx, w, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{Rollback: acceptRisk()})
	if err == nil {
		t.Fatal("stale accepted run with dead output succeeded, want failure")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("failure names no output cause: %v", err)
	}
}

// A re-mint warning that cannot print fails the run for the same
// reason: a wiped tag is an incident, and the incident must print.
func TestRunEstablishWarnWriteFailureSurfaces(t *testing.T) {
	cfg, root, lock := stageProvenRun(t)
	ctx := context.Background()
	if err := lock.SetIdentity(ctx, store.Identity{ID: newGenID(t)}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	w := errWriter{errTestStoreDown}
	err := Run(ctx, w, readonlyProbe(), lock, okCollector(nil), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", lock, lock, lock, stubClock{}, "time.example.com",
		Options{Report: func(Event) {}}, Accepts{})
	if err == nil {
		t.Fatal("re-establish with dead output succeeded, want failure")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("failure names no output cause: %v", err)
	}
}

// An armed run prunes the collector's leftover skeleton after the
// collect; a preview prunes nothing. If this fails, armed gc keeps
// piling empty dirs, or previews mutate what they promise to only
// read.
func TestRunPrunesSkeletonArmedOnly(t *testing.T) {
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	stageGhost := func(t *testing.T, root string) string {
		t.Helper()
		ghost := filepath.Join(root, "docker", "registry", "v2", "repositories", "test", "ghost", "_manifests", "tags", "old", "current")
		if err := os.MkdirAll(ghost, 0o755); err != nil {
			t.Fatalf("stage ghost: %v", err)
		}
		return ghost
	}

	cfg, root, s := stageProvenRun(t)
	ghost := stageGhost(t, root)
	var collected [][]string
	var stages []string
	var out strings.Builder
	if err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: false, Report: func(e Event) { stages = append(stages, e.Stage) }}, Accepts{}); err != nil {
		t.Fatalf("armed run: %v", err)
	}
	if _, err := os.Stat(ghost); !os.IsNotExist(err) {
		t.Errorf("ghost skeleton survives armed gc, want pruned")
	}
	if !strings.Contains(out.String(), "pruned ") {
		t.Errorf("armed run hides the prune count:\n%s", out.String())
	}
	// The post-probe runs downstream of the prune print: a run
	// that returns right after printing never emits it.
	if !slices.Contains(stages, StagePostProbe) {
		t.Errorf("armed run emitted no post-probe (stages %v), want the full tail", stages)
	}

	cfg, root, s = stageProvenRun(t)
	stagePairedGen(t, s, root)
	ghost = stageGhost(t, root)
	var preview strings.Builder
	if err := Run(context.Background(), &preview, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: true, Report: func(Event) {}}, Accepts{}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(ghost); err != nil {
		t.Errorf("preview pruned the skeleton, want it kept: %v", err)
	}
	if strings.Contains(preview.String(), "pruned ") {
		t.Errorf("preview reports a prune it never ran:\n%s", preview.String())
	}
}

// A prune failure warns but never fails the collection: the blobs
// are already gone, and occupancy races resolve safe — only real
// I/O or permission trouble lands here, named in the warning. If
// this fails, a stuck skeleton fails a good collection with it.
// Root reads through permissions, so it sits this one out.
func TestRunPruneFailureWarnsCollectStands(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through directory permissions")
	}
	cfg, root, s := stageProvenRun(t)
	dark := filepath.Join(root, "docker", "registry", "v2", "repositories", "test", "dark")
	if err := os.MkdirAll(dark, 0o755); err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	if err := os.Chmod(dark, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dark, 0o755) })
	probe := Probe(func(context.Context, string) (Mode, string, error) {
		return ModeReadonly, "", nil
	})
	var collected [][]string
	var stages []string
	var out strings.Builder
	if err := Run(context.Background(), &out, probe, s, okCollector(&collected), fileAPI{root},
		"http://registry:5000", cfg, "/bin/sh", s, s, s, stubClock{}, "time.example.com",
		Options{DryRun: false, Report: func(e Event) { stages = append(stages, e.Stage) }}, Accepts{}); err != nil {
		t.Fatalf("prune-failed run: %v", err)
	}
	if !strings.Contains(out.String(), "empty-dir cleanup incomplete") {
		t.Errorf("prune failure warned nowhere:\n%s", out.String())
	}
	// The tail runs past a failed prune: a run that returns on
	// the warning never emits the post-probe.
	if !slices.Contains(stages, StagePostProbe) {
		t.Errorf("prune-failed run emitted no post-probe (stages %v), want the full tail", stages)
	}
}
