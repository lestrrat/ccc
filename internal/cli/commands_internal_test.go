package cli

import (
	"testing"

	"github.com/lestrrat-go/ccc/internal/config"
	"github.com/lestrrat-go/ccc/internal/profile"
	"github.com/stretchr/testify/require"
)

// Stray trailing args must error, matching the parser's "never a guess"
// contract, rather than being silently dropped.
func TestProfileListRejectsStrayArgs(t *testing.T) {
	a := &app{store: profile.NewStore(t.TempDir(), t.TempDir()), cfg: &config.Config{}}
	require.NoError(t, profileList(a, nil))
	require.ErrorContains(t, profileList(a, []string{"junk"}), "unexpected argument")
	require.ErrorContains(t, profileList(a, []string{"-p", "work"}), "unexpected argument")
}
