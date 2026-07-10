// Package profile manages the per-account directories ccc mounts as the
// container's Claude Code state.
//
// Claude Code splits its state across two host paths: ~/.claude (credentials,
// CLAUDE.md, agents, projects) and ~/.claude.json (project registry, MCP
// state). A profile owns BOTH, which is why swapping profiles is total —
// symlinking ~/.claude alone leaves ~/.claude.json shared between accounts.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/lestrrat-go/ccc/internal/config"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ErrNotExist reports a missing profile.
var ErrNotExist = errors.New("profile does not exist")

// Store is the profiles/ directory under the ccc config root.
type Store struct {
	root string
	home string
}

// NewStore returns the profile store rooted at <cccRoot>/profiles.
func NewStore(cccRoot string, home string) *Store {
	return &Store{root: filepath.Join(cccRoot, "profiles"), home: home}
}

// Dir is the profile's own directory.
func (s *Store) Dir(name string) string { return filepath.Join(s.root, name) }

// ClaudeDir is mounted at $HOME/.claude in the container.
func (s *Store) ClaudeDir(name string) string { return filepath.Join(s.Dir(name), "claude") }

// ClaudeJSON is mounted at $HOME/.claude.json in the container.
func (s *Store) ClaudeJSON(name string) string { return filepath.Join(s.Dir(name), "claude.json") }

// ConfigPath is the profile's profile.json.
func (s *Store) ConfigPath(name string) string { return filepath.Join(s.Dir(name), "profile.json") }

// CacheDir is profile-owned build-cache storage, mounted at the container's
// ~/.cache when mounts.cache is enabled. Never the host's cache directory.
func (s *Store) CacheDir(name string) string { return filepath.Join(s.Dir(name), "cache") }

// VersionFile is the Claude Code pin, inside the profile's mounted claude/ dir.
const VersionFile = ".ccc-claude-version"

// VersionPath is the profile's Claude Code pin.
func (s *Store) VersionPath(name string) string {
	return filepath.Join(s.ClaudeDir(name), VersionFile)
}

// ClaudeVersion reads the profile's Claude Code pin. Returns "" when unpinned.
//
// This file lives in the profile's claude/ directory, which is mounted
// read-write into the container — so the contained process can write it. Its
// contents are validated here, before they ever reach a build arg. A malformed
// pin is a hard error, never a silently-ignored value and never a shell string.
func (s *Store) ClaudeVersion(name string) (string, error) {
	path := s.VersionPath(name)

	b, err := config.ReadStateFile(path)
	if err != nil || b == nil {
		return "", err
	}

	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", nil
	}
	if err := config.ValidateClaudeVersion(v); err != nil {
		return "", fmt.Errorf("%s: %w\nrepair it with `ccc -p %s pin` or delete the file", path, err, name)
	}
	return v, nil
}

// UpdateResultFile is where Claude Code records its last self-update attempt.
// It lives inside the profile's claude/ directory, so it is per-profile.
const UpdateResultFile = ".last-update-result.json"

// updateResult is the subset of .last-update-result.json that ccc reads.
type updateResult struct {
	Outcome   string `json:"outcome"`
	Status    string `json:"status"`
	VersionTo string `json:"version_to"`
}

// UpdateResultPath is the profile's Claude Code update record.
func (s *Store) UpdateResultPath(name string) string {
	return filepath.Join(s.ClaudeDir(name), UpdateResultFile)
}

// RequestedClaudeVersion returns the version the container's Claude Code tried
// to install and could not, or "" when there is nothing to act on.
//
// Inside the container Claude Code is installed under root-owned /usr/local, so
// its self-update always fails with no_permissions — and records the version it
// wanted in version_to. ccc reads that on the host and rebuilds. The container
// says what it wants; only the host can act on it.
//
// This file is written by Claude Code, inside a directory mounted read-write
// into the container. A malformed or hostile value is ignored rather than
// fatal: it is not ccc's file, and a corrupt one must not brick every run.
// Callers must still check IsNewerClaudeVersion before adopting the result —
// seeding a profile with `--from ~/.claude` copies the host's record, which can
// name an older version than the profile is pinned to.
func (s *Store) RequestedClaudeVersion(name string) (string, error) {
	path := s.UpdateResultPath(name)

	// This file is Claude Code's, not ccc's. A corrupt, oversized, or FIFO
	// update record must never brick a run — ignore any read/parse trouble and
	// treat it as "no pending upgrade" (the same tolerance as the parse below).
	b, err := config.ReadStateFile(path)
	if err != nil || b == nil {
		return "", nil
	}

	var r updateResult
	if err := json.Unmarshal(b, &r); err != nil {
		return "", nil // Claude Code's file, not ours: ignore what we cannot parse.
	}
	// The signal is specifically "the container tried to update and could not"
	// — inside the container /usr/local is root-owned, so its updater always
	// fails and records version_to. A *successful* record (e.g. the host's own,
	// copied in by `--from ~/.claude`) is not a request to rebuild.
	if r.Outcome != "failed" {
		return "", nil
	}
	if r.VersionTo == "" {
		return "", nil
	}
	if err := config.ValidateClaudeVersion(r.VersionTo); err != nil {
		return "", nil
	}
	if r.VersionTo == config.LatestClaudeVersion {
		return "", nil // a dist-tag is not a pin
	}
	return r.VersionTo, nil
}

// SetClaudeVersion writes the profile's Claude Code pin.
func (s *Store) SetClaudeVersion(name string, version string) error {
	if err := config.ValidateClaudeVersion(version); err != nil {
		return err
	}
	if err := s.Materialize(name); err != nil {
		return err
	}
	return config.WriteAtomic(s.VersionPath(name), []byte(version+"\n"), 0o600)
}

// Config loads the profile's profile.json.
func (s *Store) Config(name string) (*config.Profile, error) {
	return config.LoadProfile(s.ConfigPath(name), s.home)
}

// Exists reports whether the profile directory is present.
func (s *Store) Exists(name string) bool {
	fi, err := os.Stat(s.Dir(name))
	return err == nil && fi.IsDir()
}

// List returns profile names in lexical order.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", s.root, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Create makes an empty profile. Materialize prepares its mount targets.
func (s *Store) Create(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if s.Exists(name) {
		return fmt.Errorf("profile %q already exists", name)
	}
	return s.Materialize(name)
}

// Materialize ensures the profile's mount sources exist. claude.json must be a
// regular file before it is bind-mounted, otherwise the runtime creates a
// directory in its place.
//
// ValidateName is the choke point for the path-creating operations: Create,
// SetClaudeVersion, and Seed all funnel through here, so an unvalidated `name`
// with `..` segments cannot MkdirAll/write outside profiles/.
func (s *Store) Materialize(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	// The store root is ccc-owned; make it the trusted root for the symlink guard
	// below, which needs it to exist as a real directory before it can walk down.
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return fmt.Errorf("failed to create profile store: %w", err)
	}
	// Refuse a symlinked mount source BEFORE creating anything: the same guard
	// `ccc check` runs, so check and run agree on whether the profile is safe.
	if err := s.ValidateMountSources(name); err != nil {
		return err
	}
	claudeDir := s.ClaudeDir(name)
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return fmt.Errorf("failed to create profile dir: %w", err)
	}

	path := s.ClaudeJSON(name)
	// ValidateMountSources already lstat'd claude.json and accepted it (or errored
	// out above). It exists and is a private regular file, or it does not exist yet
	// and is ours to seed. Re-lstat only to distinguish those two cases.
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to stat %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		return fmt.Errorf("failed to seed %s: %w", path, err)
	}
	return nil
}

// MaterializeCache ensures the profile's cache/ mount source exists as a real
// directory under the store and returns its path. It mirrors Materialize's
// guard-then-create for claude/: cache/ is bind-mounted read-write at
// $HOME/.cache, so it is a mount source subject to the same profile-boundary
// invariant. Validating BEFORE os.MkdirAll refuses a pre-existing cache/ ->
// /outside symlink rather than following it and mounting the outside target.
func (s *Store) MaterializeCache(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	// The store root is the trusted root for the symlink guard below; it must
	// exist as a real directory before ValidateMountSources can walk down from it.
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return "", fmt.Errorf("failed to create profile store: %w", err)
	}
	if err := s.ValidateMountSources(name); err != nil {
		return "", err
	}
	dir := s.CacheDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create profile cache: %w", err)
	}
	return dir, nil
}

// ValidateMountSources rejects a profile whose bind-mount sources would escape
// the profile boundary, WITHOUT creating anything — so `ccc check` can apply the
// exact invariant a real run enforces (Materialize calls this before any mkdir)
// while staying a read-only diagnostic.
//
// A profile that has not been materialized yet is a no-op success: there is
// nothing on disk to validate, and `ccc check` on a fresh profile must not fail
// merely because the mount sources do not exist yet — the run that follows will
// create them through Materialize, which re-runs this guard.
func (s *Store) ValidateMountSources(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	// The store root is the trusted root for the symlink walk. If it does not
	// exist yet, no profile has been materialized: nothing to validate.
	if fi, err := os.Lstat(s.root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to stat %s: %w", s.root, err)
	} else if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("profile store %s is not a real directory", s.root)
	}

	// The claude/ directory is bind-mounted READ-WRITE at $HOME/.claude, so it —
	// and the profiles/<name> ancestor between the store root and it — must be a
	// real directory, never a symlink. os.MkdirAll on a pre-existing symlink-to-dir
	// succeeds, so a claude/ (or profiles/<name>) -> /outside link would otherwise
	// be silently accepted and mount the outside target into the container,
	// defeating the profile boundary. Reuse the seed copy path's guard to refuse a
	// symlinked component; a not-yet-created path stops the walk (nothing to reject).
	if err := ensureNoSymlinkPath(s.root, s.ClaudeDir(name)); err != nil {
		return err
	}

	// The cache/ directory is the profile's THIRD mount source: it is bind-mounted
	// READ-WRITE at $HOME/.cache when mounts.cache is enabled. Like claude/, a
	// pre-existing cache/ -> /outside symlink would be followed by os.MkdirAll and
	// mount the outside target read-write into the container, escaping the profile
	// boundary. Guard it with the same walk so check and run agree; a not-yet-
	// created cache/ stops the walk (nothing to reject).
	if err := ensureNoSymlinkPath(s.root, s.CacheDir(name)); err != nil {
		return err
	}

	path := s.ClaudeJSON(name)
	// Lstat, not Stat: an existing claude.json must be a real regular file, not a
	// symlink os.Stat would follow. A pre-existing claude.json -> /outside link
	// would otherwise pass the exists check and survive to be bind-mounted at
	// $HOME/.claude.json in the container. Reject any non-regular existing entry.
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // not created yet; Materialize will seed it as a real file
		}
		return fmt.Errorf("failed to stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not %s", path, fi.Mode().Type())
	}
	// A hard link is a regular file, so the check above accepts it — but its inode
	// is shared with whatever else links to it, and bind-mounting the file
	// read-write at $HOME/.claude.json would let the container mutate that outside
	// path through the shared inode, bypassing copyFile's temp+rename hard-link
	// defense. Require a private single-link inode, the same "must be a private
	// regular file" invariant copyFile enforces.
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Errorf("%s must be a private regular file, not a hard link (link count %d)", path, st.Nlink)
	}
	return nil
}

// Remove deletes the profile and everything in it, including credentials.
//
// ValidateName is not optional here: Remove is the destructive operation, and
// an unvalidated name with `..` segments makes os.RemoveAll escape profiles/
// and delete arbitrary host directories.
func (s *Store) Remove(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if !s.Exists(name) {
		return fmt.Errorf("%q: %w", name, ErrNotExist)
	}
	return os.RemoveAll(s.Dir(name))
}

// Seed copies an existing Claude Code config into the profile. from is a
// ~/.claude-style directory; its sibling <from>.json (e.g. ~/.claude.json) is
// copied too when present, because the two halves are one unit of state.
func (s *Store) Seed(name string, from string) error {
	if err := s.Materialize(name); err != nil {
		return err
	}

	fi, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", from, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", from)
	}
	// Resolve symlinks before walking: os.Stat above follows a symlinked ~/.claude
	// (common with dotfile managers), but filepath.WalkDir does NOT descend the
	// symlink root — so without this the copy silently produces an empty profile.
	// Only the tree copy is resolved; the sidecar stays next to the ORIGINAL
	// path (~/.claude.json sits beside the ~/.claude symlink, not its target).
	src := from
	if resolved, err := filepath.EvalSymlinks(from); err == nil {
		src = resolved
	}
	if err := copyTree(src, s.ClaudeDir(name)); err != nil {
		return err
	}

	sidecar := filepath.Clean(from) + ".json"
	if _, err := os.Stat(sidecar); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to stat %s: %w", sidecar, err)
	}
	// The sidecar is a single known file; unlike the tree, following a symlink
	// here is INTENDED (dotfile managers symlink ~/.claude.json), the same
	// rationale as the tree root's EvalSymlinks above. Resolve it to its
	// regular-file target so copyFile's no-follow open sees a real file rather
	// than skipping the link on ELOOP and leaving the materialized empty {}.
	resolved := sidecar
	if r, err := filepath.EvalSymlinks(sidecar); err == nil {
		resolved = r
	}
	// The sidecar's destination parent is the profile dir itself (ccc-owned),
	// so pass it as the no-symlink root: there is nothing between it and the
	// file to walk.
	dst := s.ClaudeJSON(name)
	return copyFile(resolved, dst, filepath.Dir(dst))
}

// ValidateName rejects names that would escape the profiles directory.
func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid profile name %q: use letters, digits, dot, dash, underscore", name)
	}
	return nil
}

func copyTree(src string, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			// Route directory creation through the same symlink guard as files:
			// a pre-existing claude/agents -> /outside symlink must not let
			// MkdirAll create /outside/nested, and dst itself must be a real dir.
			if err := ensureNoSymlinkPath(dst, target); err != nil {
				return err
			}
			return os.MkdirAll(target, 0o700)
		}
		// Skip sockets, devices, and dangling symlinks; they are runtime
		// artifacts (daemon.lock, ipc sockets), not config worth copying.
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target, dst)
	})
}

// ensureNoSymlinkPath rejects a destination reached through a symlinked
// component. O_NOFOLLOW guards only a single final open, so a pre-existing
// symlink among target's ancestors — a hostile claude/agents -> /outside in a
// pre-populated store, or a claude/ root that is itself a symlink — would still
// let the copy (or a MkdirAll) land outside the profile. root must itself be a
// real directory, and every component from root down to and including target is
// lstat'd; a symlink is refused. A component that does not exist yet stops the
// walk: the rest of the path is ours to create as real directories. root is the
// profile's ccc-owned claude/ destination dir; components above it are ccc-owned
// and need not be walked.
func ensureNoSymlinkPath(root string, target string) error {
	fi, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("refusing to seed: destination root %s is not a real directory", root)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	dir := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		dir = filepath.Join(dir, part)
		fi, err := os.Lstat(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // not created yet; the rest of the path is ours to make
			}
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to seed through symlinked parent %s", dir)
		}
	}
	return nil
}

func copyFile(src string, dst string, dstRoot string) error {
	// Open the source with O_NOFOLLOW so a symlink swapped in after copyTree's
	// walk (a TOCTOU race) is not followed to a file outside the source tree.
	// A symlink now surfaces as ELOOP here; treat it — and any dangling/racing
	// link — as a runtime artifact to skip, matching copyTree's non-regular skip.
	// O_NONBLOCK mirrors config.ReadStateFile: opening a FIFO swapped in during
	// the race returns immediately instead of blocking (hanging the host ccc
	// process) before the fstat regular-file check below can reject it. Regular
	// files ignore O_NONBLOCK, so the copy is unaffected.
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	fi, err := in.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", src, err)
	}
	// Re-check regularity AFTER opening the fd: the walk saw a regular file, but
	// only fstat on the opened descriptor proves this fd is still one (not a
	// FIFO/device swapped in during the race). Skip anything that is not.
	if !fi.Mode().IsRegular() {
		return nil
	}
	// Guard the destination root and every ancestor too: O_NOFOLLOW below only
	// protects the final open, not a symlinked ancestor (or a symlinked root) in
	// a pre-populated store.
	if err := ensureNoSymlinkPath(dstRoot, dst); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	// Write to a fresh temp file in the guard-verified parent, then atomically
	// rename it over dst. O_NOFOLLOW alone stops a destination SYMLINK from being
	// followed, but a pre-existing HARD LINK at dst is a regular file: opening it
	// O_TRUNC would write THROUGH it and mutate the shared inode, which may be a
	// pathname OUTSIDE the profile. Creating a brand-new inode and renaming it
	// over dst swaps the directory entry instead of writing through whatever dst
	// currently points at, closing symlink and hard-link write-through together.
	out, err := createTempFile(dst, fi.Mode().Perm())
	if err != nil {
		return err
	}
	// On any failure past this point the temp file is orphaned; remove it. On
	// success the rename consumes it, so nothing is left to clean up.
	committed := false
	defer func() {
		if !committed {
			_ = out.Close()
			_ = os.Remove(out.Name())
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("failed to copy %s: %w", src, err)
	}
	// Persist the bytes before the rename so a crash cannot leave dst pointing at
	// a fresh but empty inode.
	if err := out.Sync(); err != nil {
		return fmt.Errorf("failed to sync %s: %w", out.Name(), err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", out.Name(), err)
	}
	if err := os.Rename(out.Name(), dst); err != nil {
		return fmt.Errorf("failed to replace %s: %w", dst, err)
	}
	committed = true
	return nil
}

// createTempFile makes a fresh, exclusively-created temp file in dst's already
// guard-verified parent directory (never /tmp, so the later os.Rename is an
// atomic same-filesystem swap). Its basename is a short fixed prefix plus the
// pid and a counter — NOT dst's filename with a suffix, which for a near-
// NAME_MAX source name would overflow and fail the create with ENAMETOOLONG,
// breaking legitimately long filenames. O_CREATE|O_EXCL|O_NOFOLLOW guarantees a
// brand-new inode: it fails rather than opening any pre-existing symlink, hard
// link, or file at the candidate name, so nothing outside the profile is
// touched. The final chmod pins the exact source permission bits, which the
// umask on create may otherwise have masked off.
func createTempFile(dst string, perm fs.FileMode) (*os.File, error) {
	dir := filepath.Dir(dst)
	for n := 0; ; n++ {
		name := filepath.Join(dir, fmt.Sprintf(".ccc-tmp-%d-%d", os.Getpid(), n))
		out, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // name taken (or a link squats it); try the next candidate
			}
			return nil, fmt.Errorf("failed to create temp file for %s: %w", dst, err)
		}
		if err := out.Chmod(perm); err != nil {
			_ = out.Close()
			_ = os.Remove(name)
			return nil, fmt.Errorf("failed to chmod %s: %w", name, err)
		}
		return out, nil
	}
}
