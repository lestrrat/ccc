// Package workspace decides which host directories a session needs.
//
// ccc mounts the working directory, not the home directory. But "the working
// directory" is rarely enough on its own: git needs more than the directory you
// are standing in.
package workspace

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// Roots returns the host directories to mount read-write for a session in cwd.
//
// In a git repository that is the repository root, plus the common git
// directory when it lives outside the root. That second path is what makes
// worktrees work: a worktree's .git is a *file* pointing at
// <main-repo>/.git/worktrees/<name>, so mounting only the worktree gives the
// container a dangling gitdir and every git command fails.
//
// Outside a repository, or when git is unavailable, it is just cwd.
func Roots(cwd string) []string {
	top, common, ok := gitDirs(cwd)
	if !ok {
		return []string{cwd}
	}

	roots := []string{top}
	// In a normal checkout the common dir is <top>/.git, already covered.
	if !under(common, top) {
		roots = append(roots, common)
	}
	return roots
}

// gitDirs asks git for the repository root and common git directory.
func gitDirs(cwd string) (string, string, bool) {
	cmd := exec.Command("git", "-C", cwd,
		"rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		return "", "", false
	}

	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 2 {
		return "", "", false
	}
	return filepath.Clean(lines[0]), filepath.Clean(lines[1]), true
}

func under(path string, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}
