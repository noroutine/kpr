package keeper

import (
	"context"
)

// RegistryCard voices the distribution registry the keeper
// probes: reachability with the probed endpoint. A down
// registry still names its URL — unreachable says where, not
// just what. Standing is decided here, at assembly — renderers
// only translate it.
type RegistryCard struct {
	Standing  Standing `json:"standing"`
	URL       string   `json:"url"`
	Reachable bool     `json:"reachable"`
}

// FetchRegistryCard probes the registry through the Prober
// port and voices the card for the endpoint it probed. Nil
// prober reads unavailable, never a probe that never ran.
func FetchRegistryCard(ctx context.Context, reg Prober, url string) RegistryCard {
	if reg == nil {
		return RegistryCard{Standing: StandingUnavailable, URL: url}
	}
	if err := reg.Reachable(ctx); err != nil {
		return RegistryCard{Standing: StandingUnreachable, URL: url}
	}
	return RegistryCard{Standing: StandingOK, URL: url, Reachable: true}
}

// NewRegistryCard voices the card for an already-probed
// endpoint: the reachability an earlier probe established,
// without probing twice. FetchStatus already probes for its
// RegistryOK, so its renderers reuse that answer here — one
// probe per render, never two. Use FetchRegistryCard when the
// probe itself is the point.
func NewRegistryCard(reg Prober, reachable bool, url string) RegistryCard {
	if reg == nil {
		return RegistryCard{Standing: StandingUnavailable, URL: url}
	}
	if !reachable {
		return RegistryCard{Standing: StandingUnreachable, URL: url}
	}
	return RegistryCard{Standing: StandingOK, URL: url, Reachable: true}
}
