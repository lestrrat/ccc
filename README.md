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

ccc is **not a security sandbox.** The container isolates Claude Code profiles, nothing else.

The contained agent may see and do anything you can: host network, your SSH keys, a forwarded SSH agent, read-write source mounts. This is intentional — switching profiles must not change what the tools in your workflow can reach. Do not reason about ccc as a blast-radius control.

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
```

```sh
ccc profile create work              # empty profile
ccc profile create work --from ~/.claude   # seed from an existing config
ccc profile list                     # `*` marks default_profile
ccc profile rm work                  # deletes credentials too
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
ccc --resume                           # claude --resume
ccc -p work --resume                   # profile "work"
ccc --help                             # ccc's help
ccc -- --help                          # claude's help
ccc -- doctor                          # `claude doctor`, not `ccc doctor`
```

`--` forces passthrough. It is only needed when a Claude Code argument collides with one of ccc's reserved words: `profile`, `upgrade`, `doctor`, `help`, `version`, `--help`, `-h`, `--profile`, `-p`, `--runtime`.

Other commands:

```sh
ccc upgrade              # pin the latest Claude Code, rebuild one layer
ccc doctor               # runtime, image, mounts, resolved profile
ccc help                 # same as ccc --help
```

There is no `build` command: the image builds itself on first run, and whenever the pin or `Dockerfile.extra` changes.

## What the container sees

| Host | Container | Mode |
|------|-----------|------|
| `mounts.roots` (default `$HOME`) | identical absolute path | rw |
| `profiles/<name>/claude/` | `$HOME/.claude` | rw |
| `profiles/<name>/claude.json` | `$HOME/.claude.json` | rw |
| `~/.ssh` | `$HOME/.ssh` | ro |
| `~/.gitconfig` | `$HOME/.gitconfig` | ro |
| `$SSH_AUTH_SOCK` | same path | rw |
| gh config dir | `$HOME/.config/gh` | ro |
| `~/.config/ccc/shim/claude` | `$HOME/.local/bin/claude` | ro |

The last row shadows a host-native Claude Code, if you have one; see [Which Claude Code runs](#which-claude-code-runs).

The container user mirrors your UID, GID, username, and home directory, and roots are mounted at their **identical absolute paths**. Absolute paths therefore mean the same thing on both sides of the mount, and files written into your repositories are owned by you.

Networking is `--network=host`: dev servers on localhost stay reachable, and Claude Code's OAuth loopback callback lands on your browser during login.

The working directory must live under a configured root. ccc refuses to run otherwise rather than silently mounting it.

## Environment

ccc inherits the **whole host environment minus a denylist**, so direnv exports reach the container with no ccc-side configuration.

Always dropped:

| Variable | Why |
|----------|-----|
| `HOME`, `PATH`, `USER`, `LOGNAME`, `SHELL`, `PWD`, `OLDPWD`, `TMPDIR`, `TMP`, `TEMP`, `HOSTNAME` | container-managed; the host values are wrong inside |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` | would override the profile's own credentials and silently route every profile to a single account |

`SSH_AUTH_SOCK` is forwarded with its socket bind-mounted at the same path.

Extend with `env.deny`; re-admit a dropped variable with `env.allow` (which wins over every deny rule).

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
    "extra_dockerfile": "Dockerfile.extra"
  },
  "mounts": {
    "roots": ["~/dev/src"],
    "gh_config": "~/.config/gh"
  },
  "env": {
    "deny": [],
    "allow": []
  }
}
```

`runtime` is `auto`, `podman`, or `docker`. `mounts.roots` defaults to `["~"]`.

`profiles/<name>/profile.json`, for a per-account GitHub identity:

```json
{
  "gh_config": "~/.config/gh-work"
}
```

`.ccc.json`, in a repository root:

```json
{
  "profile": "work"
}
```

`CCC_RUNTIME` overrides `runtime`; `--runtime` overrides both.

## Image

Built locally, cached under a content-addressed tag covering the Dockerfile and the build arguments — so changing either, including moving to a host with a different UID, rebuilds rather than reusing an image with the wrong user baked in.

Contents: Node, `@anthropic-ai/claude-code`, `git`, `gh`, `ripgrep`, `jq`, the Go toolchain, and `golangci-lint`.

To add tooling without forking ccc, drop a `~/.config/ccc/Dockerfile.extra`; it is appended verbatim to the base image.

### Which Claude Code runs

The image's, at `/usr/local/bin/claude`. ccc execs that absolute path.

This matters because `$HOME` is mounted, so the container can see a host-native Claude Code at `~/.local/bin/claude`. A login shell inside the container sources the host's `~/.profile`, which prepends `~/.local/bin` — without care, `claude` would resolve to the *host's* binary, and its auto-updater would rewrite the host's installation from inside a container.

ccc shadows that path with a shim (`~/.config/ccc/shim/claude`) that execs the image's binary. The host's install is unreachable from inside, and untouched.

### Upgrading Claude Code

It upgrades itself. You do not normally run anything.

Inside the container, Claude Code is installed under root-owned `/usr/local` while the container runs as you, so its self-update always fails — and records what it wanted in `~/.claude/.last-update-result.json`, which is the profile's own mounted directory:

```json
{"path":"npm-global","outcome":"failed","status":"no_permissions",
 "version_from":"2.1.204","version_to":"2.1.205"}
```

On the next `ccc`, that `version_to` becomes the profile's pin and one image layer rebuilds:

```
ccc: Claude Code asked for 2.1.205 (have 2.1.204); rebuilding
```

The container says what it wants; only the host can act on it. That permission failure is load-bearing twice over — it is also what stops the container from rewriting your host's installation through the mounted `$HOME`.

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

Your session starts anyway, on the version that works, and the pin is untouched. Were the pin written first, the container could brick ccc: every later run would fail on an image that can never build. `ccc upgrade --to <bad>` behaves the same way — it fails without recording anything.

If a pin file is corrupted anyway, `ccc` refuses to run and says how to fix it. `ccc upgrade` is the one command that tolerates an unreadable pin, so it can always repair one:

```sh
ccc -p work upgrade          # overwrites the corrupt pin
```

To drive it by hand:

```sh
ccc upgrade                  # resolve the latest version, pin it, rebuild
ccc upgrade --to 2.1.204     # pin a specific version
ccc -p work upgrade          # pin just the "work" profile
```

The version is an explicit pin. `CLAUDE_VERSION` is the last `ARG` in the Dockerfile, immediately before the only `RUN` that uses it, so bumping it invalidates **one layer**: apt, the Go toolchain, and `golangci-lint` above it are reused. And because the image tag content-hashes the build args, a changed pin is a changed tag — the next plain `ccc` rebuilds on its own.

`ccc` never contacts the npm registry on a normal run; only `ccc upgrade` does. A pin is always a concrete version: `ccc upgrade --to latest` resolves `latest` through the registry before storing it, because a moving dist-tag would hash to a stable image tag and freeze the image forever.

The pin only invalidates the last layer, so it can never refresh the parts that float: the `node:22-bookworm` base, apt packages, and `golangci-lint@latest`. For those:

```sh
ccc upgrade --no-cache               # latest Claude Code + rebuild every layer
ccc upgrade --no-cache --to 2.1.205  # keep this version, rebuild every layer
```

#### Where the pin lives

Per-profile, in the profile's own Claude Code directory:

```
~/.config/ccc/profiles/work/claude/.ccc-claude-version    # e.g. "2.1.205"
```

Falling back to `image.claude_version` in `config.json` when a profile has no pin. So profiles can run different Claude Code versions, and a profile carries its version with it.

That file is inside a directory mounted **read-write** into the container, so the contained process can write it. Its contents become a build arg that is interpolated into a `RUN npm install -g pkg@${CLAUDE_VERSION}` executed as root. ccc therefore validates it on read: it must be `latest` or a plain semver. Anything else — `2.1.205; rm -rf /`, backticks, `$(...)`, newlines — is a hard error, never a silently ignored value and never a shell string.

## Runtime

`podman` is preferred over `docker`. Rootless podman maps the host user with `--userns=keep-id`, which gives correct file ownership without a daemon. Under docker, ccc passes `--user $(id -u):$(id -g)` against an image whose `/etc/passwd` was built with your UID, so `getpwuid(3)` resolves.
