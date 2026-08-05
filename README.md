# agent-go-proxy

A local reverse proxy for Codex CLI ChatGPT/OpenAI-compatible HTTP traffic, with JSONL logs and a MySQL-backed viewer.

## Run

```sh
go run .
```

By default this targets ChatGPT/Codex backend traffic:

```text
https://chatgpt.com/backend-api/codex
```

Then run Codex with the Codex home that contains the `agent-go-proxy` model provider:

```sh
CODEX_HOME=/Users/chenxy/codex_home/37 \
NO_PROXY=127.0.0.1,localhost \
no_proxy=127.0.0.1,localhost \
/Users/chenxy/workspace/codex/codex-cli/codex-main/codex-rs/target/debug/codex \
  --dangerously-bypass-approvals-and-sandbox
```

`NO_PROXY`/`no_proxy` keeps Codex's request to `127.0.0.1:8080` from being routed through the
system proxy.

For OpenAI Platform API key traffic:

```sh
go run . -target https://api.openai.com
```

Claude (Anthropic) traffic is supported at the same time and routed automatically by
endpoint (`/v1/messages` → Claude, `/v1/responses` → Codex). Point Claude Code at the proxy
with account login unchanged:

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 claude
```

The Claude upstream defaults to `https://api.anthropic.com` and can be overridden:

```sh
go run . -claude-target https://api.anthropic.com   # or CLAUDE_BASE_URL
```

Logs are written as JSON Lines to `log/YYYY-MM-DD.log`.

The dashboard is served by the same process:

```text
http://127.0.0.1:8080/
```

The `OPENAI` page starts ChatGPT browser OAuth through the `libcodex_bridge` dynamic library and
stores the resulting Codex `auth.json` payload in the MySQL `openai_accounts.auth_json` column.
It can query each account's current `/wham/usage` rate-limit and credit details through the same
dynamic library. Account list responses never include the credential payload. The library is
auto-detected from the adjacent `codex-bridge` checkout and can be overridden with:

```sh
CODEX_BRIDGE_LIB=/path/to/libcodex_bridge.dylib go run .
# or: go run . -codex-bridge-lib /path/to/libcodex_bridge.dylib
```

MySQL defaults:

```text
root:123456@tcp(127.0.0.1:3306)/agent_go_proxy
```

The proxy path stays synchronous only for forwarding: it reads the request body because the upstream request needs it, streams the upstream response to Codex immediately, and sends request/response recording work to an async background queue. The dashboard polls its JSON APIs every second.

Useful flags:

```sh
go run . -listen 127.0.0.1:18080
go run . -mysql-dsn 'root:123456@tcp(127.0.0.1:3306)/agent_go_proxy?parseTime=true&charset=utf8mb4&loc=Local'
# OPENAI/OUTLOOK 后端请求和 GPT 账号直连对话请求在代码里固定走 http://127.0.0.1:7899
```
