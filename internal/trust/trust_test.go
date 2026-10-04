package trust

import (
	"errors"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// mistrust404 is the registry silence sentinel reads see: a 404
// from the tag address, never corruption, never transport.
type mistrust404 struct{}

func (mistrust404) Error() string   { return "status 404" }
func (mistrust404) StatusCode() int { return 404 }

// errMistrust is non-silence failure: the tag errored without a
// status, so absence cannot be told from transport trouble.
var errMistrust = errors.New("transport boom")

// name maps one judged case to its display word — paired when the
// verdict is clean, a mistrust name per shape when it isn't. If
// this fails, a display colors a trustworthy store — or trusts a
// broken one.
func TestName(t *testing.T) {
	now := time.Now().UTC()
	row := func(tag string, ago time.Duration) policy.Row {
		return policy.Row{Repo: sentinel.Repo, Tag: tag, Digest: "sha256:" + tag,
			MediaType: sentinel.ManifestMediaType, PushedAt: now.Add(-ago), Actor: "kpr-unlock"}
	}
	served := func(id, gen string) lineage.Served {
		return lineage.Served{Payload: sentinel.Payload{V: 1, Gen: gen, ID: id,
			TS: now.Add(-time.Minute).Format(time.RFC3339), Writer: "kpr-unlock"}}
	}
	paired := store.Identity{ID: "id-a", BaselineGen: "gen-new"}
	rows := []policy.Row{row("gen-old", 2*time.Hour), row("gen-new", time.Hour)}
	judge := func(s lineage.Served) lineage.Verdict {
		return lineage.Judge(s, lineage.Local{Ident: paired, Rows: rows}, lineage.Ask{DryRun: true, Now: now})
	}
	for _, c := range []struct {
		name  string
		s     lineage.Served
		ident store.Identity
		rows  []policy.Row
		v     lineage.Verdict
		want  string
	}{
		{name: "fresh store", s: served("id-a", "gen-new"), ident: store.Identity{}, rows: nil, v: lineage.Verdict{}, want: "unpaired"},
		{name: "silent registry", s: lineage.Served{Err: mistrust404{}}, ident: paired, rows: rows, v: lineage.Verdict{}, want: "unserved"},
		{name: "broken evidence", s: lineage.Served{Err: errMistrust}, ident: paired, rows: rows, v: lineage.Verdict{}, want: "unproven"},
		{name: "foreign lineage", s: served("id-b", "gen-new"), ident: paired, rows: rows, v: lineage.Verdict{}, want: "foreign"},
		{name: "identity-less payload", s: served("", "gen-new"), ident: paired, rows: rows, v: lineage.Verdict{}, want: "foreign"},
		{name: "served older than tracked", s: served("id-a", "gen-old"), ident: paired, rows: rows, v: judge(served("id-a", "gen-old")), want: "rollback?"},
		{name: "store behind registry", s: served("id-a", "gen-fresh"), ident: paired, rows: rows, v: judge(served("id-a", "gen-fresh")), want: "unadopted"},
		{name: "clean pairing shows paired", s: served("id-a", "gen-new"), ident: paired, rows: rows, v: judge(served("id-a", "gen-new")), want: "paired"},
	} {
		if got := name(c.s, c.ident, c.rows, c.v); got != c.want {
			t.Errorf("%s: word = %q, want %q", c.name, got, c.want)
		}
	}
}
