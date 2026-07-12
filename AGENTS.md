# AGENTS.md

Guidance for AI agents and automated code reviewers working in this repository.
Human contributors: start with `README.md`.

`ccc` runs Claude Code inside a Docker/Podman container to keep each account's
credentials and state separate (multi-account). `README.md` has the full design.

## Threat model — read this before reviewing

**ccc is NOT a security isolation sandbox.** The container separates *accounts*,
not *trust*. ccc assumes you trust the code you run and the repository you launch
it in.

Because of that, the following are **deliberate features, not vulnerabilities** —
do not file them as security findings:

- the repository working tree, including its `.git`, is mounted **read-write**;
- `.ccc.json` is read from that repository and influences what gets mounted;
- `~/.ssh`, the forwarded SSH agent socket, and the `gh` OAuth token are mounted in;
- the container uses `--network host`;
- the host environment is forwarded into the container (minus a denylist).

There is no container-escape boundary to breach, because the container is not a
boundary. **Do not evaluate ccc against a prompt-injection, hostile-`.ccc.json`,
or container-escape threat model.** Correctness, UX, API-consistency, and
build-reproducibility findings are in scope; sandbox-escape findings are not.
See `README.md` → "Non-goals", and `SECURITY.md`.

## Build & test

- `go build ./...`
- `go test ./...`
- `golangci-lint run` — CI enforces it; keep the tree clean.
