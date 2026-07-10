package profile_test

import (
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/profile"
	"github.com/stretchr/testify/require"
)

// newStore returns a store rooted in a temp dir, pre-populated with names.
func newStore(t *testing.T, names ...string) (*profile.Store, string) {
	t.Helper()
	root := t.TempDir()
	home := t.TempDir()
	s := profile.NewStore(root, home)
	for _, n := range names {
		require.NoError(t, s.Create(n))
	}
	return s, home
}

func TestResolve(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		s, _ := newStore(t, "work", "personal")
		cfg := &config.Config{DefaultProfile: "personal"}

		got, err := s.Resolve("work", cfg, &config.Dir{Profile: "personal"}, "/src/.ccc.json")
		require.NoError(t, err)
		require.Equal(t, "work", got.Name)
		require.Equal(t, profile.SourceFlag, got.Source)
	})

	t.Run("dir config beats default_profile", func(t *testing.T) {
		s, _ := newStore(t, "work", "personal")
		cfg := &config.Config{DefaultProfile: "personal"}

		// The loaded .ccc.json is passed in, not reread by Resolve: the caller
		// loads it once so profile choice and mounts agree on a single version.
		got, err := s.Resolve("", cfg, &config.Dir{Profile: "work"}, "/src/.ccc.json")
		require.NoError(t, err)
		require.Equal(t, "work", got.Name)
		require.Equal(t, profile.SourceDirFile, got.Source)
		require.Equal(t, "/src/.ccc.json", got.Origin)
	})

	t.Run("dir file without a profile falls through", func(t *testing.T) {
		s, _ := newStore(t, "personal")
		cfg := &config.Config{DefaultProfile: "personal"}

		// A .ccc.json may carry only `dirs`; that names no profile, so resolution
		// falls through to default_profile rather than erroring.
		got, err := s.Resolve("", cfg, &config.Dir{Dirs: []string{"/srv"}}, "/src/.ccc.json")
		require.NoError(t, err)
		require.Equal(t, "personal", got.Name)
		require.Equal(t, profile.SourceDefault, got.Source)
	})

	t.Run("falls back to default_profile", func(t *testing.T) {
		s, _ := newStore(t, "personal")
		cfg := &config.Config{DefaultProfile: "personal"}

		got, err := s.Resolve("", cfg, nil, "")
		require.NoError(t, err)
		require.Equal(t, "personal", got.Name)
		require.Equal(t, profile.SourceDefault, got.Source)
	})

	t.Run("no profile selected is an error, never a guess", func(t *testing.T) {
		s, _ := newStore(t, "work", "personal")

		_, err := s.Resolve("", &config.Config{}, nil, "")
		require.ErrorIs(t, err, profile.ErrNoSelection)
		require.ErrorContains(t, err, "personal, work", "error must list what is available")
	})

	t.Run("unknown profile is an error", func(t *testing.T) {
		s, _ := newStore(t, "work")

		_, err := s.Resolve("nope", &config.Config{}, nil, "")
		require.ErrorIs(t, err, profile.ErrNotExist)
		// Only ErrNoSelection may bootstrap a first profile. A typo'd --profile
		// must never silently create one.
		require.NotErrorIs(t, err, profile.ErrNoSelection)
	})

	t.Run("rejects names that escape the profiles dir", func(t *testing.T) {
		s, _ := newStore(t, "work")

		_, err := s.Resolve("../../etc", &config.Config{}, nil, "")
		require.ErrorContains(t, err, "invalid profile name")
	})
}

func TestResolutionString(t *testing.T) {
	r := profile.Resolution{Name: "work", Source: profile.SourceDirFile, Origin: "/src/.ccc.json"}
	require.Equal(t, "work (via /src/.ccc.json)", r.String())

	r = profile.Resolution{Name: "personal", Source: profile.SourceDefault}
	require.Equal(t, "personal (via default_profile)", r.String())
}
