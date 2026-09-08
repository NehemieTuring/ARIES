# Claude Code harness image for ARIES (pkg/harness/claudecode).
#
# No upstream registry image exists for this (unlike OpenClaw/Hermes, which
# are pulled — see configs/versions.json): Anthropic ships Claude Code only as
# an npm package, so ARIES builds its own. This is the image validated
# end-to-end in rapport_integration_claude_code.md (§7-§9): connectivity,
# authentication (apiKeyHelper), the Bash-tool wire grammar, and the MCP
# file-tools server (write_file/read_file/edit_file/glob/grep) all confirmed
# working from a container built by this exact Dockerfile.
#
# Build:
#   docker build -t aries-claudecode:<tag> images/claudecode
# then point configs/versions.json's claudecode.image at <tag>.
FROM node:22-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends openssh-client \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code

# Fixed, non-root UID/GID: Claude Code refuses --dangerously-skip-permissions
# as root (rapport_integration_claude_code.md §7.3), and
# pkg/harness/claudecode/harness.go's Start sets container.Config.User to
# this exact "10000:10000" (runtimeUID/runtimeGID) regardless of the image's
# own default user — created here mainly so /home/aries and the npm-installed
# files are owned by the same UID the container actually runs as.
RUN groupadd --gid 10000 aries \
    && useradd --uid 10000 --gid 10000 --create-home --home-dir /home/aries --shell /bin/bash aries \
    && mkdir -p /home/aries/workspace \
    && chown -R aries:aries /home/aries

USER aries
WORKDIR /home/aries/workspace
