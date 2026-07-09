package cli

// Internal test: parse is the subtlest part of the CLI surface and has no
// exported entry point that does not also touch the filesystem.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	t.Run("bare invocation starts claude with no args", func(t *testing.T) {
		inv, err := parse(nil)
		require.NoError(t, err)
		require.Empty(t, inv.command)
		require.Empty(t, inv.claudeArgs)
	})

	t.Run("everything after -- goes to claude verbatim", func(t *testing.T) {
		inv, err := parse([]string{"--", "--resume", "-p", "explain this"})
		require.NoError(t, err)
		require.Empty(t, inv.command)
		require.Equal(t, []string{"--resume", "-p", "explain this"}, inv.claudeArgs)
	})

	t.Run("ccc flags precede --", func(t *testing.T) {
		inv, err := parse([]string{"--profile", "work", "--", "--resume"})
		require.NoError(t, err)
		require.Equal(t, "work", inv.globals.profile)
		require.Equal(t, []string{"--resume"}, inv.claudeArgs)
	})

	t.Run("equals forms", func(t *testing.T) {
		inv, err := parse([]string{"--profile=work", "--runtime=docker"})
		require.NoError(t, err)
		require.Equal(t, "work", inv.globals.profile)
		require.Equal(t, "docker", inv.globals.runtime)
	})

	// The bug that motivated the strict split: -p is --profile in ccc and
	// --print in claude, so a permissive parser swallowed `ccc -p "explain this"`
	// as a profile name.
	t.Run("-p is ccc's profile, never claude's --print", func(t *testing.T) {
		inv, err := parse([]string{"-p", "work"})
		require.NoError(t, err)
		require.Equal(t, "work", inv.globals.profile)

		inv, err = parse([]string{"--", "-p", "explain this"})
		require.NoError(t, err)
		require.Empty(t, inv.globals.profile)
		require.Equal(t, []string{"-p", "explain this"}, inv.claudeArgs)
	})

	t.Run("unknown flag is an error, not a guess", func(t *testing.T) {
		_, err := parse([]string{"--resume"})
		require.ErrorContains(t, err, `unknown flag "--resume"`)
		require.ErrorContains(t, err, "ccc -- --resume", "must show the fix")
	})

	t.Run("unknown command is an error, not a guess", func(t *testing.T) {
		_, err := parse([]string{"explain this"})
		require.ErrorContains(t, err, `unknown command "explain this"`)
		require.ErrorContains(t, err, "ccc -- explain this")
	})

	t.Run("commands dispatch with their own args", func(t *testing.T) {
		inv, err := parse([]string{"pin", "--to", "2.1.205"})
		require.NoError(t, err)
		require.Equal(t, "pin", inv.command)
		require.Equal(t, []string{"--to", "2.1.205"}, inv.cmdArgs)
	})

	t.Run("globals may precede a command", func(t *testing.T) {
		inv, err := parse([]string{"-p", "work", "pin"})
		require.NoError(t, err)
		require.Equal(t, "work", inv.globals.profile)
		require.Equal(t, "pin", inv.command)
	})

	t.Run("--help anywhere before -- is ccc's help", func(t *testing.T) {
		for _, args := range [][]string{
			{"--help"}, {"-h"}, {"--profile", "work", "--help"}, {"pin", "--help"},
		} {
			inv, err := parse(args)
			require.NoError(t, err)
			require.True(t, inv.globals.help, "%v", args)
		}
	})

	t.Run("--help after -- is claude's", func(t *testing.T) {
		inv, err := parse([]string{"--", "--help"})
		require.NoError(t, err)
		require.False(t, inv.globals.help)
		require.Equal(t, []string{"--help"}, inv.claudeArgs)
	})

	t.Run("dangling flag value is an error", func(t *testing.T) {
		_, err := parse([]string{"--profile"})
		require.ErrorContains(t, err, "needs a profile name")

		_, err = parse([]string{"--runtime"})
		require.ErrorContains(t, err, "needs a runtime name")
	})

	t.Run("only the first -- splits", func(t *testing.T) {
		inv, err := parse([]string{"--", "--", "x"})
		require.NoError(t, err)
		require.Equal(t, []string{"--", "x"}, inv.claudeArgs)
	})
}

func TestWantsHelp(t *testing.T) {
	require.True(t, wantsHelp([]string{"--help"}))
	require.True(t, wantsHelp([]string{"-h"}))
	require.True(t, wantsHelp([]string{"--to", "2.1.205", "--help"}))
	require.False(t, wantsHelp(nil))
	require.False(t, wantsHelp([]string{"--to", "2.1.205"}))
	require.False(t, wantsHelp([]string{"create", "work"}))
}

func TestReservedWordsAreDispatchable(t *testing.T) {
	for _, name := range []string{"profile", "pin", "check", "help", "version"} {
		require.Contains(t, reserved, name)
	}

	// With a strict split a ccc command can no longer shadow a claude one:
	// `ccc doctor` is an error naming `ccc -- doctor`. The names still avoid
	// collision, so that error is never needed for a real claude subcommand.
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
