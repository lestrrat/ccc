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
	require.Equal(t, []string{dir}, workspace.Roots(dir))
}

func TestRootsInRepo(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")

	// The common dir is <repo>/.git, already under the root: one mount.
	roots := workspace.Roots(repo)
	require.Equal(t, []string{realpath(t, repo)}, roots)
}

func TestRootsFromSubdirectory(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")

	sub := filepath.Join(repo, "a", "b")
	require.NoError(t, exec.Command("mkdir", "-p", sub).Run())

	// Mounting only cwd would leave git with no repository.
	require.Equal(t, []string{realpath(t, repo)}, workspace.Roots(sub))
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

	roots := workspace.Roots(wt)
	require.Len(t, roots, 2, "worktree plus the common git dir")
	require.Equal(t, realpath(t, wt), roots[0])
	require.Equal(t, filepath.Join(realpath(t, repo), ".git"), roots[1])

	// And git actually works from there, proving the second root is the one
	// the gitdir file points into.
	git(t, wt, "status", "-s")
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	require.NoError(t, err)
	return r
}
