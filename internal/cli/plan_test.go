package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// plan prints pending candidates with reasons, optionally as JSON for
// piping into other tools. If this fails, the dry-run view and the
// reap view diverge — or scripts cannot consume the plan.
func TestPlanListsDueWithReasons(t *testing.T) {
	var out bytes.Buffer
	if err := runPlan(cliCtx(), &out, cliStore(), false); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if !strings.Contains(out.String(), "scratch:10m") || !strings.Contains(out.String(), "ttl:10m elapsed") {
		t.Errorf("plan missing candidate or reason:\n%s", out.String())
	}
	if strings.Contains(out.String(), "app:latest") {
		t.Errorf("plan lists unmarked :latest:\n%s", out.String())
	}

	var jout bytes.Buffer
	if err := runPlan(cliCtx(), &jout, cliStore(), true); err != nil {
		t.Fatalf("runPlan json: %v", err)
	}
	var decoded []map[string]string
	if err := json.Unmarshal(jout.Bytes(), &decoded); err != nil {
		t.Fatalf("plan --json is not JSON: %v\n%s", err, jout.String())
	}
	if len(decoded) != 1 || decoded[0]["reason"] == "" {
		t.Errorf("plan --json = %v, want one candidate with a reason", decoded)
	}
}

// plan with several due rows renders them sorted by repo then tag: a
// swapped comparator must fail here. If this fails, the plan order
// contract is unpinned.
func TestPlanRendersRowsSorted(t *testing.T) {
	s := store.NewMemStore()
	c := context.Background()
	_ = s.Record(c, policy.Row{Repo: "zebra", Tag: "v1", Digest: "sha256:1",
		PushedAt: cliNow, Due: true, Reason: "x"})
	_ = s.Record(c, policy.Row{Repo: "apple", Tag: "v2", Digest: "sha256:2",
		PushedAt: cliNow, Due: true, Reason: "y"})
	_ = s.Record(c, policy.Row{Repo: "apple", Tag: "v1", Digest: "sha256:3",
		PushedAt: cliNow, Due: true, Reason: "z"})
	var out bytes.Buffer
	if err := runPlan(cliCtx(), &out, s, false); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	body := out.String()
	ordered := []string{"apple:v1", "apple:v2", "zebra:v1"}
	last := -1
	for _, want := range ordered {
		at := strings.Index(body, want)
		if at <= last {
			t.Errorf("plan order broken at %q:\n%s", want, body)
			break
		}
		last = at
	}
}

// plan against dead state fails naming redis instead of printing an
// empty plan: "nothing due" must mean empty, never "unreadable". If
// this fails, an outage renders as a clean bill of health.
func TestPlanOnDeadRedisFails(t *testing.T) {
	if err := runPlan(cliCtx(), io.Discard, deadStore{}, false); err == nil {
		t.Error("plan on dead redis succeeded, want an error")
	}
}

// An empty plan says so: "nothing due" must mean empty, never
// unreadable. If this fails, clean stores print blank.
func TestPlanEmptySaysNothingDue(t *testing.T) {
	var out bytes.Buffer
	if err := runPlan(cliCtx(), &out, store.NewMemStore(), false); err != nil {
		t.Fatalf("plan on empty store: %v", err)
	}
	if got := out.String(); got != "nothing due\n" {
		t.Errorf("plan = %q, want the empty line", got)
	}
}

// A broken pipe must surface as an error, not a silent short plan: a
// truncated plan piped into approval tooling reads as approval-worthy.
// If this fails, output errors vanish.
func TestPlanSurfacesWriteError(t *testing.T) {
	if err := runPlan(cliCtx(), errWriter{}, cliStore(), false); err == nil {
		t.Error("plan into broken pipe succeeded, want an error")
	}
}
