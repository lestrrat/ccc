package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lestrrat-go/ccc/internal/container"
	ccenv "github.com/lestrrat-go/ccc/internal/env"
	"github.com/lestrrat-go/ccc/internal/image"
	"github.com/lestrrat-go/ccc/internal/profile"
)

// cmdClaude is the default command: resolve a profile, ensure the image, and
// hand the terminal to Claude Code inside the container.
func cmdClaude(a *app, args []string) error {
	res, err := a.store.Resolve(a.globals.profile, a.cfg, a.cwd)
	if err != nil {
		return err
	}
	return a.exec(res, append([]string{"claude"}, args...))
}

// exec replaces the ccc process with the container runtime, so the TTY,
// signals, and exit code pass through untouched.
func (a *app) exec(res profile.Resolution, cmd []string) error {
	rt, err := a.runtime()
	if err != nil {
		return err
	}

	// Validate everything cheap before the image build: a first run that spends
	// minutes building only to reject the working directory is hostile.
	if err := a.checkWorkdir(); err != nil {
		return err
	}
	if err := a.store.Materialize(res.Name); err != nil {
		return err
	}
	mounts, err := a.mounts(res.Name)
	if err != nil {
		return err
	}

	tag, err := image.NewBuilder(rt, a.cfg, a.id).Ensure()
	if err != nil {
		return err
	}

	spec := container.Spec{
		Image:   tag,
		Workdir: a.cwd,
		Mounts:  mounts,
		Env:     ccenv.Pairs(a.env()),
		Cmd:     cmd,
		TTY:     isTerminal(os.Stdin),
	}

	// Always say which account we are about to run as. A default_profile run
	// must never be silent about the identity it picked.
	fmt.Fprintf(os.Stderr, "ccc: profile %s\n", res)

	argv := rt.RunArgs(spec, a.id)
	return syscall.Exec(argv[0], argv, os.Environ())
}

// mounts assembles the container's view of the host. Roots are mounted at
// their identical absolute paths; the profile is layered on top of $HOME.
func (a *app) mounts(name string) ([]container.Mount, error) {
	var out []container.Mount

	for _, root := range a.cfg.Mounts.Roots {
		if _, err := os.Stat(root); err != nil {
			return nil, fmt.Errorf("mount root %s is not accessible: %w", root, err)
		}
		out = append(out, container.Mount{Source: root, Target: root})
	}

	// The profile owns both halves of Claude Code's state. These sort after
	// $HOME (deeper path), so they shadow the host's real ~/.claude.
	out = append(out,
		container.Mount{Source: a.store.ClaudeDir(name), Target: filepath.Join(a.id.Home, ".claude")},
		container.Mount{Source: a.store.ClaudeJSON(name), Target: filepath.Join(a.id.Home, ".claude.json")},
	)

	// Mounted explicitly rather than relying on them falling under a root:
	// narrowing mounts.roots must not silently break git over ssh.
	// GIT_SSH_COMMAND's `-i ~/.ssh/id_x` resolves because paths are identity-mapped.
	for _, rel := range []string{".ssh", ".gitconfig"} {
		src := filepath.Join(a.id.Home, rel)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		out = append(out, container.Mount{Source: src, Target: src, ReadOnly: true})
	}

	ghConfig, err := a.ghConfig(name)
	if err != nil {
		return nil, err
	}
	if ghConfig != "" {
		out = append(out, container.Mount{
			Source:   ghConfig,
			Target:   filepath.Join(a.id.Home, ".config", "gh"),
			ReadOnly: true,
		})
	}

	// Forward the SSH agent socket at its original path, so the inherited
	// SSH_AUTH_SOCK value stays valid inside the container.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if _, err := os.Stat(sock); err == nil {
			out = append(out, container.Mount{Source: sock, Target: sock})
		}
	}
	return out, nil
}

// ghConfig resolves the gh CLI config dir: the profile's override, else the
// global default. Returns "" when the directory does not exist.
func (a *app) ghConfig(name string) (string, error) {
	pc, err := a.store.Config(name)
	if err != nil {
		return "", err
	}

	dir := a.cfg.Mounts.GhConfig
	if pc.GhConfig != "" {
		dir = pc.GhConfig
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("failed to stat gh config %s: %w", dir, err)
	}
	return dir, nil
}

// env forwards the host environment minus the denylist, then re-adds the
// variables ccc rewrites itself.
func (a *app) env() map[string]string {
	m := ccenv.Filter(os.Environ(), a.cfg.Env.Deny, a.cfg.Env.Allow)
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if _, err := os.Stat(sock); err == nil {
			m["SSH_AUTH_SOCK"] = sock
		}
	}
	return m
}

// checkWorkdir refuses to run outside the configured roots rather than
// silently mounting the working directory behind the user's back.
func (a *app) checkWorkdir() error {
	for _, root := range a.cfg.Mounts.Roots {
		if underRoot(a.cwd, root) {
			return nil
		}
	}
	return fmt.Errorf("working directory %s is not under any configured mount root (%s):\nadd it to mounts.roots in %s/config.toml",
		a.cwd, strings.Join(a.cfg.Mounts.Roots, ", "), a.cfg.Root)
}

func underRoot(path string, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
