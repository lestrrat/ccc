package image_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lestrrat-go/ccc/internal/image"
	"github.com/stretchr/testify/require"
)

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
