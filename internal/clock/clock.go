// Package clock is trustworthy wall time behind one seam: reads off
// a configured server compared against the local clock, so mints
// carry checked timestamps instead of blind ones. The offset is
// exposed (not just the verdict) so future JWT iat/exp/nbf checks
// can reason with it instead of re-deriving it.
//
// Two transports behind the Source port: NTP (stdlib UDP, precise,
// often egress-blocked) and HTTPS (a plain Date read, 1s resolution
// — plenty against the tolerance — that passes wherever the web
// does). Neither is authenticated: a spoofed reply only denies (a
// false skew refuses the mint) — it can neither forge a proof
// (fs+API) nor mint anything. The NTP originate-echo check still
// drops blind off-path replies.
//
// Default is the local clock (no check); deployments select a
// source per environment (method and server) — DFN's public time
// service, or their own air-gapped one, instead of failing checks.
package clock

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Method names the transport a check uses.
type Method string

const (
	// MethodHTTPS reads the Date header off a plain GET.
	MethodHTTPS Method = "https"
	// MethodNTP speaks SNTP over UDP.
	MethodNTP Method = "ntp"
	// MethodLocal trusts the machine clock: no external check, zero
	// offset, always proceeds. For hosts with managed time (and for
	// runs where any network check is noise); deployments that want
	// proof select https or ntp.
	MethodLocal Method = "local"
	// DefaultMethod is local: checking nothing surprises nobody,
	// and compose pins https where a source is wanted.
	DefaultMethod = MethodLocal
)

// Source is one clock transport: serverTime - localTime at receipt.
type Source interface {
	Offset(ctx context.Context, server string) (time.Duration, error)
}

const (
	// DFNServer is the default time source host, either transport.
	DFNServer = "zeitstempel.dfn.de"
	// Tolerance bounds acceptable skew: NTP over the internet lands
	// in milliseconds; a clock tens of seconds off is broken, not
	// drifting. Larger skew refuses mint paths unless overridden.
	// Pinned by the dashboard's "tolerance 30s" line, not by staged
	// skew: no test stages skew inside the second, and none should.
	Tolerance = 30 * time.Second
	// Timeout bounds one exchange: a silent server must not stall a
	// refusal path.
	// NOTE(mutants): arithmetic here only moves the deadline — the
	// silent-server test bounds the wait from above, so a shorter
	// timeout still passes and a longer one only wastes test time.
	Timeout = 3 * time.Second
	// ntpPort is appended when the server names no port.
	ntpPort = "123"
	// ntpEpoch is seconds between 1900-01-01 and 1970-01-01.
	ntpEpoch uint64 = 2208988800
)

// SkewError means the exchange succeeded and the clock is wrong by
// Offset beyond Tolerance. Callers distinguish it (refuse unless
// overridden) from transport failures (warn and proceed — integrity
// loss stays explicit in the output).
type SkewError struct {
	Offset    time.Duration
	Tolerance time.Duration
}

func (e *SkewError) Error() string {
	return fmt.Sprintf("clock skew %v exceeds %v: fix NTP or override", e.Offset.Round(time.Millisecond), e.Tolerance)
}

// stamp writes t as a 64-bit NTP transmit timestamp.
func stamp(b []byte, t time.Time) {
	ns := t.UnixNano()
	sec := uint64(ns/1e9) + ntpEpoch
	frac := uint64(ns%1e9) * (1 << 32) / 1e9
	binary.BigEndian.PutUint32(b[0:4], uint32(sec))
	binary.BigEndian.PutUint32(b[4:8], uint32(frac))
}

// unstamp reads a 64-bit NTP transmit timestamp.
func unstamp(b []byte) time.Time {
	sec := uint64(binary.BigEndian.Uint32(b[0:4])) - ntpEpoch
	frac := uint64(binary.BigEndian.Uint32(b[4:8]))
	return time.Unix(int64(sec), int64(frac*1e9/(1<<32))).UTC()
}

// addrOf appends the NTP port when the server names none.
func addrOf(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(strings.TrimSuffix(server, ":"), ntpPort)
}

// NTP speaks SNTP over UDP.
type NTP struct{}

// Offset queries server and returns serverTime - localTime at
// receipt, half-RTT compensated (symmetric path assumed — stated,
// not proven). Positive means the local clock trails.
func (NTP) Offset(ctx context.Context, server string) (time.Duration, error) {
	d := net.Dialer{Timeout: Timeout}
	conn, err := d.DialContext(ctx, "udp", addrOf(server))
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	// Cancellation unblocks the read below: without this only a
	// ctx deadline (not a bare cancel) ends the wait. No goroutine
	// for contexts that can't cancel (Done() == nil).
	if done := ctx.Done(); done != nil {
		go func() {
			<-done
			_ = conn.Close()
		}()
	}
	deadline := time.Now().Add(Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}
	req := make([]byte, 48)
	req[0] = 0x1B // LI=0, VN=3, mode=3 (client)
	t1 := time.Now()
	stamp(req[40:48], t1)
	if _, err := conn.Write(req); err != nil {
		return 0, err
	}
	resp := make([]byte, 48)
	n, err := conn.Read(resp)
	if err != nil {
		// Our own cancel-Close surfaces here: report the
		// cancellation, not a transport failure, so callers abort
		// instead of warning-and-proceeding past a dead check.
		if cerr := ctx.Err(); cerr != nil {
			return 0, cerr
		}
		return 0, err
	}
	t4 := time.Now()
	if n < 48 {
		return 0, fmt.Errorf("clock: short NTP reply (%d bytes) from %s", n, server)
	}
	if resp[0]&0x07 != 4 {
		return 0, fmt.Errorf("clock: NTP reply mode %d from %s, want server (4)", resp[0]&0x07, server)
	}
	if resp[1] == 0 || resp[1] >= 16 {
		return 0, fmt.Errorf("clock: NTP stratum %d from %s, server unsynchronized", resp[1], server)
	}
	for i := range 8 {
		if resp[24+i] != req[40+i] {
			return 0, fmt.Errorf("clock: NTP originate echo mismatch from %s", server)
		}
	}
	serverTime := unstamp(resp[40:48]).Add(t4.Sub(t1) / 2)
	return serverTime.Sub(t4), nil
}

// Offset keeps the old entry point on the NTP transport.
func Offset(ctx context.Context, server string) (time.Duration, error) {
	return NTP{}.Offset(ctx, server)
}

// Local trusts the machine clock: the zero source. It exists so
// "no check" is an explicit, visible method rather than a missing
// one — Check over it always proceeds.
type Local struct{}

// Offset returns zero: the local clock, unchecked.
func (Local) Offset(context.Context, string) (time.Duration, error) {
	return 0, nil
}

// HTTPS reads the Date header off a plain request: any status
// carries one, so even a 405 from a bare domain works. 1s header
// resolution dominates the error against the tolerance below.
type HTTPS struct {
	// Client overrides the default (3s timeout); nil is fine.
	Client *http.Client
}

// Offset returns serverTime - localTime per the served Date header.
// Positive means the local clock trails.
func (h HTTPS) Offset(ctx context.Context, server string) (time.Duration, error) {
	url := server
	if !strings.Contains(url, "://") {
		url = "https://" + strings.TrimSuffix(url, "/")
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: Timeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, err
	}
	t1 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	date := resp.Header.Get("Date")
	if date == "" {
		return 0, fmt.Errorf("clock: no Date header from %s", server)
	}
	at, err := time.Parse(http.TimeFormat, date)
	if err != nil {
		return 0, fmt.Errorf("clock: unparseable Date %q from %s", date, server)
	}
	// Half-RTT compensated like the NTP transport (same symmetric
	// assumption, stated not proven): the Date is stamped at send,
	// so slow links bill half their latency, never the full trip.
	t4 := time.Now()
	return at.Sub(t1.Add(t4.Sub(t1) / 2)), nil
}

// Check refuses skew beyond tolerance and nils otherwise. Transport
// failures return plain errors (no *SkewError) so callers can tell
// "clock wrong" (refuse unless overridden) from "clock unchecked"
// (warn and proceed).
func Check(ctx context.Context, src Source, server string, tolerance time.Duration) error {
	off, err := src.Offset(ctx, server)
	if err != nil {
		return err
	}
	// NOTE(mutants): <= is equivalent — negating a zero offset is
	// identity, and every other input takes the same branch either way.
	if off < 0 {
		off = -off
	}
	if off > tolerance {
		return &SkewError{Offset: off, Tolerance: tolerance}
	}
	return nil
}
