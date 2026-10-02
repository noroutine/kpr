package proof

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// stubInitiator is a registry peer with a script: a status or a
// dead transport, no network. If proving the mode needs the
// network, the seam is wrong.
type stubInitiator struct {
	status int
	err    error
}

func (s stubInitiator) Initiate(context.Context, string) (int, error) {
	return s.status, s.err
}

// 202 mints writable and nothing else: exactly one inhabitant.
// If this fails, writable registries stopped proving writable.
func TestProveModeMintsWritable(t *testing.T) {
	ro, wo, err := ProveMode(context.Background(), stubInitiator{status: http.StatusAccepted}, "x")
	if err != nil {
		t.Fatalf("202 = %v, want mint", err)
	}
	if wo == nil {
		t.Error("202 minted nil writable, want RegistryWritable")
	}
	if ro != nil {
		t.Errorf("202 minted readonly %v, want nothing", ro)
	}
}

// 405 mints readonly and nothing else. If this fails, readonly
// registries stopped proving readonly.
func TestProveModeMintsReadonly(t *testing.T) {
	ro, wo, err := ProveMode(context.Background(), stubInitiator{status: http.StatusMethodNotAllowed}, "x")
	if err != nil {
		t.Fatalf("405 = %v, want mint", err)
	}
	if ro == nil {
		t.Error("405 minted nil readonly, want RegistryReadonly")
	}
	if wo != nil {
		t.Errorf("405 minted writable %v, want nothing", wo)
	}
}

// Anything else mints nothing: inconclusive fails closed, never
// collects blind. If this fails, unclassifiable peers started
// minting.
func TestProveModeRefusesInconclusive(t *testing.T) {
	ro, wo, err := ProveMode(context.Background(), stubInitiator{status: http.StatusTeapot}, "x")
	if err == nil {
		t.Error("418 minted no error, want inconclusive")
	}
	if ro != nil || wo != nil {
		t.Errorf("418 minted (%v, %v), want nothing", ro, wo)
	}
}

// A dead peer passes through: unreachable is an error, not a
// mode. If this fails, outages started minting.
func TestProveModePassesTransportFailure(t *testing.T) {
	boom := errors.New("boom")
	ro, wo, err := ProveMode(context.Background(), stubInitiator{err: boom}, "x")
	if err == nil || !errors.Is(err, boom) {
		t.Errorf("dead peer error = %v, want the transport error", err)
	}
	if ro != nil || wo != nil {
		t.Errorf("dead peer minted (%v, %v), want nothing", ro, wo)
	}
}

// The zero values are nothing: without the exchange there is no
// classification. If this fails, modes can be minted from thin
// air.
func TestModeZeroIsNothing(t *testing.T) {
	var ro RegistryReadonly
	var wo RegistryWritable
	if ro != nil {
		t.Errorf("zero RegistryReadonly = %v, want nil", ro)
	}
	if wo != nil {
		t.Errorf("zero RegistryWritable = %v, want nil", wo)
	}
}
