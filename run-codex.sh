#!/usr/bin/env sh
set -eu

export CODEX_HOME=/Users/chenxy/codex_home/37
export NO_PROXY=127.0.0.1,localhost,::1
export no_proxy=127.0.0.1,localhost,::1

exec /Users/chenxy/workspace/codex/codex-cli/codex-main/codex-rs/target/debug/codex \
  --dangerously-bypass-approvals-and-sandbox "$@"
