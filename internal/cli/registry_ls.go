package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"nrtn.dev/catalyst/kpr/internal/config"
	"nrtn.dev/catalyst/kpr/internal/helpers/human"
	"nrtn.dev/catalyst/kpr/internal/proof"
	"nrtn.dev/catalyst/kpr/internal/registry"
	"nrtn.dev/catalyst/kpr/internal/registryfs"
	"nrtn.dev/catalyst/kpr/internal/sentinel"
)

var registryLsJSON, registryLsLong bool

var registryLsCmd = &cobra.Command{
	Use:   "ls [sentinels|husks]",
	Short: "List machinery tags or tagless repos as the registry sees them",
	Long: `Two registry-side listings. sentinels is the API view of
machinery: every tag under the sentinel repo with the identity
its manifest carries — generation, age, writer. The store view
(` + "`store ls sentinels`" + `) shows what kpr tracks; diffing the
two names unadopted tags and stale rows. Tag payloads only mean
something for machinery, so sentinels is the one API target. A
tag whose manifest won't parse warns past on stderr and skips —
one dangling tag never vetoes the listing. Pure API read: no
store, no proof.

husks is the filesystem view of tagless repos: one name per
line, sorted, sentinel machinery excluded — the fast answer when
analyze only counts them. A filestore proof gates it, like
analyze; --json emits the array, --long refuses (names only).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.NewBuilder().FromEnv().Build()
		if args[0] == "husks" {
			return runRegistryLsHusks(cmd.OutOrStdout(), cfg.RegistryConfig, registryLsJSON, registryLsLong)
		}
		reg := registry.NewClient(cfg.RegistryURL)
		reg.SetBasicAuth(cfg.RegistryUser, cfg.RegistryPassword)
		return runRegistryLs(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
			reg, args[0], time.Now().UTC(), registryLsJSON, registryLsLong)
	},
}

// runRegistryLsHusks names tagless repos off the proven mount, one
// per line, sorted. --json emits the array for scripts; --long
// refuses loud — a husk has no tags to detail.
func runRegistryLsHusks(out io.Writer, configPath string, asJSON, long bool) error {
	if long {
		return fmt.Errorf("registry ls husks has no --long view: names only, --json for scripts")
	}
	fsStore, err := proof.ProveFilesystemStore(configPath)
	if err != nil {
		return err
	}
	names, err := registryfs.ListHusks(fsStore)
	if err != nil {
		return err
	}
	if asJSON {
		if names == nil {
			names = []string{}
		}
		return json.NewEncoder(out).Encode(names)
	}
	for _, name := range names {
		if _, err := fmt.Fprintln(out, name); err != nil {
			return err
		}
	}
	return nil
}

// lsRegistry is the API surface listing needs: enumerate one
// repo's tags, read manifests and blobs behind them.
// *registry.Client satisfies it; tests stub it.
type lsRegistry interface {
	Catalog(ctx context.Context, repo string) ([]string, error)
	sentinel.API
}

// lsRow is one evaluated tag: the payload identity plus the
// manifest digest for joins against `store ls --long`.
type lsRow struct {
	Tag    string `json:"tag"`
	Gen    string `json:"gen"`
	ID     string `json:"id"`
	TS     string `json:"ts"`
	Writer string `json:"writer"`
	Digest string `json:"digest"`
}

// runRegistryLs evaluates every tag the target names. Sorted for
// stable reads; warnings own errW so --json stays pure data.
func runRegistryLs(ctx context.Context, out, errW io.Writer, reg lsRegistry, target string, now time.Time, asJSON, long bool) error {
	if target != "sentinels" {
		return fmt.Errorf("registry ls supports sentinels, husks: tag payloads only mean something for machinery")
	}
	tags, err := reg.Catalog(ctx, sentinel.Repo)
	if err != nil {
		return err
	}
	sort.Strings(tags)
	rows := make([]lsRow, 0, len(tags))
	for _, tag := range tags {
		p, digest, rerr := sentinel.Read(ctx, reg, sentinel.Repo, tag)
		if rerr != nil {
			if _, werr := fmt.Fprintf(errW, "Warning: %s:%s unreadable (%v), skipping\n", sentinel.Repo, tag, rerr); werr != nil {
				return werr
			}
			continue
		}
		rows = append(rows, lsRow{Tag: tag, Gen: p.Gen, ID: p.ID, TS: p.TS, Writer: p.Writer, Digest: digest})
	}
	if asJSON {
		outRows := make([]struct {
			Repo string `json:"repo"`
			lsRow
		}, 0, len(rows))
		for _, r := range rows {
			outRows = append(outRows, struct {
				Repo string `json:"repo"`
				lsRow
			}{Repo: sentinel.Repo, lsRow: r})
		}
		return json.NewEncoder(out).Encode(outRows)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if long {
		if _, err := fmt.Fprintln(tw, "REPO:TAG\tGEN\tID\tDIGEST\tPUSHED\tWRITER"); err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := fmt.Fprintf(tw, "%s:%s\t%s\t%s\t%s\t%s\t%s\n",
				sentinel.Repo, r.Tag, r.Gen, r.ID, r.Digest, r.TS, r.Writer); err != nil {
				return err
			}
		}
		return tw.Flush()
	}
	if _, err := fmt.Fprintln(tw, "REPO:TAG\tAGE\tWRITER"); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s:%s\t%s\t%s\n",
			sentinel.Repo, r.Tag, lsAge(now, r.TS), r.Writer); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// lsAge renders a payload stamp the way `store ls` renders push
// times. An unparseable stamp reads as unknown, never as a
// million-hour age.
func lsAge(now time.Time, ts string) string {
	stamp, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "unknown ts"
	}
	return human.ShortAge(now, stamp)
}

func init() {
	registryLsCmd.Flags().BoolVar(&registryLsJSON, "json", false, "Emit the listing as JSON for scripts")
	registryLsCmd.Flags().BoolVar(&registryLsLong, "long", false, "Render digest and identity columns")
	registryCmd.AddCommand(registryLsCmd)
}
