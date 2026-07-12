# agent-go-proxy 改动归档

## 项目目标

`agent-go-proxy` 是本地 Agent CLI 请求代理，默认监听 `127.0.0.1:8080`，将 `/v1/*` 请求转发到对应上游，同时记录请求、响应和 SSE 过程，并提供本地 Web 页面查看、筛选和标注会话日志。

同时支持 Codex 和 Claude 两种请求，按接口自动区分上游与解析方式（见「多 Provider 支持」）：

- Codex：转发到 `https://chatgpt.com/backend-api/codex`（`-target` / `UPSTREAM_BASE_URL`）。
- Claude：转发到 `https://api.anthropic.com`（`-claude-target` / `CLAUDE_BASE_URL`）。

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

## 多 Provider 支持

代理同时接受 Codex 和 Claude 请求，靠 `detectProvider`（main.go）按请求区分，互不影响：

- 判定优先级：路径含 `/messages` 或 `/v1/complete` → Claude；含 `/responses` 或 `/chat/completions` → Codex；再看请求头 `Anthropic-Version` / `X-Api-Key`、`User-Agent` 含 `claude`；都不命中默认回落 Codex。
- 上游按 provider 选择：Claude 走 `-claude-target`，Codex 走 `-target`，`Host` 头同步改写为对应上游。
- `conversations.agent` 落库为 `Claude` / `Codex`，首页 Agent 下拉与筛选据此区分。

转发主干（流式透传、日志、入库队列）两边共用，只有「解析」按 provider 分流：

- 请求元信息（inspect.go `requestMetaFromHeaders`）：
  - Codex：沿用 `Session_id` / `Chatgpt-Account-Id` / `X-Codex-*` 头 + OpenAI `input[]`。
  - Claude：无专用会话头，从 `metadata.user_id`（形如 `user_<hash>_account_<uuid>_session_<uuid>`）正则抽出 `session_`/`account_` 做会话聚合与账号；model 取 body `model`；首条 prompt 取 `messages[]` 第一条 user 文本，跳过 `<system-reminder>` 等注入上下文。
  - 注：Claude 若无 `metadata.user_id`，会话 id 回落成 `unknown-时间戳`，此时每个请求各自成会话，后续可据日志再调。
- Token 用量（inspect.go `extractUsageFromEvents`）：
  - Codex：OpenAI `response.completed` 事件的 `usage`。
  - Claude：Anthropic SSE，输入/缓存命中取 `message_start` 的 `message.usage`，输出取 `message_delta` 的 `usage.output_tokens`，`total = input + output`。
- 详情简略视图（web.go）：
  - 响应文本 `responseText` 在 OpenAI 解析无结果时回落 `claudeStreamBlocks`，按 `content_block_delta` 还原正文（text_delta）、思考（thinking_delta）、工具调用（content_block_start 的 tool_use + input_json_delta）。
  - 系统提示 `systemText` 兼容：老 Codex 顶层 `instructions`、Claude 顶层 `system`（字符串或内容块数组）、**新版 Codex** 放在 `input[]` 里 `role=developer` 的消息（排除注入上下文块）。
  - 请求消息 `requestMessages` / `contentText` 已同时兼容 OpenAI `input[]` 与 Anthropic `messages[]`、`input_text`/`output_text`、tool_use/tool_result 内容块。
  - 工具 `toolDetails` / `collectRawTools` 兼容：老格式顶层 `tools`、**新版 Codex** `input[]` 里 `type=additional_tools` 的条目（`namespace` 分组展开成带前缀名的子工具）。
  - 运行参数 `runtimeRows` 展示 model / tool_choice / reasoning / text / include / prompt_cache_key / **client_metadata**（新版 Codex）等。

新版 Codex（GPT-5.x + 新版 CLI）请求格式变化：系统提示从顶层 `instructions` 挪到 `input` 的 developer 消息；工具从顶层 `tools` 挪到 `input` 的 `additional_tools` 条目并按 `namespace` 分组；新增 `exec` 编排工具与 `client_metadata` 字段。Responses API 传输协议本身（input/output/SSE 事件）未变。

解析按「错了也没关系、日志已留原文」的原则做，后续可据 `log/*.log` 与 `traces` 原始数据继续调整。

### Claude CLI 接入

把 Claude Code 的请求指向本代理即可（账号正常登录，仅改地址）：

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:8080 claude
```

## 左侧菜单与路由页

Web 页面左侧有侧边栏菜单，两项：

- `Dashboard`：原会话列表页（`/`）。
- `路由`：第三方 API 配置页（`/routes`）。

两页共用同一套侧边栏，当前页高亮。侧边栏支持收起/展开，状态保存在浏览器 `localStorage.sidebarCollapsed`，Dashboard 与路由页共享同一个折叠状态。路由页是对 `api_routes` 表的增删改查：

- 每条配置字段：名称、Base URL、`API`（风格，openai / anthropic）、接口协议、Model、API Key、启用开关。
- `API`（风格）与「接口协议」联动：openai → `Chat Completions API` / `Responses API`；anthropic → `Messages API` / `Chat Completions API`。前后端各存一份协议表（web.go `PROTOCOLS` 与 store.go `routeProtocols`），必须保持一致。
- API Key 列表里打码显示，编辑时回填完整值（本地工具，暂不脱敏）。
- 启用开关（ON/OFF）点击直接切换、无二次确认，`ToggleAPIRoute` 只改 `enabled` 不动 `updated_at`（避免列表按 `updated_at` 排序时启用项跳到最前）。
- **互斥粒度：每种 `api_style` 至多一条启用**（openai 一条、anthropic 一条可同时 ON）。开启一条时只关掉同风格的其它启用项。

## 路由与第三方 API 转发

`ServeHTTP`（main.go）在转发前按请求风格挑选启用的路由：

- 风格映射：`provider=codex → openai`，`provider=claude → anthropic`。用 `EnabledAPIRouteForStyle(style)` 取该风格当前启用的那条。
- **默认模式**（该风格无启用路由）：行为不变，按 provider 转发到官方上游（`-target` / `-claude-target`）。
- **路由模式**（有启用路由）：
  - 上游改为路由的 Base URL，按协议拼 endpoint（`buildRouteUpstreamURL`：`/chat/completions`、`/responses`、`/messages`）。
  - 认证头换成路由配置的 key（`applyRouteAuth`）：openai 风格用 `Authorization: Bearer`，anthropic 风格用 `x-api-key`（并补 `anthropic-version`）；客户端自带的 `Authorization`/`X-Api-Key`/`Chatgpt-Account-Id` 先剥掉。
  - 请求体的 `model` 改写为路由配置的 Model（`rewriteModel`，仅当配置了 Model；用 RawMessage map 只替换 model 键）。
  - 协议不匹配（请求协议 ≠ 路由协议，且无法适配）→ 本地直接返回 **421 Misdirected Request**，不打上游，仍记一次错误 trace/日志。
- 记录/日志的请求体仍保存**客户端原始请求**（`recordedReqBody`），转发用的 `forwardBody` 可能已改写 model 或转成 chat，两者分离。
- 看板和 JSONL 日志里的 `model` 使用**实际出站模型**（`effectiveModel`）：路由配置有 Model 时取路由 Model；路由 Model 为空时回落转发体/原始请求体里的 model。这样 Codex CLI 本地配置仍可写 `gpt-5.6-sol`，但启用 qwen/kimi/deepseek/mimo 等第三方路由后，看板模型列显示实际请求的第三方模型。
- 入库时 `TraceStartRecord.Model` 会覆盖 `requestMetaFromHeaders` 从原始请求体解析出的 model；`conversations.model` 后续请求有有效模型时会刷新为最新实际模型，避免同一 session 首次记录为 GPT 后一直不变。

## 协议适配层（adapter.go）

当启用路由的协议是 `chat_completions`，但请求本身是 `messages`（Claude）或 `responses`（Codex）时，进入适配器——让只支持 Chat Completions 的三方模型也能被 Codex/Claude 使用：

- **请求 native → chat**（`adaptRequestToChat`）：
  - `anthropicMessagesToChat`：`system` → system 消息；`messages[]` 内容块展开（assistant `tool_use` → `tool_calls`，user `tool_result` → 独立 `role=tool` 消息，image → `image_url`）；`tools` → chat function tools；`tool_choice` 映射。
  - `openaiResponsesToChat`：`input[]` 里 developer/system → system、user → user、`function_call` → assistant `tool_calls`、`function_call_output` → `role=tool`；工具来自 `additional_tools` 条目（`namespace` 分组递归展开成 `组名__子工具` 前缀名，`custom` 归一成 function）。
  - `additional_tools` 中的 `custom` 工具会转换成 Chat function wrapper，并通过 `adapterState` 记录原始 Responses 工具类型、名称和 namespace。第三方模型返回 tool call 后，再按 state 还原为 Responses `custom_tool_call` 或 `function_call`。
  - Codex 自定义工具（例如 `exec`、`apply_patch`）必须带明确说明和固定 schema：Chat function 参数统一为 `{ "input": string }`。`exec` 的 `input` 是 Codex exec 接受的原始 JavaScript 源码；要执行 shell 命令时应写成 `await tools.exec_command({cmd: "..."})`。`apply_patch` 的 `input` 是以 `*** Begin Patch` 开头的原始 patch 文本。
- **响应 chat → native**（`adaptChatResponseToNative`）：
  - 先 `parseChatResponse` 把三方的 chat 响应（SSE 或 JSON）解析出文本、tool_calls、finish_reason、usage。
  - 再按目标协议还原：`emitAnthropicSSE/JSON`（message_start/content_block/message_delta…）或 `emitResponsesSSE/JSON`（response.created/output_item/response.completed…）。usage 一并带上，供 recorder 正常统计。
- **usage 兼容**：
  - 流式 Chat Completions 请求会补 `stream_options: {"include_usage": true}`，方便 MiMo/OpenAI 兼容接口在流末返回 usage。
  - 缓存 token 兼容 OpenAI/MiMo 的 `usage.prompt_tokens_details.cached_tokens`，也兼容 DeepSeek 的 `usage.prompt_cache_hit_tokens`。
  - reasoning token 兼容 `usage.completion_tokens_details.reasoning_tokens`。
  - 还原 Responses 时会写入 `input_tokens_details.cached_tokens` 与 `output_tokens_details.reasoning_tokens`，这样 recorder 和看板能正常统计缓存/推理 token。
- **取舍**：响应是「读完上游再一次性转吐」，不是逐 token 增量流式；客户端拿到的仍是合法原生 SSE。输入 token 取三方返回的 `prompt_tokens`（三方不返回 usage 时为 0）。
- 只在 2xx 且需适配时走转换;非 2xx 原样透传三方错误。

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
- `api_routes`：路由页配置的第三方 API 供应商。字段 `name`、`base_url`、`model`、`api_style`（openai/anthropic）、`protocol`（chat_completions/responses/messages）、`api_key`、`enabled`。旧库通过 `ensureColumn` 补 `api_style`/`protocol`/`enabled` 三列。

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

首页初始只加载 10 条会话，向下滚动接近底部时按 `limit/offset` 继续请求 `/api/dashboard` 分页追加。自动刷新只刷新第一页，间隔 3 秒，用于更新最新记录和顶部统计，避免每次进入或轮询都渲染大量历史数据。

后端列表查询使用 `ListConversationsPage`：

- 默认 `limit=10`，最大 100，`offset` 小于 0 时按 0 处理。
- 先在子查询中按筛选条件和 `updated_at DESC` 取当前页 conversations，再只聚合这一页对应的 traces token/完成时间，避免老逻辑先全量 `traces GROUP BY conversation_id` 再 join 导致首页变慢。
- `/api/conversations` 也支持 `limit` / `offset` 参数；旧的 `ListConversations` 仍保留，内部走分页并默认取 200 条。

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
- 列表底部显示加载状态：`向下滚动加载更多`、`加载中...` 或 `已加载全部记录`。
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

JSON Lines 日志写入，**按模型分文件**：

```text
log/YYYY-MM-DD-{model}.log
```

- 默认模式下模型名取客户端原始请求体的 `model`；启用第三方路由后取实际出站模型（`effectiveModel`），不同模型写到不同日志文件。
- 模型名里的 `/`、`:` 等非法字符会清洗成 `_`（`sanitizeModelForFile`），空模型归 `unknown`。

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
- `GET /routes`：第三方 API 配置页。
- `GET /api/routes`：路由配置列表 JSON。
- `POST /api/routes`：新建路由配置。
- `PUT /api/routes/{id}`：更新路由配置。
- `POST /api/routes/{id}/toggle`：切换启用状态（按 `api_style` 互斥）。
- `DELETE /api/routes/{id}`：删除路由配置。
- `GET /healthz`：健康检查。

## 当前技术栈

- Go 1.19
- `github.com/go-chi/chi/v5 v5.0.10`
- `github.com/go-sql-driver/mysql v1.7.1`

依赖版本锁定在 Go 1.19 兼容版本，避免使用需要 Go 1.21 的新版标准库包。
