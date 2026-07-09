// Package cli implements ccc's command-line surface.
//
// `ccc` with no reserved subcommand starts Claude Code: the container is an
// implementation detail, not something the user should have to name.
package cli

import (
	"fmt"
	"os"
	"os/user"
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
var reserved = map[string]func(*app, []string) error{
	"profile": cmdProfile,
	"upgrade": cmdUpgrade,
	"doctor":  cmdDoctor,
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
}

// Run executes ccc. argv excludes the program name.
func Run(argv []string) error {
	g, rest, forced := parseGlobals(argv)

	// Answered before any config or runtime work, so `ccc --help` still works
	// on a machine with no config, no profiles, and no container runtime.
	if g.help {
		return cmdHelp(nil, nil)
	}

	a, err := newApp(g)
	if err != nil {
		return err
	}

	if !forced && len(rest) > 0 {
		if cmd, ok := reserved[rest[0]]; ok {
			return cmd(a, rest[1:])
		}
	}
	return cmdClaude(a, rest)
}

// parseGlobals consumes leading ccc flags. It returns the remaining arguments
// and whether `--` forced everything after it to be passthrough.
func parseGlobals(argv []string) (globals, []string, bool) {
	var g globals
	for len(argv) > 0 {
		arg := argv[0]
		switch {
		case arg == "--":
			return g, argv[1:], true
		case arg == "--help" || arg == "-h":
			g.help = true
			return g, argv[1:], false
		case arg == "--profile" || arg == "-p":
			if len(argv) < 2 {
				return g, argv, false
			}
			g.profile, argv = argv[1], argv[2:]
		case strings.HasPrefix(arg, "--profile="):
			g.profile, argv = strings.TrimPrefix(arg, "--profile="), argv[1:]
		case arg == "--runtime":
			if len(argv) < 2 {
				return g, argv, false
			}
			g.runtime, argv = argv[1], argv[2:]
		case strings.HasPrefix(arg, "--runtime="):
			g.runtime, argv = strings.TrimPrefix(arg, "--runtime="), argv[1:]
		default:
			return g, argv, false
		}
	}
	return g, nil, false
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

	return &app{
		globals: g,
		cfg:     cfg,
		store:   profile.NewStore(root, id.Home),
		id:      id,
		cwd:     cwd,
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
	if name == "" {
		return a.cfg.Image.ClaudeVersion, nil
	}
	v, err := a.store.ClaudeVersion(name)
	if err != nil {
		return "", err
	}
	if v != "" {
		return v, nil
	}
	return a.cfg.Image.ClaudeVersion, nil
}

// builder constructs an image Builder for the given profile ("" for none).
func (a *app) builder(rt container.Runtime, name string) (*image.Builder, error) {
	v, err := a.claudeVersion(name)
	if err != nil {
		return nil, err
	}
	return image.NewBuilder(rt, a.cfg, a.id, v), nil
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
  ccc [flags] [claude args...]   start Claude Code in the resolved profile
  ccc <command> [args...]        run a ccc command (see below)
  ccc -- <claude args...>        pass everything through to claude verbatim

commands:
  profile create <name>      create a profile
    --from <dir>             seed it from an existing ~/.claude
  profile list               list profiles ('*' marks default_profile)
  profile rm <name>          delete a profile and its credentials
  upgrade                    pin the latest Claude Code, rebuild one layer
    --to <version>           pin a specific version instead ("latest"
                             resolves to a concrete version before storing)
    --no-cache               also rebuild every layer (base image, apt,
                             golangci-lint) — the pin alone cannot refresh them
  doctor                     check runtime, image, mounts, profile
  version                    print version
  help                       print this help

The image builds itself on first run; there is no build command.

There is no login command: a profile with no credentials makes Claude Code
run its own setup. To re-authenticate one, run ` + "`ccc -p <name> -- auth login`" + `.

flags:
  -p, --profile <name>       profile to run as
      --runtime <name>       podman | docker | auto
  -h, --help                 print this help

examples:
  ccc                        start Claude Code
  ccc --resume               start Claude Code with --resume
  ccc -p work --resume       ... as the "work" profile
  ccc -- --help              claude's help, not ccc's
  ccc -- doctor              ` + "`claude doctor`" + `, not ` + "`ccc doctor`" + `

Command names above are reserved: they are consumed by ccc, not passed to
claude. Use -- to force passthrough when a claude argument collides.

profile resolution, first match wins:
  1. --profile <name>
  2. .ccc.json in the current directory or an ancestor
  3. default_profile in ~/.config/ccc/config.json
`
