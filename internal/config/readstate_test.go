package config_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/stretchr/testify/require"
)

func TestReadStateFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file -> nil,nil", func(t *testing.T) {
		b, err := config.ReadStateFile(filepath.Join(dir, "nope"))
		require.NoError(t, err)
		require.Nil(t, b)
	})

	t.Run("regular file within cap", func(t *testing.T) {
		p := filepath.Join(dir, "ok.json")
		require.NoError(t, os.WriteFile(p, []byte(`{"x":1}`), 0o600))
		b, err := config.ReadStateFile(p)
		require.NoError(t, err)
		require.Equal(t, `{"x":1}`, string(b))
	})

	t.Run("over the cap is rejected, not read into memory unbounded", func(t *testing.T) {
		p := filepath.Join(dir, "big")
		f, err := os.Create(p)
		require.NoError(t, err)
		require.NoError(t, f.Truncate(config.MaxStateFileSize+1))
		require.NoError(t, f.Close())
		_, err = config.ReadStateFile(p)
		require.ErrorContains(t, err, "exceeds")
	})

	t.Run("a FIFO is refused (not a regular file), so it cannot hang the read", func(t *testing.T) {
		p := filepath.Join(dir, "fifo")
		require.NoError(t, syscall.Mkfifo(p, 0o600))
		_, err := config.ReadStateFile(p)
		require.ErrorContains(t, err, "not a regular file")
	})
}
