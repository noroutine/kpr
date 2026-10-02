package gc

import (
	"context"
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
// listens. The cache endpoint parses out of the same config the
// collector reads.
func onlinePreflight(ctx context.Context, configPath, edgeAddr string, leaseReady bool, cacheAccept, fenceAccept proof.AcceptedRisk) (proof.BlobCacheOff, proof.GatewayFencing, string, error) {
	// A parse failure is broken input, not a missing proof: no
	// checklist, no override — acceptance cannot fix unreadable.
	cacheAddr, _, _, cerr := registryRedis(configPath)
	if cerr != nil {
		return nil, nil, "", fmt.Errorf("blob cache unreadable: %v (gc reads cache.redis out of %s)", cerr, configPath)
	}
	cache, cacheErr := proof.ProveBlobCacheOff(cacheAddr, cacheAccept)
	fence, fenceErr := proof.ProveGatewayFencing(ctx, configPath, edgeAddr, leaseReady, fenceAccept)

	var lines []string
	if cacheErr == nil {
		lines = append(lines, "[ok] blob cache: none configured (deletes reclaim immediately)")
	} else {
		lines = append(lines, "[miss] blob cache: "+cacheErr.Error())
	}
	if fenceErr == nil {
		lines = append(lines, "[ok] gateway: proven edge on "+edgeAddr+" (HOLD will pin pushes)")
	} else {
		lines = append(lines, "[miss] gateway: "+fenceErr.Error())
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
