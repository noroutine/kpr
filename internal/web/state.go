package web

import (
	"context"
	"fmt"
	"strconv"
	"syscall"

	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// storeData is what the console shows about the state backend: which
// one, where it lives, whether it answered. Details come from the
// resolved config and the wired store — never a fresh env read, and
// never a secret (no password, only the address).
type storeData struct {
	Name    string
	Detail  string
	Healthy bool
	// FsNote voices filesystem size for the file backend ("91 GB
	// free of 128 GB"): capacity planning, not precision. Shown
	// only on a same-store proof — an unproven dir is nobody's
	// store to report on. Empty renders nothing.
	FsNote string
	// LockNote voices the operator intent marker: "locked" denies
	// registry-store writes (gc refuses) while reads, sweeps, and
	// the receiver keep working; "lock unknown" on a marker read
	// failure — collapsing the error into unlocked would claim
	// intent nobody recorded. Empty means unlocked: the common
	// open state needs no ink.
	LockNote string
}

// sentinelData is the live same-store proof state: the fixed
// sentinel address plus the generation the registry currently
// serves, read back through the API on every page load. No
// generation served means no proof — "unproven", never a guess.
type sentinelData struct {
	Gen    string
	TS     string
	Proven bool
}

// storeSnapshot names the wired backend for the dashboard. The type
// switch is the whole derivation: config knows addresses, only the
// constructed store knows which backend serve actually opened. The
// lock read rides along: one cheap marker check per page load, so
// the card voices intent without a second round trip anywhere.
func storeSnapshot(ctx context.Context, s store.Store, healthy bool, cfg *config.Config, proven bool) storeData {
	note := ""
	if s != nil {
		switch ok, err := s.IsUnlocked(ctx); {
		case err != nil:
			note = "lock unknown"
		case !ok:
			note = "locked"
		}
	}
	switch st := s.(type) {
	case *store.FileStore:
		fs := ""
		if proven {
			fs = fsStats(st.Dir())
		}
		return storeData{Name: "file", Detail: st.Dir(), Healthy: healthy, LockNote: note, FsNote: fs}
	case *store.RedisStore:
		return storeData{
			Name:     "redis",
			Detail:   cfg.RedisAddr + " db " + strconv.Itoa(cfg.RedisDB),
			Healthy:  healthy,
			LockNote: note,
		}
	case *store.MemStore:
		return storeData{Name: "mem", Detail: "in-memory, tests only", Healthy: healthy, LockNote: note}
	default:
		// Unknown implementations still voice the probe: the
		// note was computed from the port, not the type.
		return storeData{Name: "unavailable", LockNote: note}
	}
}

// fsStats voices the filesystem holding dir as "91 GB free of
// 128 GB (29% used)": capacity planning, not precision. Empty on
// any failure — an unreadable mount gets no ink, not an error card.
func fsStats(dir string) string {
	total, free, ok := fsSizes(dir)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s free of %s (%d%% used)", fmtBytes(free), fmtBytes(total), usedPercent(total, free))
}

// usedPercent is the glance math behind the capacity card: whole
// percent used. If this fails, the card's percent lies while free
// and total read true.
func usedPercent(total, free uint64) uint64 {
	return 100 * (total - free) / total
}

// fsSizes returns the filesystem's total and free bytes for dir.
// Blocks are counted in fragment units, not the preferred I/O
// size: virtiofs reports a 1MB Bsize over 4K fragments, so Bsize
// here would mint petabytes (see fsBlockUnit). Not-ok on any
// failure or nonsense (zero total, free above total).
func fsSizes(dir string) (total, free uint64, ok bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, 0, false
	}
	return sizesFromBlocks(uint64(fs.Blocks), uint64(fs.Bavail), fsBlockUnit(&fs))
}

// sizesFromBlocks is the pure half of fsSizes: block counts times
// the fragment unit, refused when nonsense. Split so the guard's
// truth table is unit-testable without a corrupt filesystem.
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

// fmtBytes renders byte counts in operator units: whole TB/GB
// above ten, one decimal below, MB under a gig. Precision stays in
// /api/activity and the CLI — this is a glance.
func fmtBytes(n uint64) string {
	const tb = 1024 * 1024 * 1024 * 1024
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	switch {
	case n >= 10*tb:
		return fmt.Sprintf("%d TB", n/tb)
	case n >= tb:
		return fmt.Sprintf("%.1f TB", float64(n)/tb)
	case n >= 10*gb:
		return fmt.Sprintf("%d GB", n/gb)
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	default:
		return fmt.Sprintf("%d MB", n/mb)
	}
}

// sentinelSnapshot reads the live sentinel generation for the
// dashboard, bounded like every other console probe: a down
// registry or an unproven store renders unproven, never slow and
// never 500. Nil API renders unconfigured.
func (s *Server) sentinelSnapshot(ctx context.Context) sentinelData {
	var d sentinelData
	if s.Sentinel == nil {
		return d
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	p, err := sentinel.LastProof(ctx, s.Sentinel)
	if err != nil {
		return d
	}
	d.Gen, d.TS, d.Proven = p.Gen, p.TS, true
	return d
}
