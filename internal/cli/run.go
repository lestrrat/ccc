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
	"github.com/lestrrat-go/ccc/internal/workspace"
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
	// A malformed .ccc.json blocks a run just as it did when resolution reread
	// the file: surface the deferred load error before choosing (or bootstrapping)
	// a profile, so a broken file never silently falls through to default_profile.
	if a.dirFileErr != nil {
		return profile.Resolution{}, a.dirFileErr
	}
	res, err := a.store.Resolve(a.globals.profile, a.cfg, a.dirFile, a.dirFileOrigin)
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
		tag, err := a.builderWith(rt, current).Ensure()
		if err == nil || current == "" || current == config.LatestClaudeVersion {
			// No pin to blame: a "latest"/unpinned build that fails is a real
			// problem (base image, network) and must surface, not be masked.
			return tag, err
		}
		// current is a concrete pin whose image will not build. The pin lives in
		// a container-writable file, so a bogus-but-valid version (e.g. 9.9.9)
		// would otherwise brick the profile on every future run. Fall back to
		// an unpinned session and say how to repair.
		fmt.Fprintf(os.Stderr, "ccc: pinned Claude Code %s will not build (%s)\n", current, err)
		fmt.Fprintf(os.Stderr, "ccc: repair with `%s`; starting on latest for now\n", a.pinRepairCmd(name))
		return a.builderWith(rt, config.LatestClaudeVersion).Ensure()
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
	return a.ensureImage(rt, name, "", current)
}

// pinRepairCmd names the command that repairs whichever pin is in force: the
// profile's own if it has one, otherwise the global config.json pin.
func (a *app) pinRepairCmd(name string) string {
	if name != "" {
		if v, _ := a.store.ClaudeVersion(name); v != "" {
			return fmt.Sprintf("ccc -p %s pin --to <version>", name)
		}
	}
	return "ccc pin --to <version>"
}

func orLatest(v string) string {
	if v == "" {
		return config.LatestClaudeVersion
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

	if err := a.store.Materialize(res.Name); err != nil {
		return err
	}
	mounts, err := a.preflight(res.Name)
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

// preflight validates everything cheap before the image build: a first run that
// spends minutes building only to reject the working directory is hostile.
//
// `ccc check` calls this exact function. A diagnostic that reimplements the
// checks drifts away from the code it claims to check, and then reports green
// while the real path fails.
func (a *app) preflight(name string) ([]container.Mount, error) {
	// A malformed .ccc.json is deferred from newApp to here, because only a run
	// consumes its `dirs`. This is the point that actually needs it.
	if a.dirFileErr != nil {
		return nil, a.dirFileErr
	}

	// The implicit workspace dir is the cwd when it is not in a repository.
	// Refuse the dangerous ones before anything else looks at them.
	for _, d := range workspace.Dirs(a.cwd) {
		if err := a.checkMountDir(d); err != nil {
			return nil, fmt.Errorf("%w implicitly\nrun ccc inside a git repository, or name directories in mounts.dirs in %s/%s",
				err, a.cfg.Root, config.FileName)
		}
	}

	// .ccc.json is read by walking up from the working directory, which is
	// inside the container-writable repo — so its `dirs` are UNTRUSTED. A
	// contained process could write {"dirs":["/"]} or {"dirs":["~"]} to escalate
	// the next run's mounts to host root/home read-write. Apply the same refusal
	// here. (config.json's mounts.dirs is host-only and stays trusted.)
	//
	// The guard resolves symlinks first: the container could otherwise plant
	// `repo/link -> /` and list `link`, whose literal path passes the check while
	// the bind mount follows the symlink to host root. The resolved path is
	// written back so mounts() binds THE SAME canonical path that was checked —
	// mounting the original symlink would reintroduce the bypass. A dir that does
	// not yet resolve is left for the os.Stat in mounts() to reject.
	if a.dirFile != nil {
		for i, d := range a.dirFile.Dirs {
			target := d
			if resolved, err := filepath.EvalSymlinks(d); err == nil {
				target = resolved
			}
			if err := a.checkMountDir(target); err != nil {
				return nil, fmt.Errorf("%w\n%s lists it in \"dirs\", but that file is inside the container-writable repository and may not mount /, your home, or an ancestor",
					err, config.DirConfigName)
			}
			a.dirFile.Dirs[i] = target
		}
	}
	if err := a.checkWorkdir(); err != nil {
		return nil, err
	}

	mounts, err := a.mounts(name)
	if err != nil {
		return nil, err
	}
	for _, m := range mounts {
		if err := m.Validate(); err != nil {
			return nil, err
		}
	}
	return mounts, nil
}

// mounts assembles the container's view of the host. Dirs are mounted at
// their identical absolute paths; the profile is layered on top of $HOME.
//
// Mounts are applied parent-first, so a deeper mount always wins: read-write
// roots sit on top of a read-only $HOME, and the profile sits on top of both.
func (a *app) mounts(name string) ([]container.Mount, error) {
	var out []container.Mount

	// $HOME first, when asked for: it is the shallowest path, so everything
	// below overrides it.
	switch a.cfg.Mounts.Home {
	case config.HomeRO:
		out = append(out, container.Mount{Source: a.id.Home, Target: a.id.Home, ReadOnly: true})
	case config.HomeRW:
		out = append(out, container.Mount{Source: a.id.Home, Target: a.id.Home})
	}

	// A missing dir is fatal here, not inside the container: failing with the
	// host path beats `replacement directory ../../x does not exist` from a
	// container that silently lacks the mount.
	for _, dir := range a.dirs() {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("mount directory %s is not accessible: %w", dir, err)
		}
		out = append(out, container.Mount{Source: dir, Target: dir})
	}

	if a.cfg.Mounts.Cache {
		src := a.store.CacheDir(name)
		if err := os.MkdirAll(src, 0o700); err != nil {
			return nil, fmt.Errorf("failed to create profile cache: %w", err)
		}
		out = append(out, container.Mount{Source: src, Target: filepath.Join(a.id.Home, ".cache")})
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

	// The host's Claude Code is only visible when $HOME is mounted. By default
	// it is not, so there is nothing to defend against.
	if a.cfg.Mounts.Home != config.HomeNone {
		// Resolution: a login shell inside the container sources the host's
		// ~/.profile, which prepends ~/.local/bin, so a bare `claude` would run
		// the host's binary rather than the image's. Shadow it.
		hostNative := filepath.Join(a.id.Home, image.HostNativeBin)
		if _, err := os.Stat(hostNative); err == nil {
			shim, err := image.EnsureShim(a.cfg.Root)
			if err != nil {
				return nil, err
			}
			out = append(out, container.Mount{Source: shim, Target: hostNative, ReadOnly: true})
		}
	}

	// Replacement is a separate problem, and only "rw" has it. `claude install`
	// rename(2)s a temp file over ~/.local/bin/claude; a read-only bind mount on
	// the FILE does not stop that, because rename replaces the directory entry
	// and the directory is writable. A read-only $HOME does stop it — which is
	// why "ro" needs no special-casing, and "rw" cannot be made safe.
	if a.cfg.Mounts.Home == config.HomeRW {
		fmt.Fprint(os.Stderr, homeRWWarning())
		for _, rel := range []string{".local/bin", ".local/share/claude"} {
			src := filepath.Join(a.id.Home, rel)
			if _, err := os.Stat(src); err != nil {
				continue
			}
			out = append(out, container.Mount{Source: src, Target: src, ReadOnly: true})
		}
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
	// SSH_AUTH_SOCK value stays valid inside the container. Read-only, and only
	// when it is actually a socket: an unvalidated rw mount of whatever
	// SSH_AUTH_SOCK points at lets a hostile env/direnv mount an arbitrary path
	// read-write (SSH_AUTH_SOCK=$HOME mounts the whole home; =~/.ssh/id_rsa
	// overlays the ro .ssh mount with a writable key).
	if sock := a.sshAuthSock(); sock != "" {
		out = append(out, container.Mount{Source: sock, Target: sock, ReadOnly: true})
	}
	return out, nil
}

// homeRWWarning is printed on every mounts.home "rw" run: the mode cannot fully
// protect the host home. The read-only guard above only shadows ~/.local/bin and
// ~/.local/share/claude when they ALREADY exist; if they do not, `claude
// install` inside the container creates them in the real home. Even when they
// exist a rename(2) over the directory entry defeats the file-level guard (see
// the block above), so "rw" is never a full guarantee — hence a warning rather
// than silent, partial protection.
func homeRWWarning() string {
	return strings.Join([]string{
		"ccc: WARNING: mounts.home is \"rw\"; host ~/.local is NOT fully protected.",
		"ccc: the read-only installer guard only shadows ~/.local/bin and",
		"ccc: ~/.local/share/claude when they already exist, so `claude install`",
		"ccc: inside the container may still create or rewrite them in your host home.",
		"ccc: to avoid this, do not use mounts.home \"rw\" (use \"ro\" or omit it), or",
		"ccc: pre-create those paths so the guard can mount them read-only.",
		"",
	}, "\n")
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

// checkMountDir refuses a directory that must never be a mount target: the
// filesystem root, the home directory, or an ancestor of it.
//
// Outside a git repository the implicit workspace dir is the cwd itself. Run
// `ccc` from $HOME and that would mount the whole home read-write — the exact
// exposure the narrow default exists to prevent, arrived at by accident.
// Naming such a directory in mounts.dirs is fine; falling into it is not.
func (a *app) checkMountDir(dir string) error {
	// Compare against the CANONICAL home: callers pass a symlink-resolved dir,
	// but a.id.Home (from user.Current) may itself be a symlink spelling. Without
	// resolving both sides, a dir that resolves to the real home path slips past
	// the equality/ancestor checks on a symlink-home system.
	home := a.id.Home
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	switch {
	case dir == "/":
		return fmt.Errorf("refusing to mount /")
	case dir == home:
		return fmt.Errorf("refusing to mount your home directory %s", dir)
	case underRoot(home, dir):
		return fmt.Errorf("refusing to mount %s: it contains your home directory", dir)
	default:
		return nil
	}
}

// dirs are the read-write host directories for this session.
//
// Always the repository the working directory belongs to, plus whatever
// config.json and .ccc.json add. Additive, never replacing: a repo's .ccc.json
// must not be able to unmount the repo itself, nor revoke a machine-wide dir.
func (a *app) dirs() []string {
	all := workspace.Dirs(a.cwd)
	all = append(all, a.cfg.Mounts.Dirs...)
	if a.dirFile != nil {
		all = append(all, a.dirFile.Dirs...)
	}

	seen := make(map[string]struct{}, len(all))
	out := make([]string, 0, len(all))
	for _, d := range all {
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// env forwards the host environment minus the denylist, then re-adds the
// variables ccc rewrites itself.
func (a *app) env() map[string]string {
	m := ccenv.Filter(os.Environ(), a.cfg.Env.Deny, a.cfg.Env.Allow)
	// SSH_AUTH_SOCK is controlled solely by the validated snapshot: drop whatever
	// Filter produced (env.allow could re-admit the raw host value) and re-add it
	// only when it names an actual socket ccc mounts, so the forwarded value
	// always matches a real read-only socket mount (see mounts()).
	delete(m, "SSH_AUTH_SOCK")
	if sock := a.sshAuthSock(); sock != "" {
		m["SSH_AUTH_SOCK"] = sock
	}

	// GOCACHE defaults under ~/.cache, so the profile cache mount already
	// captures it. GOMODCACHE defaults to ~/go/pkg/mod, outside it, and needs
	// pointing. Set via env rather than `go env -w`, which would persist to the
	// container's ephemeral ~/.config/go/env and vanish with the container.
	if a.cfg.Mounts.Cache {
		m["GOMODCACHE"] = filepath.Join(a.id.Home, ".cache", "go-mod")
	}
	return m
}

// checkWorkdir refuses to run outside the mounted dirs rather than silently
// mounting the working directory behind the user's back.
func (a *app) checkWorkdir() error {
	dirs := a.dirs()
	for _, dir := range dirs {
		if underRoot(a.cwd, dir) {
			return nil
		}
	}
	return fmt.Errorf("working directory %s is not under any mounted directory (%s):\nadd it to mounts.dirs in %s/config.json",
		a.cwd, strings.Join(dirs, ", "), a.cfg.Root)
}

func underRoot(path string, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// sshAuthSock returns the validated SSH_AUTH_SOCK, snapshotted once per
// invocation so the mount, the forwarded env value, and `ccc check` never
// disagree if the socket appears or disappears mid-run.
func (a *app) sshAuthSock() string {
	if a.sshSock == nil {
		v := resolveSSHAuthSock()
		a.sshSock = &v
	}
	return *a.sshSock
}

// resolveSSHAuthSock returns SSH_AUTH_SOCK only when it points at an actual
// socket, else "". Stat (following symlinks) so a legitimate symlink-to-socket
// still forwards; the ModeSocket check on the resolved target is what stops a
// hostile SSH_AUTH_SOCK (a directory like $HOME, or a regular file like a
// private key) from turning into an arbitrary rw bind mount.
func resolveSSHAuthSock() string {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return ""
	}
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return ""
	}
	return sock
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
