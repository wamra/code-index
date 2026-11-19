# Reusable base image for the code-index end-to-end test (see e2e.sh and
# scripts/test-docker.sh). It does not contain the repository: the repo is
# mounted read-only at /src when the container runs.
#
# Build: docker build -t code-index-test:latest test/docker
#
# GO_VERSION must stay >= the `go` line in go.mod (1.24.1).
# Debian 13 (trixie) base: apt-get update failed on bookworm images in this period
# (the archive InRelease carried a second signature bookworm apt could not verify).
ARG GO_VERSION=1.24
ARG NODE_IMAGE=node:lts-trixie-slim

# Node LTS comes from the official Node image: copy the runtime, not the layers.
FROM ${NODE_IMAGE} AS node

FROM golang:${GO_VERSION}-trixie

ARG DEBIAN_FRONTEND=noninteractive
ARG TARGETARCH
# Latest stable PowerShell 7 release (GitHub releases, binary archive).
ARG PWSH_VERSION=7.7.0

# System packages: git/curl/ca-certificates, shellcheck, and the libraries
# PowerShell needs on Debian 13 (libicu76, libssl3t64, zlib1g, libstdc++6, libgcc-s1).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      git curl ca-certificates shellcheck less procps jq \
      libicu76 libssl3t64 zlib1g libstdc++6 libgcc-s1 \
 && rm -rf /var/lib/apt/lists/*

# Node.js (LTS) and npm.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && node --version && npm --version

# bash-language-server (global, installed as root so it lands in /usr/local).
RUN npm install -g bash-language-server && bash-language-server --version

# PowerShell 7 from the binary archive, checked against the release's hashes
# file (UTF-16 text, hence iconv).
RUN case "${TARGETARCH:-$(dpkg --print-architecture)}" in \
      arm64) PSARCH=arm64 ;; \
      amd64) PSARCH=x64 ;; \
      *) echo "unsupported arch" >&2; exit 1 ;; \
    esac \
 && base="https://github.com/PowerShell/PowerShell/releases/download/v${PWSH_VERSION}" \
 && f="powershell-${PWSH_VERSION}-linux-${PSARCH}.tar.gz" \
 && cd /tmp \
 && curl -fsSL -o "$f" "$base/$f" \
 && curl -fsSL -o hashes.sha256 "$base/hashes.sha256" \
 && iconv -f UTF-16 -t UTF-8 hashes.sha256 | tr -d '\r' | grep " \*$f\$" | sed 's/ \*/  /' | sha256sum -c - \
 && mkdir -p /opt/microsoft/powershell/7 \
 && tar -xzf "$f" -C /opt/microsoft/powershell/7 \
 && chmod +x /opt/microsoft/powershell/7/pwsh \
 && ln -s /opt/microsoft/powershell/7/pwsh /usr/local/bin/pwsh \
 && rm -f "$f" hashes.sha256 \
 && pwsh -NoLogo -NoProfile -Command '$PSVersionTable.PSVersion.ToString()'

# Non-root user with a fresh HOME. GOPATH lives under it so `go` needs no root.
RUN useradd --create-home --shell /bin/bash --uid 1000 tester \
 && mkdir -p /home/tester/go /home/tester/.local/bin /work \
 && chown -R tester:tester /home/tester /work
USER tester
WORKDIR /home/tester
ENV HOME=/home/tester \
    GOPATH=/home/tester/go \
    GOFLAGS=-buildvcs=false \
    PATH=/home/tester/.local/bin:/usr/local/go/bin:/home/tester/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

# Claude Code: the documented native installer (https://code.claude.com/docs/en/setup).
# It installs to ~/.local/bin/claude.
RUN curl -fsSL https://claude.ai/install.sh | bash && claude --version

CMD ["bash"]
