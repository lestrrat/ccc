package image_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/container"
	"github.com/lestrrat-go/ccc/internal/image"
	"github.com/stretchr/testify/require"
)

// fakeRuntime lets Exists be tested without a real container runtime: its
// InspectLabelArgs returns a tiny argv that either prints a chosen label value
// or exits non-zero to simulate an absent image.
type fakeRuntime struct {
	label   string
	present bool
}

func (f *fakeRuntime) Name() string                                        { return "fake" }
func (f *fakeRuntime) Bin() string                                         { return "fake" }
func (f *fakeRuntime) RunArgs(container.Spec, container.Identity) []string { return nil }
func (f *fakeRuntime) BuildArgs(string, string, map[string]string, bool) []string {
	return nil
}
func (f *fakeRuntime) InspectLabelArgs(_, _ string) []string {
	if !f.present {
		return []string{"false"} // image absent: inspect exits non-zero
	}
	return []string{"printf", "%s", f.label}
}

func testBuilder(t *testing.T, rt container.Runtime) *image.Builder {
	t.Helper()
	id := container.Identity{UID: 1000, GID: 1000, User: "u", Home: "/home/u"}
	return image.NewBuilder(rt, &config.Config{}, id, "")
}

func TestTagIsFullContentHash(t *testing.T) {
	tag, err := testBuilder(t, &fakeRuntime{}).Tag()
	require.NoError(t, err)
	hash, ok := strings.CutPrefix(tag, "ccc:")
	require.True(t, ok, "tag must be ccc:<hash>")
	// A full SHA-256, not a truncated prefix: a short tag is cheap to collide,
	// and Exists relies on this hash being collision-resistant.
	require.Len(t, hash, 64, "content hash must be the full 64-hex sha256")
	require.Regexp(t, "^[0-9a-f]{64}$", hash)
}

func TestExistsAcceptsMatchingLabel(t *testing.T) {
	b := testBuilder(t, nil)
	tag, err := b.Tag()
	require.NoError(t, err)
	hash := strings.TrimPrefix(tag, "ccc:")

	// The image carries a label equal to the expected content hash.
	b = testBuilder(t, &fakeRuntime{present: true, label: hash})
	require.True(t, b.Exists(tag))
}

func TestExistsRejectsMismatchedLabel(t *testing.T) {
	// A pre-tagged imposter image is present but its label does not match the
	// expected content hash, so it must be treated as absent and rebuilt.
	b := testBuilder(t, &fakeRuntime{present: true, label: "deadbeef"})
	tag, err := b.Tag()
	require.NoError(t, err)
	require.False(t, b.Exists(tag))
}

func TestExistsRejectsMissingLabel(t *testing.T) {
	// Image present, but built by something other than ccc: no label at all.
	b := testBuilder(t, &fakeRuntime{present: true, label: ""})
	tag, err := b.Tag()
	require.NoError(t, err)
	require.False(t, b.Exists(tag))
}

func TestExistsRejectsAbsentImage(t *testing.T) {
	b := testBuilder(t, &fakeRuntime{present: false})
	tag, err := b.Tag()
	require.NoError(t, err)
	require.False(t, b.Exists(tag))
}

func TestEnsureShim(t *testing.T) {
	root := t.TempDir()

	path, err := image.EnsureShim(root)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "shim", "claude"), path)

	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.NotZero(t, fi.Mode().Perm()&0o111, "shim must be executable")

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	body := string(b)
	require.Contains(t, body, "#!/bin/sh")
	// It must exec the image's binary, never resolve "claude" on PATH again —
	// that would loop straight back into the host's ~/.local/bin/claude.
	require.Contains(t, body, "exec "+image.ClaudeBin+" \"$@\"")
}

func TestEnsureShimIsIdempotent(t *testing.T) {
	root := t.TempDir()

	first, err := image.EnsureShim(root)
	require.NoError(t, err)
	second, err := image.EnsureShim(root)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestClaudeBinIsAbsolute(t *testing.T) {
	// ccc execs this directly. A bare name would be resolved against a PATH
	// that can reach the mounted host home.
	require.True(t, filepath.IsAbs(image.ClaudeBin))
	require.False(t, filepath.IsAbs(image.HostNativeBin), "relative to $HOME")
}
