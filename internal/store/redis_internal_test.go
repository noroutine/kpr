package store

import "testing"

// The store must select the configured redis DB on the shared instance:
// DBs 0-2 belong to other tenants, the registry cache sits on 3, kpr
// rows go wherever KPR_REDIS_DB says (4 in compose). Construction is
// lazy, so the selected DB is assertable without a live server — a
// hardcoded DB 0 would silently read a foreign keyspace. If this fails,
// kpr and the registry cache (or a ceph invader) share a database.
// Internal: it reads the client's private options.
func TestNewRedisStoreSelectsConfiguredDB(t *testing.T) {
	for _, db := range []int{0, 3, 4} {
		s := NewRedisStore("localhost:6379", "", db)
		t.Cleanup(func() { _ = s.Close() })
		if got := s.rdb.Options().DB; got != db {
			t.Errorf("NewRedisStore(db %d) selected DB %d", db, got)
		}
	}
}
