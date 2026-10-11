package keeper

import (
	"fmt"
	"time"

	"nrtn.dev/catalyst/kpr/internal/helpers/human"
)

// ActivityTail is how many ring records a text view shows: the
// console and the CLI cap the same last-10 tail, said out loud.
// Moved here with the formatter so both caps read one const.
const ActivityTail = 10

// FormatOutcome prints one ring entry for either renderer: rows
// as repo:tag — outcome, row-less control events (fence flips)
// as the outcome alone, each with a human age (precise stamps
// stay in --json). A stray ": — " prefix reads as a malformed
// row. Padded voices the CLI's terminal indent; the card has
// its own padding.
func FormatOutcome(now time.Time, a ActivityEntry, padded bool) string {
	age := "unknown age"
	if !a.At.IsZero() {
		age = human.ShortAge(now, a.At)
	}
	pad := ""
	if padded {
		pad = "  "
	}
	if a.Repo == "" && a.Tag == "" {
		return fmt.Sprintf("%s%s (%s), %s", pad, a.Outcome, a.Reason, age)
	}
	return fmt.Sprintf("%s%s:%s — %s (%s), %s", pad, a.Repo, a.Tag, a.Outcome, a.Reason, age)
}
