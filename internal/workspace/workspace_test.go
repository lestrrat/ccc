package workspace_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lestrrat-go/ccc/internal/workspace"
	"github.com/stretchr/testify/require"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func TestRootsOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	require.Equal(t, []string{dir}, workspace.Dirs(dir))
}

func TestRootsInRepo(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")

	// The common dir is <repo>/.git, already under the root: one mount.
	dirs := workspace.Dirs(repo)
	require.Equal(t, []string{realpath(t, repo)}, dirs)
}

func TestRootsFromSubdirectory(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")

	sub := filepath.Join(repo, "a", "b")
	require.NoError(t, exec.Command("mkdir", "-p", sub).Run())

	// Mounting only cwd would leave git with no repository.
	require.Equal(t, []string{realpath(t, repo)}, workspace.Dirs(sub))
}

// A worktree's .git is a FILE pointing at <main>/.git/worktrees/<name>. Mount
// only the worktree and every git command fails on a dangling gitdir, so the
// common git directory must come along.
func TestRootsInWorktree(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", wt, "-b", "feature")

	dirs := workspace.Dirs(wt)
	require.Len(t, dirs, 2, "worktree plus the common git dir")
	require.Equal(t, realpath(t, wt), dirs[0])
	require.Equal(t, filepath.Join(realpath(t, repo), ".git"), dirs[1])

	// And git actually works from there, proving the second root is the one
	// the gitdir file points into.
	git(t, wt, "status", "-s")
}

// A repository path with a space (macOS "Application Support", say) must not
// disable git awareness. Splitting git's output on whitespace would over-split
// the path and fall back to mounting only cwd.
func TestRootsRepoPathWithSpace(t *testing.T) {
	base := filepath.Join(t.TempDir(), "has space")
	require.NoError(t, exec.Command("mkdir", "-p", base).Run())
	git(t, base, "init", "-q")

	dirs := workspace.Dirs(base)
	require.Equal(t, []string{realpath(t, base)}, dirs, "the space must not break resolution")
}

// A repository path with a newline is legal on Unix. Querying the two paths in
// one git invocation would make them impossible to tell apart, so each must be
// asked for separately to keep git awareness working.
func TestRootsRepoPathWithNewline(t *testing.T) {
	base := filepath.Join(t.TempDir(), "has\nnewline")
	require.NoError(t, exec.Command("mkdir", "-p", base).Run())
	git(t, base, "init", "-q")

	dirs := workspace.Dirs(base)
	require.Equal(t, []string{realpath(t, base)}, dirs, "the newline must not break resolution")
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	require.NoError(t, err)
	return r
}
