package container

import "fmt"

type podman struct{ bin string }

func (p *podman) Name() string { return "podman" }
func (p *podman) Bin() string  { return p.bin }

// RunArgs maps the host user into the container with keep-id.
//
// Rootless podman already runs the container process as the host user; keep-id
// makes the in-container UID match, so /etc/passwd (built with the host's
// UID/GID) resolves and $HOME is writable. No --user is needed or wanted.
func (p *podman) RunArgs(spec Spec, id Identity) []string {
	// --userns is a `podman run` flag, not a global one; it must follow the verb.
	out := commonRunArgs(spec)
	args := make([]string, 0, len(out)+2)
	args = append(args, p.bin, out[0], fmt.Sprintf("--userns=keep-id:uid=%d,gid=%d", id.UID, id.GID))
	return append(args, out[1:]...)
}

func (p *podman) BuildArgs(tag string, contextDir string, buildArgs map[string]string, noCache bool) []string {
	return buildArgsFor(p.bin, tag, contextDir, buildArgs, noCache)
}

func (p *podman) InspectArgs(tag string) []string {
	return []string{p.bin, "image", "exists", tag}
}
