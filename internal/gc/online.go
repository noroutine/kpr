package gc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nrtn.dev/catalyst/kpr/internal/proof"
)

// onlinePreflight evaluates the writable-run preconditions: the
// blob cache off (or accepted) and the gateway fencing (or
// accepted). Every check runs every time — the refusal names ALL
// misses with their remedies and overrides, never one at a time,
// and the dry-run preview prints the same checklist as
// information. Tokens thread to the delete boundary; a miss
// without its acceptance leaves its token nil.
//
// Lease and edge come from the run's own wiring: leaseReady is
// whether a HOLD fence was configured (file backend shares the
// lease dir; redis keeps none), edgeAddr is where the edge
// listens. The cache prover reads the same config the collector
// reads and judges both halves itself — no pre-parsed verdict
// crosses into the proof package.
func onlinePreflight(ctx context.Context, configPath, edgeAddr string, leaseReady bool, cacheAccept, fenceAccept proof.AcceptedRisk) (proof.BlobCacheOff, proof.GatewayFencing, string, error) {
	// World first, acceptance second: the report must tell
	// proven from accepted — an override that also proves reads
	// [ok], an override that waives reads [accepted] naming what
	// was waived. The accepting calls re-run the provers (pure
	// for cache; at most one more dial for the gateway, only on
	// override runs).
	_, worldCacheErr := proof.ProveBlobCacheOff(configPath, nil)
	_, worldFenceErr := proof.ProveGatewayFencing(ctx, configPath, edgeAddr, leaseReady, nil)
	cache, cacheErr := proof.ProveBlobCacheOff(configPath, cacheAccept)
	fence, fenceErr := proof.ProveGatewayFencing(ctx, configPath, edgeAddr, leaseReady, fenceAccept)

	// An unreadable config is broken input, not a missing proof:
	// no checklist, no override — acceptance cannot fix
	// unreadable. The prover marks it Unreadable so this err
	// branch routes outside the per-risk report.
	var unreadable proof.Unreadable
	if errors.As(cacheErr, &unreadable) {
		return nil, nil, "", fmt.Errorf("blob cache unreadable: %v (gc reads redis: and storage.cache out of %s)", unreadable.Err, configPath)
	}
	var lines []string
	if cacheErr != nil {
		lines = append(lines, "[miss] blob cache: "+cacheErr.Error())
	} else if worldCacheErr != nil {
		lines = append(lines, "[accepted] blob cache: "+worldCacheErr.Error())
	} else {
		lines = append(lines, "[ok] blob cache: none configured (deletes reclaim immediately)")
	}
	if fenceErr != nil {
		lines = append(lines, "[miss] gateway: "+fenceErr.Error())
	} else if worldFenceErr != nil {
		lines = append(lines, "[accepted] gateway: edge on "+edgeAddr+" unproven or silent, HOLD unwatched (--accept-unfenced)")
	} else {
		lines = append(lines, "[ok] gateway: proven edge on "+edgeAddr+" (HOLD will pin pushes)")
	}
	report := "gc online preflight (registry serving — armed collect runs under the fence):\n  " + strings.Join(lines, "\n  ")
	if cacheErr != nil || fenceErr != nil {
		flags := []string{}
		if cacheErr != nil {
			flags = append(flags, "--accept-blob-cache")
		}
		if fenceErr != nil {
			flags = append(flags, "--accept-unfenced")
		}
		return nil, nil, report, fmt.Errorf("%s\nrefusing: re-run with %s to override (each flag names the risk it accepts)",
			report, strings.Join(flags, " "))
	}
	return cache, fence, report, nil
}
