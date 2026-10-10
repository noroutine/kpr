package store

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assertOnly fails unless dir holds exactly the named entries.
func assertOnly(t *testing.T, dir string, what string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != len(want) {
		t.Errorf("dir holds %v %s, want %v", names, what, want)
		return
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("dir holds %v %s, want %v", names, what, want)
			return
		}
	}
}

// The lease lands as exactly one file: no temp residue a crashed
// writer could leave behind, content intact, 0600, gone on
// release. Readers must never see half a lease. If this fails,
// the write path litters the dir or the release leaks.
func TestHoldLandsSingleFile(t *testing.T) {
	dir := t.TempDir()
	h := FileLease{Dir: dir}
	until := time.Now().Add(time.Minute)
	release, err := h.Hold(context.Background(), until)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	assertOnly(t, dir, "after Hold", HoldFileName)
	entry, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if fi, err := entry[0].Info(); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("lease perm = %o, want 600", fi.Mode().Perm())
	}
	if got, present := h.Read(); !present || !got.Equal(until) {
		t.Errorf("Read = (%v, %v), want (%v, true)", got, present, until)
	}
	release()
	assertOnly(t, dir, "after release")
}

// Write-fail leg: see TestPutFileRefusesDiskFullWrite — the code is
// shared, so its rlimit refusal covers both callers, no seam and
// no second staging here.

// A rename that fails refuses the take: a directory pre-created at
// the lease path makes the swap fail deterministically, and the
// staged temp must be removed. If this fails, failed swaps litter
// temps beside the lease.
func TestHoldRefusesFailedRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, HoldFileName), 0o755); err != nil {
		t.Fatalf("stage blocking dir: %v", err)
	}
	if _, err := (FileLease{Dir: dir}).Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Error("Hold over a blocked rename succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "write hold lease") {
		t.Errorf("refusal = %q, want the write named", err.Error())
	}
	assertOnly(t, dir, "after refused rename", HoldFileName)
}

// Crash residue never piles up: a stale temp predating the Hold is
// swept as the new lease lands. If this fails, every crashed
// collect leaves a temp file in the fence dir forever.
func TestHoldSweepsStaleTemps(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".edge-fence-123.tmp")
	if err := os.WriteFile(stale, []byte("half a lease"), 0o600); err != nil {
		t.Fatalf("stage stale temp: %v", err)
	}
	release, err := (FileLease{Dir: dir}).Hold(context.Background(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer release()
	assertOnly(t, dir, "after Hold", HoldFileName)
}

// An empty dir skips the sweep: marker-only gates hold no path,
// so Hold must never touch the working directory. If this fails,
// a dir-less Hold sweeps (and writes its lease into) the cwd.
func TestHoldEmptyDirSkipsSweep(t *testing.T) {
	t.Chdir(t.TempDir())
	stale := ".edge-fence-123.tmp"
	if err := os.WriteFile(stale, []byte("half a lease"), 0o600); err != nil {
		t.Fatalf("stage stale temp: %v", err)
	}
	release, err := (FileLease{}).Hold(context.Background(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer release()
	assertOnly(t, ".", "after dir-less Hold", stale, HoldFileName)
}

// A lease that cannot land refuses the take: gc must hear the
// failure instead of collecting unfenced. If this fails, a gc
// collects while believing pushes are held.
func TestHoldRefusesBadDir(t *testing.T) {
	h := FileLease{Dir: filepath.Join(t.TempDir(), "no-such-dir")}
	if _, err := h.Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Error("Hold into a missing dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "write hold lease") {
		t.Errorf("refusal = %q, want the write named", err.Error())
	}
}

// A corrupt lease file reads absent: fail open, leases are not
// evidence. If this fails, garbage in the fence dir holds or
// denies traffic.
func TestHoldCorruptReadsAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, HoldFileName), []byte("{nope"), 0o600); err != nil {
		t.Fatalf("stage corrupt lease: %v", err)
	}
	if until, present := (FileLease{Dir: dir}).Read(); present {
		t.Errorf("corrupt Read = (%v,true), want absent", until)
	}
}

// failLeaseConn is a redis that answers nothing: every command
// fails, standing in for a blip mid-collect.
type failLeaseConn struct{}

func (failLeaseConn) Get(context.Context, string) ([]byte, error) {
	return nil, errLeaseDown
}

func (failLeaseConn) Set(context.Context, string, []byte, time.Duration) error {
	return errLeaseDown
}

func (failLeaseConn) Del(context.Context, string) error { return errLeaseDown }

var errLeaseDown = errors.New("redis: connection refused")

// A backend blip reads absent but loud: the edge fails open
// (leases are not evidence) yet the outage must surface in the
// log, never pass silent. If this fails, a redis timeout mid-
// collect opens the fence with no event.
func TestRedisBlipReadsAbsentAndLoud(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	l := RedisLease{Conn: failLeaseConn{}, Key: HoldLeaseKey}
	if until, present := l.Read(); present {
		t.Errorf("blip Read = (%v,true), want absent", until)
	}
	if !strings.Contains(buf.String(), HoldLeaseKey) {
		t.Errorf("log = %q, want the key named", buf.String())
	}
	if _, err := l.Hold(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Error("Hold over a down backend succeeded, want refusal")
	}
}

// Hold carries a hygiene TTL past the term: the key outlives
// `{until}` by the margin so overrun reads survive, then redis
// reaps it. If this fails, dead keys linger past every collect.
func TestRedisHoldCarriesHygieneTTL(t *testing.T) {
	conn := &recLeaseConn{rows: map[string][]byte{}, ttls: map[string]time.Duration{}}
	l := RedisLease{Conn: conn, Key: HoldLeaseKey}
	until := time.Now().Add(5 * time.Minute)
	release, err := l.Hold(context.Background(), until)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer release()
	ttl, ok := conn.ttls[HoldLeaseKey]
	if !ok {
		t.Fatalf("no TTL asked for %q", HoldLeaseKey)
	}
	if ttl < 5*time.Minute || ttl > 10*time.Minute+time.Second {
		t.Errorf("TTL = %v, want term plus margin", ttl)
	}
}

// recLeaseConn records the horizons Hold asks for: the TTL the
// map fake deliberately does not honor.
type recLeaseConn struct {
	rows map[string][]byte
	ttls map[string]time.Duration
}

func (m *recLeaseConn) Get(_ context.Context, key string) ([]byte, error) {
	raw, ok := m.rows[key]
	if !ok {
		return nil, errLeaseDown
	}
	return raw, nil
}

func (m *recLeaseConn) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	m.rows[key] = val
	m.ttls[key] = ttl
	return nil
}

func (m *recLeaseConn) Del(_ context.Context, key string) error {
	delete(m.rows, key)
	return nil
}
