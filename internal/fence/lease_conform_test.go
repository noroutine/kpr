package fence

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/store"
	"nrtn.dev/catalyst/kpr/internal/testing/fakes"
)

// The lease conformance: every medium behind the port behaves
// one way — engage, read, evaluate, release, overrun, corrupt.
// A backend lands when it passes this table; file semantics are
// the frozen reference, never the other way around. If this
// fails, the media diverged and one of them lies.

// leaseMedium opens a fresh lease plus a corrupt injector that
// writes raw bytes where its Read looks: the only
// medium-specific code in the table.
type leaseMedium struct {
	name    string
	open    func(t *testing.T) Lease
	corrupt func(t *testing.T, l Lease, raw []byte)
}

func leaseMedia(t *testing.T) []leaseMedium {
	t.Helper()
	return []leaseMedium{
		{
			name: "file",
			open: func(t *testing.T) Lease {
				return store.FileLease{Dir: t.TempDir()}
			},
			corrupt: func(t *testing.T, l Lease, raw []byte) {
				dir := l.(store.FileLease).Dir
				if err := os.WriteFile(filepath.Join(dir, store.HoldFileName), raw, 0o644); err != nil {
					t.Fatalf("stage corrupt lease: %v", err)
				}
			},
		},
		{
			name: "redis",
			open: func(t *testing.T) Lease {
				return store.RedisLease{Conn: fakes.NewMemLeaseConn(), Key: store.HoldLeaseKey}
			},
			corrupt: func(t *testing.T, l Lease, raw []byte) {
				conn := l.(store.RedisLease).Conn
				if err := conn.Set(context.Background(), store.HoldLeaseKey, raw, 0); err != nil {
					t.Fatalf("stage corrupt lease: %v", err)
				}
			},
		},
	}
}

func TestLeaseHoldReadsLive(t *testing.T) {
	for _, m := range leaseMedia(t) {
		t.Run(m.name, func(t *testing.T) {
			l := m.open(t)
			now := time.Now().UTC()
			release, err := l.Hold(context.Background(), now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Hold: %v", err)
			}
			defer release()
			if until, present := l.Read(); !present || !until.After(now) {
				t.Errorf("Read = (%v,%v), want the live term", until, present)
			}
			if _, held := l.HeldUntil(now); !held {
				t.Error("HeldUntil(now) = false, want held")
			}
		})
	}
}

func TestLeaseReleaseReadsAbsent(t *testing.T) {
	for _, m := range leaseMedia(t) {
		t.Run(m.name, func(t *testing.T) {
			l := m.open(t)
			now := time.Now().UTC()
			release, err := l.Hold(context.Background(), now.Add(time.Minute))
			if err != nil {
				t.Fatalf("Hold: %v", err)
			}
			release()
			if until, present := l.Read(); present {
				t.Errorf("Read after release = (%v,true), want absent", until)
			}
			if _, held := l.HeldUntil(now); held {
				t.Error("HeldUntil after release = true, want free")
			}
		})
	}
}

func TestLeaseOverwriteReplaces(t *testing.T) {
	for _, m := range leaseMedia(t) {
		t.Run(m.name, func(t *testing.T) {
			l := m.open(t)
			now := time.Now().UTC()
			first, err := l.Hold(context.Background(), now.Add(time.Minute))
			if err != nil {
				t.Fatalf("first Hold: %v", err)
			}
			defer first()
			later := now.Add(2 * time.Minute)
			second, err := l.Hold(context.Background(), later)
			if err != nil {
				t.Fatalf("second Hold: %v", err)
			}
			defer second()
			if until, _ := l.Read(); !until.Equal(later) {
				t.Errorf("Read = %v, want the second term %v", until, later)
			}
		})
	}
}

func TestLeaseOverrunStaysPresent(t *testing.T) {
	for _, m := range leaseMedia(t) {
		t.Run(m.name, func(t *testing.T) {
			l := m.open(t)
			now := time.Now().UTC()
			release, err := l.Hold(context.Background(), now.Add(-time.Minute))
			if err != nil {
				t.Fatalf("Hold past term: %v", err)
			}
			defer release()
			if _, present := l.Read(); !present {
				t.Error("Read of overrun lease = absent, want present-but-past")
			}
			if _, held := l.HeldUntil(now); held {
				t.Error("HeldUntil of overrun lease = true, want free")
			}
		})
	}
}

func TestLeaseCorruptReadsAbsent(t *testing.T) {
	for _, m := range leaseMedia(t) {
		t.Run(m.name, func(t *testing.T) {
			l := m.open(t)
			m.corrupt(t, l, []byte("{nope"))
			if until, present := l.Read(); present {
				t.Errorf("Read of corrupt lease = (%v,true), want absent", until)
			}
			if _, held := l.HeldUntil(time.Now().UTC()); held {
				t.Error("HeldUntil of corrupt lease = true, want free")
			}
		})
	}
}

func TestLeaseFreshReadsAbsent(t *testing.T) {
	for _, m := range leaseMedia(t) {
		t.Run(m.name, func(t *testing.T) {
			l := m.open(t)
			if until, present := l.Read(); present {
				t.Errorf("fresh Read = (%v,true), want absent", until)
			}
		})
	}
}

// The marker-only stand-in refuses Hold loudly instead of
// panicking on a nil lease: no lease, no engagement, no silence.
// If this fails, unfenced collects believe they hold.
func TestNilLeaseRefusesHold(t *testing.T) {
	g := &Gate{}
	if _, err := g.lease().Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Error("Hold through a nil lease succeeded, want refusal")
	}
	if _, present := g.lease().Read(); present {
		t.Error("Read through a nil lease = present, want absent")
	}
	if _, held := g.lease().HeldUntil(time.Now()); held {
		t.Error("HeldUntil through a nil lease = true, want free")
	}
}
