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

// Dirs returns the host directories to mount read-write for a session in cwd.
//
// In a git repository that is the repository root, plus the common git
// directory when it lives outside the root. That second path is what makes
// worktrees work: a worktree's .git is a *file* pointing at
// <main-repo>/.git/worktrees/<name>, so mounting only the worktree gives the
// container a dangling gitdir and every git command fails.
//
// Outside a repository, or when git is unavailable, it is just cwd.
func Dirs(cwd string) []string {
	top, common, ok := gitDirs(cwd)
	if !ok {
		return []string{cwd}
	}

	dirs := []string{top}
	// In a normal checkout the common dir is <top>/.git, already covered.
	if !under(common, top) {
		dirs = append(dirs, common)
	}
	return dirs
}

// gitDirs asks git for the repository root and common git directory.
func gitDirs(cwd string) (string, string, bool) {
	// Query each path in its own invocation so the output is exactly one path:
	// a repository path may contain spaces (macOS "Application Support") or even
	// a newline, and a combined query would make the two paths ambiguous to
	// parse, silently disabling worktree awareness.
	top, ok := gitPath(cwd, "--show-toplevel")
	if !ok {
		return "", "", false
	}
	common, ok := gitPath(cwd, "--git-common-dir")
	if !ok {
		return "", "", false
	}
	return top, common, true
}

// gitPath runs a single-path `git rev-parse` query and returns the one path it
// prints, with its sole trailing newline trimmed.
func gitPath(cwd, query string) (string, bool) {
	cmd := exec.Command("git", "-C", cwd,
		"rev-parse", "--path-format=absolute", query)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	path := strings.TrimSuffix(string(out), "\n")
	if path == "" {
		return "", false
	}
	return filepath.Clean(path), true
}

func under(path string, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}
