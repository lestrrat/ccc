package profile_test

import (
	"os"
	"path/filepath"
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

		got, err := s.Resolve("work", cfg, t.TempDir())
		require.NoError(t, err)
		require.Equal(t, "work", got.Name)
		require.Equal(t, profile.SourceFlag, got.Source)
	})

	t.Run("dir config beats default_profile", func(t *testing.T) {
		s, _ := newStore(t, "work", "personal")
		cfg := &config.Config{DefaultProfile: "personal"}

		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, config.DirConfigName), []byte(`profile = "work"`), 0o600))

		got, err := s.Resolve("", cfg, dir)
		require.NoError(t, err)
		require.Equal(t, "work", got.Name)
		require.Equal(t, profile.SourceDirFile, got.Source)
		require.NotEmpty(t, got.Origin)
	})

	t.Run("dir config found in ancestor", func(t *testing.T) {
		s, _ := newStore(t, "work")
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, config.DirConfigName), []byte(`profile = "work"`), 0o600))

		deep := filepath.Join(dir, "a", "b", "c")
		require.NoError(t, os.MkdirAll(deep, 0o755))

		got, err := s.Resolve("", &config.Config{}, deep)
		require.NoError(t, err)
		require.Equal(t, "work", got.Name)
	})

	t.Run("falls back to default_profile", func(t *testing.T) {
		s, _ := newStore(t, "personal")
		cfg := &config.Config{DefaultProfile: "personal"}

		got, err := s.Resolve("", cfg, t.TempDir())
		require.NoError(t, err)
		require.Equal(t, "personal", got.Name)
		require.Equal(t, profile.SourceDefault, got.Source)
	})

	t.Run("no profile selected is an error, never a guess", func(t *testing.T) {
		s, _ := newStore(t, "work", "personal")

		_, err := s.Resolve("", &config.Config{}, t.TempDir())
		require.ErrorContains(t, err, "no profile selected")
		require.ErrorContains(t, err, "personal, work", "error must list what is available")
	})

	t.Run("unknown profile is an error", func(t *testing.T) {
		s, _ := newStore(t, "work")

		_, err := s.Resolve("nope", &config.Config{}, t.TempDir())
		require.ErrorIs(t, err, profile.ErrNotExist)
	})

	t.Run("rejects names that escape the profiles dir", func(t *testing.T) {
		s, _ := newStore(t, "work")

		_, err := s.Resolve("../../etc", &config.Config{}, t.TempDir())
		require.ErrorContains(t, err, "invalid profile name")
	})
}

func TestResolutionString(t *testing.T) {
	r := profile.Resolution{Name: "work", Source: profile.SourceDirFile, Origin: "/src/.ccc.toml"}
	require.Equal(t, "work (via /src/.ccc.toml)", r.String())

	r = profile.Resolution{Name: "personal", Source: profile.SourceDefault}
	require.Equal(t, "personal (via default_profile)", r.String())
}
