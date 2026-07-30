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

## 左侧菜单、路由页与链式代理页

Web 页面左侧有侧边栏菜单，六项：

- `Dashboard`：原会话列表页（`/`）。
- `路由`：第三方 API 配置页（`/routes`）。
- `链式代理`：按顺序组合多个路由的配置页（`/chains`）。
- `OPENAI`：通过 Codex Bridge 登录和管理 GPT 账号（`/openai`）。
- `OUTLOOK`：管理 Outlook 邮箱账号、刷新其 Token、读邮件取验证码（`/outlook`，`outlook_web.go`），见「OUTLOOK 邮箱账号」。
- `API Key`：管理直连用的 API Key（`/api-keys`，`apikeys_web.go`）。

各页共用同一套侧边栏，当前页高亮。侧边栏支持收起/展开，状态保存在浏览器 `localStorage.sidebarCollapsed`。

`路由` / `OPENAI` / `API Key` 三个列表页的每一行都可**整行点击跳转**到该条目的「Token 消耗详情」图表页（和 Dashboard 行点击进详情一致）：点行内的按钮/开关/下拉/`详情`链接等交互控件时不跳转，只有点空白区域才跳。跳转地址为 `/stats/tokens?dim=route|account|api_key&id=&name=`，见「Token 消耗统计与图表」。

`API Key` 页是对 `api_keys` 表的增删改查，字段只有名称和 API Key：新增时前端 `crypto.getRandomValues` 自动生成一个 `sk-` 开头的随机 Key（可改），列表里打码显示、编辑时回填完整值。这些 Key 用于「API Key 直连」的命中判定。

`OPENAI` 页除账号摘要/额度/Token 过期外，还有「模型配置」列（模型/推理强度/速度下拉）与「拉取模型」按钮，见「GPT 账号模型配置」；右上角有「刷新全部额度」，见「一键刷新全部额度」。

路由页是对 `api_routes` 表的增删改查：

- 每条配置字段：名称、Base URL、`API`（风格，openai / anthropic）、接口协议、Model、API Key、启用开关。
- `API`（风格）与「接口协议」联动：openai → `Chat Completions API` / `Responses API`；anthropic → `Messages API` / `Chat Completions API`。前后端各存一份协议表（web.go `PROTOCOLS` 与 store.go `routeProtocols`），必须保持一致。
- API Key 列表里打码显示，编辑时回填完整值（本地工具，暂不脱敏）。
- 启用开关（ON/OFF）点击直接切换、无二次确认，`ToggleAPIRoute` 只改 `enabled` 不动 `updated_at`（避免列表按 `updated_at` 排序时启用项跳到最前）。
- **互斥粒度：每种 `api_style` 至多一条启用**（openai 一条、anthropic 一条可同时 ON）。开启一条时只关掉同风格的其它启用项。

链式代理页是对 `chain_proxies` 表的增删改查：

- 每条配置字段：名称、`API`（风格，openai / anthropic）、按顺序选择的多个路由、启用开关。
- 新增/编辑时先选择 `API` 类型，再只展示该类型下的路由；点击路由即加入链路，点击顺序就是 `1 / 2 / 3...` 的尝试顺序。
- “当前顺序”里的已选路由支持直接点击 `×` 删除，删除后编号自动重排。
- 启用开关（ON/OFF）按 `api_style` 互斥：同一 API 类型下最多一个链式代理 ON，openai 和 anthropic 各自独立。
- 链式代理只负责优先级和故障切换；其中每个子路由仍沿用路由页的 Base URL / Model / API Key / 协议配置。

### GPT 账号模型配置

OPENAI 页每个账号可配置直连时用的模型/推理强度/速度，逻辑在 `openai_login.go` + `openai_web.go`：

- 「拉取模型」调 Bridge `modelsList`（`codex_bridge_models_list` ABI，等价 CLI 的 `/model`），把该账号可用的模型目录缓存到 `openai_accounts.models_json`。目录**按账号套餐过滤**，free 账号看不到 plus 才有的模型。
- 三个下拉的可选项**由目录里每个模型各自携带**：模型（`display_name`/slug）、推理强度（`supported_reasoning_efforts`，`none|minimal|low|medium|high|xhigh|max|ultra`）、速度（`service_tiers`，外加一个固定的 `default`=标准）。换模型后推理强度/速度选项会变，前端换模型时清空另两项回落新模型默认值。
- 下拉改动即存（`selected_model` / `selected_reasoning_effort` / `selected_service_tier`）。当前这三项**只存库**，「API Key 直连」目前只按 `selected_model` 匹配账号，推理强度/速度尚未回写进转发请求体。

### 一键刷新全部额度 / 拉取全部模型

OPENAI 页右上角两个批量按钮 = 对每个账号各点一次操作列的「额度」「模型」，**后台异步执行，前端不阻塞**。两者是各自独立的任务（`openaiJob` / `openaiModelsJob`），可以同时跑、互不阻塞；前端 `setupBatchButton` 工厂各持各的定时器，共用同一套「轮询 → 显示 `刷新中 3/7…` → 重载列表 → 有失败才弹清单」逻辑。

- 「拉取全部模型」：`POST /api/openai/accounts/models-all` + `GET /api/openai/models-all`，逐个调 `RefreshModels`（按需刷凭证 → Bridge `modelsList` → 写 `models_json`，首次拉取还会把默认模型落 `selected_model`）。并发同为 3，单账号 60s 上限。

「刷新全部额度」细节：

- 列表「额度使用」列按窗口渲染：`windowLabel` 把 `limit_window_seconds` 翻成 5小时/周/月，进度条下面一行是**重置时间**（`resetLine`：优先用上游的绝对时间 `reset_at`，没有就用「拉取时刻 `status_at` + `reset_after_seconds`」推算，并显示「N 天/小时/分钟后」）；hover 整块能看到额度数据是什么时候拉的。窗口有几个由上游决定——free 账号只返回一个月窗口（`primary_window`，2592000 秒），5 小时/周窗口是 plus/pro 才有（落在 `primary_window`/`secondary_window`）。账号详情页另有完整的「周期/距离重置/重置时间」。
- 行内「额度」按钮走 `POST /api/openai/accounts/{id}/refresh` → `Refresh(id)`：① 从 `auth_json` 的 JWT 补齐 `token_expires_at`；② `EnsureFreshAuth` **按需**刷凭证（只有剩余不足 5 分钟才真刷）；③ 调 Bridge 查额度写 `status_json`。所以它主要是刷额度，不是强制刷 token。
- `POST /api/openai/accounts/refresh-all`：列出全部账号，立即返回 `{ok,total}`，刷新在后台跑；`GET /api/openai/refresh-all` 拉进度。前端每 2s 轮询、按钮显示「刷新中 3/12…」并顺带重载列表，跑完有失败才弹清单。
- 并发 `openaiQuotaRefreshConcurrency=3`（每个账号要过 Bridge 打一次 `/wham/usage`，防上游限流），单账号 60s 兜底超时（`Refresh` 内部自带 30s）。
- `Refresh` 返回 error 只为批量统计失败条数；单账号接口仍忽略它（失败原因已写进 `status_error`，页面能看到）。

任务状态类型 `batchRefreshJob` / `startBatchRefresh` 在 **`batch_refresh.go`**，OPENAI 的批量刷额度、批量拉模型与 OUTLOOK 的批量刷 Token 共用同一套（各持一个实例）：进程内存不落库、同一实例同时只跑一个任务、重复发起 409、失败原因最多留 10 条、任务用 `context.Background()` 不随请求取消。

## OUTLOOK 邮箱账号

`OUTLOOK` 页（`/outlook`，`outlook_web.go` + `outlook_refresh.go` + `outlook_mail.go`）管理注册 GPT 账号用的 Outlook 邮箱：读 `outlook_login_tokens` 表、免登录刷新 Token、直接看邮件和取验证码。**这块与代理转发无关**，纯配套工具。

表的权威 schema 归 `outlook-login-automation` skill（登录自动化）所有；本工程 `migrate` 里用同样的 `CREATE TABLE IF NOT EXISTS` 兜底，保证 skill 从没跑过时页面查询不因缺表报错，skill 建过表时是空操作。

### 账号列表

- 列：账号（邮箱 + display_name）、GPT 账号、套餐/租户、Access Token 过期、Refresh Token 过期、Cookie 数、刷新状态、更新时间、操作。
- 过期时间按剩余时长着色：已过期红、1 小时内到期黄、其余绿；零值/空显示「未知」。
- **列表接口不返回任何 token/cookie 明文**（与 OPENAI 页一致），只返回长度和过期时间等元数据。例外是 `credentials` 接口，见下。
- 整行点击进该账号的邮件页 `/outlook/accounts/{id}`。
- 「+ 新增账号」/「修改」只维护邮箱 + 密码两项。**密码明文存 `outlook_login_tokens.password`**，供后续自动登录复用；编辑弹窗回填时 `GET /api/outlook/accounts/{id}/credentials` 会明文返回密码。这是本地工具下的有意例外，和「列表不返回敏感字段」的约定相反。skill 的 upsert 列表不含 `password`，登录不会覆盖它。
- `GET /api/outlook/valid-emails` 返回「有 access token 且未过期」的邮箱列表，供外部脚本挑可用邮箱。过期判断放在 Go 里用 `time.Now()` 比，不走 MySQL `NOW()`（避开时区坑）。
- `GET /api/outlook/unregistered-emails` 返回「还没注册 GPT 账号」的邮箱列表（`has_gpt_account=0`），供批量注册流程挑下一个邮箱。判定复用 `ListOutlookAccounts`，所以标记为 0 的行会先关联 `openai_accounts` 复算并回写——刚注册完的邮箱下一次调用立刻消失。同一邮箱可能有多行（唯一键是 `email+client_id+scope`），这里按邮箱去重保留最近更新的一条；带 `?valid=1` 时再叠加「token 未过期」条件。两个接口返回结构一致：`{ok,count,emails}`。

### GPT 账号标记（has_gpt_account）

「GPT 账号」列表示该邮箱在 OPENAI 菜单里是否已有同邮箱的 GPT 账号，**存在 `outlook_login_tokens.has_gpt_account` 列里**（`ensureColumn` 补的 TINYINT，默认 0）：

- 列表查询用 `CASE WHEN t.has_gpt_account=1 THEN 1 ELSE EXISTS(SELECT 1 FROM openai_accounts oa WHERE oa.email=t.email …) END`：已经是 1 的行靠 CASE 短路**不再做关联子查询**，只有 0 的行才关联 `openai_accounts` 复算一次。
- 复算出 1 的行在返回前由 `markOutlookHasGPT` 批量回写该列，下次直接读列。
- 回写语句显式带 `updated_at=updated_at`，避免 `ON UPDATE CURRENT_TIMESTAMP` 刷新「更新时间」导致列表排序乱跳（同路由页 toggle 的坑）。
- 只做 0→1，不回退：删掉 GPT 账号不会把标记刷回「否」。

### 免登录静默刷新 Token

`refreshOutlookToken`（`outlook_refresh.go`）纯 Go 实现，不打开浏览器、不调外部脚本：

- 用库里存的登录会话 cookie 跑 `login.microsoftonline.com/consumers/oauth2/v2.0/authorize?prompt=none` 的 PKCE 静默授权，拿 `#code=` 换 token，同时刷新 access / refresh / id token。
- **不依赖 refresh_token**：该 token 是 24h 固定窗口，滚动刷新并不延长；靠 cookie 走授权才能让会话窗口真正往后滚。
- 手动跟重定向（`CheckRedirect` 返回 `ErrUseLastResponse`），逐跳收集 `Set-Cookie` 并回写 `cookies_json`，让微软续期后的 cookie 生效。
- 复用代理出站 transport（含系统代理/TLS 配置），整体 45s 超时。
- 失败写 `last_refresh_error`，页面「刷新状态」列显示失败并 hover 看原因。

### 一键刷新全部 Token（异步）

页面右上角「刷新全部 Token」= 对所有有 token 的账号各点一次「刷新 Token」，**后台异步执行，前端不阻塞**：

- `POST /api/outlook/accounts/refresh-all`：取 `access_token` 非空的账号（`ListOutlookRefreshableIDs`），立即返回 `{ok,total}`，刷新在 goroutine 里跑。
- 并发度 `outlookRefreshConcurrency=3`（微软端点限流 + 本地出站代理考虑），每账号独立 90s 超时；任务用 `context.Background()`，不受发起请求的生命周期影响（handler 早返回了）。
- 状态在进程内存 `batchRefreshJob`（`batch_refresh.go`，和 OPENAI 页「刷新全部额度」共用同一套，各持一个实例，不落库）：`running/total/done/ok/failed/errors`，同一时刻只允许一个任务，重复发起返回 **409**；失败原因带邮箱、最多留 10 条。
- `GET /api/outlook/refresh-all` 返回进度快照。前端每 2s 轮询，按钮文案变「刷新中 3/12…」并顺带重载列表（过期时间边刷边更新），跑完恢复按钮、有失败弹清单；刷新页面后会接上仍在跑的任务继续显示进度。
- 按钮点击反馈：所有按钮 `:active` 下沉+缩放+内阴影，忙碌时加 `.busy` 类在左侧转圈（单行「刷新 Token」同款）。

### 邮件与验证码

`outlook_mail.go` 直接调 Outlook Web 的 REST（`https://outlook.live.com/api/beta`），鉴权头复刻真实浏览器：`Authorization: MSAuth1.0 usertoken="<access_token>", type="MSACT"` + `x-anchormailbox: <邮箱>`。

- `outlookMailFetch` 遇 401 自动静默刷新一次再重试。
- 列表 `GET /api/outlook/accounts/{id}/messages?folder=&top=&next=`：文件夹走白名单（inbox/sentitems/drafts/deleteditems/junkemail/archive，防路径注入），翻页用上游 `@odata.nextLink`，`next` 校验 host 必须是 `outlook.live.com`（防 SSRF）。
- 详情 `GET /api/outlook/accounts/{id}/message?mid=&bodyType=html|text`：`mid` 走查询参数（消息 Id 含 `=`/`+`，不适合放 path）。
- **按邮箱取验证码** `GET /api/outlook/mail/code?email=`：定位账号 → 取收件箱最新一封 → 拉纯文本正文 → `extractVerificationCode` 按 `outlookCodePatterns` 顺序匹配（security code / 安全代码 / 验证码 / verification-temporary-one-time code / code: / 独立 6 位数字兜底），返回 `{ok,code,subject,from,received}`。
- 已知取舍：固定取**最新一封**、不按发件人或时间窗过滤，兜底正则也宽（订单号之类可能误命中）。用在自动注册流程里要注意这点。

## 路由与第三方 API 转发

`ServeHTTP`（main.go）在转发前按请求风格挑选上游，`forwardPlans` 生成候选，优先级如下：

0. **API Key 直连**（最高优先级，仅 openai 风格）：请求头 key 命中 `api_keys` 表，用 OPENAI 账号凭证直连官方 ChatGPT 后端。见「API Key 直连」。
1. 同 API 风格下启用的链式代理（`chain_proxies.enabled=1`）。
2. 同 API 风格下启用的单路由（`api_routes.enabled=1`）。
3. 官方默认上游（`-target` / `-claude-target`）。

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

### 链式代理转发

链式代理模式由 `forwardPlans` 生成多个候选 `forwardPlan`，按链路顺序尝试：

- 如果同 API 风格存在启用的链式代理，就**只走链式代理**；链内都失败时不再回退到路由页 ON 的单路由，也不回退官方上游。
- 每个子路由按它自己的协议配置构造出站请求；协议不匹配、请求转换失败、上游传输失败、HTTP 状态 `>=400`、适配后空响应都会视为该路由失败，然后尝试下一个。
- HTTP `>=400` 的中间失败会读取并丢弃响应体，关闭连接，写一条 `phase=chain_attempt` 的 JSONL 日志，然后继续下一个路由。
- 适配路径中，如果三方上游返回 HTTP 200 但 Chat 响应没有非空文本、也没有 tool call（例如 NVIDIA SSE 里只有 `data: {"error": ...}` 或空 choices），视为“空响应”失败；中间路由继续 next。
- 最后一个路由也失败时，返回最后一个路由的真实响应给 agent（避免本地造 502 导致 Codex CLI 自动重试整轮）。如果最后一个是空 200，会把最后的空响应适配后返回。
- 链式状态按 `chain_id + session_id` 存在进程内存中，不落库。成功路由会记录为 `LastSuccessRouteID`；失败路由进入默认 2 分钟冷却（`chainRouteCooldown`）。
- 下一轮同一会话进入时：冷却中的失败路由会跳过，优先从上次成功路由附近继续尝试；冷却过期后，前面失败过的路由重新进入候选，成功后会更新后续起点。
- 如果一次请求里链内全部失败，会清空该会话该链的失败冷却；用户下一次重新发起请求时，从链路第一个路由重新开始尝试。
- 如果进入请求时发现链路所有路由都在冷却中，也会清空冷却并从第一个路由重新生成候选。

## API Key 直连（GPT 账号反代）

让任意带 API Key 的客户端（原生 OpenAI SDK / Codex CLI 指向本代理）透明地借用「OPENAI」页里登录的 ChatGPT 账号，直连官方 Codex 后端，不经过路由/链式代理。逻辑在 `accountPlansForRequest`（main.go）。

命中条件（三者全满足才走直连，否则回落老逻辑）：

1. 请求是 openai 风格（Codex/Responses）；Claude 请求不适用。
2. 请求头里的 key（`Authorization: Bearer` 或 `X-Api-Key`）命中 `api_keys` 表任意一条（`MatchAPIKeyID`，返回命中的 key id 用于 token 明细关联）。
3. 按请求体的 `model` 能匹配到账号：查 `selected_model` 相同的**全部**账号作为候选集（`OpenAIAccountsForModel`，大小写不敏感）。

匹配结果与错误码（`localError` 带状态码，避免 CLI 把配置错误当临时故障反复重试）：

- 命中 → 用账号凭证直连，`applyAccountAuth` 注入 `Authorization: Bearer <access_token>` + `ChatGPT-Account-ID`，剥掉客户端自带认证头。
- 请求无 `model` / 没有账号配置该模型 → **400**，不转发也不回落（回落会用错模型且用户无感）。
- 查库失败 / 账号凭证损坏 → **503**（可能是临时的，值得重试）。

### 多账号负载均衡 + 会话粘性 + 熔断（account_pool.go）

候选集不再「取 id 最小恒命中第一个」，而是由进程内存里的 `accountPool` 调度（状态不落库）：

- **会话粘性**：同一 `session_id` 优先固定落到上次成功的账号，复用其上下文缓存以提高 `cached_tokens` 命中率。
- **负载均衡**：无粘性绑定的新会话，在健康候选里按「最少在途请求数」（least-connections）挑选，并发相同时随机打散，避免总打第一个。
- **熔断兜底**：账号请求失败（传输错误 / HTTP `>=400` / 401 刷新后仍失败 / 凭证不可用）进入 90 秒冷却（`accountCooldown`），并在它是本会话粘性账号时解绑；冷却中的账号排到候选末尾，仅当全部账号都在冷却时才作为最终兜底再试一次。
- `OrderedAccounts(session, candidates)` 给出本会话的尝试顺序：粘性优先 → 最少在途 → 随机；冷却账号殿后。`Acquire`/`Release` 维护在途计数（响应彻底结束后释放），`MarkSuccess` 绑定粘性、`MarkFailure` 熔断+解绑。

`accountPlansForRequest` 把排序后的候选逐个做成一个 `forwardPlan`（只挂候选账号，鉴权延后），`ServeHTTP` 复用与链式代理同一套 failover 循环（`failoverMode = chainMode || accountMode`）：某账号传输失败或返回 `>=400` 就自动顺位换下一个账号重试，同一请求只落一条 trace，记录的是最终命中账号的来源。每个候选真正尝试前才 `resolveAccountAuth`（按需刷新+解析 token），避免为用不到的候选浪费刷新。

### Token 自动刷新

账号的 `access_token`（JWT）会过期。存库时同时记录 `token_expires_at`（本地解 JWT 的 `exp`，`accessTokenExpiry`）。

- **主动刷新**：直连转发前 `EnsureFreshAuth` 判断剩余是否 <5 分钟（`tokenRefreshWindow`），是则调 Bridge `tokenRefresh` 刷新、回写完整 `auth.json` 与新过期时间。
- **401 兜底**：上游真返回 401 时 `retryDirectWithRefresh` 强制刷新一次并用新凭证重试（覆盖时钟偏差、服务端提前失效）。多账号直连下按当前命中账号的 `account_db_id` 精确刷新它自己（`ForceRefreshAccount`），不影响其它账号；刷新后仍失败则由 failover 循环换下一个账号。
- **并发安全**：刷新走 `refreshMu` 串行化，并传入「被拒绝的旧 token」判重——上游刷新会**轮换 refresh_token**，并发或重复刷新会把账号刷成 `refresh_token_reused` 而必须重新登录。
- **永久失败**：Bridge 返回 `permanent=true`（refresh token 过期/被复用/被吊销）时写 `refresh_error`，页面显示「需重新登录」。

### Codex 后端请求体兼容

官方 Codex 后端（`/backend-api/codex`）对请求体有硬性要求，原生 SDK 客户端不带这些字段。直连时 `prepareCodexBackendBody` 补齐：

- 强制 `store=false`（缺了 400 `Store must be set to false`）。
- 强制 `stream=true`（缺了 400 `Stream must be set to true`；上游只支持流式）。
- 剥掉后端不支持的参数（`codexBackendUnsupportedParams`，当前含 `max_output_tokens`，报 400 `Unsupported parameter`）。
- 其余缺失字段按 Bridge 的 Responses JSON Schema 默认值补齐（`ResponsesDefaults`，来自 `codex_bridge_responses_schema` ABI，`sync.Once` 只取一次）。这份 schema 是「Codex 补哪些字段」的唯一真相，避免在 Go 里硬编码一份。

### 非流式客户端：SSE→JSON 聚合

后端强制流式，但原生 SDK 的非流式调用**省略 `stream` 字段**、期待一次性 JSON。`clientWantsSSE` 据此区分（缺省视为非流式，只有显式 `stream:true` 才是要流），直连计划记 `directJSON`。

- 客户端要 SSE → 原样透传上游 SSE。
- 客户端要 JSON（`directJSON`）→ 读完上游 SSE，`aggregateResponsesSSEToJSON` 聚合成单个 Responses JSON 返回。**注意**：Codex 后端的 `response.completed` 事件里 `response.output` 是空的，真正输出项在流式过程的 `response.output_item.done` 事件里，聚合时若 `output` 为空则按 `output_index` 顺序用这些 item 重建。
- 记录/统计仍用原始 SSE（usage 与事件解析依赖它），客户端拿到的 JSON 只是聚合结果，两者分离。

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
  - 注意：部分三方 SSE 会以 HTTP 200 返回 `data: {"error": ...}`（例如上游推理过程中才失败，HTTP 头已经发出），当前解析会得到空 Chat completion；链式代理层会把这种空响应当作该路由失败并尝试 next。
- **usage 兼容**：
  - 流式 Chat Completions 请求会补 `stream_options: {"include_usage": true}`，方便 MiMo/OpenAI 兼容接口在流末返回 usage。
  - 缓存 token 兼容 OpenAI/MiMo 的 `usage.prompt_tokens_details.cached_tokens`，也兼容 DeepSeek 的 `usage.prompt_cache_hit_tokens`。
  - reasoning token 兼容 `usage.completion_tokens_details.reasoning_tokens`。
  - 还原 Responses 时会写入 `input_tokens_details.cached_tokens` 与 `output_tokens_details.reasoning_tokens`，这样 recorder 和看板能正常统计缓存/推理 token。
- **取舍**：响应是「读完上游再一次性转吐」，不是逐 token 增量流式；客户端拿到的仍是合法原生 SSE。输入 token 取三方返回的 `prompt_tokens`（三方不返回 usage 时为 0）。
- 只在 2xx 且需适配时走转换;非 2xx 原样透传三方错误。

## Token 消耗统计与图表

基于 `token_usages` 明细表，为路由 / OPENAI 账号 / API Key 提供 token 消耗的图表详情页（`stats_web.go`，模板 `token-stats`）：

- 入口：`路由`/`OPENAI`/`API Key` 三个列表页**整行点击**跳到 `/stats/tokens?dim=route|account|api_key&id=&name=`。`dim` 决定过滤列（`route_id`/`account_db_id`/`api_key_id`），`name` 仅用于页面标题展示。
- 图表页顶部可切换粒度「按天/按月/按年」，向 `/api/stats/tokens?dim=&id=&granularity=day|month|year` 拉数据。
- 后端聚合在 store.go：
  - `TokenSeries(dim, id, granularity)`：按维度过滤 + 按期间列（`date`/`month`/`CAST(year AS CHAR)`）分组，输出每个时间桶的请求数与输入/输出/缓存/合计 token。维度→过滤列、粒度→期间列都走**白名单**映射（`tokenDimColumn`/`tokenGranularityExpr`），防 SQL 注入。
  - `TokenBreakdownByModel(dim, id)`：同维度下按 `model` 分组的合计 token，用于柱状图对比。
- 前端图表用**内联 SVG 手绘**，不依赖任何外部图表库/CDN：多序列折线图（输入/输出/缓存/合计随时间）、请求数柱状图、按模型拆分柱状图，外加 5 张汇总卡片；无数据时各面板显示「暂无数据」。

## 数据库

默认 MySQL：

```text
127.0.0.1:3306 / agent_go_proxy / root / 123456
```

启动时会自动建库、建表和补迁移。

主要表：

- `conversations`：会话聚合，按 `session_id` 唯一。字段包括 `account_id`、`window_id`、`started_at`、`updated_at`、`first_prompt`、`tags`、`model`、`agent`、`status`、`trace_count`、`error_count`、`total_tokens`、`last_status`、`last_duration_ms`、`last_request_id`。
- `traces`：每次 `/v1/responses` 请求记录，保存请求/响应 headers、body、SSE events、token、状态、耗时和完成时间。
- `token_usages`：token 消耗**明细表**，每笔真实产生 token（`total_tokens>0`）的请求在 `FinishTrace` 事务里落一行，用于按维度/时间统计与画图。字段尽量用 id 关联：`trace_id`、`conversation_id`、`session_id`、`provider`、`model`、`source_type`（`direct`/`route`/`chain`/`account`）、`route_id`、`chain_id`、`api_key_id`、`account_db_id`、`account_id`、五个 token 字段，以及冗余的 `year`/`month`（`YYYY-MM`）/`date`（`YYYY-MM-DD`）方便按时间维度查询。来源信息由 `planSource(plan)` 从最终命中的转发计划算出，经 `TraceFinishRecord`→`FinishTraceInput` 传入：命中 API Key 直连记 `source_type=account`+`api_key_id`+`account_db_id`；链式代理记 `source_type=chain`+`chain_id`+命中的 `route_id`（按需求只记链内路由，不记链名）；单路由记 `source_type=route`+`route_id`；官方默认上游记 `direct`。
- `account_aliases`：`Chatgpt-Account-Id` 到自定义账号名的映射。
- `api_routes`：路由页配置的第三方 API 供应商。字段 `name`、`base_url`、`model`、`api_style`（openai/anthropic）、`protocol`（chat_completions/responses/messages）、`api_key`、`enabled`。旧库通过 `ensureColumn` 补 `api_style`/`protocol`/`enabled` 三列。
- `chain_proxies`：链式代理配置。字段 `name`、`api_style`、`route_ids`（JSON 数组，保存点击顺序）、`enabled`、`created_at`、`updated_at`。同一 `api_style` 至多一条启用；`route_ids` 里的路由必须属于同一 API 风格。
- `api_keys`：「API Key 直连」用的 Key 配置，字段 `name`、`api_key`。请求头 key 命中其中任意一条即触发直连。
- `openai_accounts`：通过 `libcodex_bridge` 动态库登录的 GPT 账号。账号摘要单独分列，完整 Codex `auth.json` 保存在 `auth_json`，额度结果保存在 `status_json`，列表 API 不返回鉴权字段。后续通过 `ensureColumn` 补的列：`token_expires_at`（access_token JWT 的 exp，用于主动刷新）、`refresh_error`（刷新永久失败原因）、`models_json`/`models_at`（缓存的模型目录）、`selected_model`/`selected_reasoning_effort`/`selected_service_tier`（页面选的模型配置）。

- `outlook_login_tokens`：Outlook 邮箱登录态。**权威 schema 归 `outlook-login-automation` skill**，本工程只用 `CREATE TABLE IF NOT EXISTS` 兜底建表（skill 没跑过时页面也能查）。主要列：`email`、`display_name`、`client_id`/`tenant_id`/`account_oid`/`home_account_id`、`scope`、`access_token`/`refresh_token`/`id_token`/`client_info`、各种过期时间（`token_issued_at`/`access_token_expires_at`/`refresh_token_expires_at`）、`cookies_json`/`cookie_count`/`user_agent`（静默刷新要用）、`last_refresh_status`/`last_refresh_error`。唯一键 `uniq_email_client_scope (email, client_id, scope(255))`。本工程通过 `ensureColumn` 补的列：`password`（手动新增/编辑填的明文登录密码，skill 的 upsert 不含此列不会覆盖）、`has_gpt_account`（是否已有同邮箱 GPT 账号，见「GPT 账号标记」）。

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
- **模型下拉**来自实际会话里的 `conversations.model`，按会话模型过滤。
- **来源下拉**（直连官方 / 路由 / 链式代理 / APIKey账号反代）按 `token_usages.source_type` 过滤，用 `EXISTS(token_usages)` 判断会话是否有匹配来源的 token 消耗。
- 搜索框提示为“搜索消息、Session...”，搜索仍覆盖 `session_id`、首条用户 prompt、model、account id 和账号别名。
- 所有筛选条件归集在 `ConversationFilter` 结构里，由 `conversationFilterFromRequest` 从查询串解析，`conversationWhere`/`ListConversationsPage`/`Stats` 统一消费。

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
- **批量删除**：每行首列有复选框，表头有「全选本页」复选框，工具条显示「已选 N 项」与「批量删除」按钮。前端用 `selectedIDs` Set 记录勾选，每次重渲染（含 3 秒自动刷新）后 `applySelection()` 恢复勾选态，保证轮询刷新不丢选择。确认后调 `POST /api/conversations/batch-delete`（`{ids:[...]}`，后端循环复用单删逻辑，含 subagent 级联）。

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
- 适配路径的 JSONL 会带 `adapter_info`：
  - `upstream_raw_body` / `upstream_decoded_body`：三方上游原始 Chat 响应与解压后文本。
  - `upstream_raw_bytes` / `upstream_decoded_bytes` / `upstream_content_encoding`。
  - `adapt_target`、`route_id`、`route_name`、`route_model`、`route_protocol`。
  - `chain_current` / `chain_next`、`chain_attempt_result`（如 `selected`、`status_failure`、`empty_response`、`final_empty_response`）。
  - `chat_text_chars`、`chat_text_trimmed_chars`、`chat_tool_calls`、`chat_finish_reason`、`chat_usage_*`、`chat_empty`。
  - 有 tool call 时记录 `chat_tool_call_details`；有文本时记录 `chat_text_preview`。
- 链式代理中间失败的候选也会单独写 JSONL，`phase=chain_attempt`，用于排查“前几条路由为什么被跳过”。这些中间失败不写入 MySQL trace，避免污染看板会话。

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
- `POST /api/conversations/batch-delete`：批量删除会话（`{ids:[...]}`，循环复用单删逻辑、含 subagent 级联）。
- `POST /api/conversations/{id}/tags`：保存会话标签。
- `POST /api/accounts/{id}/alias`：保存账号别名。
- `GET /routes`：第三方 API 配置页。
- `GET /chains`：链式代理配置页。
- `GET /api/routes`：路由配置列表 JSON。
- `POST /api/routes`：新建路由配置。
- `PUT /api/routes/{id}`：更新路由配置。
- `POST /api/routes/{id}/toggle`：切换启用状态（按 `api_style` 互斥）。
- `DELETE /api/routes/{id}`：删除路由配置。
- `GET /api/chains`：链式代理列表 JSON（同时返回可选路由）。
- `POST /api/chains`：新建链式代理。
- `PUT /api/chains/{id}`：更新链式代理。
- `POST /api/chains/{id}/toggle`：切换链式代理启用状态（按 `api_style` 互斥）。
- `DELETE /api/chains/{id}`：删除链式代理。
- `GET /api-keys`：API Key 配置页。
- `GET /api/api-keys`：API Key 列表 JSON。
- `POST /api/api-keys`：新建 API Key 配置。
- `PUT /api/api-keys/{id}`：更新 API Key 配置。
- `DELETE /api/api-keys/{id}`：删除 API Key 配置。
- `GET /openai`：GPT 账号管理页。
- `GET /openai/accounts/{id}`：GPT 账号额度详情页。
- `GET /api/openai/accounts`：GPT 账号列表，不包含鉴权 JSON。
- `DELETE /api/openai/accounts/{id}`：删除 GPT 账号及其鉴权信息。
- `POST /api/openai/logins`：启动一次 Codex Bridge 动态库浏览器登录。
- `GET /api/openai/logins/{id}`：查询登录状态。
- `POST /api/openai/logins/{id}/cancel`：取消登录。
- `POST /api/openai/accounts/{id}/refresh`：刷新该账号：本地补齐 Token 过期时间、按需刷新凭证、异步查额度。
- `POST /api/openai/accounts/refresh-all`：异步刷新全部账号的额度，立即返回 `{ok,total}`；已有任务在跑返回 409。
- `GET /api/openai/refresh-all`：批量刷额度任务的进度快照（`running/total/done/ok/failed/errors`）。
- `POST /api/openai/accounts/{id}/models`：同步拉取该账号可用模型目录并缓存。
- `POST /api/openai/accounts/models-all`：异步拉取全部账号的模型目录，立即返回 `{ok,total}`；已有任务在跑返回 409。
- `GET /api/openai/models-all`：批量拉模型任务的进度快照。
- `POST /api/openai/accounts/{id}/settings`：保存该账号选的模型/推理强度/速度。
- `GET /outlook`：Outlook 邮箱账号管理页。
- `GET /outlook/accounts/{id}`：该邮箱的邮件页。
- `GET /api/outlook/accounts`：Outlook 账号列表（不含 token/cookie 明文）。
- `POST /api/outlook/accounts`：手动新增账号（邮箱 + 密码）。
- `PUT /api/outlook/accounts/{id}`：修改邮箱 + 密码。
- `DELETE /api/outlook/accounts/{id}`：删除该行（含 token 与 cookie）。
- `GET /api/outlook/accounts/{id}/credentials`：读邮箱 + **明文密码**，供编辑弹窗回填。
- `GET /api/outlook/valid-emails`：有 access token 且未过期的邮箱列表。
- `GET /api/outlook/unregistered-emails`：还没注册 GPT 账号（`has_gpt_account=0`）的邮箱列表，按邮箱去重；`?valid=1` 只保留 token 未过期的。
- `POST /api/outlook/accounts/{id}/refresh`：静默刷新该账号的 access + refresh token（同步，成功后回读该行）。
- `POST /api/outlook/accounts/refresh-all`：异步刷新全部有 token 的账号，立即返回 `{ok,total}`；已有任务在跑返回 409。
- `GET /api/outlook/refresh-all`：一键刷新任务的进度快照（`running/total/done/ok/failed/errors`）。
- `GET /api/outlook/accounts/{id}/messages`：邮件分页列表（`?folder=&top=&next=`）。
- `GET /api/outlook/accounts/{id}/message`：单封邮件正文（`?mid=&bodyType=html|text`）。
- `GET /api/outlook/mail/code`：按邮箱取最新一封邮件里的验证码（`?email=`）。
- `GET /stats/tokens`：Token 消耗详情图表页（`?dim=route|account|api_key&id=&name=`）。
- `GET /api/stats/tokens`：Token 消耗时间序列与按模型拆分 JSON（`?dim=&id=&granularity=day|month|year`）。
- `GET /healthz`：健康检查。

## 当前技术栈

- Go 1.19
- `github.com/go-chi/chi/v5 v5.0.10`
- `github.com/go-sql-driver/mysql v1.7.1`

依赖版本锁定在 Go 1.19 兼容版本，避免使用需要 Go 1.21 的新版标准库包。
