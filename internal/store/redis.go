package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"nrtn.dev/catalyst/kpr/internal/policy"
)

// RedisStore is the production Store over go-redis.
type RedisStore struct {
	rdb *redis.Client
}

// NewRedisStore dials addr lazily: construction never fails (the first
// command fails if redis is down) — callers degrade, never crash here.
// An empty password means no authentication (the long-standing default);
// a set one is sent as AUTH on every connection (a shared,
// password-protected redis also serving the registry blobdescriptor
// cache). db selects the logical database — never assume 0: shared
// instances host other tenants there (compose: kpr rows on 4, the
// registry cache on 3).
func NewRedisStore(addr, password string, db int) *RedisStore {
	return &RedisStore{rdb: redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: db})}
}

// Close drains the client pool.
func (s *RedisStore) Close() error { return s.rdb.Close() }

// Flush drops every kpr key. Tests only: a clean slate per subtest so
// contract cases never see each other's rows.
func (s *RedisStore) Flush(ctx context.Context) error {
	return s.rdb.Del(ctx, RowsKey, CurrentKey, ActivityKey, LockKey, UnlockedKey).Err()
}

func (s *RedisStore) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

// IsUnlocked reads the intent marker: absent (fresh included) is
// locked. A redis error refuses, never guesses.
func (s *RedisStore) IsUnlocked(ctx context.Context) (bool, error) {
	n, err := s.rdb.Exists(ctx, UnlockedKey).Result()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// SetUnlocked writes or drops the marker: presence is the whole
// state, so lock is a DEL and unlock a plain SET (no expiry —
// intent persists until the operator revokes it).
func (s *RedisStore) SetUnlocked(ctx context.Context, unlocked bool) error {
	if !unlocked {
		return s.rdb.Del(ctx, UnlockedKey).Err()
	}
	return s.rdb.Set(ctx, UnlockedKey, "1", 0).Err()
}

func encodeRow(r policy.Row) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func (s *RedisStore) Record(ctx context.Context, r policy.Row) error {
	k := key(r.Repo, r.Tag)
	raw, err := s.rdb.HGet(ctx, RowsKey, k).Bytes()
	if err != nil && err != redis.Nil {
		return err
	}
	if err == nil {
		var old policy.Row
		if jerr := json.Unmarshal(raw, &old); jerr == nil && !r.PushedAt.After(old.PushedAt) {
			r.Due, r.Reason = old.Due, old.Reason
		}
	}
	return s.rdb.HSet(ctx, RowsKey, k, encodeRow(r)).Err()
}

func (s *RedisStore) All(ctx context.Context) ([]policy.Row, error) {
	vals, err := s.rdb.HVals(ctx, RowsKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]policy.Row, 0, len(vals))
	for _, v := range vals {
		var r policy.Row
		if err := json.Unmarshal([]byte(v), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *RedisStore) Due(ctx context.Context) ([]policy.Row, error) {
	all, err := s.All(ctx)
	if err != nil {
		return nil, err
	}
	var out []policy.Row
	for _, r := range all {
		if r.Due {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *RedisStore) MarkDue(ctx context.Context, repo, tag, reason string) error {
	k := key(repo, tag)
	raw, err := s.rdb.HGet(ctx, RowsKey, k).Bytes()
	var r policy.Row
	if err == nil {
		if jerr := json.Unmarshal(raw, &r); jerr != nil {
			return jerr
		}
	} else if err != redis.Nil {
		return err
	}
	r.Repo, r.Tag = repo, tag
	r.Due, r.Reason = true, reason
	return s.rdb.HSet(ctx, RowsKey, k, encodeRow(r)).Err()
}

func (s *RedisStore) UnmarkDue(ctx context.Context, repo, tag string) (bool, error) {
	k := key(repo, tag)
	raw, err := s.rdb.HGet(ctx, RowsKey, k).Bytes()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var r policy.Row
	if jerr := json.Unmarshal(raw, &r); jerr != nil {
		return false, jerr
	}
	if !r.Due {
		return false, nil
	}
	r.Due, r.Reason = false, ""
	return true, s.rdb.HSet(ctx, RowsKey, k, encodeRow(r)).Err()
}

func (s *RedisStore) ClearDue(ctx context.Context) (int, error) {
	all, err := s.All(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range all {
		if !r.Due {
			continue
		}
		r.Due, r.Reason = false, ""
		if err := s.rdb.HSet(ctx, RowsKey, key(r.Repo, r.Tag), encodeRow(r)).Err(); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *RedisStore) Delete(ctx context.Context, repo, tag string) error {
	return s.rdb.HDel(ctx, RowsKey, key(repo, tag)).Err()
}

func (s *RedisStore) SetCurrent(ctx context.Context, c Current) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, CurrentKey, b, 0).Err()
}

func (s *RedisStore) GetCurrent(ctx context.Context) (Current, error) {
	raw, err := s.rdb.Get(ctx, CurrentKey).Bytes()
	if err == redis.Nil {
		return Current{}, nil
	}
	if err != nil {
		return Current{}, err
	}
	var c Current
	if err := json.Unmarshal(raw, &c); err != nil {
		return Current{}, err
	}
	return c, nil
}

func (s *RedisStore) PushActivity(ctx context.Context, o Outcome) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	pipe := s.rdb.Pipeline()
	pipe.LPush(ctx, ActivityKey, b)
	pipe.LTrim(ctx, ActivityKey, 0, ActivityCap-1)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) Activity(ctx context.Context) ([]Outcome, error) {
	vals, err := s.rdb.LRange(ctx, ActivityKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Outcome, 0, len(vals))
	for _, v := range vals {
		var o Outcome
		if err := json.Unmarshal([]byte(v), &o); err != nil {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

func (s *RedisStore) acquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return s.rdb.SetNX(ctx, key, "1", ttl).Result()
}

func (s *RedisStore) release(ctx context.Context, key string) error {
	return s.rdb.Del(ctx, key).Err()
}

func (s *RedisStore) AcquireLock(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	return s.acquire(ctx, name, ttl)
}

func (s *RedisStore) ReleaseLock(ctx context.Context, name string) error {
	return s.release(ctx, name)
}
