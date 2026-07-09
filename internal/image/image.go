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

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/container"
)

//go:embed Dockerfile
var baseDockerfile []byte

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

// dockerfile returns the effective Dockerfile: the embedded base with the
// user's Dockerfile.extra appended verbatim.
func (b *Builder) dockerfile() ([]byte, error) {
	out := make([]byte, 0, len(baseDockerfile)+256)
	out = append(out, baseDockerfile...)

	if b.cfg.Image.ExtraDockerfile == "" {
		return out, nil
	}
	extra, err := os.ReadFile(b.cfg.Image.ExtraDockerfile)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", b.cfg.Image.ExtraDockerfile, err)
	}

	out = append(out, "\n# --- Dockerfile.extra ---\n"...)
	out = append(out, extra...)
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
	return config.DefaultClaudeVersion
}

// Tag is the content-addressed image tag. It covers the Dockerfile and the
// build args, so changing either — including switching hosts — rebuilds rather
// than silently reusing an image with the wrong UID baked in.
func (b *Builder) Tag() (string, error) {
	df, err := b.dockerfile()
	if err != nil {
		return "", err
	}

	h := sha256.New()
	h.Write(df)

	args := b.buildArgs()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%s", k, args[k])
	}

	return "ccc:" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// Exists reports whether the tagged image is already present.
func (b *Builder) Exists(tag string) bool {
	args := b.rt.InspectArgs(tag)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// Ensure returns the image tag, building the image first if it is missing.
func (b *Builder) Ensure() (string, error) {
	tag, err := b.Tag()
	if err != nil {
		return "", err
	}
	if b.Exists(tag) {
		return tag, nil
	}
	if err := b.Build(tag, false); err != nil {
		return "", err
	}
	return tag, nil
}

// Build builds the image, streaming runtime output to the user's terminal.
func (b *Builder) Build(tag string, noCache bool) error {
	df, err := b.dockerfile()
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "ccc-build-*")
	if err != nil {
		return fmt.Errorf("failed to create build context: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), df, 0o600); err != nil {
		return fmt.Errorf("failed to write Dockerfile: %w", err)
	}

	fmt.Fprintf(os.Stderr, "ccc: building image %s (first run takes a few minutes)\n", tag)

	args := b.rt.BuildArgs(tag, dir, b.buildArgs(), noCache)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stderr // keep stdout clean for the contained process
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build image %s: %w", tag, err)
	}
	return nil
}
