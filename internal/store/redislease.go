package store

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// leaseExpiryMargin is the hygiene horizon past the lease term:
// the key outlives `{until}` by this much so overrun reads
// survive, then redis reaps it. Authority stays with the
// content, never the TTL.
const leaseExpiryMargin = 5 * time.Minute

// LeaseConn is the three redis commands a HOLD lease needs: read,
// overwrite with a hygiene TTL, drop. *RedisStore satisfies it
// through its own client; tests bring an in-memory fake. Narrow
// on purpose — the lease must not reach rows, and rows must not
// reach the lease.
type LeaseConn interface {
	// Get answers (nil, nil) on a missing key; any other error
	// surfaces for the reader to log loudly. Absence is a value,
	// outage is an event — the conn does not conflate them.
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Del(ctx context.Context, key string) error
}

// redisConn adapts the store's client to the lease surface,
// translating a miss into the (nil, nil) absence once, so impls
// never import go-redis.
type redisConn struct {
	rdb *redis.Client
}

func (c redisConn) Get(ctx context.Context, key string) ([]byte, error) {
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	return raw, err
}

func (c redisConn) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, val, ttl).Err()
}

func (c redisConn) Del(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, key).Err()
}

// HoldLeaseConn advertises lease hosting on the shared redis:
// the capability the fence decision asserts. The conn shares
// the store's client and DB — no second dial.
func (s *RedisStore) HoldLeaseConn() (LeaseConn, bool) {
	return redisConn{rdb: s.rdb}, true
}

// RedisLease engages proxy HOLD leases over the shared redis:
// the same announcement file leases make, minus the dir. Hold
// overwrites unconditionally (last-writer-wins, like the file)
// with a TTL of term plus margin; reads evaluate the shared
// payload at read time, so a miss reads absent (fail open —
// leases are not evidence) while a backend error logs loudly
// and reads absent. A Hold that fails to engage refuses loudly.
// Release best-effort DELs, like Remove.
type RedisLease struct {
	Conn LeaseConn
	// Key names the lease; HoldLeaseKey, the kpr-prefixed
	// contract, in production.
	Key string
}

// Hold writes a lease expiring at until and returns its release.
// A crashed collect can never wedge pushes past the expiry —
// the bound is the real one, the key is just carriage, and the
// horizon reaps the carriage after.
func (l RedisLease) Hold(ctx context.Context, until time.Time) (func(), error) {
	if l.Conn == nil {
		return nil, errors.New("redis lease without a conn")
	}
	ttl := time.Until(until) + leaseExpiryMargin
	if ttl < leaseExpiryMargin {
		ttl = leaseExpiryMargin
	}
	if err := l.Conn.Set(ctx, l.Key, encodeLease(until), ttl); err != nil {
		return nil, err
	}
	return func() {
		_ = l.Conn.Del(context.Background(), l.Key)
	}, nil
}

// Read parses the HOLD lease: the expiry plus whether it parses
// at all. A miss or corrupt payload reads absent; a backend
// error logs loudly and reads absent — never silent, never a
// refusal. Leases are not evidence.
func (l RedisLease) Read() (until time.Time, present bool) {
	if l.Conn == nil {
		return time.Time{}, false
	}
	raw, err := l.Conn.Get(context.Background(), l.Key)
	if err != nil {
		log.Printf("lease: failed to read %q, failing open: %v", l.Key, err)
		return time.Time{}, false
	}
	if raw == nil {
		return time.Time{}, false
	}
	return parseLease(raw)
}

// HeldUntil returns the live lease expiry: only a present lease
// still in its term holds.
func (l RedisLease) HeldUntil(now time.Time) (time.Time, bool) {
	until, present := l.Read()
	if !present || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}
