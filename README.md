# ccc — Claude Code Contained

Run Claude Code in a container so that `~/.claude` can be swapped per account, without touching the host's real configuration.

```sh
ccc                                         # first run: creates a "default" profile, then logs you in
ccc profile create work                     # a second account
ccc -p work                                 # Claude Code prompts you to log in
cd ~/dev/src/acme && echo '{"profile": "work"}' > .ccc.json
ccc                                         # starts Claude Code as the "work" account
```

## Why

Claude Code splits its state across two host paths:

| Path | Contents |
|------|----------|
| `~/.claude/` | credentials, `CLAUDE.md`, `agents/`, `projects/`, caches |
| `~/.claude.json` | project registry, onboarding and MCP state |

Renaming or symlinking `~/.claude` breaks, because `~/.claude.json` is left behind and stays shared across accounts. ccc mounts **both** from a per-profile directory, so switching accounts is total and atomic.

### Why not CLAUDE_CONFIG_DIR

Claude Code honors `CLAUDE_CONFIG_DIR`, and it does relocate `.claude.json`. It is still not enough: `~/.claude/CLAUDE.md` is read from the real home regardless.

Traced with `CLAUDE_CONFIG_DIR=~/cfg` set, both files are opened and read:

```
openat("/home/u/cfg/CLAUDE.md",     O_RDONLY|O_NOCTTY) = 21
openat("/home/u/.claude/CLAUDE.md", O_RDONLY|O_NOCTTY) = 19
```

So one account's global memory leaks into every other account's sessions. Claude Code finds its state through several mechanisms — `CLAUDE_CONFIG_DIR`, `$HOME/.claude/`, `$HOME/.claude.json` — and only a mount covers all of them at once, because it makes the *path itself* resolve to the profile no matter which mechanism does the lookup.

Do not replace the mounts with an environment variable.

## Non-goals

ccc is **not a security sandbox.** The container isolates Claude Code profiles; that is the whole of its job.

The contained agent has host networking, your SSH keys, a forwarded SSH agent, and read-write access to the repository you launched it in. It can push code and reach services on localhost. Do not reason about ccc as a blast-radius control.

It does *not* mount your home directory, but that is a consequence of the default being narrow, not a security claim. `mounts.home` opts back in.

## Install

```sh
go install github.com/lestrrat-go/ccc/cmd/ccc@latest
```

Requires `podman` (preferred) or `docker`. The image is built locally on first run; there is no registry.

## Profiles

One profile = one account = one host directory = one container `$HOME` overlay.

```
~/.config/ccc/
  config.json              # global config
  Dockerfile.extra         # optional, appended to the base image
  profiles/
    work/
      claude/              # mounted at $HOME/.claude
      claude.json          # mounted at $HOME/.claude.json
      profile.json         # optional per-profile overrides
      cache/               # only when mounts.cache is on
```

```sh
ccc profile create work              # empty profile
ccc profile create work --from ~/.claude   # seed from an existing config
ccc profile list                     # `*` marks default_profile
ccc profile rm work                  # deletes credentials too (prompts; -f to skip)
```

There is no `ccc login`. A profile with no credentials is just a fresh `~/.claude`, so Claude Code runs its own setup and prompts you — exactly as it would on the host. To re-authenticate an existing profile, pass its own command through:

```sh
ccc -p work -- auth login
```

### Selection

First match wins:

1. `--profile <name>`
2. `.ccc.json` in the current directory or an ancestor
3. `default_profile` in `config.json`

Otherwise ccc errors and lists the available profiles. It never guesses — a wrong-account run is worse than a failed one. Every run prints the resolved profile to stderr, so a `default_profile` run is never silent about which account it used.

### First run

With no profiles at all, `ccc` creates one named `default` and records it as `default_profile`, so a bare `ccc` keeps working once you add a second profile later.

This is the one case where ccc acts without being told which account to use, and it is safe precisely because there are zero profiles: there is no account to pick wrongly. The moment one exists, an unresolved run is an error again — a typo'd `--profile` never creates anything.

The new profile is **empty**. ccc does not copy credentials without being asked, so Claude Code will prompt you to log in. To start from your existing setup instead:

```sh
ccc profile create default --from ~/.claude
```

## Usage

`ccc` starts Claude Code. The container is an implementation detail, not something you name:

```sh
ccc                                    # claude
ccc -- --resume                        # claude --resume
ccc -p work -- --resume                # ... as profile "work"
ccc -- -p "explain this"               # claude's --print
ccc -- doctor                          # `claude doctor`
ccc --help                             # ccc's help
ccc -- --help                          # claude's help
```

**Everything before `--` belongs to ccc; everything after goes to claude verbatim.** The split is structural, not best-effort: an argument ccc does not recognize is an error, never a guess.

That matters because the two share a flag namespace. `-p` is `--profile` in ccc and `--print` in Claude Code, so a permissive parser would swallow `ccc -p "explain this"` as a profile name. And every flag Claude Code adds would be a new collision. With a strict split, neither can shadow the other, now or later.

The cost is two characters on the common path. In exchange, ccc's flags are `-p/--profile`, `--runtime`, `-h/--help`, its commands are `profile`, `pin`, `check`, `help`, `version`, and nothing else before `--` is accepted:

```
$ ccc --resume
ccc: unknown flag "--resume"
ccc's own flags precede --; claude's go after it:
  ccc -- --resume
```

Other commands:

```sh
ccc pin                  # pin the latest Claude Code and rebuild
ccc check                # verify a session would start; non-zero if not
ccc help                 # same as ccc --help
```

`ccc check` runs the same preflight `ccc` runs — working directory, mount sources, profile, pin — and then **actually starts a container** with the real mounts and identity. That last step is the only thing that catches a malformed argument vector; a wrong flag order fails at `podman run` and nowhere else.

There is no `build` command: the image builds itself on first run, and whenever the pin or `Dockerfile.extra` changes.

## What the container sees

**Your repository, and nothing else of your home directory.**

| Host | Container | Mode |
|------|-----------|------|
| the repository (see below) | identical absolute path | rw |
| `profiles/<name>/claude/` | `$HOME/.claude` | rw |
| `profiles/<name>/claude.json` | `$HOME/.claude.json` | rw |
| `~/.ssh` | `$HOME/.ssh` | ro |
| `~/.gitconfig` | `$HOME/.gitconfig` | ro |
| `$SSH_AUTH_SOCK` | same path | ro |
| gh config dir | `$HOME/.config/gh` | ro |

`$HOME` itself is **not** mounted. So the host's `~/.local`, `~/go`, and `~/.cache` do not exist inside the container, and neither does the host's Claude Code installation.

The container user mirrors your UID, GID, username, and home directory, and every directory is mounted at its **identical absolute path**. Absolute paths therefore mean the same thing on both sides of the mount, and files written into your repositories are owned by you.

Networking is `--network=host`: dev servers on localhost stay reachable, and Claude Code's OAuth loopback callback lands on your browser during login.

### Which directories

By default, the repository the working directory belongs to — which is not the same as the working directory:

- `git rev-parse --show-toplevel`, so running `ccc` from a subdirectory still gives git a repository
- `git rev-parse --git-common-dir`, when it lives outside the root

The second is what makes **worktrees** work. A worktree's `.git` is a *file* containing `gitdir: <main-repo>/.git/worktrees/<name>`. Mount only the worktree and the container gets a dangling gitdir, so every `git` command fails.

Outside a git repository, it is just the working directory — unless that is `/`, your home directory, or a parent of it. ccc refuses to mount those implicitly:

```
FAIL  mounts   refusing to mount your home directory /home/u implicitly
run ccc inside a git repository, or name directories in mounts.dirs
```

Naming such a directory in `mounts.dirs` is your call; falling into it by running `ccc` from the wrong place is not.

The working directory must live under a mounted directory. ccc refuses to run otherwise rather than silently mounting it.

### Extra directories

`dirs` names extra host directories, mounted read-write at their identical absolute paths. They are **additive** — always on top of the repository, never instead of it, so nothing can unmount the repo you are standing in.

Machine-wide, in `config.json`:

```json
{"mounts": {"dirs": ["~/dev/src/github.com/jwx-go/mlkem"]}}
```

Or per checkout, in `.ccc.json`:

```json
{"profile": "work", "dirs": ["~/dev/src/github.com/jwx-go/mlkem"]}
```

Paths must be **absolute or `~/`-prefixed**. A relative path needs a base, and the two plausible bases — the config file's directory, and the working directory — disagree; rather than pick one and surprise half the users, ccc rejects it. A directory that does not exist on the host is a hard error at mount time, because that beats discovering it from inside the container.

ccc infers nothing. It does not read `go.mod`, and knows nothing about `replace` directives, `go.work`, or any other language's vendoring. If a build needs a sibling checkout, name it.

The motivating case: `lestrrat-go/jwx` with `replace github.com/jwx-go/mlkem => ../../jwx-go/mlkem`. Mount only `jwx` and the build fails —

```
main.go:6:2: github.com/jwx-go/mlkem@v0.0.0:
    replacement directory ../../jwx-go/mlkem does not exist
```

— because `../../jwx-go/mlkem` resolves outside the mount. Naming `mlkem` in `dirs` mounts it at its identical absolute path, so the *same relative path* resolves inside the container, and `go.mod` needs no changes.

> `.ccc.json` is a **per-checkout, per-user** file. Profile names differ between users, and so do these paths. Do not commit it; add it to `.gitignore`.

### Mounting `$HOME`

```json
{"mounts": {"home": "ro"}}
```

`"ro"` mounts your home read-only, with the repository and `mounts.dirs` read-write on top — deeper mounts win, so the repository stays writable while the rest of your home is not. This is the safe way to get breadth.

It matters because a read-only *parent directory* is what actually stops the container replacing files in it. A read-only bind mount on a file does not: `rename(2)` swaps the directory entry, and the directory would still be writable. This is not theoretical — `claude install`, which Claude Code suggests when its self-update fails, does exactly that to `~/.local/bin/claude`.

`"rw"` also exists. It mounts your home writable, and then ccc read-only-mounts `~/.local/bin` and `~/.local/share/claude` to keep the container from rewriting your host's Claude Code. Enumerating the paths that matter is guesswork; `"ro"` is a boundary. Prefer `"ro"`.

### Caches

Ephemeral. `~/.cache` and `~/go` live inside the container and vanish with it, so every session builds cold.

```json
{"mounts": {"cache": true}}
```

mounts a **profile-owned** cache directory (`profiles/<name>/cache/`) at the container's `~/.cache`, and points `GOMODCACHE` at `~/.cache/go-mod`. `GOCACHE` already defaults under `~/.cache`, so it follows for free.

ccc never mounts the *host's* `~/go/pkg/mod` or `~/.cache/go-build`. That would be a writable hole in a read-only `$HOME`, and a macOS host's `~/go/bin` is Mach-O binaries that a Linux container would put on `PATH`.

If you genuinely want to share one cache with the host, symlink the host's directory into the profile's `cache/`, and own that decision explicitly.

> **Not settled.** The cache design — profile-owned rather than host-shared, `GOMODCACHE` via environment rather than `go env -w` — is provisional. `go env -w` was rejected because it persists to the container's ephemeral `~/.config/go/env`. If the ephemeral-by-default choice proves annoying in practice, this is the first thing to revisit.

Host `GOPATH`, `GOCACHE`, `GOMODCACHE`, and `GOBIN` are dropped from the inherited environment: they name host paths the container does not mount, and inheriting them makes `go build` fail in a way that reads like a Go bug rather than a mount bug.

## Environment

ccc inherits the **whole host environment minus a denylist**, so direnv exports reach the container with no ccc-side configuration.

Always dropped:

| Variable | Why |
|----------|-----|
| `HOME`, `PATH`, `USER`, `LOGNAME`, `SHELL`, `PWD`, `OLDPWD`, `TMPDIR`, `TMP`, `TEMP`, `HOSTNAME` | container-managed; the host values are wrong inside |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` | would override the profile's own credentials and silently route every profile to a single account |
| `CLAUDE_CONFIG_DIR` | Claude Code honors it, so a forwarded value relocates state out of the mounted profile and splits the account boundary |

`SSH_AUTH_SOCK` is forwarded **only when it points at an actual socket**, with that socket bind-mounted read-only at the same path. A value that is a directory or file (a hostile `SSH_AUTH_SOCK=$HOME` or `=~/.ssh/id_rsa`) is refused rather than mounted.

Extend with `env.deny`; re-admit a dropped variable with `env.allow` (which wins over every deny rule) — except the two categories ccc controls itself. The **container-managed** variables above (`HOME`, `PATH`, `USER`, …) can never be re-admitted: forwarding the host `HOME`/`PATH` points the container away from the mounted profile, so Claude would write credentials into the wrong home and break the account boundary. And `SSH_AUTH_SOCK` is forwarded only after socket validation (above), so `env.allow` cannot re-admit a raw, unvalidated value.

## Git and SSH identity

Handled by the host, not by ccc. If you use direnv to set `GIT_SSH_COMMAND` per directory, ccc inherits it, and `~/.ssh` is mounted, so `-i ~/.ssh/id_work` resolves inside the container exactly as it does outside.

ccc adds no identity configuration of its own. `profile.json`'s `gh_config` exists only because `gh` reads `~/.config/gh`, which direnv cannot override.

## Permissions

ccc has **no** permission-related option and never injects `--dangerously-skip-permissions`.

A profile *is* a `~/.claude`, so permission behavior belongs where Claude Code already reads it:

- `profiles/<name>/claude/settings.json` → `permissions.defaultMode`
- or pass it through: `ccc -- --dangerously-skip-permissions`

## Configuration

`~/.config/ccc/config.json`. Every key is optional:

```json
{
  "runtime": "auto",
  "default_profile": "personal",
  "image": {
    "extra_dockerfile": "Dockerfile.extra",
    "default_claude_version": "2.1.205"
  },
  "mounts": {
    "dirs": ["~/dev/src/github.com/jwx-go/mlkem"],
    "home": "ro",
    "cache": true,
    "gh_config": "~/.config/gh"
  },
  "env": {
    "deny": [],
    "allow": []
  }
}
```

`runtime` is `auto`, `podman`, or `docker`. `mounts.dirs` is additive to the current repository. `mounts.home` is omitted, `"ro"`, or `"rw"`. `mounts.cache` is off.

`profiles/<name>/profile.json`, for a per-account GitHub identity:

```json
{
  "gh_config": "~/.config/gh-work"
}
```

`.ccc.json`, in a repository root — personal, not committed:

```json
{
  "profile": "work",
  "dirs": ["~/dev/src/github.com/jwx-go/mlkem"]
}
```

Both keys are optional individually. A file with only `dirs` contributes directories and lets `default_profile` pick the account.

`CCC_RUNTIME` overrides `runtime`; `--runtime` overrides both.

## Image

Built locally, cached under a content-addressed tag covering the Dockerfile and the build arguments — so changing either, including moving to a host with a different UID, rebuilds rather than reusing an image with the wrong user baked in.

Contents: Node, `@anthropic-ai/claude-code`, `git`, `gh`, `ripgrep`, `jq`, the Go toolchain, and `golangci-lint`.

To add tooling without forking ccc, drop a `~/.config/ccc/Dockerfile.extra`; it is appended verbatim to the base image.

### Which Claude Code runs

The image's, at `/usr/local/bin/claude`. ccc execs that absolute path.

By default this is uninteresting, because `$HOME` is not mounted and the host's Claude Code is not visible from inside. It becomes interesting the moment you set `mounts.home`, and then there are two separate problems.

**Resolution.** A login shell inside the container sources the host's `~/.profile`, which prepends `~/.local/bin`, so a bare `claude` would run the *host's* binary. ccc shadows that path with a shim (`~/.config/ccc/shim/claude`) that execs the image's binary instead.

**Replacement.** `claude install` — which Claude Code suggests when its npm self-update fails — writes a temp file and `rename()`s it over `~/.local/bin/claude`. Left unguarded, running it inside a ccc session downloads 257 MB into your host's `~/.local/share/claude/versions/` and repoints your host's symlink.

Under `"home": "ro"` this is impossible: `rename(2)` needs a writable parent directory. Under `"home": "rw"` ccc read-only-mounts `~/.local/bin` and `~/.local/share/claude`, which holds even against `claude install --force` — `EROFS` is not a check the installer can override — at the cost of nothing inside the container being able to install into `~/.local/bin`.

### Upgrading Claude Code

It upgrades itself. You do not normally run anything.

Inside the container, Claude Code is installed under root-owned `/usr/local` while the container runs as you, so its self-update always fails — and records what it wanted in `~/.claude/.last-update-result.json`, which is the profile's own mounted directory:

```json
{"path":"npm-global","outcome":"failed","status":"no_permissions",
 "version_from":"2.1.204","version_to":"2.1.205"}
```

On the next `ccc`, that `version_to` becomes the profile's pin and the image rebuilds (just the Claude Code layer, absent a `Dockerfile.extra`):

```
ccc: Claude Code asked for 2.1.205 (have 2.1.204); rebuilding
```

The container says what it wants; only the host can act on it.

That permission failure is load-bearing: it is both the signal ccc reads and the thing keeping the container out of `/usr/local`. The automatic updater cannot get past it — it uses the npm-global path and does not escalate, even though the container has passwordless `sudo`. (An agent that runs `sudo npm install -g` by hand does succeed, but `--rm` discards the layer, so the next run is back on the pinned version.)

The escape hatch is the *native* installer, `claude install`, which targets `$HOME` rather than `/usr/local`. See [Which Claude Code runs](#which-claude-code-runs) for why `~/.local` is mounted read-only.

ccc contacts no registry to do this. Claude Code already did the checking, so **its** `autoUpdates` setting is the on/off switch: set `"autoUpdates": false` in the profile's `settings.json` and nothing is ever asked for, so nothing is ever adopted. You are always one session behind — the session that discovers a release is the one whose update fails.

Only a strictly newer version is adopted, because `ccc profile create --from ~/.claude` copies the host's update record, which may name an older version than the profile is pinned to.

#### When the version is bad

`version_to` comes from a file the container can write, so ccc assumes it may be wrong or hostile.

A **malformed** value (`2.1.205; rm -rf /`, `$(id)`, unparseable JSON) is ignored. It never reaches a build arg.

A **well-formed but nonexistent** version — say `9.9.9` — is subtler: it passes validation, but npm cannot install it. So ccc builds *before* it pins, and keeps the working image when the build fails:

```
ccc: Claude Code asked for 9.9.9 (have 2.1.205); rebuilding
ccc: could not build Claude Code 9.9.9 (exit status 1)
ccc: staying on 2.1.205
```

Your session starts anyway, on the version that works, and the pin is untouched. Were the pin written first, the container could brick ccc: every later run would fail on an image that can never build. `ccc pin --to <bad>` behaves the same way — it fails without recording anything.

If a pin file is corrupted anyway, `ccc` refuses to run and says how to fix it. `ccc pin` is the one command that tolerates an unreadable pin, so it can always repair one:

```sh
ccc -p work pin              # overwrites the corrupt pin
```

To drive it by hand:

```sh
ccc pin                      # resolve the latest version, pin it, rebuild
ccc pin --to 2.1.204         # pin a specific version
ccc -p work pin              # pin just the "work" profile
```

A pin is `latest` or a release semver like `2.1.205`. **Prereleases (`-beta`, `-rc.1`) are refused**: ccc orders versions on the `X.Y.Z` triple alone, so a pinned prerelease would compare equal to its release and never advance to it — a stuck profile. Claude Code ships stable through npm's `latest`, so this only ever rejects a hand-typed prerelease.

The version is an explicit pin. `CLAUDE_VERSION` is the last `ARG` in the base Dockerfile, immediately before the only `RUN` that uses it, so bumping it invalidates just that layer: apt, the Go toolchain, and `golangci-lint` above it are reused. And because the image tag content-hashes the build args, a changed pin is a changed tag — the next plain `ccc` rebuilds on its own.

(A `Dockerfile.extra` is appended *below* that layer, so it sees the finished base. The trade is that an extra with its own `RUN` also rebuilds on a version bump — the base stays composable, at the cost of the one-layer property when you extend it.)

`ccc` never contacts the npm registry on a normal run; only `ccc pin` does. A pin is always a concrete version: `ccc pin --to latest` resolves `latest` through the registry before storing it, because a moving dist-tag would hash to a stable image tag and freeze the image forever.

The pin only invalidates the last layer, so it can never refresh the parts that float: the `node:22-bookworm` base, apt packages, and `golangci-lint@latest`. For those:

```sh
ccc pin --no-cache                   # latest Claude Code + rebuild every layer
ccc pin --no-cache --to 2.1.205      # keep this version, rebuild every layer
```

#### Pin vs. default

These are two different things:

- A **pin** is per-profile, in the profile's own Claude Code directory:
  `~/.config/ccc/profiles/work/claude/.ccc-claude-version` (e.g. `2.1.205`). This profile runs exactly this version.
- A **default** is global, `image.default_claude_version` in `config.json`. A profile with no pin of its own starts here — but it is only a starting point. If Claude Code inside that profile requests a newer version, ccc writes the profile a pin of its own, and it diverges upward from the default. The default does not hold a profile down.

`ccc pin` writes the default; `ccc -p work pin` writes that profile's pin. So profiles can run different versions, and each carries its own.

That file is inside a directory mounted **read-write** into the container, so the contained process can write it. Its contents become a build arg that is interpolated into a `RUN npm install -g pkg@${CLAUDE_VERSION}` executed as root. ccc therefore validates it on read: it must be `latest` or a plain semver. Anything else — `2.1.205; rm -rf /`, backticks, `$(...)`, newlines — is a hard error, never a silently ignored value and never a shell string.

## Runtime

`podman` is preferred over `docker`. Rootless podman maps the host user with `--userns=keep-id`, which gives correct file ownership without a daemon. Under docker, ccc passes `--user $(id -u):$(id -g)` against an image whose `/etc/passwd` was built with your UID, so `getpwuid(3)` resolves.
