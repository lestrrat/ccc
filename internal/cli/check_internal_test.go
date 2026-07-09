package cli

import (
	"testing"

	"github.com/lestrrat-go/ccc/internal/container"
	"github.com/stretchr/testify/require"
)

// Outside a git repository the implicit workspace dir is the cwd. `ccc` run
// from $HOME would otherwise mount the whole home read-write — the exposure the
// narrow default exists to prevent, reached by accident rather than by config.
func TestCheckImplicitDir(t *testing.T) {
	a := &app{id: container.Identity{Home: "/home/u"}}

	t.Run("refuses the filesystem root", func(t *testing.T) {
		require.ErrorContains(t, a.checkImplicitDir("/"), "refusing to mount /")
	})

	t.Run("refuses the home directory", func(t *testing.T) {
		require.ErrorContains(t, a.checkImplicitDir("/home/u"), "your home directory")
	})

	t.Run("refuses an ancestor of the home directory", func(t *testing.T) {
		require.ErrorContains(t, a.checkImplicitDir("/home"), "contains your home directory")
	})

	t.Run("allows a repository under home", func(t *testing.T) {
		require.NoError(t, a.checkImplicitDir("/home/u/dev/src/proj"))
	})

	t.Run("allows a directory outside home", func(t *testing.T) {
		require.NoError(t, a.checkImplicitDir("/opt/work"))
	})

	t.Run("allows a sibling of home", func(t *testing.T) {
		require.NoError(t, a.checkImplicitDir("/home/other"))
	})
}
