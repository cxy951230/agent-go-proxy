package main

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

var baseTemplate = template.Must(template.New("base").Funcs(template.FuncMap{
	"fmtTime": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format("2006-01-02 15:04:05")
	},
	"fmtNullTime": func(t any) string {
		if nt, ok := t.(interface {
			Value() (any, error)
		}); ok {
			v, _ := nt.Value()
			if tv, ok := v.(time.Time); ok {
				return tv.Format("2006-01-02 15:04:05")
			}
		}
		return ""
	},
	"fmtInt": func(v int64) string {
		return humanInt(v)
	},
	"short": func(v string, n int) string {
		v = strings.TrimSpace(v)
		if len(v) <= n {
			return v
		}
		return v[:n] + "..."
	},
	"jsonPretty": prettyJSON,
	"ssePretty":  prettyJSON,
	"statusClass": func(status string) string {
		if status == "LIVE" {
			return "live"
		}
		return "ok"
	},
}).Parse(indexHTML + detailHTML))

func (p *proxyServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	conversations, err := p.store.ListConversations(r.Context(), query, status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	conversationCount, traceCount, inputTokens, outputTokens, cachedTokens, err := p.store.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Now":               time.Now(),
		"Query":             query,
		"Status":            status,
		"Conversations":     conversations,
		"ConversationCount": conversationCount,
		"TraceCount":        traceCount,
		"InputTokens":       inputTokens,
		"OutputTokens":      outputTokens,
		"CachedTokens":      cachedTokens,
	}
	if err := baseTemplate.ExecuteTemplate(w, "index", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *proxyServer) handleConversationDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad conversation id", http.StatusBadRequest)
		return
	}
	conversation, traces, err := p.store.GetConversation(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	data := map[string]any{
		"Conversation": conversation,
		"Traces":       traces,
	}
	if err := baseTemplate.ExecuteTemplate(w, "detail", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *proxyServer) handleAPIConversations(w http.ResponseWriter, r *http.Request) {
	conversations, err := p.store.ListConversations(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("status"))
	writeJSON(w, conversations, err)
}

func (p *proxyServer) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	conversations, err := p.store.ListConversations(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("status"))
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	conversationCount, traceCount, inputTokens, outputTokens, cachedTokens, err := p.store.Stats(r.Context())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{
		"now":                time.Now(),
		"conversations":      conversations,
		"conversation_count": conversationCount,
		"trace_count":        traceCount,
		"input_tokens":       inputTokens,
		"output_tokens":      outputTokens,
		"cached_tokens":      cachedTokens,
	}, nil)
}

func (p *proxyServer) handleAPIConversationDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad conversation id", http.StatusBadRequest)
		return
	}
	conversation, traces, err := p.store.GetConversation(r.Context(), id)
	writeJSON(w, map[string]any{"conversation": conversation, "traces": traces}, err)
}

func (p *proxyServer) handleAPIAccountAlias(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(chi.URLParam(r, "id"))
	var payload struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := p.store.SetAccountAlias(r.Context(), accountID, payload.DisplayName); err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func writeJSON(w http.ResponseWriter, data any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}

func prettyJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(out)
}

func humanInt(v int64) string {
	s := strconv.FormatInt(v, 10)
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

const indexHTML = `
{{define "index"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>对话日志</title>
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--orange:#d46f1e;--red:#c94040;--purple:#6750d8}
    *{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
    .page{padding:22px 28px 56px}.top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}.clock{color:#d95252;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
    .filters{display:grid;grid-template-columns:170px 170px 1fr auto auto;gap:12px;margin-top:16px}
    select,input,button{height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 12px;font:inherit;color:var(--text)}
    button{cursor:pointer}.stats,.table{background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05)}
    .stats{display:flex;gap:36px;padding:22px 26px;margin-top:16px}.stat-label{font-size:12px;font-weight:600;color:var(--muted);letter-spacing:.02em}.stat-value{font-size:28px;font-weight:600;margin-top:4px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
    .table{margin-top:18px;overflow-x:auto}table{width:100%;min-width:1180px;border-collapse:collapse;table-layout:fixed}th,td{padding:14px 14px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle}th{font-size:12px;color:#606a7a;font-weight:600;letter-spacing:.04em;background:#fbfcfe}tr:last-child td{border-bottom:0}tbody tr.row-link{cursor:pointer}tbody tr.row-link:hover{background:#fafcff}
    th:nth-child(1),td:nth-child(1){width:132px}th:nth-child(2),td:nth-child(2){width:300px}th:nth-child(3),td:nth-child(3){width:112px}th:nth-child(4),td:nth-child(4){width:58px}th:nth-child(5),td:nth-child(5),th:nth-child(6),td:nth-child(6),th:nth-child(7),td:nth-child(7){width:92px}th:nth-child(8),td:nth-child(8){width:86px}th:nth-child(9),td:nth-child(9){width:90px}th:nth-child(10),td:nth-child(10){width:74px}th:nth-child(11),td:nth-child(11){width:82px}
    .prompt{font-size:14px;color:#242832}.sid{display:block;margin-top:4px;color:#8a93a3;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}
    .account-btn{height:auto;max-width:104px;padding:3px 9px;border-radius:6px;border:1px solid #b8e2df;background:#edfafa;color:#0f766e;text-align:left;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;box-shadow:none;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px;font-weight:600}.account-btn:hover{background:#e2f7f4;border-color:#81cfc7;color:#0b5d56}
    .pill{display:inline-block;border-radius:6px;padding:3px 9px;font-size:13px;font-weight:600;white-space:nowrap}.model{color:var(--purple);background:#f4f1ff;border:1px solid #ddd4ff}.agent{color:#2469e8;background:#edf4ff;border:1px solid #c6d9ff}.ok{color:var(--green);background:#ecfbf2;border:1px solid #a9e2bf}.live{color:var(--green);background:#f4fff7;border:1px solid #bbe7c8}.token{color:var(--orange);background:#fff8ee;border:1px solid #ffd09a;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
    a{color:var(--blue);text-decoration:none}.action{display:inline-flex;align-items:center;justify-content:center;min-width:48px;height:30px;border:1px solid var(--line);padding:0 10px;border-radius:7px;background:#fff;font-weight:500;white-space:nowrap}
  </style>
</head>
<body>
<main class="page">
  <div class="top"><h1>对话日志</h1><div class="clock" id="clock">{{fmtTime .Now}}</div></div>
  <form class="filters" method="get">
    <select name="date"><option>全部日期</option></select>
    <select name="status">
      <option value="all">全部状态</option>
      <option value="LIVE" {{if eq .Status "LIVE"}}selected{{end}}>LIVE</option>
      <option value="OK" {{if eq .Status "OK"}}selected{{end}}>OK</option>
    </select>
    <input name="q" value="{{.Query}}" placeholder="搜索消息、Agent、模型、Session...">
    <button type="submit">搜索</button>
    <button type="button" onclick="location.reload()">R</button>
  </form>
  <section class="stats">
    <div><div class="stat-label">会话数</div><div class="stat-value" id="stat-conversations">{{.ConversationCount}}</div></div>
    <div><div class="stat-label">Trace 数</div><div class="stat-value" id="stat-traces">{{.TraceCount}}</div></div>
    <div><div class="stat-label">输入 Token</div><div class="stat-value" id="stat-input-tokens">{{fmtInt .InputTokens}}</div></div>
    <div><div class="stat-label">输出 Token</div><div class="stat-value" id="stat-output-tokens">{{fmtInt .OutputTokens}}</div></div>
    <div><div class="stat-label">缓存 Token</div><div class="stat-value" id="stat-cached-tokens">{{fmtInt .CachedTokens}}</div></div>
  </section>
  <section class="table">
    <table>
      <thead><tr><th>开始时间</th><th>首条用户 PROMPT</th><th>账号</th><th>TRACE</th><th>输入 Token</th><th>输出 Token</th><th>缓存 Token</th><th>模型</th><th>AGENT</th><th>状态</th><th>操作</th></tr></thead>
      <tbody id="conversation-rows">
      {{range .Conversations}}
        <tr class="row-link" data-href="/conversations/{{.ID}}">
          <td>{{fmtTime .StartedAt}}</td>
          <td><span class="prompt">{{short .FirstPrompt 80}}</span><span class="sid">{{.SessionID}}</span></td>
          <td><button class="account-btn" title="{{.AccountID}}" data-account-id="{{.AccountID}}" data-account-name="{{.AccountName}}">{{if .AccountName}}{{.AccountName}}{{else}}{{short .AccountID 12}}{{end}}</button></td>
          <td>{{.TraceCount}}</td>
          <td><span class="pill token">{{fmtInt .InputTokens}}</span></td>
          <td><span class="pill token">{{fmtInt .OutputTokens}}</span></td>
          <td><span class="pill token">{{fmtInt .CachedTokens}}</span></td>
          <td><span class="pill model">{{if .Model}}{{.Model}}{{else}}unknown{{end}}</span></td>
          <td><span class="pill agent">{{.Agent}}</span></td>
          <td><span class="pill {{statusClass .Status}}">{{.Status}}</span></td>
          <td><a class="action" href="/conversations/{{.ID}}">详情</a></td>
        </tr>
      {{else}}
        <tr><td colspan="11">暂无数据。发起一次 Codex 请求后这里会出现新会话。</td></tr>
      {{end}}
      </tbody>
    </table>
  </section>
</main>
<script>
const fmtInt = n => Number(n || 0).toLocaleString('en-US');
const fmtTime = v => {
  if (!v) return '';
  const d = new Date(v);
  const pad = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + pad(d.getMonth()+1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
};
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const short = (v, n) => (v || '').length > n ? (v || '').slice(0, n) + '...' : (v || '');
const accountLabel = item => item.AccountName || short(item.AccountID || 'unknown', 12);
function statusClass(status){ return status === 'LIVE' ? 'live' : 'ok'; }
async function editAccount(button){
  const accountID = button.dataset.accountId || '';
  if (!accountID) return;
  const current = button.dataset.accountName || accountID;
  const next = prompt('账号名称', current);
  if (next === null) return;
  const rsp = await fetch('/api/accounts/' + encodeURIComponent(accountID) + '/alias', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({display_name: next.trim()})
  });
  if (rsp.ok) refreshDashboard().catch(() => {});
}
document.getElementById('conversation-rows').addEventListener('click', event => {
  const accountButton = event.target.closest('.account-btn');
  if (accountButton) {
    event.preventDefault();
    editAccount(accountButton).catch(() => {});
    return;
  }
  if (event.target.closest('a,button,input,select')) return;
  const row = event.target.closest('tr[data-href]');
  if (row) location.href = row.dataset.href;
});
async function refreshDashboard(){
  const form = document.querySelector('.filters');
  const qs = new URLSearchParams(new FormData(form));
  const rsp = await fetch('/api/dashboard?' + qs.toString(), {cache:'no-store'});
  if (!rsp.ok) return;
  const data = await rsp.json();
  document.getElementById('clock').textContent = fmtTime(data.now);
  document.getElementById('stat-conversations').textContent = data.conversation_count || 0;
  document.getElementById('stat-traces').textContent = data.trace_count || 0;
  document.getElementById('stat-input-tokens').textContent = fmtInt(data.input_tokens);
  document.getElementById('stat-output-tokens').textContent = fmtInt(data.output_tokens);
  document.getElementById('stat-cached-tokens').textContent = fmtInt(data.cached_tokens);
  const rows = document.getElementById('conversation-rows');
  rows.innerHTML = (data.conversations || []).map(item =>
    '<tr class="row-link" data-href="/conversations/' + item.ID + '">' +
      '<td>' + fmtTime(item.StartedAt) + '</td>' +
      '<td><span class="prompt">' + esc(short(item.FirstPrompt || '未捕获到用户 prompt。', 80)) + '</span><span class="sid">' + esc(item.SessionID) + '</span></td>' +
      '<td><button class="account-btn" title="' + esc(item.AccountID || '') + '" data-account-id="' + esc(item.AccountID || '') + '" data-account-name="' + esc(item.AccountName || '') + '">' + esc(accountLabel(item)) + '</button></td>' +
      '<td>' + (item.TraceCount || 0) + '</td>' +
      '<td><span class="pill token">' + fmtInt(item.InputTokens) + '</span></td>' +
      '<td><span class="pill token">' + fmtInt(item.OutputTokens) + '</span></td>' +
      '<td><span class="pill token">' + fmtInt(item.CachedTokens) + '</span></td>' +
      '<td><span class="pill model">' + esc(item.Model || 'unknown') + '</span></td>' +
      '<td><span class="pill agent">' + esc(item.Agent || 'Codex') + '</span></td>' +
      '<td><span class="pill ' + statusClass(item.Status) + '">' + esc(item.Status || 'LIVE') + '</span></td>' +
      '<td><a class="action" href="/conversations/' + item.ID + '">详情</a></td>' +
    '</tr>').join('') || '<tr><td colspan="11">暂无数据。发起一次 Codex 请求后这里会出现新会话。</td></tr>';
}
setInterval(() => refreshDashboard().catch(() => {}), 1000);
</script>
</body>
</html>
{{end}}
`

const detailHTML = `
{{define "detail"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Codex 详情</title>
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--orange:#d9851f}
    *{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--text);font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
    .page{padding:24px 28px}.head{display:flex;align-items:center;gap:12px}.back{width:38px;height:38px;border:1px solid var(--line);border-radius:8px;display:grid;place-items:center;background:white;color:#8792a2;font-size:22px;text-decoration:none}.title{font-size:19px;font-weight:600}.sid{color:#8f98a8;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}
    .bar,.notice,.turn{background:white;border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(21,35,60,.05);margin-top:16px}.bar{padding:14px 18px;display:flex;justify-content:space-between;color:#5f6978}.notice{padding:12px 14px;color:var(--orange);background:#fffaf0}
    .turn{padding:14px 18px}.turn-head{display:flex;align-items:center;gap:10px;font-size:16px;font-weight:600}.meta{color:#8791a2;font-size:13px;font-weight:400;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.json-btn{margin-left:auto;border:1px solid var(--line);border-radius:7px;padding:4px 11px;color:#7e8796;background:#fff;font-size:13px}
    .grid{display:grid;grid-template-columns:1fr;gap:10px;margin-top:12px}.box{border-radius:7px;padding:14px 18px;border:1px solid}.req{background:#f3f7ff;border-color:#c7d7fb}.res{background:#f1fbf6;border-color:#abe6c8}.sse{background:#fffaf0;border-color:#ffe0a3}.err{background:#fff1f1;border-color:#f0b3b3}.tag{display:inline-block;padding:3px 9px;border-radius:5px;color:white;font-size:13px;font-weight:600;margin-bottom:10px}.req .tag{background:#3d78e5}.res .tag{background:#14a574}.sse .tag{background:#d9851f}.err .tag{background:#c94040}
    pre{margin:0;white-space:pre-wrap;word-break:break-word;max-height:520px;overflow:auto;font:13px/1.55 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.small{font-size:13px;color:#6b7484}.stats{display:flex;gap:10px;flex-wrap:wrap;margin-top:8px}.pill{background:#f7f9fc;border:1px solid var(--line);border-radius:999px;padding:3px 9px;color:#5d6675;font-size:13px}
  </style>
</head>
<body>
<main class="page">
  <div class="head"><a class="back" href="/">←</a><div><div class="title">Codex 详情</div><div class="sid">{{.Conversation.SessionID}}</div></div></div>
  <section class="bar"><div>完整 Viewer</div><div><a href="/api/conversations/{{.Conversation.ID}}">导出 JSON</a>　复制会话 ID</div></section>
  <section class="notice" id="trace-count">正在展示 {{len .Traces}} / {{.Conversation.TraceCount}} 条记录。</section>
  <div id="trace-list">
  {{range .Traces}}
  <section class="turn">
    <div class="turn-head">▸ Turn {{.SequenceNo}} <span class="meta">{{.Status}} {{.Method}} {{.Path}} · {{.DurationMS}}ms · {{.Model}}</span><a class="json-btn" href="/api/conversations/{{$.Conversation.ID}}">JSON</a></div>
    <div class="stats">
      <span class="pill">request {{.RequestBytes}} bytes</span>
      <span class="pill">response {{.ResponseBytes}} bytes</span>
      <span class="pill">input {{.InputTokens}}</span>
      <span class="pill">output {{.OutputTokens}}</span>
      <span class="pill">total {{.TotalTokens}}</span>
      <span class="pill">cached {{.CachedTokens}}</span>
    </div>
    <div class="grid">
      {{if .Error}}<div class="box err"><span class="tag">错误</span><pre>{{.Error}}</pre></div>{{end}}
      <div class="box req"><span class="tag">请求 Headers</span><pre>{{jsonPretty .RequestHeaders}}</pre></div>
      <div class="box req"><span class="tag">请求 Body</span><pre>{{jsonPretty .RequestBody}}</pre></div>
      <div class="box res"><span class="tag">响应 Headers</span><pre>{{jsonPretty .ResponseHeaders}}</pre></div>
      <div class="box sse"><span class="tag">SSE Events</span><pre>{{ssePretty .SSEEvents}}</pre></div>
      <div class="box res"><span class="tag">响应 Body 原文</span><pre>{{.ResponseBody}}</pre></div>
    </div>
  </section>
  {{else}}
  <section class="turn">暂无 trace。</section>
  {{end}}
  </div>
</main>
<script>
const conversationID = {{.Conversation.ID}};
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const pretty = v => {
  if (!v) return '';
  try { return JSON.stringify(JSON.parse(v), null, 2); } catch (_) { return String(v); }
};
function traceHTML(t){
  const errorBox = t.Error ? '<div class="box err"><span class="tag">错误</span><pre>' + esc(t.Error) + '</pre></div>' : '';
  return '<section class="turn">' +
    '<div class="turn-head">▸ Turn ' + (t.SequenceNo || 0) + ' <span class="meta">' + (t.Status || 0) + ' ' + esc(t.Method) + ' ' + esc(t.Path) + ' · ' + (t.DurationMS || 0) + 'ms · ' + esc(t.Model) + '</span><a class="json-btn" href="/api/conversations/' + conversationID + '">JSON</a></div>' +
    '<div class="stats">' +
      '<span class="pill">request ' + (t.RequestBytes || 0) + ' bytes</span>' +
      '<span class="pill">response ' + (t.ResponseBytes || 0) + ' bytes</span>' +
      '<span class="pill">input ' + (t.InputTokens || 0) + '</span>' +
      '<span class="pill">output ' + (t.OutputTokens || 0) + '</span>' +
      '<span class="pill">total ' + (t.TotalTokens || 0) + '</span>' +
      '<span class="pill">cached ' + (t.CachedTokens || 0) + '</span>' +
    '</div>' +
    '<div class="grid">' +
      errorBox +
      '<div class="box req"><span class="tag">请求 Headers</span><pre>' + esc(pretty(t.RequestHeaders)) + '</pre></div>' +
      '<div class="box req"><span class="tag">请求 Body</span><pre>' + esc(pretty(t.RequestBody)) + '</pre></div>' +
      '<div class="box res"><span class="tag">响应 Headers</span><pre>' + esc(pretty(t.ResponseHeaders)) + '</pre></div>' +
      '<div class="box sse"><span class="tag">SSE Events</span><pre>' + esc(pretty(t.SSEEvents)) + '</pre></div>' +
      '<div class="box res"><span class="tag">响应 Body 原文</span><pre>' + esc(t.ResponseBody) + '</pre></div>' +
    '</div>' +
  '</section>';
}
let lastPayload = '';
async function refreshDetail(){
  const rsp = await fetch('/api/conversations/' + conversationID, {cache:'no-store'});
  if (!rsp.ok) return;
  const data = await rsp.json();
  const payload = JSON.stringify(data);
  if (payload === lastPayload) return;
  lastPayload = payload;
  document.getElementById('trace-count').textContent = '正在展示 ' + (data.traces || []).length + ' / ' + ((data.conversation || {}).TraceCount || 0) + ' 条记录。';
  document.getElementById('trace-list').innerHTML = (data.traces || []).map(traceHTML).join('') || '<section class="turn">暂无 trace。</section>';
}
setInterval(() => refreshDetail().catch(() => {}), 1000);
</script>
</body>
</html>
{{end}}
`
