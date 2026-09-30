package web

import (
	"context"
	"strconv"

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
	Addr   string
	Gen    string
	TS     string
	Proven bool
}

// storeSnapshot names the wired backend for the dashboard. The type
// switch is the whole derivation: config knows addresses, only the
// constructed store knows which backend serve actually opened. The
// lock read rides along: one cheap marker check per page load, so
// the card voices intent without a second round trip anywhere.
func storeSnapshot(ctx context.Context, s store.Store, healthy bool, cfg *config.Config) storeData {
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
		return storeData{Name: "file", Detail: st.Dir(), Healthy: healthy, LockNote: note}
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

// backendLabel voices the Keeper banner card for the wired backend:
// "Redis" on the redis stack, "File store" on the file one. Nil
// renders "State" — the card still degrades red, just unnamed.
func backendLabel(s store.Store) string {
	switch s.(type) {
	case *store.FileStore:
		return "File store"
	case *store.RedisStore:
		return "Redis"
	default:
		return "State"
	}
}

// sentinelSnapshot reads the live sentinel generation for the
// dashboard, bounded like every other console probe: a down
// registry or an unproven store renders unproven, never slow and
// never 500. Nil API renders unconfigured.
func (s *Server) sentinelSnapshot(ctx context.Context) sentinelData {
	d := sentinelData{Addr: sentinel.Repo + ":" + sentinel.Tag}
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
