// Package cli implements ccc's command-line surface.
//
// `ccc` with no reserved subcommand starts Claude Code: the container is an
// implementation detail, not something the user should have to name.
package cli

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/container"
	"github.com/lestrrat-go/ccc/internal/image"
	"github.com/lestrrat-go/ccc/internal/profile"
)

// Version is overridden at build time.
var Version = "dev"

// reserved first-arguments. Everything else is passed to `claude`; `--` forces
// passthrough when a claude argument would collide with one of these.
// Names are chosen NOT to collide with Claude Code's own subcommands, which as
// of 2.1.205 are: agents auth auto-mode doctor gateway install mcp plugin
// plugins project setup-token ultrareview update upgrade.
//
// Each reserved word is a claude argument that then needs `--` to pass through,
// so a collision taxes the common case. `doctor` and `upgrade` both collided;
// they are `check` and `pin`.
var reserved = map[string]func(*app, []string) error{
	"profile": cmdProfile,
	"pin":     cmdPin,
	"check":   cmdCheck,
	"help":    cmdHelp,
	"version": cmdVersion,
}

// globals are ccc's own flags, parsed before any subcommand.
type globals struct {
	profile string
	runtime string
	help    bool
}

// app is the resolved runtime context shared by every command.
type app struct {
	globals globals
	cfg     *config.Config
	store   *profile.Store
	id      container.Identity
	cwd     string
	// dirFile is the nearest .ccc.json, or nil. A per-checkout, per-user file.
	dirFile *config.Dir
	// dirFileErr defers a malformed .ccc.json: only commands that actually
	// mount (a run, `check`) care, so `ccc version` must not fail because a
	// broken file sits in an ancestor directory.
	dirFileErr error
}

// invocation is a parsed command line.
type invocation struct {
	globals globals
	// command is a reserved command name, or "" to start Claude Code.
	command string
	// cmdArgs are the command's own arguments.
	cmdArgs []string
	// claudeArgs are everything after `--`, passed to claude verbatim.
	claudeArgs []string
}

// Run executes ccc. argv excludes the program name.
func Run(argv []string) error {
	inv, err := parse(argv)
	if err != nil {
		return err
	}

	// Answered before any config or runtime work, so `ccc --help` still works
	// on a machine with no config, no profiles, and no container runtime.
	if inv.globals.help {
		return cmdHelp(nil, nil)
	}

	a, err := newApp(inv.globals)
	if err != nil {
		return err
	}

	if inv.command != "" {
		if len(inv.claudeArgs) > 0 {
			return fmt.Errorf("%s takes no arguments after --", inv.command)
		}
		return reserved[inv.command](a, inv.cmdArgs)
	}
	return cmdClaude(a, inv.claudeArgs)
}

// parse splits the command line at `--`.
//
// Everything BEFORE `--` belongs to ccc; everything after goes to claude
// verbatim. The split is structural rather than best-effort because ccc and
// claude share a flag namespace: `-p` is --profile here and --print there, so
// a permissive parser silently misroutes `ccc -p "explain this"` into a profile
// lookup. An unknown argument is an error, never a guess.
func parse(argv []string) (invocation, error) {
	var inv invocation

	before := argv
	for i, arg := range argv {
		if arg == "--" {
			before, inv.claudeArgs = argv[:i], argv[i+1:]
			break
		}
	}

	for len(before) > 0 {
		arg := before[0]

		// The first non-flag must be a command; its arguments are its own.
		if !strings.HasPrefix(arg, "-") {
			if _, ok := reserved[arg]; !ok {
				return inv, unknownArg(arg)
			}
			inv.command, inv.cmdArgs = arg, before[1:]
			if wantsHelp(inv.cmdArgs) {
				inv.globals.help = true
			}
			return inv, nil
		}

		switch {
		case arg == "--help" || arg == "-h":
			inv.globals.help = true
			return inv, nil
		case arg == "--profile" || arg == "-p":
			if len(before) < 2 {
				return inv, fmt.Errorf("%s needs a profile name", arg)
			}
			inv.globals.profile, before = before[1], before[2:]
		case strings.HasPrefix(arg, "--profile="):
			inv.globals.profile, before = strings.TrimPrefix(arg, "--profile="), before[1:]
		case arg == "--runtime":
			if len(before) < 2 {
				return inv, fmt.Errorf("%s needs a runtime name", arg)
			}
			inv.globals.runtime, before = before[1], before[2:]
		case strings.HasPrefix(arg, "--runtime="):
			inv.globals.runtime, before = strings.TrimPrefix(arg, "--runtime="), before[1:]
		default:
			return inv, unknownFlag(arg)
		}
	}
	return inv, nil
}

func unknownFlag(arg string) error {
	return fmt.Errorf("unknown flag %q\nccc's own flags precede --; claude's go after it:\n  ccc -- %s", arg, arg)
}

func unknownArg(arg string) error {
	return fmt.Errorf("unknown command %q\nclaude arguments go after --:\n  ccc -- %s", arg, arg)
}

// wantsHelp reports whether a reserved command's arguments ask for help.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

func newApp(g globals) (*app, error) {
	root, err := config.DefaultRoot()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	if g.runtime != "" {
		cfg.Runtime = g.runtime
	}

	id, err := identity()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to determine working directory: %w", err)
	}
	// Resolve symlinks so cwd agrees with git's physical --show-toplevel. Without
	// this, a repo reached through a symlinked ancestor (~/work -> /mnt/work)
	// makes underRoot(cwd, gitTop) false and ccc refuses to run inside its own
	// repo. The resolved path is also what actually gets mounted.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	// A malformed .ccc.json is deferred, not fatal here: commands that never
	// mount (version, help, profile, pin) must work regardless of what sits in
	// an ancestor directory.
	dirFile, _, _, dirFileErr := config.FindDir(cwd, id.Home)

	return &app{
		globals:    g,
		cfg:        cfg,
		store:      profile.NewStore(root, id.Home),
		id:         id,
		cwd:        cwd,
		dirFile:    dirFile,
		dirFileErr: dirFileErr,
	}, nil
}

// identity mirrors the invoking user into the container, so the container's
// $HOME path equals the host's and absolute paths mean the same thing on both
// sides of the mount.
func identity() (container.Identity, error) {
	u, err := user.Current()
	if err != nil {
		return container.Identity{}, fmt.Errorf("failed to determine current user: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return container.Identity{}, fmt.Errorf("non-numeric uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return container.Identity{}, fmt.Errorf("non-numeric gid %q: %w", u.Gid, err)
	}
	return container.Identity{UID: uid, GID: gid, User: u.Username, Home: u.HomeDir}, nil
}

func (a *app) runtime() (container.Runtime, error) {
	return container.Detect(a.cfg.Runtime)
}

// claudeVersion resolves the Claude Code pin for a profile: the profile's own
// pin (~/.claude/.ccc-claude-version), else the global one. Pass "" for
// commands that have no profile.
//
// The profile pin is read from a mounted, container-writable directory, so
// Store.ClaudeVersion validates it. An invalid pin propagates as an error
// rather than reaching a build arg.
func (a *app) claudeVersion(name string) (string, error) {
	if name != "" {
		v, err := a.store.ClaudeVersion(name)
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
	return a.globalPin()
}

// globalPin returns the validated global image.claude_version. Load no longer
// validates it (that would brick every command on a malformed value), so it is
// checked here, at point of use, with a hint that names the global scope.
func (a *app) globalPin() (string, error) {
	v := a.cfg.Image.ClaudeVersion
	if v == "" {
		return "", nil
	}
	if err := config.ValidateClaudeVersion(v); err != nil {
		return "", fmt.Errorf("%s/%s: %w\nrepair it with `ccc pin --to <version>`", a.cfg.Root, config.FileName, err)
	}
	return v, nil
}

// builder constructs an image Builder for the given profile ("" for none).
func (a *app) builder(rt container.Runtime, name string) (*image.Builder, error) {
	v, err := a.claudeVersion(name)
	if err != nil {
		return nil, err
	}
	return a.builderWith(rt, v), nil
}

// builderWith constructs an image Builder for an explicit version.
func (a *app) builderWith(rt container.Runtime, version string) *image.Builder {
	return image.NewBuilder(rt, a.cfg, a.id, version)
}

func cmdVersion(_ *app, _ []string) error {
	fmt.Println("ccc " + Version)
	return nil
}

func cmdHelp(_ *app, _ []string) error {
	fmt.Print(usage)
	return nil
}

const usage = `ccc — Claude Code Contained

Run Claude Code in a container so ~/.claude can be swapped per account.

usage:
  ccc [flags]                     start Claude Code in the resolved profile
  ccc [flags] -- <claude args>    ... passing arguments through to claude
  ccc [flags] <command> [args]    run a ccc command

Everything before -- belongs to ccc; everything after it goes to claude
verbatim. ccc and claude share flag names (-p is --profile here, --print
there), so the split is strict: an argument ccc does not know is an error,
never a guess.

commands:
  profile create <name>      create a profile
    --from <dir>             seed it from an existing ~/.claude
  profile list               list profiles ('*' marks default_profile)
  profile rm <name>          delete a profile, credentials included (prompts)
    --force, -f              skip the confirmation prompt
  pin                        pin the newest Claude Code and build that image
    --to <version>           pin this version instead ("latest" is resolved
                             to a concrete version before being stored)
    --no-cache               rebuild every layer, not just Claude Code's —
                             the only way to refresh apt and golangci-lint
  check                      verify a session would start (non-zero if not)
  version                    print ccc's version
  help                       print this help

Combine -p with pin to scope it: ` + "`ccc -p work pin`" + ` pins that profile alone.
Without -p the version is recorded globally, and every profile without a pin
of its own follows it.

There is no build command: the image builds on first run, and again whenever
the pin changes. There is no login command: a profile with no credentials
makes Claude Code run its own setup. Re-authenticate an existing profile with
` + "`ccc -p <name> -- auth login`" + `.

flags:
  -p, --profile <name>       profile to run as
      --runtime <name>       podman | docker | auto (or $CCC_RUNTIME)
  -h, --help                 print this help

examples:
  ccc                        start Claude Code
  ccc -- --resume            start Claude Code with --resume
  ccc -p work -- --resume    ... as the "work" profile
  ccc -- -p "explain this"   claude's --print, not ccc's --profile
  ccc -- doctor              ` + "`claude doctor`" + `
  ccc version                ccc's own version
  ccc -- --version           claude's version
  ccc -- --help              claude's help, not ccc's

profile resolution, first match wins:
  1. --profile <name>
  2. .ccc.json in the current directory or an ancestor
  3. default_profile in ~/.config/ccc/config.json

With no profiles at all, the first run creates one named "default".
`
