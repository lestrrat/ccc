package cli

// Internal test: parseGlobals is the subtlest part of the CLI surface and has
// no exported entry point that does not also touch the filesystem.

import (
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

	t.Run("dangling flag value does not panic", func(t *testing.T) {
		g, rest, _ := parseGlobals([]string{"--profile"})
		require.Empty(t, g.profile)
		require.Equal(t, []string{"--profile"}, rest)
	})
}

func TestReservedWordsAreDispatchable(t *testing.T) {
	for _, name := range []string{"login", "profile", "build", "doctor", "help", "version"} {
		require.Contains(t, reserved, name)
	}
}
