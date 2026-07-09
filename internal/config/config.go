// Package config loads ccc's global config, per-profile config, and the
// per-directory .ccc.json that pins a directory tree to a profile.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

	// ClaudeVersion pins the Claude Code npm version. Empty means "latest",
	// resolved once at build time and never revisited: ccc does not check the
	// registry on a normal run. `ccc upgrade` records a concrete version here,
	// which changes the image tag and triggers a one-layer rebuild.
	ClaudeVersion string `json:"claude_version,omitempty"`
}

// DefaultClaudeVersion is the npm dist-tag used when nothing is pinned.
const DefaultClaudeVersion = "latest"

// Mounts controls what host state the container sees.
type Mounts struct {
	// Roots are host directories mounted read-write at their identical absolute
	// path. Empty means "the repository the working directory belongs to".
	Roots []string `json:"roots,omitempty"`

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

// claudeVersionRe matches npm's "latest" dist-tag or a plain semver.
var claudeVersionRe = regexp.MustCompile(`^(latest|[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$`)

// IsNewerClaudeVersion reports whether a is a strictly newer release than b.
//
// b == "" or "latest" means nothing concrete is pinned, so any concrete a is
// newer. Prerelease suffixes are ignored for ordering: this only ever gates
// "should we adopt the version Claude Code asked for", and the safe answer for
// an unparseable input is no.
func IsNewerClaudeVersion(a string, b string) bool {
	av, ok := parseSemver(a)
	if !ok {
		return false
	}
	if b == "" || b == DefaultClaudeVersion {
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
		return fmt.Errorf("invalid claude version %q: want \"latest\" or a semver like \"2.1.205\"", v)
	}
	return nil
}

// Dir is a .ccc.json pinning a directory tree to a profile.
type Dir struct {
	Profile string `json:"profile"`
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
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return nil
}

// writeJSON writes v to path via a temp file, so a crash cannot leave a
// truncated config behind.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}

	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode %s: %w", path, err)
	}
	b = append(b, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("failed to replace %s: %w", path, err)
	}
	return nil
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

	// Roots are NOT defaulted to $HOME. An empty list means "the repository the
	// working directory belongs to", resolved per-invocation by the caller.
	// Mounting the whole home by default put the host's ~/.local — and with it
	// the host's Claude Code installation — inside every container.
	for i, r := range c.Mounts.Roots {
		c.Mounts.Roots[i], err = Expand(r, home)
		if err != nil {
			return err
		}
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

	if c.Image.ClaudeVersion != "" {
		if err := ValidateClaudeVersion(c.Image.ClaudeVersion); err != nil {
			return err
		}
	}
	return nil
}

// Create writes a config.json naming name as default_profile. It reports
// whether it wrote one: an existing config is never modified, so ccc cannot
// clobber or reorder settings a user hand-wrote.
func Create(root string, name string) (bool, error) {
	path := filepath.Join(root, FileName)

	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("failed to stat %s: %w", path, err)
	}

	// Only default_profile is written. Derived values that Load() materializes
	// (mount roots, gh_config) must not be frozen in as if the user chose them.
	if err := writeJSON(path, &Config{DefaultProfile: name}); err != nil {
		return false, err
	}
	return true, nil
}

// SetClaudeVersion records image.claude_version in config.json, preserving
// every other setting. Unlike Create, this is an explicit user action
// (`ccc upgrade`), so an existing config is updated rather than left alone.
func SetClaudeVersion(root string, version string) error {
	if err := ValidateClaudeVersion(version); err != nil {
		return err
	}
	path := filepath.Join(root, FileName)

	// Read raw rather than reusing a loaded Config: applyDefaults() materializes
	// derived values (mount roots, gh_config) that must not be frozen into the
	// file as if the user had written them.
	var raw Config
	if err := readJSON(path, &raw); err != nil {
		return err
	}
	raw.Image.ClaudeVersion = version
	return writeJSON(path, &raw)
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

// FindDir walks up from start looking for a .ccc.json, returning the profile
// it names and the file it was read from. Returns ok=false if none is found.
func FindDir(start string) (string, string, bool, error) {
	dir := start
	for {
		path := filepath.Join(dir, DirConfigName)
		if _, err := os.Stat(path); err == nil {
			var d Dir
			if err := readJSON(path, &d); err != nil {
				return "", "", false, err
			}
			if d.Profile == "" {
				return "", "", false, fmt.Errorf(`%s: missing "profile" key`, path)
			}
			return d.Profile, path, true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", "", false, fmt.Errorf("failed to stat %s: %w", path, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false, nil
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
