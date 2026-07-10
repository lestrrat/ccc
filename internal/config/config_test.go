package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, path string, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	t.Setenv("CCC_RUNTIME", "")

	cfg, err := config.Load(t.TempDir())
	require.NoError(t, err, "ccc must work with no configuration at all")
	require.Equal(t, "auto", cfg.Runtime)

	// Roots are NOT defaulted to $HOME: an empty list means "the repository the
	// working directory belongs to", resolved per-invocation. Defaulting to the
	// home directory put the host's ~/.local inside every container.
	require.Empty(t, cfg.Mounts.Dirs)
	require.Equal(t, config.HomeNone, cfg.Mounts.Home, "$HOME is not mounted by default")
	require.False(t, cfg.Mounts.Cache, "caches are ephemeral by default")

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".config", "gh"), cfg.Mounts.GhConfig)
}

func TestHomeModeValidation(t *testing.T) {
	for _, mode := range []string{"ro", "rw", ""} {
		root := t.TempDir()
		write(t, filepath.Join(root, config.FileName), `{"mounts":{"home":"`+mode+`"}}`)
		_, err := config.Load(root)
		require.NoError(t, err, "mode %q must be accepted", mode)
	}

	root := t.TempDir()
	write(t, filepath.Join(root, config.FileName), `{"mounts":{"home":"readonly"}}`)
	_, err := config.Load(root)
	require.ErrorContains(t, err, "invalid mounts.home")
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, config.FileName), `{
  "runtime": "podman",
  "default_profile": "personal",
  "image": {"extra_dockerfile": "Dockerfile.extra"},
  "mounts": {"dirs": ["~/dev/src", "/opt/work"]},
  "env": {"deny": ["FOO"], "allow": ["ANTHROPIC_API_KEY"]}
}`)

	cfg, err := config.Load(root)
	require.NoError(t, err)
	require.Equal(t, "podman", cfg.Runtime)
	require.Equal(t, "personal", cfg.DefaultProfile)
	require.Equal(t, []string{"FOO"}, cfg.Env.Deny)
	require.Equal(t, []string{"ANTHROPIC_API_KEY"}, cfg.Env.Allow)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "dev", "src"), cfg.Mounts.Dirs[0], "~ expands")
	require.Equal(t, "/opt/work", cfg.Mounts.Dirs[1])

	// A relative extra_dockerfile resolves against the ccc config dir.
	require.Equal(t, filepath.Join(root, "Dockerfile.extra"), cfg.Image.ExtraDockerfile)
}

// A relative path needs a base, and the config file's directory and the working
// directory disagree. Require an unambiguous path rather than pick one.
func TestRelativeDirsRejected(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, config.FileName), `{"mounts": {"dirs": ["../sibling"]}}`)

	_, err := config.Load(root)
	require.ErrorContains(t, err, "must be absolute or start with ~/")
}

// Load must NOT validate default_claude_version: it runs for every command, so a
// malformed global pin would otherwise brick `version`, `help`, and the `pin`
// that repairs it. Validation is deferred to the point of use.
func TestLoadToleratesMalformedGlobalPin(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, config.FileName), `{"image":{"default_claude_version":"beta"}}`)

	cfg, err := config.Load(root)
	require.NoError(t, err, "a malformed pin must not fail load")
	require.Equal(t, "beta", cfg.Image.DefaultClaudeVersion, "preserved verbatim for repair")
}

// Concurrent SetDefaultClaudeVersion (two `ccc pin` at once, a bootstrap racing a pin)
// must never leave a corrupt config or a stray temp file: unique temps + rename.
func TestConcurrentWritesStayValid(t *testing.T) {
	root := t.TempDir()

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_ = config.SetDefaultClaudeVersion(root, "2.1.205")
		})
	}
	wg.Wait()

	cfg, err := config.Load(root)
	require.NoError(t, err, "config must remain parseable")
	require.Equal(t, "2.1.205", cfg.Image.DefaultClaudeVersion)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file left behind")
	require.Equal(t, config.FileName, entries[0].Name())
}

// A first-run bootstrap (Create) racing a global `ccc pin` (SetDefaultClaudeVersion)
// on an absent config must not lose a key to a write built on a stale read. With the
// read-modify-write serialized under a file lock, each merges into whatever is on disk:
// SetDefaultClaudeVersion never clobbers default_profile, and Create merges its key into
// the file the pin wrote rather than skipping it. So BOTH keys survive regardless of
// which write happened first — the corruption the lock exists to prevent.
func TestConcurrentBootstrapAndPinPreserveKeys(t *testing.T) {
	t.Setenv("CCC_RUNTIME", "")

	// Loop so the interleaving that dropped a key surfaces under -race.
	for range 200 {
		root := t.TempDir()

		var wg sync.WaitGroup
		wg.Go(func() {
			_, _ = config.Create(root, "default")
		})
		wg.Go(func() {
			_ = config.SetDefaultClaudeVersion(root, "2.1.205")
		})
		wg.Wait()

		cfg, err := config.Load(root)
		require.NoError(t, err, "config must remain parseable")
		require.Equal(t, "2.1.205", cfg.Image.DefaultClaudeVersion, "the pin's key is never lost")
		require.Equal(t, "default", cfg.DefaultProfile, "the bootstrap key is never lost")
	}
}

func TestSetDefaultClaudeVersion(t *testing.T) {
	t.Run("sets image.default_claude_version and preserves known keys", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.FileName),
			`{"runtime":"docker","image":{"extra_dockerfile":"Dockerfile.extra"}}`)

		require.NoError(t, config.SetDefaultClaudeVersion(root, "2.1.205"))

		cfg, err := config.Load(root)
		require.NoError(t, err)
		require.Equal(t, "2.1.205", cfg.Image.DefaultClaudeVersion)
		require.Equal(t, "docker", cfg.Runtime)
		require.Contains(t, cfg.Image.ExtraDockerfile, "Dockerfile.extra", "sibling image key kept")
	})

	// A key ccc does not model must survive the write, not be dropped by a
	// struct round-trip.
	t.Run("preserves unknown keys", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.FileName),
			`{"future_toplevel":42,"image":{"future_image_key":"keep me"}}`)

		require.NoError(t, config.SetDefaultClaudeVersion(root, "2.1.205"))

		b, err := os.ReadFile(filepath.Join(root, config.FileName))
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(b, &raw))

		require.EqualValues(t, 42, raw["future_toplevel"])
		image, ok := raw["image"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "keep me", image["future_image_key"])
		require.Equal(t, "2.1.205", image["default_claude_version"])
	})

	t.Run("creates the file when absent", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, config.SetDefaultClaudeVersion(root, "2.1.205"))

		cfg, err := config.Load(root)
		require.NoError(t, err)
		require.Equal(t, "2.1.205", cfg.Image.DefaultClaudeVersion)
	})

	t.Run("rejects a prerelease", func(t *testing.T) {
		require.ErrorContains(t, config.SetDefaultClaudeVersion(t.TempDir(), "2.1.205-beta"), "no prereleases")
	})
}

func TestLoadInvalidJSON(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, config.FileName), `{"runtime":`)

	_, err := config.Load(root)
	require.ErrorContains(t, err, "failed to parse")
}

func TestCCCRuntimeEnvOverrides(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, config.FileName), `{"runtime": "podman"}`)
	t.Setenv("CCC_RUNTIME", "docker")

	cfg, err := config.Load(root)
	require.NoError(t, err)
	require.Equal(t, "docker", cfg.Runtime)
}

func TestCreate(t *testing.T) {
	t.Run("writes config when absent", func(t *testing.T) {
		root := t.TempDir()
		created, err := config.Create(root, "default")
		require.NoError(t, err)
		require.True(t, created)

		cfg, err := config.Load(root)
		require.NoError(t, err)
		require.Equal(t, "default", cfg.DefaultProfile)
	})

	t.Run("writes only default_profile", func(t *testing.T) {
		root := t.TempDir()
		_, err := config.Create(root, "default")
		require.NoError(t, err)

		// Load() materializes mount roots and gh_config; those are derived, not
		// user intent, and must not be written back as if they were.
		b, err := os.ReadFile(filepath.Join(root, config.FileName))
		require.NoError(t, err)

		var raw map[string]any
		require.NoError(t, json.Unmarshal(b, &raw))
		require.Equal(t, map[string]any{"default_profile": "default"}, raw)
	})

	t.Run("merges default_profile into a config that lacks it, preserving other keys", func(t *testing.T) {
		root := t.TempDir()
		// The file a global `ccc pin` writes before any bootstrap: other keys, but
		// no default_profile. Bootstrap must ADD the key, not skip the file.
		write(t, filepath.Join(root, config.FileName),
			`{"runtime":"docker","image":{"default_claude_version":"2.1.205","future_image_key":"keep me"},"future_toplevel":42}`)

		created, err := config.Create(root, "default")
		require.NoError(t, err)
		require.True(t, created, "must report that it added default_profile")

		b, err := os.ReadFile(filepath.Join(root, config.FileName))
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(b, &raw))

		require.Equal(t, "default", raw["default_profile"], "bootstrap key added")
		require.Equal(t, "docker", raw["runtime"], "sibling key preserved")
		require.EqualValues(t, 42, raw["future_toplevel"], "unmodeled key preserved")
		image, ok := raw["image"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "2.1.205", image["default_claude_version"], "the pin's key is never lost")
		require.Equal(t, "keep me", image["future_image_key"], "unmodeled image key preserved")
	})

	t.Run("never overwrites an existing default_profile", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.FileName), `{"default_profile": "mine"}`)

		created, err := config.Create(root, "default")
		require.NoError(t, err)
		require.False(t, created)

		cfg, err := config.Load(root)
		require.NoError(t, err)
		require.Equal(t, "mine", cfg.DefaultProfile)
	})

	t.Run("leaves no temp file behind", func(t *testing.T) {
		root := t.TempDir()
		_, err := config.Create(root, "default")
		require.NoError(t, err)

		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, config.FileName, entries[0].Name())
	})
}

func TestFindDir(t *testing.T) {
	t.Run("finds in ancestor", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `{"profile": "work"}`)

		deep := filepath.Join(root, "a", "b")
		require.NoError(t, os.MkdirAll(deep, 0o755))

		d, origin, ok, err := config.FindDir(deep, "/home/u")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "work", d.Profile)
		require.Equal(t, filepath.Join(root, config.DirConfigName), origin)
	})

	t.Run("nearest wins", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `{"profile": "outer"}`)

		inner := filepath.Join(root, "inner")
		require.NoError(t, os.MkdirAll(inner, 0o755))
		write(t, filepath.Join(inner, config.DirConfigName), `{"profile": "inner"}`)

		d, _, ok, err := config.FindDir(inner, "/home/u")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "inner", d.Profile)
	})

	t.Run("absent", func(t *testing.T) {
		_, _, ok, err := config.FindDir(t.TempDir(), "/home/u")
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("empty file is a mistake", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `{}`)

		_, _, _, err := config.FindDir(root, "/home/u")
		require.ErrorContains(t, err, `needs "profile", "dirs", or both`)
	})

	t.Run("dirs without a profile is valid", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `{"dirs": ["~/x"]}`)

		d, _, ok, err := config.FindDir(root, "/home/u")
		require.NoError(t, err)
		require.True(t, ok)
		require.Empty(t, d.Profile, "not a profile selection; falls through")
		require.Equal(t, []string{"/home/u/x"}, d.Dirs)
	})

	t.Run("relative dirs are rejected", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `{"dirs": ["../../jwx-go/mlkem"]}`)

		_, _, _, err := config.FindDir(root, "/home/u")
		require.ErrorContains(t, err, "must be absolute or start with ~/")
	})

	t.Run("present but malformed", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `not json`)

		_, _, _, err := config.FindDir(root, "/home/u")
		require.ErrorContains(t, err, "failed to parse")
	})
}

func TestExpand(t *testing.T) {
	require.Equal(t, "/h", must(config.Expand("~", "/h")))
	require.Equal(t, "/h/x/y", must(config.Expand("~/x/y", "/h")))
	require.Equal(t, "/abs", must(config.Expand("/abs", "/h")))
	require.Equal(t, "rel/path", must(config.Expand("rel/path", "/h")))
}

func TestExpandDir(t *testing.T) {
	// Valid: "~", "~/...", and absolute paths expand to absolute results.
	require.Equal(t, "/h", must(config.ExpandDir("~", "/h")))
	require.Equal(t, "/h/x/y", must(config.ExpandDir("~/x/y", "/h")))
	require.Equal(t, "/abs", must(config.ExpandDir("/abs", "/h")))

	// Invalid: non-"~/" tilde forms and bare relative paths are rejected.
	for _, path := range []string{"~work", "~work/sub", "rel/path", "", "~~"} {
		_, err := config.ExpandDir(path, "/h")
		require.Error(t, err, "path %q must be rejected", path)
	}
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
