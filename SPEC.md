# ccc — Claude Code Contained

Run Claude Code in a container so `~/.claude` can be swapped per account without touching the host's real `~/.claude`.

## Problem

Claude Code state is split across two host paths:

| Path | Contents |
|------|----------|
| `~/.claude/` | `.credentials.json`, `CLAUDE.md`, `agents/`, `docs/`, `projects/`, caches |
| `~/.claude.json` | project registry, onboarding + MCP state |

Renaming/symlinking `~/.claude` alone → breaks, because `~/.claude.json` is left behind and shared across accounts. ccc mounts BOTH from a per-profile directory, so switching accounts is total and atomic.

## Non-goals

ccc is NOT a security sandbox. The container isolates *Claude Code profiles*, nothing else.

The contained agent may see and do anything the invoking user can: host network, SSH keys, forwarded agent, rw source mounts. This is intentional — a profile switch must not change what the tools in your workflow can reach. Do not reason about ccc as a blast-radius control.

## Model

One profile = one account = one host directory = one container `$HOME` overlay.

- Profiles are managed: `~/.config/ccc/profiles/<name>/`
- Container is one-shot (`--rm`) per invocation.
- Default command is `claude`. `ccc` with no reserved subcommand → starts Claude Code.

## Layout

Host:

```
~/.config/ccc/
  config.toml              # global config
  Dockerfile.extra         # optional, appended to base image (user-extensible)
  profiles/
    work/
      claude/              # → mounted at $HOME/.claude
      claude.json          # → mounted at $HOME/.claude.json
      profile.toml         # per-profile overrides
    personal/
      ...
```

Container: user `lestrrat` with host UID/GID, `$HOME=/home/lestrrat` — identical to host, so absolute paths in transcripts, git worktrees, and `.ccc.toml` resolve the same inside and out.

## Profile selection

Precedence, first match wins:

1. `--profile <name>` flag
2. `.ccc.toml` in cwd, else nearest ancestor directory (`profile = "work"`)
3. `default_profile` in config.toml
4. Error — print available profiles, exit non-zero

`ccc` always prints the resolved profile name to stderr before starting, so a `default_profile` run is never silent about which account it used.

## CLI

| Command | Behavior |
|---------|----------|
| `ccc [claude-args...]` | Resolve profile, ensure image, run `claude` in container |
| `ccc -- --resume` | `--` ends ccc's flags; rest passes through verbatim |
| `ccc login <profile>` | Interactive OAuth in container; credentials persist to profile |
| `ccc profile create <name>` | Create empty profile dir |
| `ccc profile create <name> --from ~/.claude` | Seed from an existing config dir (copies `~/.claude` + `~/.claude.json`) |
| `ccc profile list` / `rm <name>` | Manage profiles |
| `ccc build [--no-cache]` | Rebuild image |
| `ccc doctor` | Check runtime, image, mounts, profile, SSH agent |

Reserved first-args: `login`, `profile`, `build`, `doctor`, `help`, `version`. Everything else → passed to `claude`. Ambiguity is resolved by `--`.

## Runtime

Auto-detect, `podman` preferred over `docker`. Override: `runtime` in config.toml, or `CCC_RUNTIME`.

Podman preferred because rootless + `keep-id` makes ownership correct with no image rebuild per host.

## UID mapping

Image is built with `--build-arg UID/GID/USER` from the host, so `/etc/passwd` has a real entry and `$HOME` is writable.

| Runtime | Flags |
|---------|-------|
| podman | `--userns=keep-id:uid=<uid>,gid=<gid>` |
| docker | `--user <uid>:<gid>` |

Files written into mounted repos end up owned by the host user. Image tag embeds uid/gid → `ccc:<hash>` where hash covers base Dockerfile + `Dockerfile.extra` + build args.

## Mounts

| Source | Target | Mode |
|--------|--------|------|
| `profiles/<name>/claude/` | `$HOME/.claude` | rw |
| `profiles/<name>/claude.json` | `$HOME/.claude.json` | rw |
| each `mounts.roots` entry | identical absolute path | rw |
| `~/.gitconfig` | `$HOME/.gitconfig` | ro |
| `~/.ssh` | `$HOME/.ssh` | ro |
| `$SSH_AUTH_SOCK` | same path + env forwarded | rw |
| gh config dir | `$HOME/.config/gh` | ro |

- `--network=host` — dev servers on localhost reachable; also lets the OAuth loopback callback land on the host browser during `ccc login`.
- Working directory = host `$PWD`. MUST be under a configured root → otherwise error, don't silently mount it.
- `~/.ssh` is mounted ro because `GIT_SSH_COMMAND -i <path>` must resolve inside the container. Identity-mapped paths → the host's `-i ~/.ssh/id_work` works unchanged.

## Environment

Inherit the full host environment, minus a denylist. direnv exports reach the container with no ccc configuration — that is the point.

Dropped, always:

| Var | Why |
|-----|-----|
| `HOME`, `PATH`, `USER`, `LOGNAME`, `SHELL`, `PWD`, `OLDPWD`, `TMPDIR`, `HOSTNAME` | container-managed; host values are wrong inside |
| `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` | would override the profile's OAuth credentials and silently route every profile to one account — defeats the tool |

`SSH_AUTH_SOCK` is rewritten, not dropped: forwarded with the socket bind-mounted at the same path.

`env.deny` in config.toml extends the denylist. `env.allow` re-admits a denied var for users who genuinely want e.g. `ANTHROPIC_API_KEY` passed through.

## Image

Built locally on first run, cached by content hash. No registry.

Base: `node` + `@anthropic-ai/claude-code`, `git`, `gh`, `ripgrep`, Go toolchain, `golangci-lint`.

`~/.config/ccc/Dockerfile.extra` is appended verbatim to the base → add tooling without forking ccc.

## Permissions

ccc has NO permission-related option. It never injects `--dangerously-skip-permissions`.

A profile *is* a `~/.claude`. Permission behavior is set where it already belongs:

- `profiles/<name>/claude/settings.json` → `permissions.defaultMode`
- or pass the flag through: `ccc -- --dangerously-skip-permissions`

Duplicating this as ccc config would create two sources of truth for one setting.

## Config

`~/.config/ccc/config.toml`:

```toml
runtime = "auto"                 # auto | podman | docker
default_profile = "personal"     # optional; last resort in profile resolution

[image]
extra_dockerfile = "Dockerfile.extra"

[mounts]
roots = ["~/dev/src"]
gh_config = "~/.config/gh"       # default; override per profile

[env]
deny  = []                       # extends the built-in denylist
allow = []                       # re-admits a denied var
```

`profiles/<name>/profile.toml`:

```toml
gh_config = "~/.config/gh-work"  # optional: per-account GitHub identity
```

`.ccc.toml` (repo root):

```toml
profile = "work"
```

## Implementation

- Go, module `github.com/lestrrat-go/ccc`, single static binary, `go install`-able.
- No container SDK — shell out to the `podman`/`docker` binary. Argument construction is the whole job; a daemon client buys nothing.
- Runtime differences isolated behind one interface with `podman`/`docker` implementations.
- `exec` the runtime (replace process) so TTY, signals, and exit codes pass through untouched.

## Git / SSH identity

Handled by the host, not by ccc. direnv sets `GIT_SSH_COMMAND` (and any `GIT_*` overrides) per directory; ccc inherits the environment and mounts `~/.ssh` ro, so the identity that applies on the host applies inside the container.

ccc adds no identity configuration of its own. `profile.toml.gh_config` exists only because `gh` reads `~/.config/gh`, which direnv cannot override.
