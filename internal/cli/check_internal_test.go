package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/container"
	"github.com/stretchr/testify/require"
)

// A repo reached through a symlinked ancestor must not make ccc refuse to run
// inside its own repo: git resolves symlinks (physical path), os.Getwd does not
// (logical), and an unresolved cwd is not "under" the git toplevel. newApp
// resolves cwd with EvalSymlinks; this checks the resulting invariant holds.
func TestSymlinkedCwdIsUnderRepo(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "realDir")
	require.NoError(t, os.MkdirAll(realDir, 0o755))
	require.NoError(t, exec.Command("git", "-C", realDir, "init", "-q").Run())
	require.NoError(t, os.Symlink("realDir", filepath.Join(base, "link")))

	logical := filepath.Join(base, "link")
	resolved, err := filepath.EvalSymlinks(logical)
	require.NoError(t, err)

	a := &app{cwd: resolved, cfg: &config.Config{}}
	require.NoError(t, a.checkWorkdir(), "resolved cwd must be inside the repo")

	// Without resolution, the logical path is not under the physical toplevel.
	a.cwd = logical
	require.Error(t, a.checkWorkdir(), "the bug this guards against")
}

// Outside a git repository the implicit workspace dir is the cwd. `ccc` run
// from $HOME would otherwise mount the whole home read-write — the exposure the
// narrow default exists to prevent, reached by accident rather than by config.
func TestCheckImplicitDir(t *testing.T) {
	a := &app{id: container.Identity{Home: "/home/u"}}

	t.Run("refuses the filesystem root", func(t *testing.T) {
		require.ErrorContains(t, a.checkImplicitDir("/"), "refusing to mount /")
	})

	t.Run("refuses the home directory", func(t *testing.T) {
		require.ErrorContains(t, a.checkImplicitDir("/home/u"), "your home directory")
	})

	t.Run("refuses an ancestor of the home directory", func(t *testing.T) {
		require.ErrorContains(t, a.checkImplicitDir("/home"), "contains your home directory")
	})

	t.Run("allows a repository under home", func(t *testing.T) {
		require.NoError(t, a.checkImplicitDir("/home/u/dev/src/proj"))
	})

	t.Run("allows a directory outside home", func(t *testing.T) {
		require.NoError(t, a.checkImplicitDir("/opt/work"))
	})

	t.Run("allows a sibling of home", func(t *testing.T) {
		require.NoError(t, a.checkImplicitDir("/home/other"))
	})
}
