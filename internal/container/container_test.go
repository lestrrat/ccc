package container_test

import (
	"slices"
	"testing"

	"github.com/lestrrat-go/ccc/internal/container"
	"github.com/stretchr/testify/require"
)

func runtimeOrSkip(t *testing.T, name string) container.Runtime {
	t.Helper()
	rt, err := container.Detect(name)
	if err != nil {
		t.Skipf("%s not installed: %s", name, err)
	}
	return rt
}

var testIdentity = container.Identity{UID: 501, GID: 20, User: "u", Home: "/home/u"}

func testSpec() container.Spec {
	return container.Spec{
		Image:   "ccc:abc123",
		Workdir: "/home/u/src/proj",
		Mounts: []container.Mount{
			// Deliberately declared child-first to prove ordering is enforced.
			{Source: "/p/claude.json", Target: "/home/u/.claude.json"},
			{Source: "/p/claude", Target: "/home/u/.claude"},
			{Source: "/gh", Target: "/home/u/.config/gh", ReadOnly: true},
			{Source: "/home/u", Target: "/home/u"},
		},
		Env: []string{"GOPRIVATE=x"},
		Cmd: []string{"claude", "--resume"},
	}
}

// indexOf returns the position of the "-v src:tgt[:ro]" value in argv.
func indexOfMount(argv []string, value string) int {
	return slices.Index(argv, value)
}

func TestMountOrdering(t *testing.T) {
	rt := runtimeOrSkip(t, "docker")
	argv := rt.RunArgs(testSpec(), testIdentity)

	home := indexOfMount(argv, "/home/u:/home/u")
	claude := indexOfMount(argv, "/p/claude:/home/u/.claude")
	claudeJSON := indexOfMount(argv, "/p/claude.json:/home/u/.claude.json")
	gh := indexOfMount(argv, "/gh:/home/u/.config/gh:ro")

	require.NotEqual(t, -1, home)
	require.NotEqual(t, -1, gh)

	// The profile must be layered on top of $HOME, otherwise the host's real
	// ~/.claude shadows it and profile switching silently does nothing.
	require.Less(t, home, claude, "$HOME must be mounted before ~/.claude")
	require.Less(t, home, claudeJSON, "$HOME must be mounted before ~/.claude.json")
	require.Less(t, home, gh, "$HOME must be mounted before ~/.config/gh")
}

func TestReadOnlyMount(t *testing.T) {
	rt := runtimeOrSkip(t, "docker")
	argv := rt.RunArgs(testSpec(), testIdentity)
	require.Contains(t, argv, "/gh:/home/u/.config/gh:ro")
	require.Contains(t, argv, "/p/claude:/home/u/.claude", "profile must be writable")
}

func TestDockerRunArgs(t *testing.T) {
	rt := runtimeOrSkip(t, "docker")
	argv := rt.RunArgs(testSpec(), testIdentity)

	require.Equal(t, rt.Bin(), argv[0])
	require.Equal(t, "run", argv[1], "--user must follow the run verb, not precede it")
	require.Equal(t, "--user", argv[2])
	require.Equal(t, "501:20", argv[3])

	require.Contains(t, argv, "--rm")
	require.Contains(t, argv, "host")
	require.Equal(t, []string{"ccc:abc123", "claude", "--resume"}, argv[len(argv)-3:],
		"image then command must terminate argv")
}

func TestPodmanRunArgs(t *testing.T) {
	rt := runtimeOrSkip(t, "podman")
	argv := rt.RunArgs(testSpec(), testIdentity)

	require.Equal(t, rt.Bin(), argv[0])
	// --userns is a `podman run` flag, not a global one: it must FOLLOW the verb.
	// Placing it before `run` makes podman fail with "unknown flag: --userns".
	require.Equal(t, "run", argv[1])
	require.Equal(t, "--userns=keep-id:uid=501,gid=20", argv[2])
	require.NotContains(t, argv, "--user", "keep-id already maps the host user")
}

func TestWorkdirAndEnv(t *testing.T) {
	rt := runtimeOrSkip(t, "docker")
	argv := rt.RunArgs(testSpec(), testIdentity)

	w := slices.Index(argv, "-w")
	require.NotEqual(t, -1, w)
	require.Equal(t, "/home/u/src/proj", argv[w+1])
	require.Contains(t, argv, "GOPRIVATE=x")
}

func TestDetectUnknown(t *testing.T) {
	_, err := container.Detect("lxc")
	require.ErrorContains(t, err, "unknown runtime")
}
