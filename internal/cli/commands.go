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

// cmdPin pins a Claude Code version and rebuilds. Because CLAUDE_VERSION is
// declared immediately before the npm install layer, a bump invalidates just
// that layer and the content-hash label footer: with no Dockerfile.extra this
// costs one npm install, not a full image rebuild. (A Dockerfile.extra is
// appended below npm, so an extra with its own RUN also rebuilds on a bump.)
//
// Without --profile it writes the global default (image.default_claude_version
// in config.json). With --profile it writes that profile's pin (the
// .ccc-claude-version file), so profiles can run different Claude Code versions.
func cmdPin(a *app, args []string) error {
	var to string
	var noCache bool
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--to":
			if i+1 >= len(args) {
				return fmt.Errorf("pin: --to needs a version")
			}
			to = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--to="):
			to = strings.TrimPrefix(args[i], "--to=")
		case args[i] == "--no-cache":
			noCache = true
		default:
			return fmt.Errorf("pin: unexpected argument %q", args[i])
		}
	}

	// Scope: an explicit --profile pins that profile; otherwise pin globally.
	// Deliberately does NOT fall back to .ccc.json — pinning is a rare,
	// deliberate act and must not silently depend on the current directory.
	scope := a.globals.profile
	if scope != "" {
		// Validate before Exists: a traversal name like "../../../.ssh" resolves
		// to an existing dir, passes Exists, and would otherwise reach a path op.
		if err := profile.ValidateName(scope); err != nil {
			return err
		}
		if !a.store.Exists(scope) {
			return fmt.Errorf("%q: %w", scope, profile.ErrNotExist)
		}
	}

	to, err := resolveTarget(to, func() (string, error) {
		fmt.Fprintf(os.Stderr, "ccc: resolving %s@%s from the npm registry\n", npm.ClaudeCode, config.LatestClaudeVersion)
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
	// A corrupt pin is tolerated here, and only here: `ccc pin` must be able
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

	// The tag hashes the pin, so an existing image already has this version.
	// --no-cache still rebuilds: it is the only way to refresh the base image,
	// apt, and golangci-lint, none of which the version pin can invalidate.
	//
	// Prepare composes the Dockerfile once and builds that exact snapshot, so
	// Dockerfile.extra cannot change between the tag and the build. It builds
	// BEFORE we persist: a version that cannot be installed must not become a
	// pin, or every later run would fail on an unbuildable image.
	_, built, err := b.Prepare(noCache)
	if err != nil {
		return err
	}
	if !built && current == to {
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
	if to == "" || to == config.LatestClaudeVersion {
		v, err := latest()
		if err != nil {
			return "", err
		}
		to = v
	}
	if err := config.ValidateClaudeVersion(to); err != nil {
		return "", err
	}
	if to == config.LatestClaudeVersion {
		return "", fmt.Errorf("registry resolved to %q, not a concrete version", to)
	}
	return to, nil
}

// pinnedAt returns the version recorded at exactly this scope, without falling
// back to the global pin. "" means nothing is pinned there.
func (a *app) pinnedAt(scope string) (string, error) {
	if scope == "" {
		return a.cfg.Image.DefaultClaudeVersion, nil
	}
	return a.store.ClaudeVersion(scope)
}

// pin records the version globally, or in a profile when scope is non-empty.
func (a *app) pin(scope string, version string) error {
	if scope == "" {
		if err := config.SetDefaultClaudeVersion(a.cfg.Root, version); err != nil {
			return err
		}
		a.cfg.Image.DefaultClaudeVersion = version
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
		return profileList(a, args[1:])
	case "create":
		return profileCreate(a, args[1:])
	case "rm", "remove":
		return profileRemove(a, args[1:])
	default:
		return fmt.Errorf("profile: unknown subcommand %q", args[0])
	}
}

func profileList(a *app, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("profile list: unexpected argument %q", args[0])
	}
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

// confirm prompts for a yes/no answer on the terminal.
//
// Without a TTY it refuses rather than assuming: a piped or scripted `rm` that
// wants to skip the prompt must say so with --force, so the destructive default
// is never "proceed silently".
func confirm(prompt string) (bool, error) {
	if !isTerminal(os.Stdin) {
		return false, fmt.Errorf("%s\nrefusing without a terminal; pass --force to skip this prompt", prompt)
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)

	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return false, nil // empty line or EOF: treat as no
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func profileRemove(a *app, args []string) error {
	var name string
	var force bool
	for _, arg := range args {
		switch {
		case arg == "--force" || arg == "-f":
			force = true
		case name == "":
			name = arg
		default:
			return fmt.Errorf("profile rm: unexpected argument %q", arg)
		}
	}
	if name == "" {
		return fmt.Errorf("usage: ccc profile rm <name> [--force]")
	}

	// Removing the configured default_profile leaves a bare `ccc` pointing at a
	// profile that no longer exists. Warn before doing it (even with --force) so
	// the break is not a surprise on the next run.
	if name == a.cfg.DefaultProfile {
		fmt.Fprintf(os.Stderr, "ccc: WARNING: %q is the default_profile in %s/config.json;\nccc: after removal a bare `ccc` fails until you set a new default_profile or pass -p.\n", name, a.cfg.Root)
	}

	// rm deletes credentials and all of the profile's state, irreversibly. A
	// typo of an existing name would otherwise wipe it silently.
	if !force {
		if !a.store.Exists(name) {
			return fmt.Errorf("%q: %w", name, profile.ErrNotExist)
		}
		ok, err := confirm(fmt.Sprintf("delete profile %q and its credentials?", name))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}

	if err := a.store.Remove(name); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "removed profile %s\n", name)
	return nil
}
