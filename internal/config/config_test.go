package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	require.Empty(t, cfg.Mounts.Roots)
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
  "mounts": {"roots": ["~/dev/src", "/opt/work"]},
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
	require.Equal(t, filepath.Join(home, "dev", "src"), cfg.Mounts.Roots[0], "~ expands")
	require.Equal(t, "/opt/work", cfg.Mounts.Roots[1])

	// A relative extra_dockerfile resolves against the ccc config dir.
	require.Equal(t, filepath.Join(root, "Dockerfile.extra"), cfg.Image.ExtraDockerfile)
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

	t.Run("leaves an existing config completely untouched", func(t *testing.T) {
		root := t.TempDir()
		body := `{"runtime": "docker", "mounts": {"roots": ["/opt/work"]}}`
		write(t, filepath.Join(root, config.FileName), body)

		created, err := config.Create(root, "default")
		require.NoError(t, err)
		require.False(t, created, "must report that it did not write")

		b, err := os.ReadFile(filepath.Join(root, config.FileName))
		require.NoError(t, err)
		require.Equal(t, body, string(b), "byte-for-byte unchanged")
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

		name, origin, ok, err := config.FindDir(deep)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "work", name)
		require.Equal(t, filepath.Join(root, config.DirConfigName), origin)
	})

	t.Run("nearest wins", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `{"profile": "outer"}`)

		inner := filepath.Join(root, "inner")
		require.NoError(t, os.MkdirAll(inner, 0o755))
		write(t, filepath.Join(inner, config.DirConfigName), `{"profile": "inner"}`)

		name, _, ok, err := config.FindDir(inner)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "inner", name)
	})

	t.Run("absent", func(t *testing.T) {
		_, _, ok, err := config.FindDir(t.TempDir())
		require.NoError(t, err)
		require.False(t, ok)
	})

	t.Run("present but missing profile key", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `{}`)

		_, _, _, err := config.FindDir(root)
		require.ErrorContains(t, err, `missing "profile" key`)
	})

	t.Run("present but malformed", func(t *testing.T) {
		root := t.TempDir()
		write(t, filepath.Join(root, config.DirConfigName), `not json`)

		_, _, _, err := config.FindDir(root)
		require.ErrorContains(t, err, "failed to parse")
	})
}

func TestExpand(t *testing.T) {
	require.Equal(t, "/h", must(config.Expand("~", "/h")))
	require.Equal(t, "/h/x/y", must(config.Expand("~/x/y", "/h")))
	require.Equal(t, "/abs", must(config.Expand("/abs", "/h")))
	require.Equal(t, "rel/path", must(config.Expand("rel/path", "/h")))
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}
