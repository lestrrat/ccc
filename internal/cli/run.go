package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/container"
	ccenv "github.com/lestrrat-go/ccc/internal/env"
	"github.com/lestrrat-go/ccc/internal/image"
	"github.com/lestrrat-go/ccc/internal/profile"
)

// cmdClaude is the default command: resolve a profile, ensure the image, and
// hand the terminal to Claude Code inside the container.
func cmdClaude(a *app, args []string) error {
	res, err := a.resolveOrBootstrap()
	if err != nil {
		return err
	}
	// Absolute path, not "claude": PATH inside the container can reach the
	// host's native install via the mounted $HOME.
	return a.exec(res, append([]string{image.ClaudeBin}, args...))
}

// resolveOrBootstrap resolves a profile, creating a first one if none exist.
//
// Bootstrapping is confined to ErrNoSelection on an empty store: with zero
// profiles there is no account to pick wrongly. Once a profile exists, an
// unresolved run is still an error rather than a guess.
func (a *app) resolveOrBootstrap() (profile.Resolution, error) {
	res, err := a.store.Resolve(a.globals.profile, a.cfg, a.cwd)
	if err == nil {
		return res, nil
	}
	if !errors.Is(err, profile.ErrNoSelection) {
		return profile.Resolution{}, err
	}

	empty, emptyErr := a.store.IsEmpty()
	if emptyErr != nil {
		return profile.Resolution{}, emptyErr
	}
	if !empty {
		return profile.Resolution{}, err
	}
	return a.bootstrap()
}

// bootstrap creates the first profile, writing a config.json that names it as
// default_profile so a bare `ccc` keeps working once a second profile exists.
func (a *app) bootstrap() (profile.Resolution, error) {
	name := profile.DefaultName
	if err := a.store.Create(name); err != nil {
		return profile.Resolution{}, err
	}

	created, err := config.Create(a.cfg.Root, name)
	if err != nil {
		return profile.Resolution{}, err
	}
	a.cfg.DefaultProfile = name

	// The profile starts empty: ccc never copies credentials without being
	// asked, so Claude Code runs its own setup and prompts for login.
	fmt.Fprintf(os.Stderr, "ccc: first run — created profile %q\n", name)
	fmt.Fprintf(os.Stderr,
		"ccc: to start from an existing config instead: `ccc profile create <name> --from ~/.claude`\n")

	if created {
		fmt.Fprintf(os.Stderr, "ccc: wrote %s/%s naming it default_profile\n", a.cfg.Root, config.FileName)
	} else {
		// A hand-written config is never rewritten. Say so, because otherwise a
		// bare `ccc` starts failing as soon as a second profile is created.
		fmt.Fprintf(os.Stderr,
			"ccc: %s/%s exists and was left untouched; add \"default_profile\": %q to keep bare `ccc` working\n",
			a.cfg.Root, config.FileName, name)
	}

	return profile.Resolution{Name: name, Source: profile.SourceBootstrap}, nil
}

// pendingClaudeUpgrade reports the version the container's Claude Code tried and
// failed to install last session, and the version in force now.
//
// It does NOT persist anything: `version_to` is written by a process inside the
// container, and a well-formed but nonexistent version (say "9.9.9") passes
// validation yet cannot be installed. Persisting before the build succeeds
// would let the container brick ccc — every later run would fail on a pin that
// can never build. Adopt only after the image exists.
//
// ccc contacts no registry here: Claude Code already did the checking, and its
// own `autoUpdates` setting is therefore the on/off switch. Disable it in the
// profile's settings.json and there is nothing to adopt.
//
// Only a strictly newer version is offered. `ccc profile create --from
// ~/.claude` copies the host's update record, which may name a version older
// than this profile is pinned to; following that would be a silent downgrade.
func (a *app) pendingClaudeUpgrade(name string) (string, string, error) {
	current, err := a.claudeVersion(name)
	if err != nil {
		return "", "", err
	}

	want, err := a.store.RequestedClaudeVersion(name)
	if err != nil || want == "" {
		return "", current, err
	}
	if !config.IsNewerClaudeVersion(want, current) {
		return "", current, nil
	}
	return want, current, nil
}

// ensureImage builds the image for the version Claude Code asked for, falling
// back to the version already in force when that build fails.
//
// A failed upgrade must never block the session: the requested version may
// simply not exist. Warn, keep the working image, carry on.
func (a *app) ensureImage(rt container.Runtime, name string, want string, current string) (string, error) {
	if want == "" {
		b := a.builderWith(rt, current)
		return b.Ensure()
	}

	fmt.Fprintf(os.Stderr, "ccc: Claude Code asked for %s (have %s); rebuilding\n", want, orLatest(current))
	tag, err := a.builderWith(rt, want).Ensure()
	if err == nil {
		// Only now is the pin safe to record: the image for it exists.
		if err := a.store.SetClaudeVersion(name, want); err != nil {
			return "", err
		}
		return tag, nil
	}

	fmt.Fprintf(os.Stderr, "ccc: could not build Claude Code %s (%s)\n", want, err)
	fmt.Fprintf(os.Stderr, "ccc: staying on %s\n", orLatest(current))
	return a.builderWith(rt, current).Ensure()
}

func orLatest(v string) string {
	if v == "" {
		return config.DefaultClaudeVersion
	}
	return v
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

	// The pin is per-profile, so the image tag is too. A changed pin is a
	// changed tag, and Ensure() rebuilds — no version inspection needed.
	want, current, err := a.pendingClaudeUpgrade(res.Name)
	if err != nil {
		return err
	}
	tag, err := a.ensureImage(rt, res.Name, want, current)
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

	// Shadow the host's native Claude Code. $HOME is mounted, and a login shell
	// inside the container sources the host's ~/.profile, which prepends
	// ~/.local/bin — so `claude` would otherwise resolve to the host's binary
	// and, worse, self-update the host's installation from inside a container.
	hostNative := filepath.Join(a.id.Home, image.HostNativeBin)
	if _, err := os.Stat(hostNative); err == nil {
		shim, err := image.EnsureShim(a.cfg.Root)
		if err != nil {
			return nil, err
		}
		out = append(out, container.Mount{Source: shim, Target: hostNative, ReadOnly: true})
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
	return fmt.Errorf("working directory %s is not under any configured mount root (%s):\nadd it to mounts.roots in %s/config.json",
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
