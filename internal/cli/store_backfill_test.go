package cli

import (
	"bytes"
	"strings"
	"testing"
)

// The backfill command exposes its contract on the help screen:
// the glob positional, preview-by-default arming, the config it
// resolves the mount from, and the one accepted risk. If this
// fails, the flags drifted from backfill.Run.
func TestStoreBackfillHelpNamesContract(t *testing.T) {
	var buf bytes.Buffer
	RootCmd.SetOut(&buf)
	defer RootCmd.SetOut(nil)
	RootCmd.SetArgs([]string{"store", "backfill", "--help"})
	defer RootCmd.SetArgs(nil)
	Execute()
	out := buf.String()
	for _, want := range []string{"repo-glob", "no-dry-run", "accept-rollback", "config"} {
		if !strings.Contains(out, want) {
			t.Errorf("backfill help omits %q", want)
		}
	}
}
