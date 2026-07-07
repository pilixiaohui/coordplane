FROM node:22-bookworm-slim

ARG CLAUDE_CODE_NPM_PACKAGE=@anthropic-ai/claude-code

RUN apt-get update \
  && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    coreutils \
    git \
    jq \
    openssh-client \
    sqlite3 \
  && rm -rf /var/lib/apt/lists/*

RUN npm install -g "${CLAUDE_CODE_NPM_PACKAGE}" \
  && npm cache clean --force

RUN groupadd --gid 10001 agent \
  && useradd --uid 10001 --gid 10001 --home-dir /home/agent --create-home --shell /bin/bash agent \
  && mkdir -p /workspace/project /home/agent \
  && chown -R agent:agent /workspace /home/agent

ENV HOME=/home/agent
WORKDIR /workspace/project
USER agent

CMD ["sleep", "3600"]
