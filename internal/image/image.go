// Package image builds ccc's container image on demand.
//
// The image is built locally and cached under a tag derived from its inputs.
// There is no registry: the user controls what goes into the image, and
// `Dockerfile.extra` extends it without forking ccc.
package image

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/container"
)

// contentHashLabel is the immutable label baked into the image at build time,
// carrying the full content hash of the Dockerfile and build args. Exists
// verifies it: the tag alone is untrusted because any local image can be tagged
// ccc:<hash>, and running an imposter would hand it the user's profile, repo,
// SSH agent, and host network.
const contentHashLabel = "ccc.content-hash"

// contentHashArg names the build arg carrying the full hash into the build so
// the Dockerfile can stamp it into contentHashLabel. It is passed at build time
// only and is deliberately NOT part of the hashed build args (that would be
// circular), so it never influences the hash it records.
const contentHashArg = "CCC_CONTENT_HASH"

//go:embed Dockerfile
var baseDockerfile []byte

// contentHashFooter is appended after the base Dockerfile AND any user
// Dockerfile.extra, so it is always the final instruction in the composed
// image. Stamping the label last guarantees a user's Dockerfile.extra cannot
// override ccc.content-hash (which would make Exists reject a genuine build
// forever), and keeps CCC_CONTENT_HASH out of ARG scope for the extra's build
// steps. CCC_CONTENT_HASH is passed at build time only and is deliberately NOT
// part of the hashed build args, so it never influences the hash it carries.
const contentHashFooter = "\n# --- ccc content-hash verification (always last) ---\n" +
	"ARG " + contentHashArg + "=\n" +
	"LABEL " + contentHashLabel + "=${" + contentHashArg + "}\n"

// ClaudeBin is where npm installs Claude Code in the image.
//
// ccc execs this absolute path rather than resolving "claude" on PATH: the
// host's $HOME is mounted, and a login shell there sources ~/.profile, which
// typically prepends ~/.local/bin — where a host-native Claude Code lives.
const ClaudeBin = "/usr/local/bin/claude"

// HostNativeBin is the host's native install location, shadowed inside the
// container so `claude` never resolves to the host's binary.
//
// Shadowing governs resolution only. It does NOT prevent replacement: a
// read-only bind mount on a file still allows rename(2) over its directory
// entry. Preventing `claude install` from rewriting the host's installation
// requires mounting the parent directories read-only; see cli.mounts.
const HostNativeBin = ".local/bin/claude"

// shim redirects any in-container lookup of the host's native claude to the
// image's own binary.
const shim = "#!/bin/sh\n# Written by ccc. Shadows the host's native Claude Code inside the container.\nexec " + ClaudeBin + " \"$@\"\n"

// EnsureShim writes the shim into the ccc config root and returns its path.
// Bind-mount sources must be host paths, so the shim cannot live in the image.
func EnsureShim(root string) (string, error) {
	path := filepath.Join(root, "shim", "claude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("failed to create shim dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(shim), 0o755); err != nil {
		return "", fmt.Errorf("failed to write %s: %w", path, err)
	}
	return path, nil
}

// Builder produces and caches the ccc image for one host identity.
type Builder struct {
	rt      container.Runtime
	cfg     *config.Config
	id      container.Identity
	version string
}

// NewBuilder returns a Builder. The receiver is immutable configuration.
//
// version is the resolved Claude Code pin — a profile's override, else the
// global one, else "". It is a separate parameter rather than read off cfg
// because it varies per profile while cfg does not.
func NewBuilder(rt container.Runtime, cfg *config.Config, id container.Identity, version string) *Builder {
	return &Builder{rt: rt, cfg: cfg, id: id, version: version}
}

// dockerfile returns the effective Dockerfile: the embedded base, then the
// user's Dockerfile.extra (if any) appended verbatim, then ccc's content-hash
// verification footer as the final instruction. The footer is last so a user's
// Dockerfile.extra can neither override ccc.content-hash nor see
// CCC_CONTENT_HASH in its ARG scope.
func (b *Builder) dockerfile() ([]byte, error) {
	out := make([]byte, 0, len(baseDockerfile)+len(contentHashFooter)+256)
	out = append(out, baseDockerfile...)

	if b.cfg.Image.ExtraDockerfile != "" {
		extra, err := os.ReadFile(b.cfg.Image.ExtraDockerfile)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", b.cfg.Image.ExtraDockerfile, err)
		}
		out = append(out, "\n# --- Dockerfile.extra ---\n"...)
		out = append(out, extra...)
	}

	out = append(out, contentHashFooter...)
	return out, nil
}

func (b *Builder) buildArgs() map[string]string {
	return map[string]string{
		"UID":      strconv.Itoa(b.id.UID),
		"GID":      strconv.Itoa(b.id.GID),
		"USERNAME": b.id.User,
		// The container user's home must equal the host's, so identical absolute
		// paths mean the same thing across the mount. It is not always
		// /home/<user> (macOS, ostree, LDAP), and every mount lands at id.Home.
		"HOME":           b.id.Home,
		"CLAUDE_VERSION": b.claudeVersion(),
	}
}

// claudeVersion is the resolved pin, or npm's "latest" dist-tag when nothing is
// pinned. Because the tag hashes the build args, a pinned version that changes
// produces a new tag and Ensure() rebuilds; an unpinned "latest" hashes to a
// stable tag and is therefore never revisited. That is deliberate: `ccc` must
// not silently reinstall Claude Code behind the user's back.
func (b *Builder) claudeVersion() string {
	if b.version != "" {
		return b.version
	}
	return config.LatestClaudeVersion
}

// contentHash composes the Dockerfile and hashes that snapshot together with
// the build args. It is a convenience over contentHashFor for callers that only
// need the hash; Prepare composes once and calls contentHashFor directly so the
// tag and the built image derive from the same bytes.
func (b *Builder) contentHash() (string, error) {
	df, err := b.dockerfile()
	if err != nil {
		return "", err
	}
	return b.contentHashFor(df), nil
}

// contentHashFor is the full hex SHA-256 over the given composed Dockerfile
// bytes and the build args. The Tag embeds it and the build stamps it into
// contentHashLabel, so an existing image can be verified against it rather than
// trusted by tag. It takes the composed bytes as an argument so a caller can
// hash exactly the snapshot it will build into the context — the tag and the
// built image must correspond, or different content could be cached under a
// benign content-hash tag.
//
// The full digest — not a prefix — is used: a truncated tag is only a naming
// convenience, but a short hash is cheap to collide, and Exists relies on this
// value being collision-resistant to reject a pre-tagged imposter image.
func (b *Builder) contentHashFor(df []byte) string {
	h := sha256.New()
	h.Write(df)

	args := b.buildArgs()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "\x00%s=%s", k, args[k])
	}

	return hex.EncodeToString(h.Sum(nil))
}

// Tag is the content-addressed image tag. It covers the Dockerfile and the
// build args, so changing either — including switching hosts — rebuilds rather
// than silently reusing an image with the wrong UID baked in.
func (b *Builder) Tag() (string, error) {
	hash, err := b.contentHash()
	if err != nil {
		return "", err
	}
	return "ccc:" + hash, nil
}

// Exists reports whether an image for tag is present AND was built by ccc from
// exactly these inputs. The tag is not trusted on its own: any local image can
// be tagged ccc:<hash>, so ccc reads the immutable content-hash label baked in
// at build time and accepts the image only when it matches the expected hash.
// A missing image, a missing label, or a mismatch is treated as absent so ccc
// rebuilds rather than running an attacker-supplied image.
func (b *Builder) Exists(tag string) bool {
	want, err := b.contentHash()
	if err != nil {
		return false
	}
	return b.existsWith(tag, want)
}

// existsWith reports whether tag's image carries a ccc.content-hash label equal
// to want. Ensure passes the hash of the single Dockerfile snapshot it is about
// to build, avoiding a second read of Dockerfile.extra between tagging and the
// existence check.
func (b *Builder) existsWith(tag, want string) bool {
	args := b.rt.InspectLabelArgs(tag, contentHashLabel)
	out, err := exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == want
}

// Prepare composes the Dockerfile exactly once and threads that single snapshot
// through the tag, the existence check, and the build context, returning the
// resolved tag and whether a build actually ran.
//
// Composing once is what closes the tag<->build TOCTOU: reading Dockerfile.extra
// again for the build could otherwise build different content than the tag names
// and cache it under that benign content-hash tag. Ensure and `ccc pin` both go
// through here, so neither path can reintroduce that split.
//
// With noCache, or when no verified image is present, it builds and reports
// built=true. When a verified image already exists and noCache is false it
// builds nothing and reports built=false — the signal `ccc pin` uses to decide
// it is already on the requested version.
func (b *Builder) Prepare(noCache bool) (tag string, built bool, err error) {
	df, err := b.dockerfile()
	if err != nil {
		return "", false, err
	}
	hash := b.contentHashFor(df)
	tag = "ccc:" + hash
	if noCache || !b.existsWith(tag, hash) {
		if err := b.buildWith(tag, df, noCache); err != nil {
			return "", false, err
		}
		return tag, true, nil
	}
	return tag, false, nil
}

// Ensure returns the image tag, building the image first if it is missing. It is
// Prepare with caching left on and the build flag discarded.
func (b *Builder) Ensure() (string, error) {
	tag, _, err := b.Prepare(false)
	return tag, err
}

// buildWith builds the image from the given composed Dockerfile bytes,
// streaming runtime output to the user's terminal.
func (b *Builder) buildWith(tag string, df []byte, noCache bool) error {
	dir, err := os.MkdirTemp("", "ccc-build-*")
	if err != nil {
		return fmt.Errorf("failed to create build context: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), df, 0o600); err != nil {
		return fmt.Errorf("failed to write Dockerfile: %w", err)
	}

	fmt.Fprintf(os.Stderr, "ccc: building image %s (first run takes a few minutes)\n", tag)

	// Stamp the full content hash into the image as a label so Exists can verify
	// this image is genuinely ccc's. It is added here rather than in buildArgs so
	// it never feeds back into the hash it records. tag is "ccc:<hash>", so the
	// hash is recovered from it without re-reading the Dockerfile.
	buildArgs := b.buildArgs()
	buildArgs[contentHashArg] = strings.TrimPrefix(tag, "ccc:")

	args := b.rt.BuildArgs(tag, dir, buildArgs, noCache)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stderr // keep stdout clean for the contained process
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build image %s: %w", tag, err)
	}
	return nil
}
