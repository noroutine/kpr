package proof

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// FilesystemStore clears local store walks: the registry config
// carries a filesystem storage root, so tag links, revisions, and
// blobs are bytes on this mount — not s3 keys, not somebody
// else's disk. Sealed like every evidence; nil never clears. The
// proven root rides inside the token (Root), so a walk reads the
// path it proved, never a second parse beside it.
type FilesystemStore interface {
	// Root is the storage root the token was proven for.
	Root() string

	sealed()
}

type filesystemStore struct {
	root string
}

func (p filesystemStore) Root() string { return p.root }
func (filesystemStore) sealed()        {}

// ProveFilesystemStore reads the storage driver the registry was
// configured with and mints only for a filesystem root. Anything
// else — s3 and friends, a filesystem key with no root, garbage,
// absent file — refuses with the remedy: local walks only
// understand the shared directory layout.
func ProveFilesystemStore(configPath string) (FilesystemStore, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("prove filesystem store: read %s: %w", configPath, err)
	}
	var cfg struct {
		Storage struct {
			Filesystem struct {
				RootDirectory string `yaml:"rootdirectory"`
			} `yaml:"filesystem"`
		} `yaml:"storage"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("prove filesystem store: parse %s: %w", configPath, err)
	}
	if cfg.Storage.Filesystem.RootDirectory == "" {
		return nil, fmt.Errorf("prove filesystem store: no filesystem storage root in %s: local walks need the shared directory layout", configPath)
	}
	return filesystemStore{root: cfg.Storage.Filesystem.RootDirectory}, nil
}
