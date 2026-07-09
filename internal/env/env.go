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
var profileBreaking = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
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
// re-admits a denied name and wins over every deny rule.
func Filter(environ []string, extraDeny []string, allow []string) map[string]string {
	denied := make(map[string]struct{})
	for _, k := range DefaultDeny() {
		denied[k] = struct{}{}
	}
	for _, k := range extraDeny {
		denied[k] = struct{}{}
	}
	for _, k := range allow {
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
