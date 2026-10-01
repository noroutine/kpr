// Package lineage is the pairing verdict: served sentinel evidence
// against store state, for every operation that acts on a registry.
// Pure — no network, no store calls; callers read both sides and
// Judge. The verdict table in docs/SENTINELS.md mirrors these cases
// one to one; the e2e matrix pins each outcome.
//
// Verdicts key on observables only (served content, stored pairing
// and rows, mode, flags) — mount truth is unobservable, never an
// input. Generations compare by row push time (the keep-N order),
// identities by exact string: an empty served identity never
// matches, so pre-pairing payloads refuse like foreign ones instead
// of being silently overwritten.
package lineage

import (
	"context"
	"fmt"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Served is one sentinel read: the payload and manifest digest when
// the tag answered, the read error when it didn't.
type Served struct {
	Payload sentinel.Payload
	Digest  string
	Err     error
}

// Local is the store side: the pairing plus every tracked row. Judge
// scopes rows to the sentinel repo itself.
type Local struct {
	Ident store.Identity
	Rows  []policy.Row
}

// Rows is the one method verdicts need over tracked state. The full
// store satisfies it; use cases declare only this.
type Rows interface {
	All(ctx context.Context) ([]policy.Row, error)
}

// IdentityStore pairs and re-pairs the lineage: the two methods the
// mint path needs. The full store satisfies it; use cases declare
// only these.
type IdentityStore interface {
	GetIdentity(ctx context.Context) (store.Identity, error)
	SetIdentity(ctx context.Context, id store.Identity) error
}

// Ask is the operation shape: preview or armed, overridden or not,
// at what time.
type Ask struct {
	DryRun bool
	Force  bool
	Now    time.Time
}

// Verdict is one decided case. Exactly one of the paths holds:
// Refuse (Proceed false, Establish false), Establish (armed
// silence — mint with the pairing, generating it when unpaired), or
// Proceed (act; Stale warns; Heal proposes an adopt-record row, nil
// in dry runs so previews stay read-only).
type Verdict struct {
	Proceed   bool
	Reason    string
	Action    string
	Stale     bool
	Establish bool
	Heal      *policy.Row
}

func refuse(reason, action string) Verdict {
	return Verdict{Reason: reason, Action: action}
}

// Judge decides one case. Now defaults to wall clock when unset.
func Judge(s Served, l Local, ask Ask) Verdict {
	now := ask.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if s.Err != nil {
		if sentinel.Absent(s.Err) {
			if ask.DryRun {
				return refuse(
					fmt.Sprintf("no sentinel served: %v", s.Err),
					"run an armed `kpr gc` or `kpr store unlock` first to establish pairing (or check the volume mount)")
			}
			return Verdict{Establish: true,
				Reason: fmt.Sprintf("no sentinel served: %v; establishing pairing by mint", s.Err),
				Action: "minting with the stored pairing (generating one when unpaired)"}
		}
		return refuse(
			fmt.Sprintf("sentinel unreadable: %v", s.Err),
			"verify the registry is reachable and the tag serves a valid generation")
	}
	p := s.Payload
	if p.ID == "" {
		return refuse(
			"serves an identity-less generation (pre-pairing)",
			"remove the stale tags or wipe the volume, then re-run to establish pairing; never auto-adopted")
	}
	if l.Ident.ID == "" {
		return refuse(
			fmt.Sprintf("store unpaired and the registry serves identity %s", p.ID),
			"run `kpr store adopt` to pair this store (or `kpr adopt <identity>` to pin the expected one)")
	}
	if p.ID != l.Ident.ID {
		return refuse(
			fmt.Sprintf("foreign lineage: serves %s, store paired to %s", p.ID, l.Ident.ID),
			"run `kpr store adopt` to re-pair with the served lineage (or check the volume mount if unintended)")
	}
	ts, err := time.Parse(time.RFC3339, p.TS)
	if err != nil {
		return refuse(
			fmt.Sprintf("served timestamp unparseable: %q", p.TS),
			"inspect the served payload for corruption")
	}
	if ts.After(now.Add(clock.Tolerance)) {
		return refuse(
			fmt.Sprintf("served generation from the future: %s", p.TS),
			"check clocks (NTP) on the minter and this host")
	}
	var mine []policy.Row
	for _, r := range l.Rows {
		if r.Repo == sentinel.Repo {
			mine = append(mine, r)
		}
	}
	known := map[string]bool{}
	var max policy.Row
	for _, r := range mine {
		known[r.Tag] = true
		// Ties break by tag, never slice order: uuid7 tags sort by
		// mint time, so equal pushes still have a newest.
		// NOTE(mutants): >= is equivalent — tags are unique per row
		// set, so r.Tag == max.Tag only for the row already held;
		// no second row can take the other branch.
		if r.PushedAt.After(max.PushedAt) ||
			(r.PushedAt.Equal(max.PushedAt) && r.Tag > max.Tag) {
			max = r
		}
	}
	if known[p.Gen] && p.Gen != max.Tag {
		if p.Gen != "" && p.Gen == l.Ident.BaselineGen {
			return Verdict{Proceed: true,
				Reason: fmt.Sprintf("serves adopted baseline %s (rollback accepted via `kpr store adopt --gen`)", p.Gen),
				Action: "none (armed runs mint past it)"}
		}
		reason := fmt.Sprintf("serves generation %s older than tracked %s (rollback?)", p.Gen, max.Tag)
		if ask.DryRun || ask.Force {
			return Verdict{Proceed: true, Stale: true, Reason: reason,
				Action: "run `kpr store adopt --gen " + p.Gen + "` to accept the rollback as baseline"}
		}
		return refuse(reason, "re-run with --force if the registry was intentionally restored")
	}
	if !known[p.Gen] {
		actor := p.Writer
		if actor == "" {
			actor = "kpr-heal"
		}
		v := Verdict{Proceed: true,
			Reason: fmt.Sprintf("serves untracked generation %s of our lineage; adopting", p.Gen),
			Action: "adopt-recorded for keep-N (armed runs only)"}
		if !ask.DryRun {
			v.Heal = &policy.Row{Repo: sentinel.Repo, Tag: p.Gen, Digest: s.Digest,
				MediaType: sentinel.ManifestMediaType, PushedAt: ts, Actor: actor}
		}
		return v
	}
	return Verdict{Proceed: true,
		Reason: fmt.Sprintf("serves tracked generation %s of our lineage", p.Gen),
		Action: "none (armed runs mint past it)"}
}
