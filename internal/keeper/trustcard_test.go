package keeper_test

import (
	"context"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/keeper"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// trust404 is the registry silence sentinel reads see: a 404
// from the tag address, never corruption, never transport.
type trust404 struct{}

func (trust404) Error() string   { return "status 404" }
func (trust404) StatusCode() int { return 404 }

// trustServed programs proofStub with one served generation:
// manifest naming the payload blob, blob carrying gen/id/ts.
func trustServed(gen, id string, err error) proofStub {
	if err != nil {
		return proofStub{err: err}
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	return proofStub{
		man:  []byte(`{"schemaVersion":2,"config":{"digest":"sha256:pay"}}`),
		blob: []byte(`{"v":1,"gen":"` + gen + `","ts":"` + ts + `","writer":"kpr-unlock","id":"` + id + `"}`),
	}
}

// The trust card voices the dry lineage verdict with the
// standing it earns: paired reads ok, anomalies degrade or
// deny, missing evidence never alarms. If this fails, the
// store view hides the verdict trust judges.
func TestTrustCardVoicesVerdict(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	row := func(tag string, ago time.Duration) policy.Row {
		return policy.Row{Repo: sentinel.Repo, Tag: tag, Digest: "sha256:" + tag,
			MediaType: sentinel.ManifestMediaType, PushedAt: now.Add(-ago), Actor: "kpr-unlock"}
	}
	paired := store.Identity{ID: "id-a", BaselineGen: "gen-new"}
	tracked := []policy.Row{row("gen-old", 2*time.Hour), row("gen-new", time.Hour)}
	for _, tc := range []struct {
		name  string
		api   sentinel.API
		ident store.Identity
		rows  []policy.Row
		word  string
		stand keeper.Standing
	}{
		{"fresh store", nil, store.Identity{}, nil, "unpaired", keeper.StandingUnproven},
		{"silent registry", trustServed("", "", trust404{}), paired, tracked, "unserved", keeper.StandingUnreachable},
		{"served newest tracked", trustServed("gen-new", "id-a", nil), paired, tracked, "paired", keeper.StandingOK},
		{"served untracked", trustServed("gen-fresh", "id-a", nil), paired, tracked, "behind", keeper.StandingDegraded},
		{"served older than tracked", trustServed("gen-old", "id-a", nil), paired, tracked, "rollback?", keeper.StandingDegraded},
		{"served foreign id", trustServed("gen-new", "id-b", nil), paired, tracked, "foreign", keeper.StandingDenied},
	} {
		got := keeper.FetchTrustCard(ctx, tc.api, tc.ident, tc.rows)
		if got.Word != tc.word || got.Standing != tc.stand {
			t.Errorf("%s = (%q, %q), want (%q, %q)", tc.name, got.Word, got.Standing, tc.word, tc.stand)
		}
	}
}
