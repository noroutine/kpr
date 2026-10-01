package proof

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/clock"
)

// stubSource is a clock with a script: fixed offset or a dead
// transport, no network. If the Checker needs the network, the
// seam is wrong.
type stubSource struct {
	off time.Duration
	err error
}

func (s stubSource) Offset(context.Context, string) (time.Duration, error) {
	return s.off, s.err
}

// A bounded clock mints: the exchange ran, skew inside
// tolerance, timestamps mean something. If this fails, clean
// checks stopped proving.
func TestCheckMintsWhenBounded(t *testing.T) {
	c, err := Checker{Tolerance: time.Minute}.Check(context.Background(), stubSource{off: time.Second}, "x")
	if err != nil {
		t.Fatalf("bounded check = %v, want mint", err)
	}
	if c == nil {
		t.Fatal("bounded check minted nil, want BoundedClock")
	}
}

// Skew refuses with the clock's own error, and mints nothing:
// callers keep their warn-vs-refuse contract. If this fails,
// the Checker rewrote evidence it should have passed through.
func TestCheckRefusesSkewUntouched(t *testing.T) {
	c, err := Checker{Tolerance: time.Minute}.Check(context.Background(), stubSource{off: time.Hour}, "x")
	if c != nil {
		t.Errorf("skewed check minted %v, want nothing", c)
	}
	var skew *clock.SkewError
	if !errors.As(err, &skew) {
		t.Errorf("skewed check error = %v, want *clock.SkewError", err)
	}
}

// A dead source passes through too: unchecked is a warning, not
// a proof. If this fails, outages started minting.
func TestCheckPassesTransportFailure(t *testing.T) {
	boom := errors.New("boom")
	c, err := Checker{Tolerance: time.Minute}.Check(context.Background(), stubSource{err: boom}, "x")
	if c != nil {
		t.Errorf("failed check minted %v, want nothing", c)
	}
	if !errors.Is(err, boom) {
		t.Errorf("failed check error = %v, want the source error", err)
	}
}

// The zero value is nothing: without a clean check there is no
// bound. If this fails, time can be minted from thin air.
func TestBoundedClockZeroIsNothing(t *testing.T) {
	var c BoundedClock
	if c != nil {
		t.Errorf("zero BoundedClock = %v, want nil", c)
	}
}

// The local clock is an explicit method, not a missing one: no
// exchange, zero offset, always proceeds. If this fails, "no
// check" stopped being visible.
func ExampleChecker_Check() {
	c, err := Checker{Tolerance: clock.Tolerance}.Check(context.Background(), clock.Local{}, "unused")
	fmt.Println(c != nil, err)
	// Output: true <nil>
}
