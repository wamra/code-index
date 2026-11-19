# A consumer machine: Debian with Claude Code and nothing else.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/* && useradd --create-home --shell /bin/bash user
USER user
ENV HOME=/home/user PATH=/home/user/.local/bin:/usr/local/bin:/usr/bin:/bin DISABLE_AUTOUPDATER=1
RUN curl -fsSL https://claude.ai/install.sh | bash && claude --version
WORKDIR /home/user
