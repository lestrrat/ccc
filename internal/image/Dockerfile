FROM node:22-bookworm

ARG UID=1000
ARG GID=1000
ARG USERNAME=user
# HOME is the HOST's home path, mirrored into the container so that identical
# absolute paths mean the same thing on both sides of the mount. It is NOT
# always /home/${USERNAME}: macOS is /Users/x, ostree is /var/home/x, LDAP homes
# vary. ccc places every mount at the host home, so the container user's home
# must match it exactly.
ARG HOME=/home/user
ARG GO_VERSION=1.26.1

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
      gnupg \
      jq \
      less \
      openssh-client \
      ripgrep \
      sudo \
 && rm -rf /var/lib/apt/lists/*

# gh CLI, from GitHub's own apt repository.
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

# Go toolchain.
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-$(dpkg --print-architecture).tar.gz" \
      | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:/usr/local/bin:$PATH

RUN GOBIN=/usr/local/bin GOTOOLCHAIN=local \
      /usr/local/go/bin/go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest \
 && rm -rf /root/go /root/.cache/go-build

# Recreate the container user with the host's UID/GID so that (a) files written
# into mounted repositories are owned by the invoking user and (b) getpwuid(3)
# resolves, which docker's --user needs. The base image ships node:1000, which
# collides whenever the host user is also 1000.
#
# Every ${HOME}/${USERNAME} interpolation is quoted: a home or short-name with a
# space (rare, but the values are the host's, unvalidated) would otherwise
# field-split and break useradd. `mkdir -p $(dirname $HOME)` because useradd -m
# does not create missing parents, and a host home may be nested (/var/home/x,
# /Users/x) with no parent in the image. The sudoers drop-in is a FIXED,
# dot-free filename: sudo ignores any /etc/sudoers.d file whose name contains a
# `.`, so a username like "john.doe" would otherwise silently lose sudo.
RUN if getent passwd ${UID} >/dev/null; then userdel -r "$(getent passwd ${UID} | cut -d: -f1)" || true; fi \
 && if getent group ${GID} >/dev/null; then groupdel "$(getent group ${GID} | cut -d: -f1)" || true; fi \
 && groupadd -g ${GID} "${USERNAME}" \
 && mkdir -p "$(dirname "${HOME}")" \
 && useradd -u ${UID} -g ${GID} -s /bin/bash -d "${HOME}" -m "${USERNAME}" \
 && printf '%s ALL=(ALL) NOPASSWD:ALL\n' "${USERNAME}" > /etc/sudoers.d/ccc \
 && chmod 0440 /etc/sudoers.d/ccc

# Claude Code, LAST. Installed globally under /usr/local so it survives the host
# $HOME being mounted over the container's home directory at runtime.
#
# CLAUDE_VERSION is declared immediately before its only use: a build arg
# invalidates the layer cache from its declaration point onward, so bumping the
# version rebuilds this layer and reuses apt/Go/golangci-lint above it. This is
# the last RUN in the BASE Dockerfile; a user's Dockerfile.extra is appended
# below it (so the extra sees the finished base), which means an extra with its
# own RUN also rebuilds on a version bump. That is the intended trade — the base
# stays composable — not a bug.
ARG CLAUDE_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_VERSION} && npm cache clean --force

USER ${USERNAME}
ENV HOME=${HOME}

# NOTE: ccc appends the content-hash verification footer (ARG CCC_CONTENT_HASH +
# LABEL ccc.content-hash) after this base AND after any user Dockerfile.extra, so
# it is always the final instruction. See internal/image/image.go dockerfile().
