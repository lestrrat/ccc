// Package env decides which host environment variables reach the container.
//
// ccc inherits the whole host environment minus a denylist, so that direnv
// exports (GIT_SSH_COMMAND, GOPRIVATE, proxies, ...) work with no ccc-side
// configuration.
package env

import (
	"sort"
	"strings"
)

// containerManaged are set by the container itself; host values are wrong
// inside it.
var containerManaged = []string{
	"HOME", "PATH", "USER", "LOGNAME", "SHELL",
	"PWD", "OLDPWD", "TMPDIR", "TMP", "TEMP", "HOSTNAME",
}

// profileBreaking would override the profile's own credentials and silently
// route every profile to a single account — the exact failure ccc exists to
// prevent. Re-admit via `env.allow` if you genuinely want key-based auth.
//
// CLAUDE_CONFIG_DIR belongs here too: Claude Code honors it, so a forwarded
// value makes the container read/write state at that path instead of the
// mounted profile — splitting or sharing state across accounts, defeating the
// swap. The profile is mounted at $HOME/.claude regardless, so denying the env
// var is what keeps the boundary intact.
var profileBreaking = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CONFIG_DIR",
}

// hostPathScoped name host filesystem paths that the container does not mount.
// Inheriting them points tools at directories that do not exist inside the
// container: `go build` with the host's GOMODCACHE fails in a way that reads
// like a Go bug, not a mount bug. The container's own defaults are correct.
var hostPathScoped = []string{
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOBIN",
}

// handledSeparately are forwarded by the run logic with rewritten values.
var handledSeparately = []string{
	"SSH_AUTH_SOCK",
}

// DefaultDeny returns the built-in denylist.
func DefaultDeny() []string {
	deny := make([]string, 0, len(containerManaged)+len(profileBreaking)+len(hostPathScoped)+len(handledSeparately))
	deny = append(deny, containerManaged...)
	deny = append(deny, profileBreaking...)
	deny = append(deny, hostPathScoped...)
	deny = append(deny, handledSeparately...)
	return deny
}

// Filter selects the host environment entries to forward. environ is in
// os.Environ form ("K=V"). extraDeny extends the built-in denylist; allow
// re-admits a denied name and wins over every deny rule, except for the
// containerManaged set, which allow can never re-admit: forwarding the host's
// HOME/PATH/etc. points the container at paths that do not match the mounted
// profile, so Claude would write credentials into the wrong home and defeat the
// account boundary ccc exists to enforce.
func Filter(environ []string, extraDeny []string, allow []string) map[string]string {
	denied := make(map[string]struct{})
	for _, k := range DefaultDeny() {
		denied[k] = struct{}{}
	}
	for _, k := range extraDeny {
		denied[k] = struct{}{}
	}
	managed := make(map[string]struct{}, len(containerManaged))
	for _, k := range containerManaged {
		managed[k] = struct{}{}
	}
	for _, k := range allow {
		if _, isManaged := managed[k]; isManaged {
			continue
		}
		delete(denied, k)
	}

	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if _, bad := denied[k]; bad {
			continue
		}
		out[k] = v
	}
	return out
}

// Pairs renders an environment map as sorted "K=V" strings, so that generated
// container arguments are deterministic.
func Pairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
