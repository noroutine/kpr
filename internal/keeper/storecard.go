package keeper

import (
	"context"
	"fmt"
	"strconv"
	"syscall"

	"nrtn.dev/catalyst/kpr/internal/helpers/human"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// StoreCard is what the console shows about the state backend
// and what `status store` prints: which backend, where it
// lives, whether it answered, the operator intent, and the
// filesystem size for the file backend. Never a secret (no
// password, only the address). Standing is decided here, at
// assembly — renderers only translate it.
type StoreCard struct {
	Standing Standing `json:"standing"`
	Name     string   `json:"name"`
	Detail   string   `json:"detail"`
	Healthy  bool     `json:"healthy"`
	// LockNote voices the operator intent marker: "locked"
	// denies registry-store writes (gc refuses) while reads,
	// sweeps, and the receiver keep working; "lock unknown" on
	// a marker read failure — collapsing the error into
	// unlocked would claim intent nobody recorded. Empty means
	// unlocked: the common open state needs no ink.
	LockNote string `json:"lockNote,omitempty"`
	// Identity voices the lineage pairing verdicts judge the
	// served generation against, with the accepted baseline
	// when one was recorded. "unpaired" names itself; "unknown"
	// on a read failure, never a guess. Empty on no store:
	// nothing to pair, so renderers skip the line.
	Identity string `json:"identity,omitempty"`
	// FsNote voices filesystem size for the file backend ("91
	// GB free of 128 GB"): capacity planning, not precision.
	// Shown only on a proven store — an unproven dir is
	// nobody's store to report on. Empty renders nothing.
	FsNote string `json:"fsNote,omitempty"`
}

// FetchStoreCard names the wired backend for the card. The
// type switch is the whole derivation: callers know
// addresses, only the constructed store knows which backend
// serve actually opened. The lock read rides along: one cheap
// marker check per fetch, so the card voices intent without a
// second round trip anywhere.
func FetchStoreCard(ctx context.Context, s store.Store, healthy bool, redisAddr string, redisDB int, proven bool) StoreCard {
	if s == nil {
		return StoreCard{Standing: StandingUnavailable, Name: "unavailable", Healthy: healthy}
	}
	note := ""
	standing := StandingOK
	switch ok, err := s.IsUnlocked(ctx); {
	case err != nil:
		note = "lock unknown"
		standing = StandingDegraded
	case !ok:
		note = "locked"
		standing = StandingLocked
	}
	// A lock verdict wins over the probe: operator intent (or a
	// failed marker read) is the more specific news; unreachable
	// voices only a backend with nothing louder to say.
	if !healthy && standing == StandingOK {
		standing = StandingUnreachable
	}
	// The pairing read rides along with the lock read: one cheap
	// marker check per fetch, so the card voices identity without
	// a second round trip anywhere.
	ident := ""
	if id, ierr := s.GetIdentity(ctx); ierr != nil {
		ident = "unknown"
	} else if id.ID == "" {
		ident = "unpaired"
	} else if id.BaselineGen != "" {
		ident = id.ID + " (baseline " + id.BaselineGen + ")"
	} else {
		ident = id.ID
	}
	switch st := s.(type) {
	case *store.FileStore:
		fs := ""
		if proven {
			fs = fsStats(st.Dir())
		}
		return StoreCard{Standing: standing, Name: "file", Detail: st.Dir(), Healthy: healthy, LockNote: note, FsNote: fs, Identity: ident}
	case *store.RedisStore:
		return StoreCard{
			Standing: standing,
			Name:     "redis",
			Detail:   redisAddr + " db " + strconv.Itoa(redisDB),
			Healthy:  healthy,
			LockNote: note,
			Identity: ident,
		}
	case *store.MemStore:
		return StoreCard{Standing: standing, Name: "mem", Detail: "in-memory, tests only", Healthy: healthy, LockNote: note, Identity: ident}
	default:
		// Unknown implementations still voice the probe: the
		// note was computed from the port, not the type.
		return StoreCard{Standing: standing, Name: "unavailable", Healthy: healthy, LockNote: note, Identity: ident}
	}
}

// fsStats voices the filesystem holding dir as "91 GB free of
// 128 GB (29% used)": capacity planning, not precision. Empty
// on any failure — an unreadable mount gets no ink, not an
// error card.
func fsStats(dir string) string {
	total, free, ok := fsSizes(dir)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s free of %s (%d%% used)", human.Capacity(free), human.Capacity(total), human.UsedPercent(total, free))
}

// fsSizes returns the filesystem's total and free bytes for
// dir. Blocks are counted in fragment units, not the preferred
// I/O size: virtiofs reports a 1MB Bsize over 4K fragments, so
// Bsize here would mint petabytes (see fsBlockUnit). Not-ok on
// any failure or nonsense (zero total, free above total).
func fsSizes(dir string) (total, free uint64, ok bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, 0, false
	}
	return sizesFromBlocks(uint64(fs.Blocks), uint64(fs.Bavail), fsBlockUnit(&fs))
}

// sizesFromBlocks is the pure half of fsSizes: block counts
// times the fragment unit, refused when nonsense. Split so the
// guard's truth table is unit-testable without a corrupt
// filesystem.
func sizesFromBlocks(blocks, bavail, unit uint64) (total, free uint64, ok bool) {
	total = blocks * unit
	free = bavail * unit
	if !saneSizes(total, free) {
		return 0, 0, false
	}
	return total, free, true
}

// saneSizes refuses nonsense before it reaches the card: zero
// total, or free above total. If this fails, a corrupt Statfs
// voices petabytes as capacity.
func saneSizes(total, free uint64) bool {
	return total != 0 && free <= total
}
