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

// A source entry that is a symlink to a file OUTSIDE the tree must never have
// its target copied into the profile. This is the first line of defense:
// copyTree's WalkDir sees the symlink as non-regular and skips it before
// copyFile is ever called. copyFile's own O_NOFOLLOW source open (the TOCTOU
// race defense) is exercised by the destination-symlink tests below, which
// actually reach it.
func TestSeedDoesNotFollowSymlinkOutsideTree(t *testing.T) {
	s, _ := newStore(t)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("SECRET"), 0o600))

	src := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.MkdirAll(src, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "real.txt"), []byte("x"), 0o600))
	// A symlink to a live file outside the source tree stands in for the file an
	// attacker swaps in between the walk and the open.
	require.NoError(t, os.Symlink(outside, filepath.Join(src, "escape.txt")))

	require.NoError(t, s.Seed("work", src))

	// The regular file copies; the escaping symlink is skipped, target uncopied.
	_, err := os.Stat(filepath.Join(s.ClaudeDir("work"), "real.txt"))
	require.NoError(t, err)
	_, err = os.Lstat(filepath.Join(s.ClaudeDir("work"), "escape.txt"))
	require.ErrorIs(t, err, os.ErrNotExist, "symlink to outside file must not be copied")
}

// A symlinked ~/.claude.json sidecar (dotfile managers commonly symlink it)
// must be followed and its target copied, not silently dropped: following the
// symlink is INTENDED for this single known file, unlike the tree copy. Before
// the fix, copyFile skipped it on ELOOP, leaving the materialized empty {}.
func TestSeedCopiesSymlinkedSidecar(t *testing.T) {
	s, _ := newStore(t)

	src := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.MkdirAll(src, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "CLAUDE.md"), []byte("# hi"), 0o600))

	// The real registry lives elsewhere; the sidecar beside ~/.claude is a link.
	target := filepath.Join(t.TempDir(), "real.claude.json")
	require.NoError(t, os.WriteFile(target, []byte(`{"projects":{"p":1}}`), 0o600))
	require.NoError(t, os.Symlink(target, src+".json"))

	require.NoError(t, s.Seed("work", src))

	b, err := os.ReadFile(s.ClaudeJSON("work"))
	require.NoError(t, err)
	require.JSONEq(t, `{"projects":{"p":1}}`, string(b), "symlinked sidecar must be followed and copied, not dropped")
}

// A pre-populated store whose claude/agents is a symlink to an outside dir must
// not let seeding write agents/a.md through it. O_NOFOLLOW guards only the
// final path component, so copyFile lstat's every parent under the profile's
// claude/ root and refuses a symlinked one. This drives copyFile directly: the
// dest exists as a symlink at open time, exercising the parent-symlink guard.
func TestSeedRejectsSymlinkedDestParent(t *testing.T) {
	s, _ := newStore(t)
	require.NoError(t, s.Materialize("work"))

	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(s.ClaudeDir("work"), "agents")))

	src := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "agents"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "agents", "a.md"), []byte("agent"), 0o600))

	require.Error(t, s.Seed("work", src), "seeding through a symlinked parent must fail")
	_, err := os.Stat(filepath.Join(outside, "a.md"))
	require.ErrorIs(t, err, os.ErrNotExist, "must not write through the symlinked parent")
}

// A pre-existing symlink AT a destination file must not be followed and its
// target truncated: copyFile opens the destination O_NOFOLLOW. This reaches
// copyFile's destination no-follow branch (the symlink exists at open time).
func TestSeedDoesNotFollowSymlinkedDestFile(t *testing.T) {
	s, _ := newStore(t)
	require.NoError(t, s.Materialize("work"))

	outside := filepath.Join(t.TempDir(), "outside.md")
	require.NoError(t, os.WriteFile(outside, []byte("KEEP"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(s.ClaudeDir("work"), "CLAUDE.md")))

	src := filepath.Join(t.TempDir(), ".claude")
	require.NoError(t, os.MkdirAll(src, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "CLAUDE.md"), []byte("# new"), 0o600))

	require.Error(t, s.Seed("work", src), "seeding onto a symlinked dest file must fail")
	b, err := os.ReadFile(outside)
	require.NoError(t, err)
	require.Equal(t, "KEEP", string(b), "symlink target must not be truncated or overwritten")
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
