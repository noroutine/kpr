package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
)

// FileStore is a Store on per-row JSON files: no redis, no extra
// services beyond the state dir itself. Point the dir at
// `<registry-root>/kpr` and the registry root becomes a
// self-contained backup — store, rows, locks, and run state move as
// one unit, invisible to the collector and catalog walks (they only
// descend into `docker/registry/v2/repositories`).
//
// Crash-proofing is a write protocol, not a format: every mutation
// is an atomic filesystem op (temp-file rename, unlink, mkdir) and
// live files are never opened for writing, so readers see the old or
// the new version, never a torn one. Mutating ops serialize on a dir
// lock; named locks are flock'd files the kernel releases on holder
// death (strictly better than redis TTL expiry). Local volumes only:
// flock and fresh mtimes don't survive NFS (see docs/STORES.md).
type FileStore struct {
	dir string

	mu sync.Mutex
	// held lock files by name: the fd stays open for the hold, so
	// the kernel owns release on crash. Never unlinked — a new file
	// would be a new lock the old holder doesn't exclude.
	locks map[string]*os.File
}

// NewFileStore builds a file store rooted at dir (created on first
// use, Ping proves it writable).
func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir, locks: map[string]*os.File{}}
}

// Dir names the state root for operators (console, logs). The only
// backend detail the dashboard renders — rows/locks layout stays in
// docs/STORES.md.
func (s *FileStore) Dir() string { return s.dir }

func (s *FileStore) rowsDir() string     { return filepath.Join(s.dir, "rows") }
func (s *FileStore) locksDir() string    { return filepath.Join(s.dir, "locks") }
func (s *FileStore) currentFile() string { return filepath.Join(s.dir, "current.json") }

// unlockedFile is the intent marker: presence means `kpr store unlock`
// proved the shared store and opened it. Empty file — presence is
// the whole state, like the lock files it sits beside.
func (s *FileStore) unlockedFile() string { return filepath.Join(s.dir, "unlocked") }

// identityFile holds the lineage pairing as JSON: absent means the
// store never paired, unlike the unlocked marker where presence is
// the whole state.
func (s *FileStore) identityFile() string { return filepath.Join(s.dir, "identity.json") }
func (s *FileStore) activityFile() string {
	return filepath.Join(s.dir, "activity.json")
}

// esc maps one path element to a filename: readable, reversible,
// never a separator or a traversal.
func esc(el string) (string, error) {
	if el == "" || el == "." || el == ".." {
		return "", fmt.Errorf("filestore: bad path element %q", el)
	}
	return url.PathEscape(el), nil
}

// rowFile maps repo+tag to its row file, mirroring the registry
// hierarchy for readability: rows/noroutine/kpr-app/v1.json.
func (s *FileStore) rowFile(repo, tag string) (string, error) {
	if tag == "" {
		return "", fmt.Errorf("filestore: empty tag")
	}
	elems := []string{s.rowsDir()}
	for _, el := range strings.Split(repo, "/") {
		e, err := esc(el)
		if err != nil {
			return "", err
		}
		elems = append(elems, e)
	}
	etag, err := esc(tag)
	if err != nil {
		return "", err
	}
	return filepath.Join(append(elems, etag+".json")...), nil
}

// putFile writes data atomically: temp file in the same dir (same
// filesystem, so the rename can't cross devices), then rename over
// the target. Readers see old or new, never partial.
func putFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// ensureDir creates the dir, naming the op on failure: a bare
// PathError must never leave this package unexplained.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("filestore: mkdir %s: %w", dir, err)
	}
	return nil
}

// dirLock serializes mutating ops across processes sharing the dir.
// Held for milliseconds per op — never across calls — so blocking is
// fine; the kernel releases on holder death.
func (s *FileStore) dirLock() (func(), error) {
	if err := ensureDir(s.dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// Ping proves the dir exists and writable (banner red, sweeper
// skips): a probe file, not just a Stat.
func (s *FileStore) Ping(context.Context) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".ping-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_ = tmp.Close()
	return os.Remove(name)
}

func readRow(path string) (policy.Row, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return policy.Row{}, false, nil
		}
		return policy.Row{}, false, err
	}
	var r policy.Row
	if err := json.Unmarshal(raw, &r); err != nil {
		return policy.Row{}, false, fmt.Errorf("filestore: %s unparseable: %w", path, err)
	}
	return r, true, nil
}

func writeRow(path string, r policy.Row) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return putFile(path, raw)
}

// Record upserts with the shared rule: a re-notification of the same
// push preserves the due mark, a newer push restarts the promise.
func (s *FileStore) Record(_ context.Context, r policy.Row) error {
	path, err := s.rowFile(r.Repo, r.Tag)
	if err != nil {
		return err
	}
	unlock, err := s.dirLock()
	if err != nil {
		return err
	}
	defer unlock()
	old, ok, err := readRow(path)
	if err != nil {
		return err
	}
	if ok && !r.PushedAt.After(old.PushedAt) {
		r.Due, r.Reason = old.Due, old.Reason
	}
	return writeRow(path, r)
}

// collect scans every row file: unparseable files refuse loudly with
// the path (foreign garbage or disk trouble — never silent), while
// non-JSON files (operator READMEs) are skipped.
func (s *FileStore) collect() ([]policy.Row, error) {
	var out []policy.Row
	err := filepath.WalkDir(s.rowsDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		r, _, err := readRow(path)
		if err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *FileStore) All(context.Context) ([]policy.Row, error) {
	return s.collect()
}

func (s *FileStore) Due(context.Context) ([]policy.Row, error) {
	rows, err := s.collect()
	if err != nil {
		return nil, err
	}
	var out []policy.Row
	for _, r := range rows {
		if r.Due {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *FileStore) MarkDue(_ context.Context, repo, tag, reason string) error {
	path, err := s.rowFile(repo, tag)
	if err != nil {
		return err
	}
	unlock, err := s.dirLock()
	if err != nil {
		return err
	}
	defer unlock()
	r, _, err := readRow(path)
	if err != nil {
		return err
	}
	r.Repo, r.Tag = repo, tag
	r.Due, r.Reason = true, reason
	return writeRow(path, r)
}

func (s *FileStore) ClearDue(context.Context) (int, error) {
	unlock, err := s.dirLock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	rows, err := s.collect()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if !r.Due {
			continue
		}
		r.Due, r.Reason = false, ""
		path, err := s.rowFile(r.Repo, r.Tag)
		if err != nil {
			return n, err
		}
		if err := writeRow(path, r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *FileStore) UnmarkDue(_ context.Context, repo, tag string) (bool, error) {
	path, err := s.rowFile(repo, tag)
	if err != nil {
		return false, err
	}
	unlock, err := s.dirLock()
	if err != nil {
		return false, err
	}
	defer unlock()
	r, ok, err := readRow(path)
	if err != nil {
		return false, err
	}
	if !ok || !r.Due {
		return false, nil
	}
	r.Due, r.Reason = false, ""
	return true, writeRow(path, r)
}

func (s *FileStore) Delete(_ context.Context, repo, tag string) error {
	path, err := s.rowFile(repo, tag)
	if err != nil {
		return err
	}
	unlock, err := s.dirLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *FileStore) SetCurrent(_ context.Context, c Current) error {
	unlock, err := s.dirLock()
	if err != nil {
		return err
	}
	defer unlock()
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return putFile(s.currentFile(), raw)
}

func (s *FileStore) GetCurrent(context.Context) (Current, error) {
	raw, err := os.ReadFile(s.currentFile())
	if err != nil {
		if os.IsNotExist(err) {
			return Current{}, nil
		}
		return Current{}, err
	}
	var c Current
	if err := json.Unmarshal(raw, &c); err != nil {
		return Current{}, fmt.Errorf("filestore: current.json unparseable: %w", err)
	}
	return c, nil
}

func (s *FileStore) PushActivity(_ context.Context, o Outcome) error {
	unlock, err := s.dirLock()
	if err != nil {
		return err
	}
	defer unlock()
	activity, err := s.Activity(context.Background())
	if err != nil {
		return err
	}
	activity = append([]Outcome{o}, activity...)
	// NOTE(mutants): >= is equivalent — trimming an exactly-cap ring
	// is identity, and append reaches exactly-cap only from below.
	if len(activity) > ActivityCap {
		activity = activity[:ActivityCap]
	}
	raw, err := json.Marshal(activity)
	if err != nil {
		return err
	}
	return putFile(s.activityFile(), raw)
}

func (s *FileStore) Activity(context.Context) ([]Outcome, error) {
	raw, err := os.ReadFile(s.activityFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Outcome
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("filestore: activity.json unparseable: %w", err)
	}
	return out, nil
}

// lockClaim is the JSON a held lock file carries: who and since, for
// the operator with cat. Truth stays with the flock, never the file —
// a leftover claim without the lock means nothing.
type lockClaim struct {
	Holder string    `json:"holder"`
	Since  time.Time `json:"since"`
}

func holder() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// AcquireLock takes the named single-flight lock, non-blocking: held
// elsewhere reads false, never waits. The ttl is accepted and
// ignored — the kernel releases on holder death, which bounds every
// hold better than an expiry. Lock files are never unlinked: a new
// file would be a new lock the old holder doesn't exclude.
func (s *FileStore) AcquireLock(_ context.Context, name string, _ time.Duration) (bool, error) {
	if err := os.MkdirAll(s.locksDir(), 0o755); err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(s.locksDir(), name+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return false, nil
	}
	claim, err := json.Marshal(lockClaim{Holder: holder(), Since: time.Now().UTC()})
	if err != nil {
		_ = f.Close()
		return false, err
	}
	// Truncate first: a shorter claim over a longer one would leave
	// trailing bytes confusing the operator's cat.
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return false, err
	}
	if _, err := f.WriteAt(claim, 0); err != nil {
		_ = f.Close()
		return false, err
	}
	s.mu.Lock()
	if old, ok := s.locks[name]; ok {
		_ = old.Close()
	}
	s.locks[name] = f
	s.mu.Unlock()
	return true, nil
}

// ReleaseLock drops the named lock by closing the held file. Unknown
// names are a no-op; a release failure mode doesn't exist (close
// errors are swallowed — the kernel already released).
func (s *FileStore) ReleaseLock(_ context.Context, name string) error {
	s.mu.Lock()
	f, ok := s.locks[name]
	if ok {
		delete(s.locks, name)
	}
	s.mu.Unlock()
	if ok {
		_ = f.Close()
	}
	return nil
}

// IsUnlocked stats the intent marker: missing (fresh stores
// included) reads locked. A stat failure other than not-exist
// refuses, never guesses.
func (s *FileStore) IsUnlocked(_ context.Context) (bool, error) {
	_, err := os.Stat(s.unlockedFile())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// SetUnlocked creates or drops the marker. Unlock ensures the dir
// (Ping may never have run); lock removes a missing marker as a
// no-op, so double-lock stays quiet.
func (s *FileStore) SetUnlocked(_ context.Context, unlocked bool) error {
	if !unlocked {
		if err := os.Remove(s.unlockedFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return putFile(s.unlockedFile(), []byte{})
}

// GetIdentity reads the pairing: missing file (fresh stores
// included) is unpaired. A read failure other than not-exist, or
// unparseable JSON, refuses — a pairing nobody can read is no
// pairing to compare against.
func (s *FileStore) GetIdentity(_ context.Context) (Identity, error) {
	raw, err := os.ReadFile(s.identityFile())
	if err != nil {
		if os.IsNotExist(err) {
			return Identity{}, nil
		}
		return Identity{}, err
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return Identity{}, fmt.Errorf("filestore: identity.json unparseable: %w", err)
	}
	return id, nil
}

// SetIdentity writes the pairing outright, ensuring the dir like
// the marker does.
func (s *FileStore) SetIdentity(_ context.Context, id Identity) error {
	raw, err := json.Marshal(id)
	if err != nil {
		return err
	}
	return putFile(s.identityFile(), raw)
}

// Close releases every held lock file: clean shutdown hands nothing
// to the kernel. State files need no closing — each op is complete
// on return.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, f := range s.locks {
		_ = f.Close()
		delete(s.locks, name)
	}
	return nil
}
