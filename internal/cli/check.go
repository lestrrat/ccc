package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/lestrrat-go/ccc/internal/container"
)

// cmdCheck verifies that a session would start, and exits non-zero when it
// would not.
//
// It runs the same preflight `ccc` runs, then actually starts a container with
// the real mounts and identity. That last step is the only thing that catches
// a malformed argument vector — a wrong flag order fails at `podman run`, and
// nothing short of running it will tell you.
func cmdCheck(a *app, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("check: unexpected argument %q", args[0])
	}
	var failed bool
	step := func(label string, err error, detail string) {
		if err != nil {
			failed = true
			fmt.Printf("FAIL  %-14s %s\n", label, err)
			return
		}
		fmt.Printf("ok    %-14s %s\n", label, detail)
	}

	fmt.Printf("      %-14s %s\n", "config", a.cfg.Root)
	fmt.Printf("      %-14s %s (uid=%d gid=%d)\n", "identity", a.id.User, a.id.UID, a.id.GID)

	rt, err := a.runtime()
	if err != nil {
		step("runtime", err, "")
		return errFailed(failed)
	}
	step("runtime", nil, rt.Name()+" ("+rt.Bin()+")")

	// Resolve without bootstrapping: a diagnostic must not create a profile.
	res, err := a.store.Resolve(a.globals.profile, a.cfg, a.cwd)
	if err != nil {
		step("profile", err, "")
		return errFailed(failed)
	}
	step("profile", nil, res.String())

	pinned, err := a.claudeVersion(res.Name)
	if err != nil {
		step("claude pin", err, "")
	} else {
		step("claude pin", nil, orLatest(pinned))
	}

	// The real preflight: working directory inside a mounted dir, and every
	// mount source present on the host.
	mounts, err := a.preflight(res.Name)
	if err != nil {
		step("mounts", err, "")
		return errFailed(failed)
	}
	step("mounts", nil, fmt.Sprintf("%d, workdir %s", len(mounts), a.cwd))
	for _, m := range mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		fmt.Printf("      %-14s %s -> %s (%s)\n", "", m.Source, m.Target, mode)
	}

	b, err := a.builder(rt, res.Name)
	if err != nil {
		step("image", err, "")
		return errFailed(failed)
	}
	tag, err := b.Tag()
	if err != nil {
		step("image", err, "")
		return errFailed(failed)
	}
	if !b.Exists(tag) {
		step("image", nil, tag+" (not built; will build on first run)")
		fmt.Printf("      %-14s %s\n", "container", "skipped (no image yet)")
		return errFailed(failed)
	}
	step("image", nil, tag)

	// Start a container for real. Two of this project's worst bugs — podman's
	// --userns placement and a mount that cannot be created — were invisible
	// until something actually ran.
	step("container", smokeTest(rt, a.id, tag, mounts, a.cwd), "starts with these mounts")

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		fmt.Printf("      %-14s %s\n", "ssh agent", sock)
	} else {
		fmt.Printf("      %-14s %s\n", "ssh agent", "not running (git over ssh needs a key in ~/.ssh)")
	}

	return errFailed(failed)
}

// smokeTest runs `true` in the container with the session's real arguments.
func smokeTest(rt container.Runtime, id container.Identity, tag string, mounts []container.Mount, cwd string) error {
	spec := container.Spec{
		Image:   tag,
		Workdir: cwd,
		Mounts:  mounts,
		Cmd:     []string{"/bin/true"},
	}
	argv := rt.RunArgs(spec, id)

	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%s", lastLine(msg))
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	return lines[len(lines)-1]
}

func errFailed(failed bool) error {
	if failed {
		return fmt.Errorf("check failed")
	}
	return nil
}
