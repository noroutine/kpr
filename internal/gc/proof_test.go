package gc

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// Missing binary or config refuses with the remedy instead of failing
// mid-collect: gc degrades by construction when the store isn't
// shared into this container. If this fails, a bare image (no mounts)
// crashes on paths instead of explaining them.
func TestReadyGatesMissingPrereqs(t *testing.T) {
	if err := Ready("/bin/registry", "/etc/distribution/config.yml"); err != nil {
		t.Logf("note: this host lacks %v (fine outside the image)", err)
	}
	if err := Ready("/no/such/binary", "/etc/distribution/config.yml"); err == nil {
		t.Error("missing binary passed readiness, want refusal")
	} else if !strings.Contains(err.Error(), "/no/such/binary") {
		t.Errorf("refusal names no path: %v", err)
	}
	if err := Ready("/bin/sh", "/no/such/config.yml"); err == nil {
		t.Error("missing config passed readiness, want refusal")
	} else if !strings.Contains(err.Error(), "/no/such/config.yml") {
		t.Errorf("refusal names no path: %v", err)
	}
}

// The collector's blobdescriptor cache must answer before anything is
// collected: an unreachable cache mis-marks (live layers look
// unreferenced) and the run deletes what it must keep. No redis
// section means inmemory cache — nothing to gate. If this fails, gc
// collects blind on a broken cache connection.
func TestCacheGateDialsRegistryRedis(t *testing.T) {
	dir := t.TempDir()
	withRedis := filepath.Join(dir, "redis.yml")
	if err := os.WriteFile(withRedis, []byte("redis:\n  addr: 127.0.0.1:1\n  password: wrong\n  db: 3\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := CacheReady(context.Background(), withRedis); err == nil {
		t.Error("unreachable redis cache passed, want refusal")
	} else if !strings.Contains(err.Error(), "blobdescriptor") {
		t.Errorf("refusal names no cause: %v", err)
	}
	plain := filepath.Join(dir, "plain.yml")
	if err := os.WriteFile(plain, []byte("storage:\n  filesystem:\n    rootdirectory: /var/lib/registry\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := CacheReady(context.Background(), plain); err != nil {
		t.Errorf("cacheless config gated: %v", err)
	}
	if err := CacheReady(context.Background(), filepath.Join(dir, "absent.yml")); err == nil {
		t.Error("absent config passed the cache gate, want refusal")
	}
}

// The cache gate's happy path answers: a redis that pongs clears.
// If this fails, the success return below the ping is untested and
// a break there fails every cached run.
func TestCacheGateAcceptsAnsweringCache(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				serveFakeRedis(c)
			}()
		}
	}()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "redis.yml")
	if err := os.WriteFile(cfg, []byte("redis:\n  addr: "+ln.Addr().String()+"\n"), 0o644); err != nil {
		t.Fatalf("stage config: %v", err)
	}
	if err := CacheReady(context.Background(), cfg); err != nil {
		t.Errorf("answering cache gated: %v", err)
	}
}

// serveFakeRedis answers just enough RESP for a go-redis Ping: the
// v9 handshake (HELLO) gets an empty map, everything after pongs.
// A real redis is a test dependency nobody wants; this proves the
// gate dials and listens, not the wire grammar.
func serveFakeRedis(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if !strings.HasPrefix(line, "*") {
			continue
		}
		args, err := readRespArgs(r, line)
		if err != nil || len(args) == 0 {
			return
		}
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			_, _ = c.Write([]byte("%0\r\n"))
		case "PING":
			_, _ = c.Write([]byte("+PONG\r\n"))
			return
		default:
			_, _ = c.Write([]byte("+OK\r\n"))
		}
	}
}

// readRespArgs consumes one *-array's arguments after its header
// line: N ($len, bytes) pairs. Only the words matter — the fake
// routes on the command, not the grammar.
func readRespArgs(r *bufio.Reader, header string) ([]string, error) {
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

// The redis endpoint reads the collector's own grammar: plural
// addrs win over singular, the env password overrides the file,
// garbage refuses, absence is not an error. If this fails, the
// cache gate dials the wrong redis — or none.
func TestRegistryRedisGrammar(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
		return p
	}
	if _, _, _, err := registryRedis(filepath.Join(dir, "absent.yml")); err == nil {
		t.Error("absent config parsed, want the read error")
	}
	if _, _, _, err := registryRedis(write("garbage.yml", "redis:\n\tbad: [unclosed")); err == nil {
		t.Error("garbage config parsed, want refusal")
	} else if !strings.Contains(err.Error(), "garbage.yml") {
		t.Errorf("refusal names no path: %v", err)
	}
	plural := write("plural.yml", "redis:\n  addr: first:6379\n  addrs:\n  - winner:6379\n  - second:6379\n  password: filepw\n  db: 2\n")
	addr, pw, db, err := registryRedis(plural)
	if err != nil {
		t.Fatalf("plural config: %v", err)
	}
	if addr != "winner:6379" || pw != "filepw" || db != 2 {
		t.Errorf("redis = (%q, %q, %d), want (winner:6379, filepw, 2)", addr, pw, db)
	}
	t.Setenv("REGISTRY_REDIS_PASSWORD", "envpw")
	if _, pw, _, err := registryRedis(plural); err != nil {
		t.Fatalf("env override: %v", err)
	} else if pw != "envpw" {
		t.Errorf("password = %q, want the env override envpw", pw)
	}
}

// An unwritable root refuses the mint before the read-back: the
// generation is never half-laid. If this fails, a read-only mount
// reports a same-store mismatch instead of the write error.
func TestWriteVerifiedGenerationRefusesUnwritable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o644); err != nil {
		t.Fatalf("stage blocker: %v", err)
	}
	payload := sentinel.Payload{V: 1, Gen: "gen", ID: "id", TS: "ts", Writer: "test"}
	if _, err := writeVerifiedGeneration(context.Background(), fileAPI{t.TempDir()}, blocker, payload); err == nil {
		t.Fatal("mint under a blocked root succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "unwritable") {
		t.Errorf("refusal names no cause: %v", err)
	}
}
