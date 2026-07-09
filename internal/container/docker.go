package container

import "fmt"

type docker struct{ bin string }

func (d *docker) Name() string { return "docker" }
func (d *docker) Bin() string  { return d.bin }

// RunArgs runs as the host UID/GID directly.
//
// The image is built with the host's UID/GID baked into /etc/passwd, so this
// resolves to a real user with a writable $HOME. Without that, --user leaves
// the process with no passwd entry and tools that call getpwuid(3) break.
func (d *docker) RunArgs(spec Spec, id Identity) []string {
	// commonRunArgs starts with "run"; --user must follow it, not precede it.
	out := commonRunArgs(spec)
	args := make([]string, 0, len(out)+3)
	args = append(args, d.bin, out[0], "--user", fmt.Sprintf("%d:%d", id.UID, id.GID))
	return append(args, out[1:]...)
}

func (d *docker) BuildArgs(tag string, contextDir string, buildArgs map[string]string, noCache bool) []string {
	return buildArgsFor(d.bin, tag, contextDir, buildArgs, noCache)
}

func (d *docker) InspectArgs(tag string) []string {
	return []string{d.bin, "image", "inspect", tag}
}
