package cli

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A hostile SSH_AUTH_SOCK pointing at a dir/file must not become an rw mount;
// only an actual socket is forwarded.
func TestSSHAuthSock(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("SSH_AUTH_SOCK", "")
	require.Empty(t, sshAuthSock(), "unset -> empty")

	t.Setenv("SSH_AUTH_SOCK", dir)
	require.Empty(t, sshAuthSock(), "a directory (e.g. $HOME) is refused")

	reg := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(reg, nil, 0o600))
	t.Setenv("SSH_AUTH_SOCK", reg)
	require.Empty(t, sshAuthSock(), "a regular file (e.g. a private key) is refused")

	sockPath := filepath.Join(dir, "agent.sock")
	l, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer l.Close()
	t.Setenv("SSH_AUTH_SOCK", sockPath)
	require.Equal(t, sockPath, sshAuthSock(), "a real socket is forwarded")
}
