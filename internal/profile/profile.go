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

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read %s: %w", path, err)
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

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read %s: %w", path, err)
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
func (s *Store) Materialize(name string) error {
	if err := os.MkdirAll(s.ClaudeDir(name), 0o700); err != nil {
		return fmt.Errorf("failed to create profile dir: %w", err)
	}

	path := s.ClaudeJSON(name)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to stat %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		return fmt.Errorf("failed to seed %s: %w", path, err)
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
	if err := copyTree(from, s.ClaudeDir(name)); err != nil {
		return err
	}

	sidecar := filepath.Clean(from) + ".json"
	if _, err := os.Stat(sidecar); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to stat %s: %w", sidecar, err)
	}
	return copyFile(sidecar, s.ClaudeJSON(name))
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
			return os.MkdirAll(target, 0o700)
		}
		// Skip sockets, devices, and dangling symlinks; they are runtime
		// artifacts (daemon.lock, ipc sockets), not config worth copying.
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src string, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	fi, err := in.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("failed to copy %s: %w", src, err)
	}
	return out.Close()
}
