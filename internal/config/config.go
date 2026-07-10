// Package config loads ccc's global config, per-profile config, and the
// per-directory .ccc.json that pins a directory tree to a profile.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// DirConfigName is the per-directory config file naming a profile.
const DirConfigName = ".ccc.json"

// FileName is the global config file, relative to the ccc config root.
const FileName = "config.json"

// Config is ~/.config/ccc/config.json.
type Config struct {
	Runtime        string `json:"runtime,omitempty"`
	DefaultProfile string `json:"default_profile,omitempty"`
	Image          Image  `json:"image,omitzero"`
	Mounts         Mounts `json:"mounts,omitzero"`
	Env            Env    `json:"env,omitzero"`

	// Root is the ccc config directory this Config was loaded from.
	Root string `json:"-"`
}

// Image controls how the container image is produced.
type Image struct {
	// ExtraDockerfile is appended verbatim to the base Dockerfile.
	// Relative paths resolve against the ccc config directory.
	ExtraDockerfile string `json:"extra_dockerfile,omitempty"`

	// DefaultClaudeVersion is the Claude Code version a profile uses when it has
	// no pin of its own. It is a DEFAULT, not a pin: a profile that inherits it
	// still tracks its own updates and can diverge upward. The true pin is the
	// per-profile .ccc-claude-version file. Empty means npm's "latest".
	DefaultClaudeVersion string `json:"default_claude_version,omitempty"`
}

// LatestClaudeVersion is npm's "latest" dist-tag: the value that means "not
// concretely pinned". Distinct from Image.DefaultClaudeVersion, the configured
// default a profile falls back to.
const LatestClaudeVersion = "latest"

// Mounts controls what host state the container sees.
type Mounts struct {
	// Dirs are extra host directories mounted read-write at their identical
	// absolute path, in ADDITION to the repository the working directory
	// belongs to. Never instead of it: nothing should be able to unmount the
	// repository you are standing in.
	//
	// ccc does not infer these. It never reads go.mod, and knows nothing about
	// replace directives or workspace files — if a build needs a sibling
	// checkout, you name it here.
	Dirs []string `json:"dirs,omitempty"`

	// Home mounts the host's $HOME: "" (default) not at all, "ro" read-only,
	// "rw" read-write.
	//
	// "ro" is the safe way to get breadth: Roots are mounted read-write on top
	// of it, and a read-only parent directory is what actually stops
	// `claude install` from replacing the host's binary — rename(2) needs a
	// writable directory, not just a writable file.
	Home string `json:"home,omitempty"`

	// Cache mounts a profile-owned cache directory at the container's ~/.cache
	// and points GOMODCACHE into it, so ephemeral containers do not rebuild
	// from cold. It is never the host's cache: that would be a writable hole in
	// a read-only $HOME, and a macOS host's artifacts are useless to a Linux
	// container anyway.
	Cache bool `json:"cache,omitempty"`

	// GhConfig is the gh CLI config directory. A profile may override it.
	GhConfig string `json:"gh_config,omitempty"`
}

// Home mount modes.
const (
	HomeNone = ""
	HomeRO   = "ro"
	HomeRW   = "rw"
)

// Env controls environment inheritance. ccc forwards the whole host
// environment minus a built-in denylist; these extend and override it.
type Env struct {
	Deny  []string `json:"deny,omitempty"`
	Allow []string `json:"allow,omitempty"`
}

// Profile is profiles/<name>/profile.json. Everything Claude Code itself can
// read lives in the profile's claude/ directory, so this stays deliberately
// small: it holds only settings Claude Code cannot know about.
type Profile struct {
	GhConfig string `json:"gh_config,omitempty"`
}

// claudeVersionRe matches npm's "latest" dist-tag or a plain release semver.
//
// Prerelease suffixes (-beta, -rc.1) are deliberately NOT accepted. ccc orders
// versions on the X.Y.Z triple alone; a prerelease would compare equal to its
// release, so once pinned it would never advance to the final release — a stuck
// profile. Claude Code ships stable via npm's "latest", so the only way to pin
// a prerelease is by hand, and that is exactly what this refuses.
var claudeVersionRe = regexp.MustCompile(`^(latest|[0-9]+\.[0-9]+\.[0-9]+)$`)

// IsNewerClaudeVersion reports whether a is a strictly newer release than b.
//
// b == "" or "latest" means nothing concrete is pinned, so any concrete a is
// newer. Inputs are release semvers (ValidateClaudeVersion rejects
// prereleases), so ordering on the X.Y.Z triple is total; an unparseable input
// is treated as not-newer.
func IsNewerClaudeVersion(a string, b string) bool {
	av, ok := parseSemver(a)
	if !ok {
		return false
	}
	if b == "" || b == LatestClaudeVersion {
		return true
	}
	bv, ok := parseSemver(b)
	if !ok {
		return false
	}
	for i := range av {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	base, _, _ := strings.Cut(v, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ValidateClaudeVersion rejects anything that is not a dist-tag or semver.
//
// The pin reaches `npm install -g pkg@${CLAUDE_VERSION}` inside a Dockerfile
// RUN, where the shell would interpret `&&`, `;`, or backticks — and the build
// runs as root. The per-profile pin lives in the profile's claude/ directory,
// which IS mounted read-write into the container, so its contents are
// attacker-reachable by definition. Validation is what makes that safe:
// anything that is not a dist-tag or a semver is an error, never a build arg.
func ValidateClaudeVersion(v string) error {
	if !claudeVersionRe.MatchString(v) {
		return fmt.Errorf("invalid claude version %q: want \"latest\" or a release semver like \"2.1.205\" (no prereleases)", v)
	}
	return nil
}

// Dir is a .ccc.json: a per-checkout, per-user file. It is NOT meant to be
// committed — profile names differ between users, and so do the paths in Dirs.
type Dir struct {
	Profile string   `json:"profile,omitempty"`
	Dirs    []string `json:"dirs,omitempty"`
}

// DefaultRoot returns the ccc config directory, honoring XDG.
func DefaultRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to locate user config dir: %w", err)
	}
	return filepath.Join(dir, "ccc"), nil
}

// readJSON decodes path into v. A missing file leaves v untouched.
func readJSON(path string, v any) error {
	b, err := ReadStateFile(path)
	if err != nil || b == nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return nil
}

// MaxStateFileSize caps every ccc state file. These files (config.json,
// .ccc.json, the pin, the update record) are tiny; the cap exists because some
// of them live in container-writable mounts, so an unbounded os.ReadFile on the
// host would let a contained process OOM or hang (a 100G file, or a FIFO) the
// host ccc process.
const MaxStateFileSize = 4 << 20 // 4 MiB

// ReadStateFile reads a ccc state file with the safety a container-writable
// source demands: it must be a regular file (not a FIFO/device/dir), and the
// read is bounded by MaxStateFileSize. A missing file returns (nil, nil).
//
// The file is opened O_NONBLOCK and fstat'd through the returned descriptor, not
// pre-Lstat'd by path: a container-writable directory could swap a regular file
// for a FIFO/symlink between an Lstat and the open (TOCTOU). O_NONBLOCK also
// means opening a FIFO returns immediately instead of blocking, so the
// regular-file check can then reject it.
func ReadStateFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	b, err := io.ReadAll(io.LimitReader(f, MaxStateFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	if len(b) > MaxStateFileSize {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", path, MaxStateFileSize)
	}
	return b, nil
}

// writeJSON marshals v and writes it to path via WriteAtomic.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode %s: %w", path, err)
	}
	return WriteAtomic(path, append(b, '\n'), 0o600)
}

// WriteAtomic writes b to path atomically: a unique temp file, fsync'd, then
// renamed over path. The temp name is unique (os.CreateTemp) so concurrent
// writers cannot clobber each other's temp, and the fsync means a crash after
// the rename cannot surface a truncated file. Exported so every small state
// file (the per-profile pin included) gets the same guarantee.
func WriteAtomic(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// On any failure past this point, do not leave the temp behind.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to replace %s: %w", path, err)
	}
	return nil
}

// withConfigLock serializes a read-modify-write of config.json against other
// callers in this process and in other ccc processes. WriteAtomic makes a single
// write atomic, but does not order two racing read-modify-writes: a first-run
// bootstrap (Create) and a global `ccc pin` (SetDefaultClaudeVersion) can each
// read the absent-or-old file and then write, and the later write clobbers the
// earlier one's key. This holds an exclusive advisory lock (flock) for the whole
// of fn so the read and the write are one indivisible step.
//
// The lock is taken on the config root directory's own descriptor, so it needs
// no lock file and leaves nothing behind for callers that count the directory's
// entries. flock is per open file description, so two goroutines that each open
// the directory contend correctly, and the lock is released reliably on return.
func withConfigLock(root string, fn func() error) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", root, err)
	}
	f, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", root, err)
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("failed to lock %s: %w", root, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	return fn()
}

// Load reads config.json from root, applying defaults. A missing file is not
// an error: ccc must work with no configuration at all.
func Load(root string) (*Config, error) {
	cfg := &Config{}
	if err := readJSON(filepath.Join(root, FileName), cfg); err != nil {
		return nil, err
	}
	cfg.Root = root

	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() error {
	if c.Runtime == "" {
		c.Runtime = "auto"
	}
	if v := os.Getenv("CCC_RUNTIME"); v != "" {
		c.Runtime = v
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to locate home dir: %w", err)
	}

	// Dirs are ADDITIVE to the repository the working directory belongs to,
	// which the caller resolves per-invocation. $HOME is never a default:
	// mounting it put the host's ~/.local — and with it the host's Claude Code
	// installation — inside every container.
	if err := expandDirs(c.Mounts.Dirs, home); err != nil {
		return err
	}

	switch c.Mounts.Home {
	case HomeNone, HomeRO, HomeRW:
	default:
		return fmt.Errorf("invalid mounts.home %q: want \"ro\", \"rw\", or omitted", c.Mounts.Home)
	}

	if c.Mounts.GhConfig == "" {
		c.Mounts.GhConfig = filepath.Join(home, ".config", "gh")
	}
	c.Mounts.GhConfig, err = Expand(c.Mounts.GhConfig, home)
	if err != nil {
		return err
	}

	if c.Image.ExtraDockerfile != "" {
		p, err := Expand(c.Image.ExtraDockerfile, home)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(c.Root, p)
		}
		c.Image.ExtraDockerfile = p
	}

	// claude_version is NOT validated here. Load runs for every command, so a
	// malformed global pin would otherwise brick `version`, `help`, and the very
	// `pin` that repairs it. It is validated where consumed (a.claudeVersion),
	// exactly as the per-profile pin is.
	return nil
}

// Create writes a config.json naming name as default_profile. It reports
// whether it wrote one: an existing config is never modified, so ccc cannot
// clobber or reorder settings a user hand-wrote.
func Create(root string, name string) (bool, error) {
	path := filepath.Join(root, FileName)

	// The stat and the write are one step under the lock, so a SetDefaultClaudeVersion
	// racing this cannot have its key clobbered by a write based on a stale "absent"
	// stat: either this sees no file and writes, or it sees the file the pin wrote and
	// leaves it be.
	var created bool
	err := withConfigLock(root, func() error {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("failed to stat %s: %w", path, err)
		}

		// Only default_profile is written. Derived values that Load() materializes
		// (mount roots, gh_config) must not be frozen in as if the user chose them.
		if err := writeJSON(path, &Config{DefaultProfile: name}); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

// SetDefaultClaudeVersion records image.default_claude_version in config.json,
// preserving every other setting. Unlike Create, this is an explicit user action
// (a global `ccc pin`), so an existing config is updated rather than left alone.
//
// The file is merged as a raw map, not round-tripped through Config: a key ccc
// does not model (a newer option, a typo worth keeping) must survive the write.
// Decoding into a struct and re-marshalling would silently drop it. This also
// avoids freezing in the derived values Load() materializes.
func SetDefaultClaudeVersion(root string, version string) error {
	if err := ValidateClaudeVersion(version); err != nil {
		return err
	}
	path := filepath.Join(root, FileName)

	// Read and write are one step under the lock so a concurrent bootstrap (Create)
	// or another pin cannot slot a write in between and lose a key: whatever is on
	// disk when this reads is exactly what this merges into and rewrites.
	return withConfigLock(root, func() error {
		doc := map[string]any{}
		if err := readJSON(path, &doc); err != nil {
			return err
		}

		image, _ := doc["image"].(map[string]any)
		if image == nil {
			image = map[string]any{}
		}
		image["default_claude_version"] = version
		doc["image"] = image

		return writeJSON(path, doc)
	})
}

// LoadProfile reads profiles/<name>/profile.json. A missing file yields a
// zero Profile.
func LoadProfile(path string, home string) (*Profile, error) {
	var p Profile
	if err := readJSON(path, &p); err != nil {
		return nil, err
	}
	if p.GhConfig != "" {
		expanded, err := Expand(p.GhConfig, home)
		if err != nil {
			return nil, err
		}
		p.GhConfig = expanded
	}
	return &p, nil
}

// FindDir walks up from start looking for a .ccc.json, returning its contents
// and the file it was read from. Returns ok=false if none is found.
//
// home expands ~ in Dirs. Both keys are optional individually, but a file with
// neither is a mistake worth reporting.
func FindDir(start string, home string) (*Dir, string, bool, error) {
	dir := start
	for {
		path := filepath.Join(dir, DirConfigName)
		if _, err := os.Stat(path); err == nil {
			var d Dir
			if err := readJSON(path, &d); err != nil {
				return nil, "", false, err
			}
			if d.Profile == "" && len(d.Dirs) == 0 {
				return nil, "", false, fmt.Errorf(`%s: needs "profile", "dirs", or both`, path)
			}
			if err := expandDirs(d.Dirs, home); err != nil {
				return nil, "", false, fmt.Errorf("%s: %w", path, err)
			}
			return &d, path, true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", false, fmt.Errorf("failed to stat %s: %w", path, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, "", false, nil
		}
		dir = parent
	}
}

// Expand resolves a leading ~ against home and makes the path absolute.
func Expand(path string, home string) (string, error) {
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:]), nil
	}
	if !filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Clean(path), nil
}

// ExpandDir is Expand for mount directories, rejecting relative paths.
//
// A relative path needs a base, and the two plausible bases disagree: the
// config file's directory, or the working directory. Rather than pick one and
// surprise half the users, require an unambiguous path. `.ccc.json` is a
// per-checkout, per-user file — profile names alone make it unshareable — so
// an absolute path costs nothing.
func ExpandDir(path string, home string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty mount directory")
	}
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("mount directory %q must be absolute or start with ~/", path)
	}
	return Expand(path, home)
}

// expandDirs validates and expands a list of mount directories.
func expandDirs(dirs []string, home string) error {
	for i, d := range dirs {
		expanded, err := ExpandDir(d, home)
		if err != nil {
			return err
		}
		dirs[i] = expanded
	}
	return nil
}
