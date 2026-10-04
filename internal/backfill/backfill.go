// Package backfill adopts pre-kpr tags into tracked rows: the
// receiver only sees pushes that land while kpr watches, so tags
// pushed before (or past) it need a one-shot import. Backfill
// reads tag-link mtimes off the shared mount (bytes, not names),
// gates per run on the served generation without minting, and
// records absent rows only — never clobbering receiver-known
// rows. The one risk it accepts is a restored generation
// (--accept-rollback); everything else refuses. Full story in
// docs/BACKFILL.md.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nrtn.dev/catalyst/kpr/internal/lineage"
	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

// ActorBackfill signs every backfilled row: the actor names the
// recording component, never the pusher (docs/ARCHITECTURE.md).
// Unknown-age rows default keep — backfill lands not-due.
const ActorBackfill = "kpr-backfill"

// SentinelPrefix bounds the machinery namespace: our sentinels
// live here, and enumeration skips the whole prefix
// (docs/BACKFILL.md). Exported so the catalog scan, the fs walk,
// and the store view split machinery the same way — three
// literals would drift, and the comparison would follow.
const SentinelPrefix = "noroutine/kpr-"

// Registry is the API surface backfill needs: enumerate repos,
// list tags, HEAD digests. *registry.Client satisfies it; tests
// stub it.
type Registry interface {
	CatalogAll(ctx context.Context) ([]string, error)
	Catalog(ctx context.Context, repo string) ([]string, error)
	ManifestDigest(ctx context.Context, repo, tag string) (digest, mediaType string, err error)
}

// Rows is the tracked state backfill checks absence against. The
// full store satisfies it; use cases declare only this.
type Rows interface {
	All(ctx context.Context) ([]policy.Row, error)
}

// Recorder stamps the absent rows. The full store satisfies it;
// use cases declare only this.
type Recorder interface {
	Record(ctx context.Context, r policy.Row) error
}

// Options tunes a backfill run: which repos, preview or armed.
// Log takes the per-tag stream (one line per verdict: would
// record/recorded, would skip/skipped with the reason); nil
// discards it. Progress reports the running summary on the
// tracked baseline, per listed repo, and per tag verdict — the
// caller throttles rendering; nil skips it.
type Options struct {
	RepoGlob string
	DryRun   bool
	Log      io.Writer
	Progress func(Summary)
}

// Summary counts a run: the adoptable baseline (tracked rows
// outside the sentinel prefix — what `store ls` shows), the
// sentinel rows apart, the scan so far (repos listed, tags
// named, tagless husks apart), then stamped, deliberately
// skipped, and error-skipped. Failed never refuses the run —
// catalog failures do that, loudly.
type Summary struct {
	Tracked   int
	Sentinels int
	Repos     int
	Tags      int
	Recorded  int
	Skipped   int
	Husks     int
	Failed    int
}

// Run gates on the served generation without minting — the verdict
// is classified with the caller's arming, so a dry run warns
// through a restored generation while an armed run refuses it
// unless rollback-accepted — then enumerates the catalog,
// digests every tag, and records the absent ones with their link
// mtimes, sentinel fossils capped at the floater's push. The
// floater itself never becomes a row. A locked store refuses with
// the unlock named; a stranger store refuses; tagless repos count
// as husks, never warn.
func Run(ctx context.Context, w io.Writer, api sentinel.API, reg Registry, rows Rows, rec Recorder, ids lineage.IdentityStore, lock proof.Locker, root string, opts Options, rollback proof.AcceptedRisk) (Summary, error) {
	var sum Summary
	log := opts.Log
	if log == nil {
		log = io.Discard
	}
	progress := func() {
		if opts.Progress != nil {
			opts.Progress(sum)
		}
	}
	// Intent opens the run: the marker read through the prover, so
	// a locked store refuses with the identical words as gc.
	if _, err := proof.ProveUnlockedStore(ctx, lock); err != nil {
		if errors.Is(err, proof.ErrLocked) {
			return sum, err
		}
		return sum, fmt.Errorf("store lock unreadable: %w", err)
	}
	now := time.Now().UTC()
	pay, rerr := sentinel.LastProof(ctx, api)
	allRows, err := rows.All(ctx)
	if err != nil {
		return sum, fmt.Errorf("tracked state unreadable: %w", err)
	}
	ident, err := ids.GetIdentity(ctx)
	if err != nil {
		return sum, fmt.Errorf("lineage unreadable: %w", err)
	}
	v := lineage.Judge(
		lineage.Served{Payload: pay, Err: rerr},
		lineage.Local{Ident: ident, Rows: allRows},
		lineage.Ask{DryRun: opts.DryRun, Force: rollback != nil, Now: now})
	if !v.Proceed {
		return sum, fmt.Errorf("%s — %s", v.Reason, v.Action)
	}
	if v.Stale {
		if _, err := fmt.Fprintf(w, "Warning: %s — %s\n", v.Reason, v.Action); err != nil {
			return sum, err
		}
	}
	tracked := map[string]bool{}
	for _, r := range allRows {
		tracked[r.Repo+"\x00"+r.Tag] = true
		if strings.HasPrefix(r.Repo, SentinelPrefix) {
			sum.Sentinels++
			continue
		}
		sum.Tracked++
	}
	progress()
	repos, err := reg.CatalogAll(ctx)
	if err != nil {
		return sum, fmt.Errorf("backfill enumeration: %w", err)
	}
	var matched []string
	for _, repo := range repos {
		if opts.RepoGlob != "" {
			ok, merr := policy.MatchImage(opts.RepoGlob, repo)
			if merr != nil || !ok {
				continue
			}
		}
		matched = append(matched, repo)
	}
	if opts.RepoGlob != "" && len(matched) == 0 {
		return sum, fmt.Errorf("backfill: no catalog repository matches %q", opts.RepoGlob)
	}
	for _, repo := range matched {
		tags, err := reg.Catalog(ctx, repo)
		if err != nil && !isNotFound(err) {
			return sum, fmt.Errorf("backfill tags for %s: %w", repo, err)
		}
		if len(tags) == 0 {
			// A 404 tags/list reads tagless, and so does an
			// empty list: nothing to adopt either way. Counted
			// apart and never warned per repo — but named in
			// the stream, or no grep ever finds it.
			sum.Husks++
			progress()
			verb := "skipped"
			if opts.DryRun {
				verb = "would skip"
			}
			if _, werr := fmt.Fprintf(log, "%s %s (husk: no tags)\n", verb, repo); werr != nil {
				return sum, werr
			}
			continue
		}
		sum.Repos++
		sum.Tags += len(tags)
		progress()
		// The floater caps sentinel rows: a fossil rewritten past
		// the live generation would otherwise outrank it in keep-N
		// ordering. No floater, no cap; an unreadable one warns
		// and runs uncapped.
		newest := time.Time{}
		if strings.HasPrefix(repo, SentinelPrefix) {
			lm, lerr := tagMtime(root, repo, sentinel.Tag)
			switch {
			case lerr == nil:
				newest = lm
			case os.IsNotExist(lerr):
			default:
				if _, werr := fmt.Fprintf(w, "Warning: %s:%s mtime unreadable (%v), sentinel rows uncapped\n", repo, sentinel.Tag, lerr); werr != nil {
					return sum, werr
				}
			}
		}
		// skip counts a deliberate skip and streams it: the
		// per-tag log names every verdict, not just records, so a
		// skip-heavy run stays greppable.
		skip := func(tag, reason string) error {
			sum.Skipped++
			progress()
			verb := "skipped"
			if opts.DryRun {
				verb = "would skip"
			}
			_, err := fmt.Fprintf(log, "%s %s:%s (%s)\n", verb, repo, tag, reason)
			return err
		}
		for _, tag := range tags {
			// The floater never becomes a row: it is a pointer,
			// not inventory, and "latest" wins every
			// newest-generation tiebreak by tag. Its mtime still
			// caps fossil rows above; only the row is skipped.
			if tag == sentinel.Tag && strings.HasPrefix(repo, SentinelPrefix) {
				if err := skip(tag, "floater"); err != nil {
					return sum, err
				}
				continue
			}
			if tracked[repo+"\x00"+tag] {
				if err := skip(tag, "tracked"); err != nil {
					return sum, err
				}
				continue
			}
			digest, mediaType, derr := reg.ManifestDigest(ctx, repo, tag)
			if derr != nil {
				sum.Failed++
				progress()
				if _, werr := fmt.Fprintf(w, "Warning: %s:%s digest unreadable (%v), skipping\n", repo, tag, derr); werr != nil {
					return sum, werr
				}
				continue
			}
			mtime, merr := tagMtime(root, repo, tag)
			if merr != nil || mtime.IsZero() {
				sum.Failed++
				progress()
				if _, werr := fmt.Fprintf(w, "Warning: %s:%s mtime unreadable (%v), skipping\n", repo, tag, merr); werr != nil {
					return sum, werr
				}
				continue
			}
			if !newest.IsZero() && mtime.After(newest) {
				mtime = newest
			}
			row := policy.Row{
				Repo: repo, Tag: tag,
				Digest: digest, MediaType: mediaType,
				PushedAt: mtime, Actor: ActorBackfill,
			}
			if opts.DryRun {
				sum.Recorded++
				progress()
				if _, werr := fmt.Fprintf(log, "would record %s:%s %s\n", repo, tag, digest); werr != nil {
					return sum, werr
				}
				continue
			}
			if err := rec.Record(ctx, row); err != nil {
				return sum, fmt.Errorf("backfill record %s:%s: %w", repo, tag, err)
			}
			sum.Recorded++
			progress()
			if _, werr := fmt.Fprintf(log, "recorded %s:%s %s\n", repo, tag, digest); werr != nil {
				return sum, werr
			}
		}
	}
	// No summary print: warnings own w, the caller renders the
	// counters from the returned Summary — one renderer, never a
	// live line plus a settled line saying the same.
	return sum, nil
}

// statusCoder classifies registry failures without importing the
// client: absence (404) skips by count, everything else refuses.
type statusCoder interface {
	StatusCode() int
}

func isNotFound(err error) bool {
	var sc statusCoder
	return errors.As(err, &sc) && sc.StatusCode() == 404
}

// tagMtime reads the tag's push time off the shared mount: the link
// file the registry rewrites on every push carries the push time in
// its mtime, under the driver's fixed docker/registry/v2 prefix. A
// missing link (tag without storage) errors — there are no
// zero-time rows.
func tagMtime(root, repo, tag string) (time.Time, error) {
	fi, err := os.Stat(filepath.Join(root, "docker", "registry", "v2",
		"repositories", repo, "_manifests", "tags", tag, "current", "link"))
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime().UTC(), nil
}
