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

// captureRuntime records the tag and the exact Dockerfile bytes handed to a
// build, so a test can assert the built context corresponds to the tag. Its
// BuildArgs returns a no-op argv that exits 0, so buildWith's exec succeeds
// without a real container runtime.
type captureRuntime struct {
	tag        string
	dockerfile []byte
}

func (c *captureRuntime) Name() string                                        { return "capture" }
func (c *captureRuntime) Bin() string                                         { return "capture" }
func (c *captureRuntime) RunArgs(container.Spec, container.Identity) []string { return nil }
func (c *captureRuntime) BuildArgs(tag, contextDir string, _ map[string]string, _ bool) []string {
	c.tag = tag
	c.dockerfile, _ = os.ReadFile(filepath.Join(contextDir, "Dockerfile"))
	return []string{"true"} // a no-op build that exits 0
}
func (c *captureRuntime) InspectLabelArgs(_, _ string) []string {
	return []string{"false"} // image absent, so Ensure proceeds to build
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

func TestDockerfileFooterIsLast(t *testing.T) {
	label := "LABEL ccc.content-hash="

	t.Run("no extra", func(t *testing.T) {
		df, err := testBuilder(t, nil).Dockerfile()
		require.NoError(t, err)
		body := string(df)
		// The verification footer is the final instruction; nothing follows the
		// content-hash LABEL that could shift the image after it is stamped.
		require.Contains(t, body, label)
		require.Equal(t, strings.LastIndex(body, "LABEL "), strings.LastIndex(body, label),
			"ccc.content-hash must be the last LABEL")
	})

	t.Run("extra cannot override the label", func(t *testing.T) {
		dir := t.TempDir()
		extra := filepath.Join(dir, "Dockerfile.extra")
		// A hostile extra stamps its own ccc.content-hash. If ccc's footer did not
		// come last, this value would win and Exists would reject every rebuild.
		require.NoError(t, os.WriteFile(extra,
			[]byte("RUN echo hi\nLABEL ccc.content-hash=hijacked\n"), 0o600))

		id := container.Identity{UID: 1000, GID: 1000, User: "u", Home: "/home/u"}
		b := image.NewBuilder(nil, &config.Config{Image: config.Image{ExtraDockerfile: extra}}, id, "")
		df, err := b.Dockerfile()
		require.NoError(t, err)
		body := string(df)

		// ccc's footer must appear after the extra's content, so its LABEL is the
		// one that survives.
		require.Greater(t, strings.LastIndex(body, label), strings.Index(body, "LABEL ccc.content-hash=hijacked"),
			"ccc's content-hash footer must be stamped after Dockerfile.extra")
		require.True(t, strings.HasSuffix(strings.TrimRight(body, "\n"), "${CCC_CONTENT_HASH}"),
			"the content-hash LABEL must be the final instruction")
	})
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

func TestEnsureBuildsTheSnapshotItTagged(t *testing.T) {
	dir := t.TempDir()
	extra := filepath.Join(dir, "Dockerfile.extra")
	require.NoError(t, os.WriteFile(extra, []byte("RUN echo one\n"), 0o600))

	rt := &captureRuntime{}
	id := container.Identity{UID: 1000, GID: 1000, User: "u", Home: "/home/u"}
	b := image.NewBuilder(rt, &config.Config{Image: config.Image{ExtraDockerfile: extra}}, id, "")

	tag, err := b.Ensure()
	require.NoError(t, err)

	// The image is built from exactly the bytes the tag names: hashing the
	// Dockerfile written into the build context must reproduce the tag it was
	// built under. A second read of Dockerfile.extra between tagging and the
	// build could otherwise cache different content under this benign tag.
	require.Equal(t, tag, rt.tag)
	require.NotEmpty(t, rt.dockerfile, "the build context Dockerfile must be captured")
	require.Equal(t, "ccc:"+b.ContentHashFor(rt.dockerfile), rt.tag,
		"the built Dockerfile must hash to the tag it was built under")
}

func TestPrepareBuildsTheSnapshotItTagged(t *testing.T) {
	dir := t.TempDir()
	extra := filepath.Join(dir, "Dockerfile.extra")
	require.NoError(t, os.WriteFile(extra, []byte("RUN echo pin\n"), 0o600))

	rt := &captureRuntime{}
	id := container.Identity{UID: 1000, GID: 1000, User: "u", Home: "/home/u"}
	b := image.NewBuilder(rt, &config.Config{Image: config.Image{ExtraDockerfile: extra}}, id, "")

	// `ccc pin` goes through Prepare, not Ensure. It must build the exact
	// snapshot it tagged: hashing the Dockerfile written into the build context
	// must reproduce the tag it was built under, or a Dockerfile.extra that
	// changed between tagging and the build could cache different content under
	// this benign tag.
	tag, built, err := b.Prepare(false)
	require.NoError(t, err)
	require.True(t, built, "an absent image must be built")
	require.Equal(t, tag, rt.tag)
	require.NotEmpty(t, rt.dockerfile, "the build context Dockerfile must be captured")
	require.Equal(t, "ccc:"+b.ContentHashFor(rt.dockerfile), rt.tag,
		"the built Dockerfile must hash to the tag it was built under")
}

func TestPrepareSkipsVerifiedImage(t *testing.T) {
	id := container.Identity{UID: 1000, GID: 1000, User: "u", Home: "/home/u"}
	tag, err := image.NewBuilder(nil, &config.Config{}, id, "").Tag()
	require.NoError(t, err)
	hash := strings.TrimPrefix(tag, "ccc:")

	// A verified image is present, so Prepare must not rebuild and must report
	// built=false — the signal cmdPin uses for its "already on <version>"
	// short-circuit. The runtime's BuildArgs would return nil, so a build here
	// would fail loudly.
	b := image.NewBuilder(&fakeRuntime{present: true, label: hash}, &config.Config{}, id, "")
	got, built, err := b.Prepare(false)
	require.NoError(t, err)
	require.Equal(t, tag, got)
	require.False(t, built, "a verified image must not be rebuilt")
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
