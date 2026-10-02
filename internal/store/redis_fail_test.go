package store_test

// The redis driver under a programmable wire: a fake RESP server
// failing named commands with -ERR and serving canned keys. Every
// driver verdict — propagate the wire error, skip the corrupt,
// default the absent — proves itself without a live redis. If any
// of these fail, the backend invents state the wire never sent.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// wireScript programs one fake-redis backend: fail names the
// commands answered -ERR, str seeds GET/HGET answers, hvals and
// lrange seed the scans, existsN answers EXISTS. Writes land in a
// small live state (strings, hashes) so read-back asserts what the
// driver wrote — a minimal redis, not a canned parrot.
type wireScript struct {
	fail    map[string]bool
	str     map[string]string
	hvals   []string
	lrange  []string
	existsN int

	mu   sync.Mutex
	kv   map[string]string
	hash map[string]map[string]string
	// calls records the argument vectors of bound-carrying
	// commands: the trim window and the scan range are the
	// driver's promises, and the wire must show them.
	calls map[string][][]string
}

func (s *wireScript) record(cmd string, args []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string][][]string{}
	}
	s.calls[cmd] = append(s.calls[cmd], args)
}

func (s *wireScript) get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv != nil {
		if v, ok := s.kv[key]; ok {
			return v, true
		}
	}
	v, ok := s.str[key]
	return v, ok
}

func (s *wireScript) set(key, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv == nil {
		s.kv = map[string]string{}
	}
	s.kv[key] = val
}

func (s *wireScript) hget(hash, field string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.hash[hash]; ok {
		if v, ok := h[field]; ok {
			return v, true
		}
	}
	v, ok := s.str[hash+"\x00"+field]
	return v, ok
}

func (s *wireScript) hset(hash, field, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hash == nil {
		s.hash = map[string]map[string]string{}
	}
	h, ok := s.hash[hash]
	if !ok {
		h = map[string]string{}
		s.hash[hash] = h
	}
	h[field] = val
}

func (s *wireScript) hdel(hash, field string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.hash[hash]; ok {
		delete(h, field)
	}
}

func (s *wireScript) hall(hash string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, h := range []map[string]string{s.hash[hash]} {
		for _, v := range h {
			out = append(out, v)
		}
	}
	return out
}

func serveWire(t *testing.T, script *wireScript) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveWireConn(c, script)
		}
	}()
	return ln.Addr().String()
}

func readArgs(r *bufio.Reader, header string) ([]string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "*")))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for range n {
		ln, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		m, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "$")))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, m+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:m]))
	}
	return args, nil
}

func bulk(s string) string { return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s) }

func serveWireConn(c net.Conn, script *wireScript) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if !strings.HasPrefix(line, "*") {
			continue
		}
		args, err := readArgs(r, line)
		if err != nil || len(args) == 0 {
			return
		}
		cmd := strings.ToUpper(args[0])
		if script.fail[cmd] {
			_, _ = c.Write([]byte("-ERR injected " + cmd + " failure\r\n"))
			continue
		}
		if cmd == "LTRIM" || cmd == "LRANGE" {
			script.record(cmd, args[1:])
		}
		switch cmd {
		case "HELLO":
			_, _ = c.Write([]byte("%0\r\n"))
		case "PING":
			_, _ = c.Write([]byte("+PONG\r\n"))
		case "SELECT", "CLIENT", "LTRIM":
			_, _ = c.Write([]byte("+OK\r\n"))
		case "SET":
			if slices.Contains(args, "NX") {
				_, _ = c.Write([]byte(":1\r\n"))
			} else {
				script.set(args[1], args[2])
				_, _ = c.Write([]byte("+OK\r\n"))
			}
		case "GET":
			if v, ok := script.get(args[1]); ok {
				_, _ = c.Write([]byte(bulk(v)))
			} else {
				_, _ = c.Write([]byte("$-1\r\n"))
			}
		case "HGET":
			if v, ok := script.hget(args[1], args[2]); ok {
				_, _ = c.Write([]byte(bulk(v)))
			} else {
				_, _ = c.Write([]byte("$-1\r\n"))
			}
		case "HSET":
			script.hset(args[1], args[2], args[3])
			_, _ = c.Write([]byte(":1\r\n"))
		case "HDEL":
			script.hdel(args[1], args[2])
			_, _ = c.Write([]byte(":1\r\n"))
		case "DEL":
			script.mu.Lock()
			for _, k := range args[1:] {
				delete(script.kv, k)
				delete(script.hash, k)
			}
			script.mu.Unlock()
			_, _ = c.Write([]byte(":1\r\n"))
		case "EXISTS", "SETNX", "LPUSH":
			if cmd == "EXISTS" {
				_, _ = fmt.Fprintf(c, ":%d\r\n", script.existsN)
			} else {
				_, _ = c.Write([]byte(":1\r\n"))
			}
		case "HVALS":
			vals := append(script.hall(args[1]), script.hvals...)
			var b strings.Builder
			fmt.Fprintf(&b, "*%d\r\n", len(vals))
			for _, v := range vals {
				b.WriteString(bulk(v))
			}
			_, _ = c.Write([]byte(b.String()))
		case "LRANGE":
			var b strings.Builder
			fmt.Fprintf(&b, "*%d\r\n", len(script.lrange))
			for _, v := range script.lrange {
				b.WriteString(bulk(v))
			}
			_, _ = c.Write([]byte(b.String()))
		default:
			_, _ = c.Write([]byte("+OK\r\n"))
		}
	}
}

const (
	wireRowDue  = `{"Repo":"app","Tag":"v1","Digest":"sha256:a","PushedAt":"2026-01-01T00:00:00Z","Actor":"kpr-gc","Due":true,"Reason":"x"}`
	wireRowCalm = `{"Repo":"app","Tag":"v9","Digest":"sha256:b","PushedAt":"2026-01-01T00:00:00Z","Actor":"kpr-gc","Due":false,"Reason":""}`
	wireOutcome = `{"repo":"app","tag":"v1","reason":"x","outcome":"deleted","at":"2026-01-01T00:00:00Z"}`
)

func wireStore(t *testing.T, script *wireScript) *store.RedisStore {
	t.Helper()
	s := store.NewRedisStore(serveWire(t, script), "", 0)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A dead command fails the op that needs it: every verb voices the
// wire error, never a guess. If this fails, one verb invents an
// answer while redis is down.
func TestRedisWireFailuresRefuse(t *testing.T) {
	row := policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:a", PushedAt: time.Now().UTC()}
	dueHvals := &wireScript{hvals: []string{wireRowDue}}
	for _, tc := range []struct {
		name   string
		script *wireScript
		call   func(context.Context, *store.RedisStore) error
	}{
		{"ping", &wireScript{fail: map[string]bool{"PING": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.Ping(ctx) }},
		{"isunlocked", &wireScript{fail: map[string]bool{"EXISTS": true}}, func(ctx context.Context, s *store.RedisStore) error { _, err := s.IsUnlocked(ctx); return err }},
		{"lock", &wireScript{fail: map[string]bool{"DEL": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.SetUnlocked(ctx, false) }},
		{"unlock", &wireScript{fail: map[string]bool{"SET": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.SetUnlocked(ctx, true) }},
		{"identity read", &wireScript{fail: map[string]bool{"GET": true}}, func(ctx context.Context, s *store.RedisStore) error { _, err := s.GetIdentity(ctx); return err }},
		{"identity write", &wireScript{fail: map[string]bool{"SET": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.SetIdentity(ctx, store.Identity{}) }},
		{"record read", &wireScript{fail: map[string]bool{"HGET": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.Record(ctx, row) }},
		{"record write", &wireScript{fail: map[string]bool{"HSET": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.Record(ctx, row) }},
		{"all", &wireScript{fail: map[string]bool{"HVALS": true}}, func(ctx context.Context, s *store.RedisStore) error { _, err := s.All(ctx); return err }},
		{"due", &wireScript{fail: map[string]bool{"HVALS": true}}, func(ctx context.Context, s *store.RedisStore) error { _, err := s.Due(ctx); return err }},
		{"mark read", &wireScript{fail: map[string]bool{"HGET": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.MarkDue(ctx, "a", "v", "x") }},
		{"mark write", &wireScript{fail: map[string]bool{"HSET": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.MarkDue(ctx, "a", "v", "x") }},
		{"unmark read", &wireScript{fail: map[string]bool{"HGET": true}}, func(ctx context.Context, s *store.RedisStore) error { _, err := s.UnmarkDue(ctx, "a", "v"); return err }},
		{"unmark write", &wireScript{
			fail: map[string]bool{"HSET": true},
			str:  map[string]string{"kpr:rows\x00app\x00v1": wireRowDue},
		}, func(ctx context.Context, s *store.RedisStore) error {
			_, err := s.UnmarkDue(ctx, "app", "v1")
			return err
		}},
		{"clear scan", &wireScript{fail: map[string]bool{"HVALS": true}}, func(ctx context.Context, s *store.RedisStore) error { _, err := s.ClearDue(ctx); return err }},
		{"clear write", &wireScript{fail: map[string]bool{"HSET": true}, hvals: dueHvals.hvals}, func(ctx context.Context, s *store.RedisStore) error {
			_, err := s.ClearDue(ctx)
			return err
		}},
		{"delete", &wireScript{fail: map[string]bool{"HDEL": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.Delete(ctx, "a", "v") }},
		{"current write", &wireScript{fail: map[string]bool{"SET": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.SetCurrent(ctx, store.Current{}) }},
		{"current read", &wireScript{fail: map[string]bool{"GET": true}}, func(ctx context.Context, s *store.RedisStore) error { _, err := s.GetCurrent(ctx); return err }},
		{"push", &wireScript{fail: map[string]bool{"LPUSH": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.PushActivity(ctx, store.Outcome{}) }},
		{"activity", &wireScript{fail: map[string]bool{"LRANGE": true}}, func(ctx context.Context, s *store.RedisStore) error { _, err := s.Activity(ctx); return err }},
		{"flush", &wireScript{fail: map[string]bool{"DEL": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.Flush(ctx) }},
		{"acquire", &wireScript{fail: map[string]bool{"SET": true}}, func(ctx context.Context, s *store.RedisStore) error {
			_, err := s.AcquireLock(ctx, "k", time.Minute)
			return err
		}},
		{"release", &wireScript{fail: map[string]bool{"DEL": true}}, func(ctx context.Context, s *store.RedisStore) error { return s.ReleaseLock(ctx, "k") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := wireStore(t, tc.script)
			if err := tc.call(context.Background(), s); err == nil {
				t.Errorf("%s with a failing wire succeeded, want refusal", tc.name)
			}
		})
	}
}

// Absent keys read as fresh state: no marker is locked, no pairing
// is unpaired, no current is empty. If this fails, a fresh backend
// errors where it should default.
func TestRedisAbsentKeysReadFresh(t *testing.T) {
	s := wireStore(t, &wireScript{})
	ctx := context.Background()
	if ok, err := s.IsUnlocked(ctx); err != nil || ok {
		t.Errorf("IsUnlocked absent = (%v, %v), want (false, nil)", ok, err)
	}
	if id, err := s.GetIdentity(ctx); err != nil || id != (store.Identity{}) {
		t.Errorf("GetIdentity absent = (%v, %v), want (empty, nil)", id, err)
	}
	if cur, err := s.GetCurrent(ctx); err != nil || cur != (store.Current{}) {
		t.Errorf("GetCurrent absent = (%v, %v), want (empty, nil)", cur, err)
	}
	if ok, err := s.UnmarkDue(ctx, "a", "v"); err != nil || ok {
		t.Errorf("UnmarkDue absent = (%v, %v), want (false, nil)", ok, err)
	}
}

// Corrupt values refuse the read that meets them: identity,
// current, mark, and unmark name the torn key, while scans skip
// the torn element and keep the good. If this fails, wire garbage
// parses as state (or poisons the whole scan).
func TestRedisCorruptValuesRefuseOrSkip(t *testing.T) {
	ctx := context.Background()
	str := map[string]string{
		"kpr:identity":               "not json",
		"kpr:sweep:current":          "not json",
		"kpr:rows\x00app\x00v1":      "not json",
		"kpr:rows\x00app\x00garbage": "not json",
	}
	s := wireStore(t, &wireScript{str: str, hvals: []string{"not json", wireRowCalm}, lrange: []string{"not json", wireOutcome}})
	if _, err := s.GetIdentity(ctx); err == nil {
		t.Error("GetIdentity over torn key succeeded, want refusal")
	}
	if _, err := s.GetCurrent(ctx); err == nil {
		t.Error("GetCurrent over torn key succeeded, want refusal")
	}
	if err := s.MarkDue(ctx, "app", "v1", "x"); err == nil {
		t.Error("MarkDue over torn row succeeded, want refusal")
	}
	if _, err := s.UnmarkDue(ctx, "app", "garbage"); err == nil {
		t.Error("UnmarkDue over torn row succeeded, want refusal")
	}
	rows, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All over mixed scan: %v", err)
	}
	if len(rows) != 1 || rows[0].Tag != "v9" {
		t.Errorf("All skipped nothing: %v, want only the good row", rows)
	}
	acts, err := s.Activity(ctx)
	if err != nil {
		t.Fatalf("Activity over mixed scan: %v", err)
	}
	if len(acts) != 1 || acts[0].Tag != "v1" {
		t.Errorf("Activity skipped nothing: %v, want only the good outcome", acts)
	}
	if ok, err := s.IsUnlocked(ctx); err != nil {
		t.Fatalf("IsUnlocked: %v", err)
	} else if ok {
		t.Error("IsUnlocked with exists 0 = true, want false")
	}
}

// Pushes trim the ring to the cap on the wire: LTRIM carries
// 0..ActivityCap-1, and a full scan reads 0..-1. The bounds are
// the driver's promises — an off-by-one grows the ring forever
// or pages the scan. If this fails, the ring is unbounded (or
// the scan partial) while the driver claims otherwise.
func TestRedisWireCarriesRingBounds(t *testing.T) {
	script := &wireScript{}
	s := wireStore(t, script)
	ctx := context.Background()
	if err := s.PushActivity(ctx, store.Outcome{Repo: "app"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := s.Activity(ctx); err != nil {
		t.Fatalf("activity: %v", err)
	}
	trims := script.calls["LTRIM"]
	wantStop := strconv.Itoa(store.ActivityCap - 1)
	if len(trims) != 1 || len(trims[0]) != 3 || trims[0][1] != "0" || trims[0][2] != wantStop {
		t.Errorf("LTRIM calls = %v, want one [key 0 %s]", trims, wantStop)
	}
	ranges := script.calls["LRANGE"]
	if len(ranges) != 1 || len(ranges[0]) != 3 || ranges[0][1] != "0" || ranges[0][2] != "-1" {
		t.Errorf("LRANGE calls = %v, want one [key 0 -1]", ranges)
	}
}

// A due row in the scan clears exactly once: the rewrite lands and
// the calm row stays untouched. If this fails, the clear miscounts
// (or rewrites what it should not).
func TestRedisClearDueClearsDueOnly(t *testing.T) {
	s := wireStore(t, &wireScript{hvals: []string{wireRowDue, wireRowCalm}})
	n, err := s.ClearDue(context.Background())
	if err != nil {
		t.Fatalf("ClearDue: %v", err)
	}
	if n != 1 {
		t.Errorf("ClearDue cleared %d, want 1", n)
	}
}

// Due filters the scan to marked rows: one due among calm returns
// one. If this fails, the plan lists what is not due (or drops
// what is).
func TestRedisDueFiltersScan(t *testing.T) {
	s := wireStore(t, &wireScript{hvals: []string{wireRowDue, wireRowCalm}})
	due, err := s.Due(context.Background())
	if err != nil {
		t.Fatalf("Due: %v", err)
	}
	if len(due) != 1 || due[0].Tag != "v1" {
		t.Errorf("Due = %v, want only the marked row", due)
	}
}

const (
	wireIdentity = `{"id":"id-1","baseline_gen":"gen-1"}`
	wireCurrent  = `{"pass_id":"p1","stage":"collect","trigger":"manual","due":2,"done":1}`
)

// Stored singletons decode back: identity and current round-trip
// through the wire byte-identical. If this fails, the pairing or
// the pass pointer corrupts in transit.
func TestRedisSingletonsDecode(t *testing.T) {
	s := wireStore(t, &wireScript{str: map[string]string{
		"kpr:identity":      wireIdentity,
		"kpr:sweep:current": wireCurrent,
	}})
	if id, err := s.GetIdentity(context.Background()); err != nil {
		t.Fatalf("GetIdentity: %v", err)
	} else if id.ID != "id-1" || id.BaselineGen != "gen-1" {
		t.Errorf("identity = %+v, want the stored pairing", id)
	}
	if cur, err := s.GetCurrent(context.Background()); err != nil {
		t.Fatalf("GetCurrent: %v", err)
	} else if cur.PassID != "p1" || cur.Due != 2 {
		t.Errorf("current = %+v, want the stored pointer", cur)
	}
}

// A calm stored row unmarks to (false, nil): nothing due, nothing
// to do, no error. If this fails, calm rows error on unmark.
func TestRedisUnmarkCalmIsFalseNil(t *testing.T) {
	s := wireStore(t, &wireScript{str: map[string]string{"kpr:rows\x00app\x00v9": wireRowCalm}})
	if ok, err := s.UnmarkDue(context.Background(), "app", "v9"); err != nil || ok {
		t.Errorf("UnmarkDue calm = (%v, %v), want (false, nil)", ok, err)
	}
}

// An older re-push preserves the mark while a newer push
// overwrites it: the read-back proves what the driver wrote, not
// what the test staged. If this fails, stale pushes unmark rows
// (or fresh pushes keep dead reasons).
func TestRedisRecordPreservesMarkOnOlderPush(t *testing.T) {
	ctx := context.Background()
	gen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := wireStore(t, &wireScript{})
	due := policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:a", PushedAt: gen,
		Due: true, Reason: "x"}
	if err := s.Record(ctx, due); err != nil {
		t.Fatalf("record due: %v", err)
	}
	stale := policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:old", PushedAt: gen.Add(-time.Hour)}
	if err := s.Record(ctx, stale); err != nil {
		t.Fatalf("stale record: %v", err)
	}
	rows, err := s.All(ctx)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows) != 1 || !rows[0].Due || rows[0].Reason != "x" || rows[0].Digest != "sha256:old" {
		t.Errorf("stale re-push = %+v, want the old digest with the mark kept", rows)
	}
	fresh := policy.Row{Repo: "app", Tag: "v1", Digest: "sha256:new", PushedAt: gen.Add(time.Hour)}
	if err := s.Record(ctx, fresh); err != nil {
		t.Fatalf("fresh record: %v", err)
	}
	rows, err = s.All(ctx)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rows) != 1 || rows[0].Due || rows[0].Digest != "sha256:new" {
		t.Errorf("fresh re-push = %+v, want the new digest unmarked", rows)
	}
}
