package cli

import (
	"context"
	"strings"
	"testing"

	"nrtn.dev/catalyst/kpr/internal/cli/deps"
	"nrtn.dev/catalyst/kpr/internal/config"
)

// Adopt bootstraps the pairing onto silence: a fresh store facing
// an empty registry pairs to the pinned IDENT without minting. If
// this fails, the explicit ceremony cannot onboard a fresh deploy.
func TestAdoptBootstrapsSilence(t *testing.T) {
	regRoot := t.TempDir()
	srv := serveRegistry(t, regRoot, true)
	defer srv.Close()
	dir := t.TempDir()
	ident := "0193abcd-0000-7000-8000-0000000000aa"
	t.Setenv(config.EnvCLINoDryRun, "true")
	out, err := runCmdWithArgs(t, dir, srv.URL, adoptCmd, []string{ident})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !strings.Contains(out, "paired to "+ident) {
		t.Errorf("adopt output lacks the pairing:\n%s", out)
	}
	if !strings.Contains(out, "kpr store unlock") {
		t.Errorf("adopt output lacks the unlock follow-up:\n%s", out)
	}
	backend, storeDir := resolveTestBackend(t)
	s, err := deps.OpenStore(config.NewBuilder().FromEnv().Build(), backend, storeDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()
	paired, err := s.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if paired.ID != ident {
		t.Errorf("paired = %q, want the pinned %q", paired.ID, ident)
	}
}

// Adopt previews by default: no flag, no env, no pairing — the
// would-tense names the pairing and the store stays unpaired. If
// this fails, an unwired flag arms instead of previewing.
func TestAdoptPreviewsByDefault(t *testing.T) {
	regRoot := t.TempDir()
	srv := serveRegistry(t, regRoot, true)
	defer srv.Close()
	dir := t.TempDir()
	ident := "0193abcd-0000-7000-8000-0000000000ab"
	out, err := runCmdWithArgs(t, dir, srv.URL, adoptCmd, []string{ident})
	if err != nil {
		t.Fatalf("adopt preview: %v", err)
	}
	if !strings.Contains(out, "would pair to "+ident) {
		t.Errorf("adopt preview lacks the would-pairing:\n%s", out)
	}
	backend, storeDir := resolveTestBackend(t)
	s, err := deps.OpenStore(config.NewBuilder().FromEnv().Build(), backend, storeDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()
	paired, err := s.GetIdentity(context.Background())
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if paired.ID != "" {
		t.Errorf("preview paired to %q, want unpaired", paired.ID)
	}
}
