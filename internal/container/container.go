// Package container builds the argument vectors for the underlying container
// runtime. ccc shells out to the podman/docker binary rather than using a
// client SDK: constructing arguments is the entire job, and a daemon client
// buys nothing.
package container

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Mount is a host path exposed inside the container.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

func (m Mount) arg() string {
	s := m.Source + ":" + m.Target
	if m.ReadOnly {
		return s + ":ro"
	}
	return s
}

// Validate rejects a mount the runtime's `-v` syntax cannot express. `-v` is
// colon-delimited with no escape, so a `:` anywhere in the path produces a
// spec the runtime mis-parses (it fails closed, but with an opaque error).
func (m Mount) Validate() error {
	for _, p := range []string{m.Source, m.Target} {
		if strings.Contains(p, ":") {
			return fmt.Errorf("mount path %q contains ':', which the container runtime cannot express in a -v flag", p)
		}
	}
	return nil
}

// Identity is the host user the container runs as, so files written into
// mounted repositories end up owned by the invoking user.
type Identity struct {
	UID  int
	GID  int
	User string
	Home string
}

// Spec fully describes one `ccc` invocation's container.
type Spec struct {
	Image   string
	Workdir string
	Mounts  []Mount
	Env     []string // "K=V", already filtered and sorted
	Cmd     []string
	TTY     bool
}

// Runtime is a container CLI. Implementations differ only in how they map the
// host user into the container.
type Runtime interface {
	// Name is "podman" or "docker".
	Name() string
	// Bin is the absolute path to the executable.
	Bin() string
	// RunArgs builds the full argv (including Bin) for a one-shot run.
	RunArgs(Spec, Identity) []string
	// BuildArgs builds the full argv (including Bin) for an image build.
	BuildArgs(tag string, contextDir string, buildArgs map[string]string, noCache bool) []string
	// InspectLabelArgs builds the full argv (including Bin) to read a named image
	// label. ccc verifies an image's baked-in content hash rather than trusting
	// its tag, so a missing image (the inspect exits non-zero) and a mismatched
	// label are both treated as absent.
	InspectLabelArgs(tag, label string) []string
}

// Detect resolves the runtime named by pref, or auto-detects one.
//
// podman is preferred over docker: rootless podman's keep-id user namespace
// gives correct file ownership without a daemon.
func Detect(pref string) (Runtime, error) {
	switch pref {
	case "podman":
		return newRuntime("podman")
	case "docker":
		return newRuntime("docker")
	case "", "auto":
		for _, name := range []string{"podman", "docker"} {
			rt, err := newRuntime(name)
			if err == nil {
				return rt, nil
			}
		}
		return nil, fmt.Errorf("no container runtime found: install podman or docker")
	default:
		return nil, fmt.Errorf("unknown runtime %q: want podman, docker, or auto", pref)
	}
}

func newRuntime(name string) (Runtime, error) {
	bin, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("%s not found in PATH: %w", name, err)
	}
	switch name {
	case "podman":
		return &podman{bin: bin}, nil
	case "docker":
		return &docker{bin: bin}, nil
	}
	return nil, fmt.Errorf("unknown runtime %q", name)
}

// commonRunArgs are the arguments shared by both runtimes. Everything that
// differs between them is the user-namespace mapping.
func commonRunArgs(spec Spec) []string {
	args := []string{"run", "--rm", "--init"}
	if spec.TTY {
		args = append(args, "-it")
	} else {
		args = append(args, "-i")
	}

	// Host networking: dev servers on localhost stay reachable, and Claude Code's
	// OAuth loopback callback lands on the host's browser during login.
	args = append(args, "--network", "host")

	for _, m := range sortMounts(spec.Mounts) {
		args = append(args, "-v", m.arg())
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	if spec.Workdir != "" {
		args = append(args, "-w", spec.Workdir)
	}

	args = append(args, spec.Image)
	return append(args, spec.Cmd...)
}

// sortMounts orders mounts parent-before-child, so a profile mounted at
// $HOME/.claude lands on top of a $HOME mount rather than being shadowed by it.
func sortMounts(in []Mount) []Mount {
	out := make([]Mount, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		di := strings.Count(out[i].Target, "/")
		dj := strings.Count(out[j].Target, "/")
		if di != dj {
			return di < dj
		}
		return out[i].Target < out[j].Target
	})
	return out
}

func buildArgsFor(bin string, tag string, contextDir string, buildArgs map[string]string, noCache bool) []string {
	args := []string{bin, "build", "-t", tag}
	if noCache {
		args = append(args, "--no-cache")
	}

	keys := make([]string, 0, len(buildArgs))
	for k := range buildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
	}
	return append(args, contextDir)
}

// inspectLabelArgs reads one label off an image with a Go-template format. Both
// runtimes accept the identical `image inspect --format` invocation, and both
// exit non-zero when the image is absent.
func inspectLabelArgs(bin, tag, label string) []string {
	return []string{bin, "image", "inspect", "--format", fmt.Sprintf("{{ index .Config.Labels %q }}", label), tag}
}
