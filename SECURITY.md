# Security Policy

## Threat model & non-goals

`ccc` is **not a security isolation sandbox.** It runs Claude Code in a container
to separate per-account credentials and state — not to sandbox untrusted code. It
assumes you trust the code you run and the repository you launch it in.

By design the contained process has host networking, your SSH keys and forwarded
SSH agent, your `gh` OAuth token, and read-write access to the repository
(including its `.git`) and its `.ccc.json`. These are conveniences that let Claude
Code do real work — not a blast-radius control. Do not rely on ccc as a boundary
against malicious code. See `README.md` → "Non-goals".

The following are therefore **not** security vulnerabilities in ccc:

- the container reaching the host network, host services on localhost, or the SSH agent;
- host credentials (SSH keys, `gh` token, forwarded environment) being visible inside the container;
- a `.ccc.json` in a repository influencing what is mounted;
- the container writing to the mounted repository or its `.git`.

## Reporting a vulnerability

Genuine defects in ccc's **own** code are in scope — for example, mounting or
exposing something the configuration did not ask for, a crash while parsing
untrusted *configuration*, or a supply-chain issue in the image build. Please
report them via
[GitHub Security Advisories](https://github.com/lestrrat/ccc/security/advisories/new),
or open an issue if the report does not need to be private.
