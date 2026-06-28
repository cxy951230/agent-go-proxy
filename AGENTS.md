# agent-go-proxy 改动归档

## 项目目标

`agent-go-proxy` 是本地 Codex CLI 请求代理，默认监听 `127.0.0.1:8080`，将 Codex 的 `/v1/*` 请求转发到 `https://chatgpt.com/backend-api/codex`，同时记录请求、响应和 SSE 过程，提供本地 Web 页面查看会话日志。

## 启动方式

```sh
./agent-go-proxy
```

Codex CLI 通过本地代理访问：

```sh
CODEX_HOME=/Users/chenxy/codex_home/37 \
NO_PROXY=127.0.0.1,localhost \
no_proxy=127.0.0.1,localhost \
/Users/chenxy/workspace/codex/codex-cli/codex-main/codex-rs/target/debug/codex \
  --dangerously-bypass-approvals-and-sandbox
```

## 转发原则

- 只代理 `/v1/*` 路径，避免浏览器 `favicon.ico` 等页面请求被当成 Codex 会话。
- 请求体必须读取，因为要转发给上游。
- 响应体边读边写回 Codex，保证转发实时。
- MySQL 入库、SSE 解析、token 统计全部走后台异步队列，不阻塞响应转发。

## 数据库

默认 MySQL：

```text
127.0.0.1:3306 / agent_go_proxy / root / 123456
```

启动时会自动建库、建表和补迁移。

主要表：

- `conversations`：会话聚合，按 `session_id` 唯一。
- `traces`：每次 `/v1/responses` 请求记录，保存请求/响应 headers、body、SSE events、token。
- `account_aliases`：`Chatgpt-Account-Id` 到自定义账号名的映射。

## 账号识别

从请求头 `Chatgpt-Account-Id` 读取账号 id：

- 列表默认展示短账号 id。
- 鼠标悬停可看到完整 id。
- 点击账号 badge 可编辑别名。
- 别名保存到 `account_aliases`，后续新数据匹配同一 id 自动展示别名。

## Prompt 识别

Codex 请求里会混入 `<environment_context>`、`<permissions instructions>` 等上下文。

当前逻辑会跳过这些注入上下文，取第一条真正的 user prompt。旧数据如果已保存为注入上下文，启动迁移会尝试从第一条 trace 的 request body 重新修复。

## Web 页面

首页：

- 每秒轮询 `/api/dashboard` 异步刷新。
- 整行点击进入详情。
- 点击账号 badge 编辑账号别名，不触发行跳转。
- 状态筛选只保留 `LIVE` 和 `OK`。
- 顶部统计显示：会话数、Trace 数、输入 Token、输出 Token、缓存 Token。
- 列表 token 列显示：输入 Token、输出 Token、缓存 Token。

详情页：

- 每秒轮询 `/api/conversations/{id}`。
- 按 trace 展示请求 headers、请求 body、响应 headers、SSE events、响应原文。
- 暂不合并 SSE 对话内容，保留详细原始视图。

## 日志

JSON Lines 日志仍写入：

```text
log/YYYY-MM-DD.log
```

当前按用户要求暂不脱敏，请求头和请求体会完整保存。

## API

- `GET /`：会话列表页。
- `GET /conversations/{id}`：会话详情页。
- `GET /api/dashboard`：首页轮询数据。
- `GET /api/conversations`：会话列表 JSON。
- `GET /api/conversations/{id}`：会话详情 JSON。
- `POST /api/accounts/{id}/alias`：保存账号别名。
- `GET /healthz`：健康检查。

## 当前技术栈

- Go 1.19
- `github.com/go-chi/chi/v5 v5.0.10`
- `github.com/go-sql-driver/mysql v1.7.1`

依赖版本锁定在 Go 1.19 兼容版本，避免使用需要 Go 1.21 的新版标准库包。
