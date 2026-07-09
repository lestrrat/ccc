package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/image"
)

func cmdBuild(a *app, args []string) error {
	var noCache bool
	for _, arg := range args {
		switch arg {
		case "--no-cache":
			noCache = true
		default:
			return fmt.Errorf("build: unknown flag %q", arg)
		}
	}

	rt, err := a.runtime()
	if err != nil {
		return err
	}
	b := image.NewBuilder(rt, a.cfg, a.id)
	tag, err := b.Tag()
	if err != nil {
		return err
	}
	return b.Build(tag, noCache)
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

	b := image.NewBuilder(rt, a.cfg, a.id)
	tag, err := b.Tag()
	if err != nil {
		return err
	}
	status := "not built — will build on first run"
	if b.Exists(tag) {
		status = "present"
	}
	fmt.Printf("image:        %s (%s)\n", tag, status)

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
