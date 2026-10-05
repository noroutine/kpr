package clideps

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// Opening state against a dead redis must fail fast naming redis —
// the operator typo'd the address and needs to know it now, not after
// a hang. If this fails, keeper commands stall or blame the wrong
// backend.
func TestOpenStoreNamesDeadRedis(t *testing.T) {
	// Silence now derives the file backend, so ask for redis
	// explicitly — this case is about redis, not about derivation.
	clearStoreEnv(t)
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	cfg := config.NewBuilder().WithRedisAddr("127.0.0.1:1").Build()
	if _, err := OpenStore(cfg); err == nil {
		t.Error("OpenStore on dead redis succeeded, want a fast error")
	} else if !strings.Contains(err.Error(), "redis") {
		t.Errorf("error = %q, want it to name redis", err.Error())
	}
}

// The backend name voices file with its dir, redis otherwise: the
// refusal names what the operator must fix. If this fails, outages
// blame the wrong backend.
func TestStoreNamesVoiceBackend(t *testing.T) {
	dir := t.TempDir()
	if got := StoreName(store.NewFileStore(dir)); got != "file store" {
		t.Errorf("StoreName(file) = %q, want file store", got)
	}
	if got := StoreName(store.NewMemStore()); got != "redis" {
		t.Errorf("StoreName(other) = %q, want redis", got)
	}
}

// Conflicting backend env refuses with the conflict named: guessing
// state wrong is worse than not booting. If this fails, file+redis
// together pick one silently.
func TestOpenStoreRefusesConflictingBackend(t *testing.T) {
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvRedisAddr, "127.0.0.1:1")
	cfg := config.NewBuilder().FromEnv().Build()
	if _, err := OpenStore(cfg); err == nil {
		t.Error("OpenStore on conflicting backend succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "conflicts") {
		t.Errorf("refusal = %q, want the conflict named", err.Error())
	}
}

// A file backend rooted at a non-directory refuses naming the dir:
// the operator learns the path is wrong, not that redis is down.
// If this fails, a bad store dir blames redis.
func TestOpenStoreRefusesBadFileDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("in the way"), 0o644); err != nil {
		t.Fatalf("stage blocker: %v", err)
	}
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, blocker)
	cfg := config.NewBuilder().FromEnv().Build()
	if _, err := OpenStore(cfg); err == nil {
		t.Error("OpenStore on file-backed dir succeeded, want refusal")
	} else if !strings.Contains(err.Error(), blocker) {
		t.Errorf("refusal = %q, want the dir named", err.Error())
	}
}

// fakeRedis answers just enough RESP for a go-redis Ping: HELLO
// gets an empty map, PING pongs, anything else oks. A real redis is
// a test dependency nobody wants; this proves OpenStore dials and
// selects, not the wire grammar.
func fakeRedis(t *testing.T) string {
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
			go serveFakeRedisConn(c)
		}
	}()
	return ln.Addr().String()
}

func serveFakeRedisConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if !strings.HasPrefix(line, "*") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
		if err != nil {
			return
		}
		var first string
		for i := range n {
			ln, err := r.ReadString('\n')
			if err != nil {
				return
			}
			m, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "$")))
			if err != nil {
				return
			}
			buf := make([]byte, m+2)
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			if i == 0 {
				first = string(buf[:m])
			}
		}
		switch strings.ToUpper(first) {
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

// Opening redis state against an answering cache succeeds: the
// success return is a real path, not a hope. If this fails, every
// redis deploy refuses at boot.
func TestOpenStoreRedisSuccess(t *testing.T) {
	addr := fakeRedis(t)
	cfg := config.NewBuilder().WithRedisAddr(addr).Build()
	s, err := OpenStore(cfg)
	if err != nil {
		t.Fatalf("OpenStore on answering redis: %v", err)
	}
	_ = s.Close()
}

// KPR_STORE=file opens the file backend (fail-fast Ping like redis):
// the operator gets file state or a refusal naming the dir, never a
// silent redis. If this fails, file mode boots something else.
func TestOpenStoreFileBackend(t *testing.T) {
	clearStoreEnv(t)
	t.Setenv(config.EnvStore, "file")
	t.Setenv(config.EnvStoreDir, t.TempDir())
	cfg := config.NewBuilder().FromEnv().Build()
	s, err := OpenStore(cfg)
	if err != nil {
		t.Fatalf("OpenStore(file): %v", err)
	}
	if _, ok := s.(*store.FileStore); !ok {
		t.Errorf("store = %T, want *store.FileStore", s)
	}
}
