package keeper_test

import (
	"context"
	"errors"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/keeper"
)

// proofStub is a sentinel.API with a programmed generation: the
// proof card's three postures without a registry.
type proofStub struct {
	man, blob []byte
	err       error
}

func (s proofStub) GetManifest(context.Context, string, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.man, nil
}

func (s proofStub) GetBlob(context.Context, string, string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.blob, nil
}

// The proof card voices what the registry serves: a served
// generation reads ok with its id, a silent registry unproven,
// no API closed. If this fails, the card guesses where it
// should voice.
func TestProofCardVoicesServedGeneration(t *testing.T) {
	ctx := context.Background()
	if got := keeper.FetchProofCard(ctx, nil); got.Standing != keeper.StandingClosed || got.Proven() {
		t.Errorf("nil api = (%q, proven %v), want (closed, false)", got.Standing, got.Proven())
	}
	if got := keeper.FetchProofCard(ctx, proofStub{err: errors.New("registry: 500")}); got.Standing != keeper.StandingUnproven || got.Proven() || got.Gen != "" {
		t.Errorf("error api = (%q, %q), want (unproven, empty)", got.Standing, got.Gen)
	}
	live := proofStub{
		man:  []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc"}}`),
		blob: []byte(`{"v":1,"gen":"019-test-gen","ts":"2026-09-30T00:00:00Z","writer":"test"}`),
	}
	if got := keeper.FetchProofCard(ctx, live); got.Standing != keeper.StandingOK || !got.Proven() || got.Gen != "019-test-gen" {
		t.Errorf("live api = (%q, %q), want (ok, 019-test-gen)", got.Standing, got.Gen)
	}
}
