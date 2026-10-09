package gc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"nrtn.dev/catalyst/kpr/internal/event"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// writeVerifiedGeneration is the shared tail of every mint ceremony
// (armed gc runs, unlock): one generation written and read back,
// refusing identically everywhere — one funnel, no copies. Returns
// the manifest digest for keep-N. The future FreshGeneration token
// is minted here, beside the Write and Verify it names; until its
// first consumer arrives it stays parked, not faked.
func writeVerifiedGeneration(ctx context.Context, api sentinel.API, root string, payload sentinel.Payload) (string, error) {
	md, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, payload)
	if err != nil {
		return "", fmt.Errorf("sentinel generation unwritable under %s: %w", root, err)
	}
	if err := sentinel.Verify(ctx, api, sentinel.Repo, sentinel.Tag, payload.Gen); err != nil {
		return "", fmt.Errorf("kpr does not share this registry's store: %v", err)
	}
	return md, nil
}

// collectWritableArmed runs the collector for real against a
// writable registry: the preflight clearance is demanded at the
// delete boundary, not just at the pre-mint gate (a run that
// arrives here without it refuses instead of collecting blind).
// Previews and readonly runs take the bare collect — only the
// writable-armed path carries the extra demand, which is why only
// it has a variant. Either token nil means the preflight never
// cleared (or was bypassed): refuse, naming the gate that owns
// the override.
func collectWritableArmed(ctx context.Context, out io.Writer, collect Collector, binPath string, args []string, report event.Reporter, cache proof.BlobCacheOff, gating proof.GatewayFencingAvailable) error {
	if cache == nil {
		return errors.New("online clearance missing for the blob cache: pass the online preflight (or re-run with --accept-blob-cache)")
	}
	if gating == nil {
		return errors.New("online clearance missing for the gateway: pass the online preflight (or re-run with --accept-unfenced)")
	}
	return collect(ctx, out, binPath, args, report)
}

// ready infers at runtime whether this container can collect at all:
// the stock binary and the registry config it reads store paths from
// must both exist. Anything missing refuses with the remedy — a bare
// image (no shared mounts) explains itself instead of failing mid-run.
func ready(binPath, configPath string) error {
	if st, err := os.Stat(binPath); err != nil || st.IsDir() {
		return fmt.Errorf("gc unavailable: registry binary not found at %s (image must COPY it from the registry image)", binPath)
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("gc unavailable: registry config not found at %s (mount the registry config here, see KPR_REGISTRY_CONFIG)", configPath)
	}
	return nil
}
