package clock

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// fixedSource reports one offset for every check: exact-boundary
// skew no network exchange can hold still.
type fixedSource struct{ off time.Duration }

func (s fixedSource) Offset(context.Context, string) (time.Duration, error) {
	return s.off, nil
}

// An offset exactly at tolerance still proceeds: the bound refuses
// past it, not at it. If this fails, boundary skew flips verdicts.
func TestCheckAcceptsExactTolerance(t *testing.T) {
	ctx := context.Background()
	for _, off := range []time.Duration{Tolerance, -Tolerance, 0} {
		if err := Check(ctx, fixedSource{off}, "time.example.com", Tolerance); err != nil {
			t.Errorf("offset %v refused: %v", off, err)
		}
	}
	// Past tolerance refuses on both signs: the absolute value is
	// taken before comparing, so a slow clock fails like a fast one.
	// If this fails, negative skew never refuses.
	for _, off := range []time.Duration{Tolerance + time.Second, -(Tolerance + time.Second)} {
		err := Check(ctx, fixedSource{off}, "time.example.com", Tolerance)
		var skew *SkewError
		if !errors.As(err, &skew) {
			t.Errorf("offset %v = %v, want *SkewError", off, err)
		}
	}
}

// Five seconds of skew still proceeds: the bound is thirty seconds,
// not one — NTP over the internet lands in milliseconds, so single
// seconds are noise, not breakage. The literal pins the tuning: any
// arithmetic on Tolerance refuses this. If this fails, ordinary drift
// blocks every mint path.
func TestCheckAcceptsSingleDigitSkew(t *testing.T) {
	ctx := context.Background()
	for _, off := range []time.Duration{5 * time.Second, -5 * time.Second} {
		if err := Check(ctx, fixedSource{off}, "time.example.com", Tolerance); err != nil {
			t.Errorf("offset %v refused: %v", off, err)
		}
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

// The skew message names both sides: the measured offset and the
// bound it crossed. If this fails, refusals stop saying how wrong
// the clock is.
func TestSkewErrorNamesOffsetAndTolerance(t *testing.T) {
	err := (&SkewError{Offset: 90 * time.Second, Tolerance: Tolerance}).Error()
	for _, want := range []string{"1m30s", Tolerance.String()} {
		if !strings.Contains(err, want) {
			t.Errorf("skew message %q lacks %q", err, want)
		}
	}
}

// addrOf appends the NTP port only when the server names none: an
// explicit port is never overridden. If this fails, custom-port
// time sources get :123 stapled on.
func TestAddrOfKeepsExplicitPort(t *testing.T) {
	if got := addrOf("time.example.com:1123"); got != "time.example.com:1123" {
		t.Errorf("addrOf kept = %q, want the explicit port untouched", got)
	}
	if got := addrOf("time.example.com"); got != "time.example.com:123" {
		t.Errorf("addrOf bare = %q, want :123 appended", got)
	}
}

// An undialable server fails at dial: no packet, no wait. If this
// fails, a misconfigured time source hangs to the timeout.
func TestNTPOffsetRefusesUndialable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Offset(ctx, "999.999.999.999:123"); err == nil {
		t.Error("undialable offset succeeded, want refusal")
	}
}

// A caller deadline earlier than the transport timeout wins: the
// exchange ends at the caller's bound, not the transport's. If this
// fails, tight callers wait out the full NTP timeout.
func TestNTPOffsetHonorsCallerDeadline(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Offset(ctx, pc.LocalAddr().String()); err == nil {
		t.Error("silent server accepted, want refusal")
	} else if took := time.Since(start); took > Timeout {
		t.Errorf("caller deadline took %v, want under the %v transport timeout", took, Timeout)
	}
}

// A server that hears but never answers fails on transport, not on
// cancellation: the read deadline (not the context) owns the error,
// so callers warn-and-proceed instead of aborting. If this fails, a
// blackhole time source aborts the run.
func TestNTPOffsetSilentServerIsTransportError(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Offset(ctx, pc.LocalAddr().String()); err == nil {
		t.Fatal("silent server accepted, want refusal")
	} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("silent server error = %v, want the transport timeout, not cancellation", err)
	}
}

// fakeNTPMismatch answers with a corrupted originate echo: a reply
// that is not for our request. The offset refuses instead of
// measuring someone else's exchange. If this fails, replayed NTP
// traffic measures.
func fakeNTPMismatch(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 48)
		for {
			_, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := make([]byte, 48)
			resp[0] = 4
			resp[1] = 1
			copy(resp[24:32], buf[40:48])
			resp[24] ^= 0xFF
			stamp(resp[40:48], time.Now())
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestOffsetRejectsForeignEcho(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Offset(ctx, fakeNTPMismatch(t)); err == nil {
		t.Error("foreign echo accepted, want refusal")
	} else if !strings.Contains(err.Error(), "originate echo mismatch") {
		t.Errorf("refusal = %q, want the echo named", err.Error())
	}
}

// Bare names mean https: the scheme is added, never guessed from
// content. The attempt below fails TLS (the hermetic server speaks
// plain http), which is the point — the failure proves the https
// scheme was chosen. If this fails, bare time sources dial plain.
func TestHTTPSOffsetBareMeansHTTPS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	bare := strings.TrimPrefix(srv.URL, "http://")
	if _, err := (HTTPS{}).Offset(ctx, bare); err == nil {
		t.Error("bare https offset succeeded against plain http, want the TLS refusal")
	}
}

// An unbuildable URL refuses before dialing. If this fails, a bad
// time source dials garbage.
func TestHTTPSOffsetBadURLRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (HTTPS{}).Offset(ctx, "https://exa\tmple.com"); err == nil {
		t.Error("bad-URL offset succeeded, want refusal")
	}
}

// datelessServer answers 200 with no Date: the transport works, the
// clock input does not. Unparseable dates refuse the same way. If
// this fails, dateless answers measure as zero skew.
type datelessRoundTripper struct{ date string }

func (f datelessRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	h := http.Header{}
	if f.date != "" {
		h.Set("Date", f.date)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: h}, nil
}

func TestHTTPSOffsetDatelessRefuses(t *testing.T) {
	ctx := context.Background()
	h := HTTPS{Client: &http.Client{Transport: datelessRoundTripper{}}}
	if _, err := h.Offset(ctx, "https://time.example.com"); err == nil {
		t.Error("dateless offset succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "no Date header") {
		t.Errorf("refusal = %q, want the header named", err.Error())
	}
	h = HTTPS{Client: &http.Client{Transport: datelessRoundTripper{date: "not a date"}}}
	if _, err := h.Offset(ctx, "https://time.example.com"); err == nil {
		t.Error("garbage-date offset succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "unparseable Date") {
		t.Errorf("refusal = %q, want the parse named", err.Error())
	}
}

// errSource fails the exchange: Check passes transport failures
// through plain (never typed skew), so callers warn-and-proceed.
// If this fails, a down time source reads as a wrong clock.
type errSource struct{ err error }

func (s errSource) Offset(context.Context, string) (time.Duration, error) {
	return 0, s.err
}

func TestCheckPassesTransportThrough(t *testing.T) {
	boom := errors.New("no route to time source")
	if err := Check(context.Background(), errSource{boom}, "time.example.com", Tolerance); !errors.Is(err, boom) {
		t.Errorf("transport failure = %v, want it passed through", err)
	}
}

// The method knob becomes exactly one transport: unknown values
// land on the harmless default, never a crash. If this fails,
// mints check time through a different transport than the one
// the operator configured.
func TestNewSourceMapsMethods(t *testing.T) {
	for _, tc := range []struct {
		method Method
		want   Source
	}{
		{MethodLocal, Local{}},
		{MethodHTTPS, HTTPS{}},
		{MethodNTP, NTP{}},
		{"sundial", Local{}},
	} {
		if got := NewSource(tc.method); got != tc.want {
			t.Errorf("method %q -> %T, want %T", tc.method, got, tc.want)
		}
	}
}
