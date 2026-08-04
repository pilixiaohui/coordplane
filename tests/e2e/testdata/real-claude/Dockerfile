# Real Claude Code runtime for the live acceptance gates (RMA-01/02/03).
# Pinned: Debian (glibc) base so the glibc-linked coordlink binary executes;
# bash + SHELL for Claude Code's Bash tool; Go 1.22.5 for the wordfreq task;
# Claude Code 2.1.126 exactly (e2e-real-cli.sh verifies this version).
FROM node:22-bookworm
RUN wget -q https://go.dev/dl/go1.22.5.linux-amd64.tar.gz -O /tmp/go.tgz \
 && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:$PATH SHELL=/bin/bash
RUN npm install -g @anthropic-ai/claude-code@2.1.126
