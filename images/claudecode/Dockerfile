# Claude Code harness image for ARIES (pkg/harness/claudecode).
#
# Anthropic publishes Claude Code as an npm package, so ARIES builds this
# image locally. OpenClaw and Hermes are pulled from configs/versions.json.
#
# Build:
#   docker build -t aries-claudecode:2.1.245 images/claudecode
# then keep configs/versions.json claudecode.image on that tag.
FROM node:22-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends openssh-client \
    && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_CODE_VERSION=2.1.245
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}
ENV DISABLE_AUTOUPDATER=1

# Claude Code refuses --dangerously-skip-permissions as root. The image USER
# is the runtime user; deployment requests have no separate user field.
RUN groupadd --gid 10000 aries \
    && useradd --uid 10000 --gid 10000 --create-home --home-dir /home/aries --shell /bin/bash aries \
    && mkdir -p /home/aries/workspace \
    && chown -R aries:aries /home/aries

USER aries
WORKDIR /home/aries/workspace
