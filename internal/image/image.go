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

// Builder produces and caches the ccc image for one host identity.
type Builder struct {
	rt  container.Runtime
	cfg *config.Config
	id  container.Identity
}

// NewBuilder returns a Builder. The receiver is immutable configuration.
func NewBuilder(rt container.Runtime, cfg *config.Config, id container.Identity) *Builder {
	return &Builder{rt: rt, cfg: cfg, id: id}
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
	}
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
