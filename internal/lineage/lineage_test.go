package lineage

import (
	"errors"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

var (
	vnow   = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	vident = "0193abcd-0000-7000-8000-0000ffff0001"
	vother = "0193abcd-0000-7000-8000-0000ffff0002"
	vgen1  = "0193abcd-0000-7000-8000-000000000001"
	vgen2  = "0193abcd-0000-7000-8000-000000000002"
	vgen3  = "0193abcd-0000-7000-8000-000000000003"
)

// statusErr carries an HTTP shape without the production client:
// the verdict matches the status interface, like the reader does.
type statusErr struct{ status int }

func (e statusErr) Error() string   { return "status" }
func (e statusErr) StatusCode() int { return e.status }

func vsilence() Served { return Served{Err: statusErr{status: 404}} }

func vrow(gen string, ago time.Duration) policy.Row {
	return policy.Row{Repo: sentinel.Repo, Tag: gen, Digest: "sha256:" + gen,
		MediaType: sentinel.ManifestMediaType, PushedAt: vnow.Add(-ago), Actor: "kpr-gc"}
}

func vserved(id, gen string) Served {
	return Served{Payload: sentinel.Payload{V: 1, Gen: gen, ID: id,
		TS: vnow.Add(-time.Minute).Format(time.RFC3339), Writer: "kpr-gc"}, Digest: "sha256:" + gen}
}

// A PushedAt tie resolves by generation tag, not slice order: two
// rows pushed in the same instant still have a deterministic newest
// (uuid7 tags sort by mint time). If this fails, the rollback
// verdict depends on store iteration order.
func TestJudgeTieBreaksOnTag(t *testing.T) {
	for _, rows := range [][]policy.Row{
		{vrow(vgen1, time.Hour), vrow(vgen3, time.Hour)},
		{vrow(vgen3, time.Hour), vrow(vgen1, time.Hour)},
	} {
		local := Local{Ident: store.Identity{ID: vident}, Rows: rows}
		got := Judge(vserved(vident, vgen1), local, Ask{Now: vnow})
		if got.Proceed {
			t.Errorf("rows %v: serving the tied-older gen proceeded (%q)", rows, got.Reason)
		}
		if got := Judge(vserved(vident, vgen3), local, Ask{Now: vnow}); !got.Proceed {
			t.Errorf("rows %v: serving the tied-newest gen refused (%q)", rows, got.Reason)
		}
	}
}

// The adopt-record carries the served writer: keep-N attributes
// the adopted generation to whoever minted it, not to the healer.
// If this fails, adopted rows all read "kpr-heal".
func TestJudgeHealKeepsWriter(t *testing.T) {
	got := Judge(
		vserved(vident, "0193abcd-0000-7000-8000-000000000009"),
		Local{Ident: store.Identity{ID: vident}},
		Ask{Now: vnow})
	if got.Heal == nil {
		t.Fatalf("untracked own generation proposed no heal (%q)", got.Reason)
	}
	if got.Heal.Actor != "kpr-gc" {
		t.Errorf("heal actor = %q, want the served writer", got.Heal.Actor)
	}
}

func TestJudgeMatrix(t *testing.T) {
	paired := Local{Ident: store.Identity{ID: vident}, Rows: []policy.Row{
		vrow(vgen1, 3*time.Hour), vrow(vgen2, 2*time.Hour), vrow(vgen3, time.Hour),
	}}
	for _, tc := range []struct {
		name           string
		served         Served
		local          Local
		ask            Ask
		proceed        bool
		stale          bool
		establish      bool
		heal           bool
		reason, action string
	}{
		{
			name:   "fresh match proceeds dry and armed",
			served: vserved(vident, vgen3), local: paired,
			ask: Ask{DryRun: true, Now: vnow}, proceed: true,
		},
		{
			name:   "fresh match proceeds armed",
			served: vserved(vident, vgen3), local: paired,
			ask: Ask{Now: vnow}, proceed: true,
		},
		{
			name:   "stale warns dry",
			served: vserved(vident, vgen1), local: paired,
			ask: Ask{DryRun: true, Now: vnow}, proceed: true, stale: true,
		},
		{
			name:   "stale refuses armed",
			served: vserved(vident, vgen1), local: paired,
			ask: Ask{Now: vnow}, proceed: false, action: "--force",
		},
		{
			name:   "stale force proceeds warned",
			served: vserved(vident, vgen1), local: paired,
			ask: Ask{Force: true, Now: vnow}, proceed: true, stale: true,
		},
		{
			name:   "adopted baseline proceeds armed",
			served: vserved(vident, vgen1),
			local: Local{Ident: store.Identity{ID: vident, BaselineGen: vgen1}, Rows: []policy.Row{
				vrow(vgen1, 3*time.Hour), vrow(vgen3, time.Hour),
			}},
			ask: Ask{Now: vnow}, proceed: true,
		},
		{
			name:   "unknown gen heals armed",
			served: vserved(vident, "0193abcd-0000-7000-8000-000000000009"), local: paired,
			ask: Ask{Now: vnow}, proceed: true, heal: true,
		},
		{
			name:   "unknown gen proposes no heal dry",
			served: vserved(vident, "0193abcd-0000-7000-8000-000000000009"), local: paired,
			ask: Ask{DryRun: true, Now: vnow}, proceed: true, heal: false,
		},
		{
			name:   "foreign refuses both",
			served: vserved(vother, vgen3), local: paired,
			ask: Ask{DryRun: true, Now: vnow}, proceed: false, action: "kpr store adopt",
		},
		{
			name:   "foreign refuses armed force",
			served: vserved(vother, vgen3), local: paired,
			ask: Ask{Force: true, Now: vnow}, proceed: false, action: "kpr store adopt",
		},
		{
			name:   "unpaired store refuses served lineage",
			served: vserved(vother, vgen3), local: Local{},
			ask: Ask{DryRun: true, Now: vnow}, proceed: false, action: "kpr store adopt",
		},
		{
			name:   "id-less served refuses like foreign",
			served: vserved("", vgen3), local: paired,
			ask: Ask{DryRun: true, Now: vnow}, proceed: false,
		},
		{
			name:   "silence refuses dry",
			served: vsilence(), local: paired,
			ask: Ask{DryRun: true, Now: vnow}, proceed: false,
		},
		{
			name:   "silence establishes armed",
			served: vsilence(), local: paired,
			ask: Ask{Now: vnow}, proceed: false, establish: true,
		},
		{
			name:   "garbage refuses both",
			served: Served{Err: errors.New("payload unparseable")}, local: paired,
			ask: Ask{DryRun: true, Now: vnow}, proceed: false,
		},
		{
			name: "future timestamp refuses",
			served: Served{Payload: sentinel.Payload{V: 1, Gen: vgen3, ID: vident,
				TS: vnow.Add(time.Hour).Format(time.RFC3339)}, Digest: "sha256:x"},
			local: paired, ask: Ask{DryRun: true, Now: vnow}, proceed: false,
		},
		{
			name: "unparseable timestamp refuses",
			served: Served{Payload: sentinel.Payload{V: 1, Gen: vgen3, ID: vident, TS: "not-a-time"},
				Digest: "sha256:x"},
			local: paired, ask: Ask{DryRun: true, Now: vnow}, proceed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Judge(tc.served, tc.local, tc.ask)
			if got.Proceed != tc.proceed {
				t.Errorf("Proceed = %v, want %v (reason %q)", got.Proceed, tc.proceed, got.Reason)
			}
			if got.Stale != tc.stale {
				t.Errorf("Stale = %v, want %v", got.Stale, tc.stale)
			}
			if got.Establish != tc.establish {
				t.Errorf("Establish = %v, want %v", got.Establish, tc.establish)
			}
			if (got.Heal != nil) != tc.heal {
				t.Errorf("Heal = %+v, want presence %v", got.Heal, tc.heal)
			}
			if tc.heal && (got.Heal.Tag == "" || got.Heal.Digest == "" || got.Heal.PushedAt.IsZero()) {
				t.Errorf("Heal row incomplete: %+v", got.Heal)
			}
			if got.Reason == "" {
				t.Error("empty Reason: refusals and warnings must name the cause")
			}
			if !tc.proceed && !tc.establish && got.Action == "" {
				t.Error("refusal without Action: every no needs a runnable next step")
			}
			if tc.action != "" && !strings.Contains(got.Action, tc.action) {
				t.Errorf("Action = %q, want substring %q", got.Action, tc.action)
			}
		})
	}
}
