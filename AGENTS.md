# agent-go-proxy 改动归档

## 项目目标

`agent-go-proxy` 是本地 Codex CLI 请求代理，默认监听 `127.0.0.1:8080`，将 Codex 的 `/v1/*` 请求转发到 `https://chatgpt.com/backend-api/codex`，同时记录请求、响应和 SSE 过程，并提供本地 Web 页面查看、筛选和标注会话日志。

网页标题为 `AGENT-GO-PROXY`，favicon 使用本机 `/Users/chenxy/Desktop/331.jpg`。

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

- 只代理 `/v1/*` 路径，避免浏览器页面请求被当成 Codex 会话。
- 本地页面、API、favicon 等由本服务自己处理；其他非 `/v1/*` 路径返回本地结果或 404。
- 请求体必须读取，因为要转发给上游，同时用于解析会话信息。
- 响应体边读边写回 Codex，保证转发实时。
- MySQL 入库、SSE 解析、token 统计全部走后台异步队列，不阻塞响应转发。

## 数据库

默认 MySQL：

```text
127.0.0.1:3306 / agent_go_proxy / root / 123456
```

启动时会自动建库、建表和补迁移。

主要表：

- `conversations`：会话聚合，按 `session_id` 唯一。字段包括 `account_id`、`window_id`、`started_at`、`updated_at`、`first_prompt`、`tags`、`model`、`agent`、`status`、`trace_count`、`error_count`、`total_tokens`、`last_status`、`last_duration_ms`、`last_request_id`。
- `traces`：每次 `/v1/responses` 请求记录，保存请求/响应 headers、body、SSE events、token、状态、耗时和完成时间。
- `account_aliases`：`Chatgpt-Account-Id` 到自定义账号名的映射。

迁移会确保历史数据补齐 `account_id`、`tags` 等字段，并尝试修复旧数据里误保存为注入上下文的 `first_prompt`。

## 账号识别

从请求头 `Chatgpt-Account-Id` 读取账号 id：

- 列表默认展示短账号 id。
- 鼠标悬停可看到完整 id。
- 点击账号 badge 可编辑别名。
- 别名保存到 `account_aliases`，后续新数据匹配同一 id 自动展示别名。
- 首页账号筛选只展示已经设置别名的账号，避免下拉框里出现大量不可读 id。

## Prompt 识别

Codex 请求里会混入系统和运行上下文。

当前逻辑会跳过以下注入内容，取第一条真正的用户 prompt：

- `# AGENTS.md instructions`
- `<environment_context>`
- `<permissions instructions>`
- `<app-context>`
- `<collaboration_mode>`
- `<skills_instructions>`
- `<plugins_instructions>`
- `<skill>`

如果会话里的 `first_prompt` 为空或已经被保存成上述注入上下文，新的请求进入时会尝试用请求体里的真实用户消息修复。

## 会话状态

状态由 trace 完成情况和错误情况决定：

- `LIVE`：还有未完成的 trace，例如请求已进入但 SSE/响应还没结束。
- `ERROR`：没有未完成 trace，但会话内至少有一次请求响应异常。
- `OK`：没有未完成 trace，且没有错误请求。

前端颜色：

- `LIVE` 使用橙色运行中样式。
- `ERROR` 使用红色错误样式。
- `OK` 使用绿色完成样式。

当前没有实现“多轮复杂任务是否真正完成”的额外推断，状态只表示代理层请求/响应是否仍在进行或是否出错。

## 首页

首页每秒轮询 `/api/dashboard` 异步刷新。

筛选：

- 日期下拉来自数据库中实际存在的 `updated_at` 日期。
- 状态下拉支持 `LIVE`、`OK`、`ERROR`。
- Agent 下拉来自实际会话里的 `agent`。
- 账号下拉只展示有别名的账号。
- 搜索框提示为“搜索消息、Session...”，搜索仍覆盖 `session_id`、首条用户 prompt、model、account id 和账号别名。

统计：

- 顶部统计显示会话数、Trace 数、输入 Token、输出 Token、缓存 Token。

列表：

- 时间列展示 `updated_at`，即最近一次请求进入时间，不再展示开始时间。
- 每个新请求进入时更新会话 `updated_at`；响应结束不更新该字段。
- 首条用户 prompt 展示解析后的真实用户第一句。
- 支持给会话打 `标签`，点击 `+` 或标签按钮编辑，服务端限制 255 字符。
- `耗时(分)` 表示会话开始到最新 trace 结束的分钟数；不足一分钟显示小数，列表里不带单位。
- 三个 token 字段合并为一列 `Token 输入/输出/缓存`，竖向展示输入、输出、缓存三个值。
- 行内还有账号、标签、Trace 数、模型、Agent、状态和删除按钮；进入详情靠点击表格行空白区域。
- 删除按钮会先弹出确认框，确认后删除会话；`traces` 依赖外键级联一起删除。

## 详情页

详情页支持 `详细` / `简略` 两种模式：

- 默认直接展示 `简略`。
- 初始 HTML 内嵌会话和 trace JSON，前端先用这份数据立即渲染简略模式。
- 详细模式也用同一份已加载数据渲染，切换时不再重新等待接口。
- 页面仍每秒轮询 `/api/conversations/{id}` 更新最新数据。

简略模式：

- 每个 trace 展示请求和响应两块内容，方便快速阅读对话。
- 请求内容从 request body 的 `input/messages` 角色内容中解析。
- 响应内容从 response body 或 SSE 事件中解析，包含 `response.completed`、`response.output_item.done` 等事件里的 assistant 文本。
- 如果请求或响应里没有可读文本，会展示对应的空内容提示。

详细模式：

- 保留原始排查视图。
- 按 trace 展示请求 headers、请求 body、响应 headers、SSE events、响应原文和 token/耗时等元信息。

## 会话上下文解析

简略模式顶部只展示一次会话上下文，来源于该会话第一条请求，而不是每轮对话重复展示。

上下文分 tab 展示：

- `概览`：系统字符数、消息数、Tools、Skills、MCP、上下文、媒体等汇总。
- `系统`：顶层 `instructions` 和系统提示预览。
- `Tools`：顶层 `tools`，展示名称、描述和参数信息。
- `Skills`：同时解析显式 `<skill>` 块，以及 `<skills_instructions>` 里的 `### Available skills` markdown 列表，展示 skill 名称、描述、文件路径和原始预览。
- `MCP`：展示 MCP、resource、resource template 相关工具。
- `上下文`：展示 permissions、environment、app-context、collaboration mode、skills instructions、plugins instructions 等注入上下文。
- `运行参数`：展示 model、tool choice、parallel tool calls、reasoning、stream、store、include、prompt cache key、text、请求/响应大小等。
- `媒体`：统计并展示 input image/image 等媒体信息。

## 日志

JSON Lines 日志写入：

```text
log/YYYY-MM-DD.log
```

当前按用户要求暂不脱敏，请求头和请求体会完整保存。

## API

- `GET /`：会话列表页。
- `GET /conversations/{id}`：会话详情页。
- `GET /favicon.ico`：网页 favicon。
- `GET /assets/favicon.jpg`：favicon 图片资源。
- `GET /api/dashboard`：首页轮询数据，包含统计、会话列表和筛选选项。
- `GET /api/conversations`：会话列表 JSON。
- `GET /api/conversations/{id}`：会话详情 JSON。
- `DELETE /api/conversations/{id}`：删除会话及其 trace。
- `POST /api/conversations/{id}/tags`：保存会话标签。
- `POST /api/accounts/{id}/alias`：保存账号别名。
- `GET /healthz`：健康检查。

## 当前技术栈

- Go 1.19
- `github.com/go-chi/chi/v5 v5.0.10`
- `github.com/go-sql-driver/mysql v1.7.1`

依赖版本锁定在 Go 1.19 兼容版本，避免使用需要 Go 1.21 的新版标准库包。
