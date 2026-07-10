package profile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lestrrat-go/ccc/internal/profile"
	"github.com/stretchr/testify/require"
)

func TestMaterialize(t *testing.T) {
	s, _ := newStore(t)
	require.NoError(t, s.Materialize("work"))

	fi, err := os.Stat(s.ClaudeDir("work"))
	require.NoError(t, err)
	require.True(t, fi.IsDir())

	// claude.json must exist as a regular file before it is bind-mounted,
	// otherwise the runtime creates a directory in its place.
	fi, err = os.Stat(s.ClaudeJSON("work"))
	require.NoError(t, err)
	require.True(t, fi.Mode().IsRegular())
}

func TestMaterializeIsIdempotent(t *testing.T) {
	s, _ := newStore(t)
	require.NoError(t, s.Materialize("work"))
	require.NoError(t, os.WriteFile(s.ClaudeJSON("work"), []byte(`{"keep":1}`), 0o600))

	require.NoError(t, s.Materialize("work"))

	b, err := os.ReadFile(s.ClaudeJSON("work"))
	require.NoError(t, err)
	require.JSONEq(t, `{"keep":1}`, string(b), "must not clobber existing state")
}

func TestCreateRejectsDuplicate(t *testing.T) {
	s, _ := newStore(t, "work")
	require.ErrorContains(t, s.Create("work"), "already exists")
}

func TestSeedCopiesBothHalvesOfState(t *testing.T) {
	s, _ := newStore(t)

	// A realistic ~/.claude plus its ~/.claude.json sibling.
	src := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "agents"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "CLAUDE.md"), []byte("# hi"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "agents", "a.md"), []byte("agent"), 0o600))
	require.NoError(t, os.WriteFile(src+".json", []byte(`{"projects":{}}`), 0o600))

	require.NoError(t, s.Seed("work", src))

	b, err := os.ReadFile(filepath.Join(s.ClaudeDir("work"), "CLAUDE.md"))
	require.NoError(t, err)
	require.Equal(t, "# hi", string(b))

	b, err = os.ReadFile(filepath.Join(s.ClaudeDir("work"), "agents", "a.md"))
	require.NoError(t, err)
	require.Equal(t, "agent", string(b))

	// The sidecar is the half that symlinking ~/.claude alone would miss.
	b, err = os.ReadFile(s.ClaudeJSON("work"))
	require.NoError(t, err)
	require.JSONEq(t, `{"projects":{}}`, string(b))
}

func TestSeedWithoutSidecar(t *testing.T) {
	s, _ := newStore(t)
	src := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.MkdirAll(src, 0o700))

	require.NoError(t, s.Seed("work", src))

	b, err := os.ReadFile(s.ClaudeJSON("work"))
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(b), "falls back to the materialized empty registry")
}

func TestSeedSkipsIrregularFiles(t *testing.T) {
	s, _ := newStore(t)
	src := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.MkdirAll(src, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "real.txt"), []byte("x"), 0o600))
	// A dangling symlink stands in for daemon sockets and lock files.
	require.NoError(t, os.Symlink("/nonexistent/target", filepath.Join(src, "dangling")))

	require.NoError(t, s.Seed("work", src))

	_, err := os.Stat(filepath.Join(s.ClaudeDir("work"), "real.txt"))
	require.NoError(t, err)
	_, err = os.Lstat(filepath.Join(s.ClaudeDir("work"), "dangling"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestListAndRemove(t *testing.T) {
	s, _ := newStore(t, "zeta", "alpha")

	names, err := s.List()
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "zeta"}, names, "lexical order")

	require.NoError(t, s.Remove("alpha"))
	require.False(t, s.Exists("alpha"))

	require.ErrorIs(t, s.Remove("alpha"), profile.ErrNotExist)
}

// Remove is the destructive op; an unvalidated name with `..` would make
// os.RemoveAll escape profiles/ and delete arbitrary host directories.
func TestRemoveRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	s := profile.NewStore(root, home)

	// A directory that must survive: a sibling of profiles/ under the ccc root.
	victim := filepath.Join(root, "VICTIM")
	require.NoError(t, os.MkdirAll(victim, 0o755))

	require.ErrorContains(t, s.Remove("../VICTIM"), "invalid profile name")
	require.DirExists(t, victim, "traversal must not delete it")

	require.ErrorContains(t, s.Remove("../../etc"), "invalid profile name")
}

func TestListEmptyStore(t *testing.T) {
	s := profile.NewStore(t.TempDir(), t.TempDir())
	names, err := s.List()
	require.NoError(t, err)
	require.Empty(t, names)
}

func TestIsEmpty(t *testing.T) {
	s := profile.NewStore(t.TempDir(), t.TempDir())
	empty, err := s.IsEmpty()
	require.NoError(t, err)
	require.True(t, empty, "an unwritten store is empty, not an error")

	require.NoError(t, s.Create("default"))
	empty, err = s.IsEmpty()
	require.NoError(t, err)
	require.False(t, empty)
}

func TestValidateName(t *testing.T) {
	for _, bad := range []string{"", "../etc", "a/b", ".hidden", "-lead"} {
		require.Error(t, profile.ValidateName(bad), "must reject %q", bad)
	}
	for _, ok := range []string{"work", "acct-2", "a.b_c"} {
		require.NoError(t, profile.ValidateName(ok), "must accept %q", ok)
	}
}

// A symlinked ~/.claude (dotfile managers do this) must still seed its contents:
// os.Stat follows the link but WalkDir does not descend a symlink root.
func TestSeedFollowsSymlinkedSource(t *testing.T) {
	s, _ := newStore(t)

	realDir := filepath.Join(t.TempDir(), "dotfiles-claude")
	require.NoError(t, os.MkdirAll(realDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "CLAUDE.md"), []byte("# hi"), 0o600))

	link := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.Symlink(realDir, link))
	// sidecar sits beside the symlink, not the target
	require.NoError(t, os.WriteFile(link+".json", []byte(`{"projects":{}}`), 0o600))

	require.NoError(t, s.Seed("work", link))

	b, err := os.ReadFile(filepath.Join(s.ClaudeDir("work"), "CLAUDE.md"))
	require.NoError(t, err)
	require.Equal(t, "# hi", string(b), "symlinked source contents must be copied, not silently skipped")

	b, err = os.ReadFile(s.ClaudeJSON("work"))
	require.NoError(t, err)
	require.JSONEq(t, `{"projects":{}}`, string(b), "sidecar beside the symlink must still copy")
}
