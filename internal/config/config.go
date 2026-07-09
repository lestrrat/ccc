// Package config loads ccc's global config, per-profile config, and the
// per-directory .ccc.toml that pins a directory tree to a profile.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// DirConfigName is the per-directory config file naming a profile.
const DirConfigName = ".ccc.toml"

// Config is ~/.config/ccc/config.toml.
type Config struct {
	Runtime        string `toml:"runtime"`
	DefaultProfile string `toml:"default_profile"`
	Image          Image  `toml:"image"`
	Mounts         Mounts `toml:"mounts"`
	Env            Env    `toml:"env"`

	// Root is the ccc config directory this Config was loaded from.
	Root string `toml:"-"`
}

// Image controls how the container image is produced.
type Image struct {
	// ExtraDockerfile is appended verbatim to the base Dockerfile.
	// Relative paths resolve against the ccc config directory.
	ExtraDockerfile string `toml:"extra_dockerfile"`
}

// Mounts controls what host state the container sees.
type Mounts struct {
	// Roots are host directories mounted at their identical absolute path.
	// The working directory must live under one of them.
	Roots []string `toml:"roots"`
	// GhConfig is the gh CLI config directory. A profile may override it.
	GhConfig string `toml:"gh_config"`
}

// Env controls environment inheritance. ccc forwards the whole host
// environment minus a built-in denylist; these extend and override it.
type Env struct {
	Deny  []string `toml:"deny"`
	Allow []string `toml:"allow"`
}

// Profile is profiles/<name>/profile.toml. Everything Claude Code itself can
// read lives in the profile's claude/ directory, so this stays deliberately
// small: it holds only settings Claude Code cannot know about.
type Profile struct {
	GhConfig string `toml:"gh_config"`
}

// Dir is a .ccc.toml pinning a directory tree to a profile.
type Dir struct {
	Profile string `toml:"profile"`
}

// DefaultRoot returns the ccc config directory, honoring XDG.
func DefaultRoot() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to locate user config dir: %w", err)
	}
	return filepath.Join(dir, "ccc"), nil
}

// Load reads config.toml from root, applying defaults. A missing file is not
// an error: ccc must work with no configuration at all.
func Load(root string) (*Config, error) {
	cfg := &Config{Root: root}
	path := filepath.Join(root, "config.toml")
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		}
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

	// Default to the whole home directory. ccc isolates Claude Code profiles,
	// not the filesystem — the contained agent should see what the user sees.
	if len(c.Mounts.Roots) == 0 {
		c.Mounts.Roots = []string{home}
	}
	for i, r := range c.Mounts.Roots {
		c.Mounts.Roots[i], err = Expand(r, home)
		if err != nil {
			return err
		}
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
	return nil
}

// LoadProfile reads profiles/<name>/profile.toml. A missing file yields a
// zero Profile.
func LoadProfile(path string, home string) (*Profile, error) {
	var p Profile
	if _, err := toml.DecodeFile(path, &p); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("failed to parse %s: %w", path, err)
		}
		return &p, nil
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

// FindDir walks up from start looking for a .ccc.toml, returning the profile
// it names and the file it was read from. Returns ok=false if none is found.
func FindDir(start string) (string, string, bool, error) {
	dir := start
	for {
		path := filepath.Join(dir, DirConfigName)
		var d Dir
		_, err := toml.DecodeFile(path, &d)
		switch {
		case err == nil && d.Profile != "":
			return d.Profile, path, true, nil
		case err == nil:
			return "", "", false, fmt.Errorf("%s: missing `profile` key", path)
		case !errors.Is(err, fs.ErrNotExist):
			return "", "", false, fmt.Errorf("failed to parse %s: %w", path, err)
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
