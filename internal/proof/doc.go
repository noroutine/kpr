// Package proof owns evidence: values a stage takes as arguments
// so a call that cannot supply them cannot be constructed. The
// direction matters — use cases dictate, adapters provide: flags,
// env, and config are evidence sources at the boundary, and the
// signature decides what must exist.
//
// One file per proof kind: same_store.go proves identity,
// armed.go carries human intent to mutate, risk.go carries the
// accepted risk derived from that intent, checked.go carries the clock
// bound, unlocked.go carries the marker intent, fresh.go names
// the minted generation. Each file also holds the evaluation
// that mints it from adapter readings. Every evidence is
// sealed — only its constructors inhabit it, and the zero value
// is nil. Adapters report intent by calling a constructor; the
// package owns all evidence shapes.
package proof
