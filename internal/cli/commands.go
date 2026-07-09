package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/npm"
	"github.com/lestrrat-go/ccc/internal/profile"
)

// cmdUpgrade pins a Claude Code version and rebuilds. Because CLAUDE_VERSION is
// declared as the last ARG in the Dockerfile, only the final layer is
// invalidated: this costs one npm install, not a full image rebuild.
//
// Without --profile the pin is global; with it, the pin lives in that profile's
// profile.json, so profiles can run different Claude Code versions.
func cmdUpgrade(a *app, args []string) error {
	var to string
	var noCache bool
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--to":
			if i+1 >= len(args) {
				return fmt.Errorf("upgrade: --to needs a version")
			}
			to = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--to="):
			to = strings.TrimPrefix(args[i], "--to=")
		case args[i] == "--no-cache":
			noCache = true
		default:
			return fmt.Errorf("upgrade: unexpected argument %q", args[i])
		}
	}

	// Scope: an explicit --profile pins that profile; otherwise pin globally.
	// Deliberately does NOT fall back to .ccc.json — pinning is a rare,
	// deliberate act and must not silently depend on the current directory.
	scope := a.globals.profile
	if scope != "" && !a.store.Exists(scope) {
		return fmt.Errorf("%q: %w", scope, profile.ErrNotExist)
	}

	to, err := resolveTarget(to, func() (string, error) {
		fmt.Fprintf(os.Stderr, "ccc: resolving %s@%s from the npm registry\n", npm.ClaudeCode, config.DefaultClaudeVersion)
		return npm.Latest(context.Background(), npm.ClaudeCode)
	})
	if err != nil {
		return err
	}

	// Compare against the pin stored AT THIS SCOPE, not the effective one. A
	// profile that merely inherits the global version is still unpinned: naming
	// it explicitly means "pin it here", so that a later global change does not
	// silently move this profile.
	//
	// A corrupt pin is tolerated here, and only here: `ccc upgrade` must be able
	// to repair one. Everywhere else it is a hard error.
	current, err := a.pinnedAt(scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccc: ignoring unreadable pin (%s)\n", err)
		current = ""
	}
	rt, err := a.runtime()
	if err != nil {
		return err
	}

	b := a.builderWith(rt, to)
	tag, err := b.Tag()
	if err != nil {
		return err
	}

	// The tag hashes the pin, so an existing image already has this version.
	// --no-cache still rebuilds: it is the only way to refresh the base image,
	// apt, and golangci-lint, none of which the version pin can invalidate.
	if noCache || !b.Exists(tag) {
		// Build BEFORE persisting: a version that cannot be installed must not
		// become a pin, or every later run would fail on an unbuildable image.
		if err := b.Build(tag, noCache); err != nil {
			return err
		}
	} else if current == to {
		fmt.Fprintf(os.Stderr, "ccc: already on %s\n", to)
		return nil
	}

	if err := a.pin(scope, to); err != nil {
		return err
	}

	where := "globally"
	if scope != "" {
		where = "for profile " + scope
	}
	if current == "" {
		fmt.Fprintf(os.Stderr, "ccc: pinned Claude Code %s %s\n", to, where)
	} else {
		fmt.Fprintf(os.Stderr, "ccc: upgraded Claude Code %s -> %s %s\n", current, to, where)
	}
	return nil
}

// resolveTarget turns the requested version into a concrete one to store.
//
// A stored pin must never be "latest": the image tag hashes the build args, so
// a moving dist-tag hashes to a stable tag and the image would never be rebuilt
// again — the pin would silently freeze whatever was installed first. Both an
// empty --to and an explicit `--to latest` therefore resolve through the
// registry, which is the one place a network call is expected.
func resolveTarget(to string, latest func() (string, error)) (string, error) {
	if to == "" || to == config.DefaultClaudeVersion {
		v, err := latest()
		if err != nil {
			return "", err
		}
		to = v
	}
	if err := config.ValidateClaudeVersion(to); err != nil {
		return "", err
	}
	if to == config.DefaultClaudeVersion {
		return "", fmt.Errorf("registry resolved to %q, not a concrete version", to)
	}
	return to, nil
}

// pinnedAt returns the version recorded at exactly this scope, without falling
// back to the global pin. "" means nothing is pinned there.
func (a *app) pinnedAt(scope string) (string, error) {
	if scope == "" {
		return a.cfg.Image.ClaudeVersion, nil
	}
	return a.store.ClaudeVersion(scope)
}

// pin records the version globally, or in a profile when scope is non-empty.
func (a *app) pin(scope string, version string) error {
	if scope == "" {
		if err := config.SetClaudeVersion(a.cfg.Root, version); err != nil {
			return err
		}
		a.cfg.Image.ClaudeVersion = version
		return nil
	}
	return a.store.SetClaudeVersion(scope, version)
}

func cmdProfile(a *app, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccc profile <create|list|rm> [args...]")
	}
	switch args[0] {
	case "list":
		return profileList(a)
	case "create":
		return profileCreate(a, args[1:])
	case "rm", "remove":
		return profileRemove(a, args[1:])
	default:
		return fmt.Errorf("profile: unknown subcommand %q", args[0])
	}
}

func profileList(a *app) error {
	names, err := a.store.List()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "no profiles yet: create one with `ccc profile create <name>`")
		return nil
	}
	for _, n := range names {
		marker := " "
		if n == a.cfg.DefaultProfile {
			marker = "*"
		}
		fmt.Printf("%s %s\n", marker, n)
	}
	return nil
}

func profileCreate(a *app, args []string) error {
	var name, from string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--from":
			if i+1 >= len(args) {
				return fmt.Errorf("profile create: --from needs a directory")
			}
			from = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--from="):
			from = strings.TrimPrefix(args[i], "--from=")
		case name == "":
			name = args[i]
		default:
			return fmt.Errorf("profile create: unexpected argument %q", args[i])
		}
	}
	if name == "" {
		return fmt.Errorf("usage: ccc profile create <name> [--from <dir>]")
	}

	if err := a.store.Create(name); err != nil {
		return err
	}
	if from == "" {
		fmt.Fprintf(os.Stderr, "created profile %s\nrun `ccc -p %s`; Claude Code will prompt you to log in\n", name, name)
		return nil
	}

	expanded, err := config.Expand(from, a.id.Home)
	if err != nil {
		return err
	}
	if err := a.store.Seed(name, expanded); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "created profile %s, seeded from %s\n", name, expanded)
	return nil
}

func profileRemove(a *app, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ccc profile rm <name>")
	}
	name := args[0]
	if err := a.store.Remove(name); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "removed profile %s\n", name)
	return nil
}

func cmdDoctor(a *app, _ []string) error {
	fmt.Printf("config root:  %s\n", a.cfg.Root)
	fmt.Printf("identity:     %s (uid=%d gid=%d)\n", a.id.User, a.id.UID, a.id.GID)
	fmt.Printf("home:         %s\n", a.id.Home)
	fmt.Printf("mount roots:  %s\n", strings.Join(a.cfg.Mounts.Roots, ", "))

	rt, err := a.runtime()
	if err != nil {
		fmt.Printf("runtime:      ERROR %s\n", err)
		return err
	}
	fmt.Printf("runtime:      %s (%s)\n", rt.Name(), rt.Bin())

	b, err := a.builder(rt, a.globals.profile)
	if err != nil {
		return err
	}
	tag, err := b.Tag()
	if err != nil {
		return err
	}
	status := "not built — will build on first run"
	if b.Exists(tag) {
		status = "present"
	}
	fmt.Printf("image:        %s (%s)\n", tag, status)

	pinned, err := a.claudeVersion(a.globals.profile)
	if err != nil {
		return err
	}
	if pinned == "" {
		pinned = config.DefaultClaudeVersion + " (unpinned; `ccc upgrade` to pin)"
	}
	fmt.Printf("claude:       %s\n", pinned)

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		fmt.Printf("ssh agent:    %s\n", sock)
	} else {
		fmt.Printf("ssh agent:    not running (git over ssh may need a key in ~/.ssh)\n")
	}

	names, err := a.store.List()
	if err != nil {
		return err
	}
	fmt.Printf("profiles:     %s\n", strings.Join(names, ", "))

	res, err := a.store.Resolve(a.globals.profile, a.cfg, a.cwd)
	if err != nil {
		fmt.Printf("resolved:     none — %s\n", err)
		return nil
	}
	fmt.Printf("resolved:     %s\n", res)
	return nil
}
