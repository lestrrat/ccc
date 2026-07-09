package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	t.Setenv("CCC_RUNTIME", "")

	cfg, err := config.Load(t.TempDir())
	require.NoError(t, err, "ccc must work with no configuration at all")
	require.Equal(t, "auto", cfg.Runtime)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, []string{home}, cfg.Mounts.Roots, "defaults to the whole home dir")
	require.Equal(t, filepath.Join(home, ".config", "gh"), cfg.Mounts.GhConfig)
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	body := `
runtime = "podman"
default_profile = "personal"

[image]
extra_dockerfile = "Dockerfile.extra"

[mounts]
roots = ["~/dev/src", "/opt/work"]

[env]
deny = ["FOO"]
allow = ["ANTHROPIC_API_KEY"]
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(body), 0o600))

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

	// Relative extra_dockerfile resolves against the ccc config dir.
	require.Equal(t, filepath.Join(root, "Dockerfile.extra"), cfg.Image.ExtraDockerfile)
}

func TestLoadInvalidTOML(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte("runtime = ["), 0o600))

	_, err := config.Load(root)
	require.ErrorContains(t, err, "failed to parse")
}

func TestCCCRuntimeEnvOverrides(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.toml"), []byte(`runtime = "podman"`), 0o600))
	t.Setenv("CCC_RUNTIME", "docker")

	cfg, err := config.Load(root)
	require.NoError(t, err)
	require.Equal(t, "docker", cfg.Runtime)
}

func TestFindDir(t *testing.T) {
	t.Run("finds in ancestor", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, config.DirConfigName), []byte(`profile = "work"`), 0o600))

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
		require.NoError(t, os.WriteFile(filepath.Join(root, config.DirConfigName), []byte(`profile = "outer"`), 0o600))

		inner := filepath.Join(root, "inner")
		require.NoError(t, os.MkdirAll(inner, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(inner, config.DirConfigName), []byte(`profile = "inner"`), 0o600))

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
		require.NoError(t, os.WriteFile(filepath.Join(root, config.DirConfigName), []byte("# empty"), 0o600))

		_, _, _, err := config.FindDir(root)
		require.ErrorContains(t, err, "missing `profile` key")
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
