package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/proof"
)

// sweep runs the pass in-process and prints its summary: dry-run
// plans (the row stays), armed deletes by digest (the row goes).
// No console, no POST — the sweeper lives in the CLI. If this
// fails, the pass either cannot run or hides the counts.
func TestSweepRunsDirectDryRunAndArmed(t *testing.T) {
	s, stub := pairedSweepStore(t)
	var out bytes.Buffer
	if err := runSweep(cliCtx(), &out, s, stub, nil, ""); err != nil {
		t.Fatalf("dry-run sweep: %v", err)
	}
	if !strings.Contains(out.String(), "1 performed") || !strings.Contains(out.String(), "1 planned") {
		t.Errorf("dry-run summary missing counts:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "(dry run — nothing deleted)") {
		t.Errorf("dry-run summary hides the mode:\n%s", out.String())
	}
	if rows, _ := s.All(context.Background()); len(rows) != 1 {
		t.Errorf("dry-run kept %d rows, want the 1 planned row kept", len(rows))
	}

	as, astub := pairedSweepStore(t)
	var aout bytes.Buffer
	if err := runSweep(cliCtx(), &aout, as, astub, proof.Arm(true, false), ""); err != nil {
		t.Fatalf("armed sweep: %v", err)
	}
	if !strings.Contains(aout.String(), "1 performed") || !strings.Contains(aout.String(), "1 planned") {
		t.Errorf("armed summary missing counts:\n%s", aout.String())
	}
	if strings.Contains(aout.String(), "dry run") {
		t.Errorf("armed summary claims dry-run:\n%s", aout.String())
	}
	if rows, _ := as.All(context.Background()); len(rows) != 0 {
		t.Errorf("armed kept %d rows, want the deleted row dropped", len(rows))
	}
}

// sweep on dead state reports the outage in the summary instead of
// failing: the pass runs nowhere, resolves nothing, and narrates
// that. If this fails, an outage prints a confident count of
// nothing.
func TestSweepOutageReportsInsteadOfFailing(t *testing.T) {
	var out bytes.Buffer
	stub := sweepStub{stubProofAPI: stubProofAPI{err: errors.New("redis: connection refused")}}
	if err := runSweep(cliCtx(), &out, deadStore{}, stub, nil, ""); err != nil {
		t.Errorf("sweep on dead state failed: %v", err)
	}
	if !strings.Contains(out.String(), "failed:") {
		t.Errorf("outage summary names no failure:\n%s", out.String())
	}
}

// A pipe breaking mid-summary surfaces the error: a half-printed pass
// summary must not read as success. If this fails, truncated output
// passes silently.
func TestSweepSurfacesMidSummaryWriteError(t *testing.T) {
	s, stub := pairedSweepStore(t)
	if err := runSweep(cliCtx(), &failAfterWriter{}, s, stub, nil, ""); err == nil {
		t.Error("sweep into failing pipe succeeded, want an error")
	}
}

// A pipe breaking on a failure line surfaces the error too: the
// summary line already printed, so only the failures-line check
// catches it. If this fails, a truncated failure list reads as a
// clean pass.
func TestSweepSurfacesFailureLineWriteError(t *testing.T) {
	s, stub := pairedSweepStore(t)
	stub.delErr = errors.New("registry: 500")
	var ok bytes.Buffer
	if err := runSweep(cliCtx(), &ok, s, stub, proof.Arm(true, false), ""); err != nil {
		t.Fatalf("armed failing sweep: %v", err)
	}
	if !strings.Contains(ok.String(), "failed:") {
		t.Fatalf("no failure lines to break on:\n%s", ok.String())
	}
	fs, fstub := pairedSweepStore(t)
	fstub.delErr = errors.New("registry: 500")
	if err := runSweep(cliCtx(), &failAfterWriter{n: 1}, fs, fstub, proof.Arm(true, false), ""); err == nil {
		t.Error("sweep failing on the failures line succeeded, want an error")
	}
}

// --output tees the summary plus failure lines into a file while
// stdout keeps them: an outage narrates in both places. Row records ride
// OTLP-only and never touch stdout (the terminal belongs to
// the live line), so their absence here is the contract. A bad
// path refuses before the pass. If this fails, the stream lands
// in one place only, or nowhere.
func TestSweepOutputTeesFailuresToFile(t *testing.T) {
	s, stub := pairedSweepStore(t)
	stub.delErr = errors.New("registry: 500")
	stream := filepath.Join(t.TempDir(), "sweep.log")
	var out bytes.Buffer
	if err := runSweep(cliCtx(), &out, s, stub, proof.Arm(true, false), stream); err != nil {
		t.Fatalf("armed failing sweep: %v", err)
	}
	raw, err := os.ReadFile(stream)
	if err != nil {
		t.Fatalf("read stream file: %v", err)
	}
	if !strings.Contains(string(raw), "failed:") {
		t.Errorf("stream file names no failure:\n%s", raw)
	}
	if !strings.Contains(out.String(), "failed:") {
		t.Errorf("stdout lost its failure lines:\n%s", out.String())
	}
	if strings.Contains(out.String(), "sweep row") {
		t.Errorf("row chatter on stdout:\n%s", out.String())
	}
	fs, fstub := pairedSweepStore(t)
	fstub.delErr = errors.New("registry: 500")
	if err := runSweep(cliCtx(), io.Discard, fs, fstub, proof.Arm(true, false),
		filepath.Join(t.TempDir(), "gone", "sweep.log")); err == nil {
		t.Error("sweep --output into missing dir succeeded, want refusal")
	}
}

// --output captures the settled summary even when nothing fails:
// a clean pass must leave its one-line receipt in the log, not an
// empty file. If this fails, --output only carries failures and a
// green run logs nothing.
func TestSweepOutputCapturesSummary(t *testing.T) {
	s, stub := pairedSweepStore(t)
	stream := filepath.Join(t.TempDir(), "sweep.log")
	var out bytes.Buffer
	if err := runSweep(cliCtx(), &out, s, stub, proof.Arm(true, false), stream); err != nil {
		t.Fatalf("armed sweep: %v", err)
	}
	raw, err := os.ReadFile(stream)
	if err != nil {
		t.Fatalf("read stream file: %v", err)
	}
	if !strings.Contains(string(raw), "1 performed") {
		t.Errorf("stream file lacks the summary line:\n%s", raw)
	}
	if !strings.Contains(string(raw), "swept scratch:10m sha256:a") {
		t.Errorf("stream file lacks the per-row verdict:\n%s", raw)
	}
	if strings.Contains(out.String(), "swept scratch:10m") {
		t.Errorf("stdout carries per-row chatter:\n%s", out.String())
	}
}

// The sweep command arms from its own flag or the one-shot env var —
// never the dead serve loop's. The flag binding and the env half are
// both wired here: RunE only sees the proof. If this fails, `sweep`
// answers to the wrong var and the suite can't see it.
func TestSweepArmingWiring(t *testing.T) {
	plain := config.NewBuilder().Build()
	if armed := proof.Arm(sweepNoDryRun, plain.CLINoDryRun); !proof.Unarmed(armed) {
		t.Error("sweep armed by default, want implicit dry-run")
	}
	envArmed := config.NewBuilder().WithCLINoDryRun(true).Build()
	if armed := proof.Arm(sweepNoDryRun, envArmed.CLINoDryRun); proof.Unarmed(armed) {
		t.Error("KPR_CLI_NO_DRY_RUN=true left sweep disarmed, want armed")
	}
	// The cobra binding: parsing --no-dry-run must flip the same
	// global RunE reads. Reset after: the flag is process-global.
	if err := sweepCmd.Flags().Set("no-dry-run", "true"); err != nil {
		t.Fatalf("set --no-dry-run: %v", err)
	}
	defer func() {
		_ = sweepCmd.Flags().Set("no-dry-run", "false")
	}()
	if armed := proof.Arm(sweepNoDryRun, plain.CLINoDryRun); proof.Unarmed(armed) {
		t.Error("--no-dry-run parsed but sweep stayed disarmed, want armed")
	}
}
