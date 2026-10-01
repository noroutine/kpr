// Package clock is trustworthy wall time behind one seam: SNTP reads
// off a configured server compared against the local clock, so mints
// carry checked timestamps instead of blind ones. The offset is
// exposed (not just the verdict) so future JWT iat/exp/nbf checks
// can reason with it instead of re-deriving it.
//
// No new dependencies — the client is stdlib UDP. No authentication
// either: NTP is spoofable, and a spoofed reply only denies (a false
// skew refuses the mint) — it can neither forge a proof (fs+API) nor
// mint anything. The originate-echo check still drops blind
// off-path replies.
//
// Default source is DFN's public time service, overridable per
// environment (KPR_NTP_SERVER) — air-gapped sites point at their own
// instead of failing every check.
package clock

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	// DFNServer is the default NTP source.
	DFNServer = "zeitstempel.dfn.de"
	// Tolerance bounds acceptable skew: NTP over the internet lands
	// in milliseconds; a clock tens of seconds off is broken, not
	// drifting. Larger skew refuses mint paths unless overridden.
	Tolerance = 30 * time.Second
	// Timeout bounds one exchange: a silent server must not stall a
	// refusal path.
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

// Offset queries server and returns serverTime - localTime at
// receipt, half-RTT compensated (symmetric path assumed — stated,
// not proven). Positive means the local clock trails.
func Offset(ctx context.Context, server string) (time.Duration, error) {
	d := net.Dialer{Timeout: Timeout}
	conn, err := d.DialContext(ctx, "udp", addrOf(server))
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
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

// Check refuses skew beyond tolerance and nils otherwise. Transport
// failures return plain errors (no *SkewError) so callers can tell
// "clock wrong" (refuse unless overridden) from "clock unchecked"
// (warn and proceed).
func Check(ctx context.Context, server string, tolerance time.Duration) error {
	off, err := Offset(ctx, server)
	if err != nil {
		return err
	}
	if off < 0 {
		off = -off
	}
	if off > tolerance {
		return &SkewError{Offset: off, Tolerance: tolerance}
	}
	return nil
}
