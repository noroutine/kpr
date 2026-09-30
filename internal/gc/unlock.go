package gc

import (
	"context"
	"fmt"
	"io"
	"time"

	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// UnlockStore records the operator's write intent: the one method
// unlock needs. store.Store satisfies it; the use case declares
// only this.
type UnlockStore interface {
	SetUnlocked(ctx context.Context, unlocked bool) error
}

// Unlock proves the shared store with a fresh generation and records
// the intent to allow registry-store writes. Mode-agnostic: the
// proof lands on the mount and reads back through the API on
// writable and readonly registries alike. Anything unproven —
// unwritten or unreadable generation — refuses and the store stays
// locked: unlock on a stranger's (or no) store never opens writes.
func Unlock(ctx context.Context, w io.Writer, api sentinel.API, configPath string, st UnlockStore) error {
	root, err := StoreRoot(configPath)
	if err != nil {
		return err
	}
	gen, err := sentinel.NewGen()
	if err != nil {
		return err
	}
	payload := sentinel.Payload{V: 1, Gen: gen, TS: time.Now().UTC().Format(time.RFC3339), Writer: "kpr-unlock"}
	if _, err := sentinel.Write(root, sentinel.Repo, sentinel.Tag, payload); err != nil {
		return fmt.Errorf("sentinel generation unwritable under %s: %w", root, err)
	}
	if err := sentinel.Verify(ctx, api, sentinel.Repo, sentinel.Tag, gen); err != nil {
		return fmt.Errorf("kpr does not share this registry's store: %v", err)
	}
	if err := st.SetUnlocked(ctx, true); err != nil {
		return fmt.Errorf("proof held but the intent marker failed: %w", err)
	}
	_, err = fmt.Fprintf(w, "store unlocked: shared store proven via %s:%s generation %s\n", sentinel.Repo, sentinel.Tag, gen)
	return err
}
