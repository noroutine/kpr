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
