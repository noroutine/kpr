package clock

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// dateServer answers every request with a fixed Date header: hermetic
// HTTPS time. If HTTPS has no source behind it, these don't compile.
func dateServer(t *testing.T, at time.Time) string {
	return dateServerHold(t, at, 0)
}

// dateServerHold stamps the Date at receipt, then holds the answer:
// return-path asymmetry the compensation must halve, not bill.
func dateServerHold(t *testing.T, at time.Time, hold time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stamped := at
		if hold > 0 {
			stamped = time.Now()
			time.Sleep(hold)
		}
		w.Header().Set("Date", stamped.UTC().Format(http.TimeFormat))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A held answer bills half its hold, never the full trip: the
// compensation divides the round trip like the NTP transport's.
// The hold is seconds because the Date header quantizes to whole
// seconds — anything smaller drowns in truncation. If this fails,
// slow links read as clock skew.
func TestHTTPSOffsetCompensatesRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	url := dateServerHold(t, time.Now(), 3*time.Second)
	src := HTTPS{Client: &http.Client{Timeout: 10 * time.Second}}
	off, err := src.Offset(ctx, url)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off < -2700*time.Millisecond || off > -1300*time.Millisecond {
		t.Errorf("offset = %v, want ~-1.5s", off)
	}
}

// A cancelled NTP read aborts instead of riding out the timeout:
// callers must not warn-and-proceed past a check nobody wants.
func TestNTPOffsetAbortsOnCancel(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 48)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = Offset(ctx, pc.LocalAddr().String())
	if err == nil {
		t.Fatal("cancelled offset succeeded, want context.Canceled")
	} else if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > Timeout {
		t.Errorf("cancel took %v, want faster than the %v timeout", took, Timeout)
	}
}

func TestHTTPSOffsetMeasuresSkew(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := dateServer(t, time.Now().Add(90*time.Second))
	off, err := HTTPS{}.Offset(ctx, url)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off < 85*time.Second || off > 95*time.Second {
		t.Errorf("offset = %v, want ~+90s", off)
	}
}

func TestHTTPSOffsetHealthy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := dateServer(t, time.Now())
	off, err := HTTPS{}.Offset(ctx, url)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off < -3*time.Second || off > 3*time.Second {
		t.Errorf("offset = %v, want ~0", off)
	}
}

func TestHTTPSOffsetUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (HTTPS{}).Offset(ctx, "https://127.0.0.1:1"); err == nil {
		t.Error("closed-port offset succeeded, want an error")
	}
}

// Check composes over any source: a 90s HTTPS skew refuses typed.
func TestCheckEnforcesToleranceOverHTTPS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := dateServer(t, time.Now().Add(90*time.Second))
	err := Check(ctx, HTTPS{}, url, Tolerance)
	if err == nil {
		t.Fatal("90s HTTPS skew accepted, want refusal")
	}
	var skew *SkewError
	if !errors.As(err, &skew) {
		t.Fatalf("error = %T, want *SkewError", err)
	}
}

// fakeNTP serves one crafted response per request: transmit fixed at
// start+shift, stratum and mode adjustable, short garbage on demand.
// Tests measure Offset against it — hermetic, no network.
func fakeNTP(t *testing.T, shift time.Duration, stratum byte, mode byte, short bool) string {
	return fakeNTPDelay(t, shift, stratum, mode, short, 0)
}

// fakeNTPDelay answers like fakeNTP after holding the response for
// delay: a nonzero delay proves the offset math compensates the
// round trip instead of billing it to the clock.
func fakeNTPDelay(t *testing.T, shift time.Duration, stratum byte, mode byte, short bool, delay time.Duration) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 48)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if short {
				_, _ = pc.WriteTo(buf[:10], addr)
				continue
			}
			_ = n
			resp := make([]byte, 48)
			resp[0] = mode & 0x07
			resp[1] = stratum
			copy(resp[24:32], buf[40:48])
			stamp(resp[40:48], time.Now().Add(shift))
			// Hold the stamped answer: return-path asymmetry the
			// symmetric assumption can only halve, never bill
			// in full.
			if delay > 0 {
				time.Sleep(delay)
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestOffsetMeasuresServerShift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := fakeNTP(t, 1500*time.Millisecond, 1, 4, false)
	off, err := Offset(ctx, addr)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off < 1200*time.Millisecond || off > 1800*time.Millisecond {
		t.Errorf("offset = %v, want ~+1500ms", off)
	}
}

// A slow return path biases the symmetric assumption by half the
// delay — but the compensation divides the round trip, never
// multiplies it. If this fails, Offset bills network time to the
// clock.
func TestOffsetCompensatesRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := fakeNTPDelay(t, 1500*time.Millisecond, 1, 4, false, 300*time.Millisecond)
	off, err := Offset(ctx, addr)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off < 1500*time.Millisecond-400*time.Millisecond || off > 1500*time.Millisecond+100*time.Millisecond {
		t.Errorf("offset = %v, want ~+1500ms minus half the 300ms hold", off)
	}
}

// The stamp/unstamp pair round-trips within a millisecond: the
// request's transmit time is real evidence, not filler. If this
// fails, the client interrogates servers with a broken clock.
func TestStampRoundTrips(t *testing.T) {
	now := time.Now()
	b := make([]byte, 8)
	stamp(b, now)
	if got := unstamp(b); absDuration(got.Sub(now)) > time.Millisecond {
		t.Errorf("round trip drifted %v", got.Sub(now))
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// Local trusts blindly: zero offset, no error, any server string
// accepted and ignored. If this fails, the default transport
// phones home.
func TestLocalOffsetIsZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	off, err := (Local{}).Offset(ctx, "anything.invalid")
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off != 0 {
		t.Errorf("offset = %v, want zero", off)
	}
	if err := Check(ctx, Local{}, "anything.invalid", Tolerance); err != nil {
		t.Errorf("local check refused: %v", err)
	}
}

func TestOffsetNegativeShift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := fakeNTP(t, -2*time.Second, 2, 4, false)
	off, err := Offset(ctx, addr)
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off > -1500*time.Millisecond || off < -2500*time.Millisecond {
		t.Errorf("offset = %v, want ~-2000ms", off)
	}
}

func TestOffsetRejectsUnsynchronized(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, stratum := range []byte{0, 16} {
		addr := fakeNTP(t, 0, stratum, 4, false)
		if _, err := Offset(ctx, addr); err == nil {
			t.Errorf("stratum %d accepted, want refusal", stratum)
		}
	}
}

func TestOffsetRejectsShortPacket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := fakeNTP(t, 0, 1, 4, true)
	if _, err := Offset(ctx, addr); err == nil {
		t.Error("short packet accepted, want refusal")
	}
}

func TestOffsetRejectsNonServerMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := fakeNTP(t, 0, 1, 3, false)
	if _, err := Offset(ctx, addr); err == nil {
		t.Error("client-mode reply accepted, want refusal")
	}
}

func TestCheckEnforcesTolerance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	okAddr := fakeNTP(t, 100*time.Millisecond, 1, 4, false)
	if err := Check(ctx, NTP{}, okAddr, Tolerance); err != nil {
		t.Errorf("100ms skew refused: %v", err)
	}
	badAddr := fakeNTP(t, 90*time.Second, 1, 4, false)
	err := Check(ctx, NTP{}, badAddr, Tolerance)
	if err == nil {
		t.Fatal("90s skew accepted, want refusal")
	}
	var skew *SkewError
	if !errors.As(err, &skew) {
		t.Fatalf("error = %T, want *SkewError", err)
	}
}
