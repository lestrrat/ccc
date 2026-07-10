package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/container"
	"github.com/lestrrat-go/ccc/internal/profile"
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

// .ccc.json lives in the container-writable repo, so a contained process could
// write {"dirs":["/"]} or {"dirs":["~"]} to escalate the next run's mounts to
// host root/home read-write. preflight must refuse those before mounting.
func TestPreflightRefusesUntrustedCccJsonDirs(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", repo, "init", "-q").Run())
	cwd, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	// Canonicalize home too: the guard resolves symlinks, and on macOS a temp
	// dir under /var resolves to /private/var, which would otherwise not match
	// an unresolved id.Home. Real id.Home (from user.Current) is already canonical.
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	// A symlink inside the container-writable repo pointing at / or $HOME must be
	// refused by its resolved target, not merely its literal path.
	rootLink := filepath.Join(cwd, "rootlink")
	require.NoError(t, os.Symlink("/", rootLink))
	homeLink := filepath.Join(cwd, "homelink")
	require.NoError(t, os.Symlink(home, homeLink))

	for _, bad := range []string{"/", home, filepath.Dir(home), rootLink, homeLink} {
		a := &app{
			cwd:     cwd,
			id:      container.Identity{Home: home},
			cfg:     &config.Config{},
			dirFile: &config.Dir{Dirs: []string{bad}},
		}
		_, err := a.preflight("")
		require.Error(t, err, "must refuse .ccc.json dir %q", bad)
		require.Contains(t, err.Error(), config.DirConfigName)
	}

	// A benign .ccc.json dir (a sibling of home, not / or home) is fine.
	a := &app{
		cwd:     cwd,
		id:      container.Identity{Home: home},
		cfg:     &config.Config{},
		dirFile: &config.Dir{Dirs: []string{cwd}}, // the repo itself
	}
	// mounts() needs a store; this only checks the guard does not fire on a safe
	// dir, so stop before mounts by asserting the dir passes checkMountDir.
	require.NoError(t, a.checkMountDir(cwd))
}

// When $HOME itself is a symlink spelling (id.Home from user.Current), a dir
// that resolves to the real home path must still be refused: checkMountDir
// canonicalizes both sides.
func TestCheckMountDirCanonicalizesHome(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "realhome")
	require.NoError(t, os.MkdirAll(realHome, 0o755))
	realHome, err := filepath.EvalSymlinks(realHome)
	require.NoError(t, err)
	homeLink := filepath.Join(base, "homelink")
	require.NoError(t, os.Symlink(realHome, homeLink))

	a := &app{id: container.Identity{Home: homeLink}} // id.Home is the symlink spelling
	require.Error(t, a.checkMountDir(realHome), "a dir resolving to the real home must be refused")
	require.NoError(t, a.checkMountDir(filepath.Join(realHome, "sub", "proj")))
}

// Outside a git repository the implicit workspace dir is the cwd. `ccc` run
// from $HOME would otherwise mount the whole home read-write — the exposure the
// narrow default exists to prevent, reached by accident rather than by config.
func TestCheckMountDir(t *testing.T) {
	a := &app{id: container.Identity{Home: "/home/u"}}

	t.Run("refuses the filesystem root", func(t *testing.T) {
		require.ErrorContains(t, a.checkMountDir("/"), "refusing to mount /")
	})

	t.Run("refuses the home directory", func(t *testing.T) {
		require.ErrorContains(t, a.checkMountDir("/home/u"), "your home directory")
	})

	t.Run("refuses an ancestor of the home directory", func(t *testing.T) {
		require.ErrorContains(t, a.checkMountDir("/home"), "contains your home directory")
	})

	t.Run("allows a repository under home", func(t *testing.T) {
		require.NoError(t, a.checkMountDir("/home/u/dev/src/proj"))
	})

	t.Run("allows a directory outside home", func(t *testing.T) {
		require.NoError(t, a.checkMountDir("/opt/work"))
	})

	t.Run("allows a sibling of home", func(t *testing.T) {
		require.NoError(t, a.checkMountDir("/home/other"))
	})
}

// mounts() must bind the SAME canonical path the guard checked, not the original
// symlink — otherwise a preflight-checked link could still resolve elsewhere.
func TestPreflightMountsCanonicalCccJsonDir(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	store := profile.NewStore(root, home)
	require.NoError(t, store.Create("p"))

	repo := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", repo, "init", "-q").Run())
	cwd, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)

	realDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	link := filepath.Join(cwd, "sibling")
	require.NoError(t, os.Symlink(realDir, link))

	a := &app{
		cwd:     cwd,
		id:      container.Identity{Home: home},
		cfg:     &config.Config{},
		store:   store,
		dirFile: &config.Dir{Dirs: []string{link}},
	}
	mounts, err := a.preflight("p")
	require.NoError(t, err)

	var found bool
	for _, m := range mounts {
		require.NotEqual(t, link, m.Source, "must not mount the symlink path")
		if m.Source == realDir {
			found = true
		}
	}
	require.True(t, found, "the sibling dir must be mounted at its resolved path")
}
