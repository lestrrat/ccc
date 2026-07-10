package env_test

import (
	"testing"

	"github.com/lestrrat-go/ccc/internal/env"
	"github.com/stretchr/testify/require"
)

func TestFilter(t *testing.T) {
	environ := []string{
		"GIT_SSH_COMMAND=ssh -i /home/u/.ssh/id_work",
		"GOPRIVATE=github.com/acme/*",
		"HOME=/home/u",
		"PATH=/usr/bin",
		"PWD=/home/u/src",
		"SSH_AUTH_SOCK=/run/agent.sock",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"CLAUDE_CONFIG_DIR=/tmp/shared",
		"ANTHROPIC_AUTH_TOKEN=tok",
		"MALFORMED",
		"=novalue",
	}

	t.Run("forwards direnv exports", func(t *testing.T) {
		got := env.Filter(environ, nil, nil)
		require.Equal(t, "ssh -i /home/u/.ssh/id_work", got["GIT_SSH_COMMAND"])
		require.Equal(t, "github.com/acme/*", got["GOPRIVATE"])
	})

	t.Run("drops container-managed vars", func(t *testing.T) {
		got := env.Filter(environ, nil, nil)
		for _, k := range []string{"HOME", "PATH", "PWD"} {
			require.NotContains(t, got, k)
		}
	})

	t.Run("drops credentials that would hijack the profile", func(t *testing.T) {
		got := env.Filter(environ, nil, nil)
		require.NotContains(t, got, "ANTHROPIC_API_KEY")
		require.NotContains(t, got, "ANTHROPIC_AUTH_TOKEN")
		require.NotContains(t, got, "CLAUDE_CONFIG_DIR", "would relocate profile state and split the account boundary")
	})

	t.Run("drops SSH_AUTH_SOCK; run logic re-adds it", func(t *testing.T) {
		got := env.Filter(environ, nil, nil)
		require.NotContains(t, got, "SSH_AUTH_SOCK")
	})

	// These name host paths the container does not mount. Inheriting them makes
	// `go build` fail against a GOMODCACHE that does not exist inside, in a way
	// that reads like a Go bug rather than a mount bug.
	t.Run("drops host-path-scoped go vars", func(t *testing.T) {
		got := env.Filter([]string{
			"GOPATH=/home/u/go",
			"GOCACHE=/home/u/.cache/go-build",
			"GOMODCACHE=/home/u/go/pkg/mod",
			"GOBIN=/home/u/go/bin",
			"GOPRIVATE=github.com/acme/*",
		}, nil, nil)

		for _, k := range []string{"GOPATH", "GOCACHE", "GOMODCACHE", "GOBIN"} {
			require.NotContains(t, got, k)
		}
		require.Contains(t, got, "GOPRIVATE", "not a path; must still be forwarded")
	})

	t.Run("allow re-admits a denied var", func(t *testing.T) {
		got := env.Filter(environ, nil, []string{"ANTHROPIC_API_KEY"})
		require.Equal(t, "sk-ant-secret", got["ANTHROPIC_API_KEY"])
	})

	t.Run("allow wins over extra deny", func(t *testing.T) {
		got := env.Filter(environ, []string{"GOPRIVATE"}, []string{"GOPRIVATE"})
		require.Contains(t, got, "GOPRIVATE")
	})

	t.Run("extra deny drops a forwarded var", func(t *testing.T) {
		got := env.Filter(environ, []string{"GOPRIVATE"}, nil)
		require.NotContains(t, got, "GOPRIVATE")
	})

	t.Run("skips malformed entries", func(t *testing.T) {
		got := env.Filter(environ, nil, nil)
		require.NotContains(t, got, "MALFORMED")
		require.NotContains(t, got, "")
	})
}

func TestPairs(t *testing.T) {
	got := env.Pairs(map[string]string{"B": "2", "A": "1"})
	require.Equal(t, []string{"A=1", "B=2"}, got, "sorted for deterministic argv")
}
