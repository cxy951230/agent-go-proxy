package main

import (
	"database/sql"
	"encoding/json"
	"errors"
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
	"fmtMinutes": func(v float64) string {
		return formatMinutes(v)
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
	"toJSON": func(v any) template.JS {
		raw, err := json.Marshal(v)
		if err != nil {
			return "null"
		}
		return template.JS(raw)
	},
	"statusClass": func(status string) string {
		switch status {
		case "LIVE":
			return "live"
		case "ERROR":
			return "error"
		default:
			return "ok"
		}
	},
}).Parse(indexHTML + detailHTML))

func (p *proxyServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	conversations, err := p.store.ListConversations(r.Context(), query, status, date, agent, accountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filterOptions, err := p.store.FilterOptions(r.Context())
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
		"Date":              date,
		"Agent":             agent,
		"AccountID":         accountID,
		"FilterOptions":     filterOptions,
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

func (p *proxyServer) handleFavicon(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "/Users/chenxy/Desktop/331.jpg")
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
	conversations, err := p.store.ListConversations(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("status"), r.URL.Query().Get("date"), r.URL.Query().Get("agent"), r.URL.Query().Get("account_id"))
	writeJSON(w, conversations, err)
}

func (p *proxyServer) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	conversations, err := p.store.ListConversations(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("status"), r.URL.Query().Get("date"), r.URL.Query().Get("agent"), r.URL.Query().Get("account_id"))
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	filterOptions, err := p.store.FilterOptions(r.Context())
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
		"filter_options":     filterOptions,
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

func (p *proxyServer) handleAPIConversationTags(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad conversation id", http.StatusBadRequest)
		return
	}
	var payload struct {
		Tags string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := p.store.SetConversationTags(r.Context(), id, payload.Tags); err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func (p *proxyServer) handleAPIConversationDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad conversation id", http.StatusBadRequest)
		return
	}
	if err := p.store.DeleteConversation(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
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

func formatMinutes(v float64) string {
	if v < 0 {
		v = 0
	}
	if v >= 10 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

const indexHTML = `
{{define "index"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--orange:#d46f1e;--red:#c94040;--purple:#6750d8}
    *{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
    .page{padding:22px 28px 56px}.top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}.clock{color:#d95252;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
    .filters{display:grid;grid-template-columns:150px 150px 150px 190px 1fr auto;gap:12px;margin-top:16px}
    select,input,button{height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 12px;font:inherit;color:var(--text)}
    button{cursor:pointer}.stats,.table{background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05)}
    .stats{display:flex;gap:36px;padding:22px 26px;margin-top:16px}.stat-label{font-size:12px;font-weight:600;color:var(--muted);letter-spacing:.02em}.stat-value{font-size:28px;font-weight:600;margin-top:4px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
    .table{margin-top:18px;overflow-x:auto}table{width:100%;min-width:1280px;border-collapse:collapse;table-layout:fixed}th,td{padding:14px 14px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle}th{font-size:12px;color:#606a7a;font-weight:600;letter-spacing:.04em;background:#fbfcfe}tr:last-child td{border-bottom:0}tbody tr.row-link{cursor:pointer}tbody tr.row-link:hover{background:#fafcff}
    th:nth-child(1),td:nth-child(1){width:128px}th:nth-child(2),td:nth-child(2){width:285px}th:nth-child(3),td:nth-child(3){width:96px}th:nth-child(4),td:nth-child(4){width:90px}th:nth-child(5),td:nth-child(5){width:58px}th:nth-child(6),td:nth-child(6){width:170px}th:nth-child(7),td:nth-child(7){width:86px}th:nth-child(8),td:nth-child(8){width:90px}th:nth-child(9),td:nth-child(9){width:70px}th:nth-child(10),td:nth-child(10){width:74px}th:nth-child(11),td:nth-child(11){width:82px}
    .prompt{font-size:14px;color:#242832}.sid{display:block;margin-top:4px;color:#8a93a3;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}
    .account-btn,.tag-btn{height:auto;max-width:104px;padding:3px 9px;border-radius:6px;border:1px solid #b8e2df;background:#edfafa;color:#0f766e;text-align:left;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;box-shadow:none;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px;font-weight:600}.account-btn:hover,.tag-btn:hover{background:#e2f7f4;border-color:#81cfc7;color:#0b5d56}.tag-btn{max-width:82px;border-color:#d8e0eb;background:#f7f9fc;color:#526074;font-family:inherit}
    .pill{display:inline-block;border-radius:6px;padding:3px 9px;font-size:13px;font-weight:600;white-space:nowrap}.model{color:var(--purple);background:#f4f1ff;border:1px solid #ddd4ff}.agent{color:#2469e8;background:#edf4ff;border:1px solid #c6d9ff}.ok{color:var(--green);background:#ecfbf2;border:1px solid #a9e2bf}.live{color:#fff;background:#f5821f;border:1px solid #f5821f}.error{color:var(--red);background:#fff1f1;border:1px solid #f0b3b3}.token{color:var(--orange);background:#fff8ee;border:1px solid #ffd09a;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.token-stack{display:inline-grid;gap:2px;line-height:1.35;min-width:92px;text-align:right}
    a{color:var(--blue);text-decoration:none}.action{display:inline-flex;align-items:center;justify-content:center;min-width:48px;height:30px;border:1px solid var(--line);padding:0 10px;border-radius:7px;background:#fff;font-weight:500;white-space:nowrap}.delete-btn{color:var(--red);border-color:#f0b3b3;background:#fff6f6}.delete-btn:hover{background:#fff1f1;border-color:#e07d7d}
  </style>
</head>
<body>
<main class="page">
  <div class="top"><h1>对话日志</h1><div class="clock" id="clock">{{fmtTime .Now}}</div></div>
  <form class="filters" method="get">
    <select name="date" id="date-filter">
      <option value="all">全部日期</option>
      {{range .FilterOptions.Dates}}<option value="{{.}}" {{if eq $.Date .}}selected{{end}}>{{.}}</option>{{end}}
    </select>
    <select name="status">
      <option value="all">全部状态</option>
      <option value="LIVE" {{if eq .Status "LIVE"}}selected{{end}}>LIVE</option>
      <option value="OK" {{if eq .Status "OK"}}selected{{end}}>OK</option>
      <option value="ERROR" {{if eq .Status "ERROR"}}selected{{end}}>ERROR</option>
    </select>
    <select name="agent" id="agent-filter">
      <option value="all">全部 Agent</option>
      {{range .FilterOptions.Agents}}<option value="{{.}}" {{if eq $.Agent .}}selected{{end}}>{{.}}</option>{{end}}
    </select>
    <select name="account_id" id="account-filter">
      <option value="all">全部账号</option>
      {{range .FilterOptions.AccountAliases}}<option value="{{.AccountID}}" {{if eq $.AccountID .AccountID}}selected{{end}}>{{.DisplayName}}</option>{{end}}
    </select>
    <input name="q" value="{{.Query}}" placeholder="搜索消息、Session...">
    <button type="submit">搜索</button>
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
      <thead><tr><th>时间</th><th>首条用户 PROMPT</th><th>账号</th><th>标签</th><th>TRACE</th><th>Token<br>输入/输出/缓存</th><th>模型</th><th>AGENT</th><th>耗时(分)</th><th>状态</th><th>操作</th></tr></thead>
      <tbody id="conversation-rows">
      {{range .Conversations}}
        <tr class="row-link" data-href="/conversations/{{.ID}}">
          <td>{{fmtTime .UpdatedAt}}</td>
          <td><span class="prompt">{{short .FirstPrompt 80}}</span><span class="sid">{{.SessionID}}</span></td>
          <td><button class="account-btn" title="{{.AccountID}}" data-account-id="{{.AccountID}}" data-account-name="{{.AccountName}}">{{if .AccountName}}{{.AccountName}}{{else}}{{short .AccountID 12}}{{end}}</button></td>
          <td><button class="tag-btn" title="{{.Tags}}" data-conversation-id="{{.ID}}" data-tags="{{.Tags}}">{{if .Tags}}{{short .Tags 10}}{{else}}+{{end}}</button></td>
          <td>{{.TraceCount}}</td>
          <td><span class="pill token token-stack"><span>{{fmtInt .InputTokens}}</span><span>{{fmtInt .OutputTokens}}</span><span>{{fmtInt .CachedTokens}}</span></span></td>
          <td><span class="pill model">{{if .Model}}{{.Model}}{{else}}unknown{{end}}</span></td>
          <td><span class="pill agent">{{.Agent}}</span></td>
          <td>{{fmtMinutes .DurationMin}}</td>
          <td><span class="pill {{statusClass .Status}}">{{.Status}}</span></td>
          <td><button class="action delete-btn" data-delete-id="{{.ID}}" data-session-id="{{.SessionID}}">删除</button></td>
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
const fmtMinutes = v => {
  v = Math.max(0, Number(v || 0));
  return v >= 10 ? Math.round(v).toString() : v.toFixed(1);
};
function statusClass(status){
  if (status === 'LIVE') return 'live';
  if (status === 'ERROR') return 'error';
  return 'ok';
}
function syncSelectOptions(select, allLabel, options, getValue, getLabel){
  if (!select) return;
  const current = select.value || 'all';
  const html = ['<option value="all">' + esc(allLabel) + '</option>']
    .concat((options || []).map(item => {
      const value = getValue(item);
      const label = getLabel(item);
      return '<option value="' + esc(value) + '">' + esc(label) + '</option>';
    })).join('');
  if (select.dataset.optionsHtml !== html) {
    select.innerHTML = html;
    select.dataset.optionsHtml = html;
  }
  select.value = Array.from(select.options).some(opt => opt.value === current) ? current : 'all';
}
function syncFilterOptions(options){
  options = options || {};
  syncSelectOptions(document.getElementById('date-filter'), '全部日期', options.Dates || [], v => v, v => v);
  syncSelectOptions(document.getElementById('agent-filter'), '全部 Agent', options.Agents || [], v => v, v => v);
  syncSelectOptions(document.getElementById('account-filter'), '全部账号', options.AccountAliases || [], v => v.AccountID, v => v.DisplayName);
}
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
async function editTags(button){
  const conversationID = button.dataset.conversationId || '';
  if (!conversationID) return;
  const current = button.dataset.tags || '';
  const next = prompt('标签', current);
  if (next === null) return;
  const rsp = await fetch('/api/conversations/' + encodeURIComponent(conversationID) + '/tags', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({tags: next.trim()})
  });
  if (rsp.ok) refreshDashboard().catch(() => {});
}
async function deleteConversation(button){
  const conversationID = button.dataset.deleteId || '';
  if (!conversationID) return;
  const sessionID = button.dataset.sessionId || '';
  const message = '确认删除这个会话吗？\\n\\n' + (sessionID ? ('Session: ' + sessionID + '\\n') : '') + '会话和所有 trace 都会被删除。';
  if (!confirm(message)) return;
  const rsp = await fetch('/api/conversations/' + encodeURIComponent(conversationID), {method: 'DELETE'});
  if (!rsp.ok) {
    alert('删除失败：' + await rsp.text());
    return;
  }
  refreshDashboard().catch(() => {});
}
document.getElementById('conversation-rows').addEventListener('click', event => {
  const accountButton = event.target.closest('.account-btn');
  if (accountButton) {
    event.preventDefault();
    editAccount(accountButton).catch(() => {});
    return;
  }
  const tagButton = event.target.closest('.tag-btn');
  if (tagButton) {
    event.preventDefault();
    editTags(tagButton).catch(() => {});
    return;
  }
  const deleteButton = event.target.closest('.delete-btn');
  if (deleteButton) {
    event.preventDefault();
    deleteConversation(deleteButton).catch(err => alert('删除失败：' + err));
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
  syncFilterOptions(data.filter_options);
  const rows = document.getElementById('conversation-rows');
  rows.innerHTML = (data.conversations || []).map(item =>
    '<tr class="row-link" data-href="/conversations/' + item.ID + '">' +
      '<td>' + fmtTime(item.UpdatedAt) + '</td>' +
      '<td><span class="prompt">' + esc(short(item.FirstPrompt || '未捕获到用户 prompt。', 80)) + '</span><span class="sid">' + esc(item.SessionID) + '</span></td>' +
      '<td><button class="account-btn" title="' + esc(item.AccountID || '') + '" data-account-id="' + esc(item.AccountID || '') + '" data-account-name="' + esc(item.AccountName || '') + '">' + esc(accountLabel(item)) + '</button></td>' +
      '<td><button class="tag-btn" title="' + esc(item.Tags || '') + '" data-conversation-id="' + item.ID + '" data-tags="' + esc(item.Tags || '') + '">' + esc(item.Tags ? short(item.Tags, 10) : '+') + '</button></td>' +
      '<td>' + (item.TraceCount || 0) + '</td>' +
      '<td><span class="pill token token-stack"><span>' + fmtInt(item.InputTokens) + '</span><span>' + fmtInt(item.OutputTokens) + '</span><span>' + fmtInt(item.CachedTokens) + '</span></span></td>' +
      '<td><span class="pill model">' + esc(item.Model || 'unknown') + '</span></td>' +
      '<td><span class="pill agent">' + esc(item.Agent || 'Codex') + '</span></td>' +
      '<td>' + fmtMinutes(item.DurationMin) + '</td>' +
      '<td><span class="pill ' + statusClass(item.Status) + '">' + esc(item.Status || 'LIVE') + '</span></td>' +
      '<td><button class="action delete-btn" data-delete-id="' + item.ID + '" data-session-id="' + esc(item.SessionID || '') + '">删除</button></td>' +
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
  <title>AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--orange:#d9851f}
    *{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--text);font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
    .page{padding:24px 28px}.head{display:flex;align-items:center;justify-content:space-between;gap:16px}.head-main{display:flex;align-items:center;gap:12px}.back{width:38px;height:38px;border:1px solid var(--line);border-radius:8px;display:grid;place-items:center;background:white;color:#8792a2;font-size:22px;text-decoration:none}.title{font-size:19px;font-weight:600}.sid{color:#8f98a8;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}
    .mode-switch{display:inline-flex;align-items:center;padding:3px;border:1px solid var(--line);border-radius:8px;background:#fff;box-shadow:0 1px 3px rgba(21,35,60,.05)}.mode-btn{height:31px;border:0;border-radius:6px;background:transparent;color:#6f7887;padding:0 14px;font:600 13px/1 -apple-system,BlinkMacSystemFont,"Segoe UI",Arial,sans-serif;cursor:pointer}.mode-btn.active{background:#2f6fed;color:#fff;box-shadow:0 1px 2px rgba(47,111,237,.22)}
    .bar,.notice,.turn{background:white;border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(21,35,60,.05);margin-top:16px}.bar{padding:14px 18px;display:flex;justify-content:space-between;color:#5f6978}.notice{padding:12px 14px;color:var(--orange);background:#fffaf0}
    .turn{padding:14px 18px}.turn-head{display:flex;align-items:center;gap:10px;font-size:16px;font-weight:600}.meta{color:#8791a2;font-size:13px;font-weight:400;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.json-btn{margin-left:auto;border:1px solid var(--line);border-radius:7px;padding:4px 11px;color:#7e8796;background:#fff;font-size:13px}
    .grid{display:grid;grid-template-columns:1fr;gap:10px;margin-top:12px}.box{border-radius:7px;padding:14px 18px;border:1px solid}.req{background:#f3f7ff;border-color:#c7d7fb}.res{background:#f1fbf6;border-color:#abe6c8}.sse{background:#fffaf0;border-color:#ffe0a3}.err{background:#fff1f1;border-color:#f0b3b3}.tag{display:inline-block;padding:3px 9px;border-radius:5px;color:white;font-size:13px;font-weight:600;margin-bottom:10px}.req .tag{background:#3d78e5}.res .tag{background:#14a574}.sse .tag{background:#d9851f}.err .tag{background:#c94040}
    .brief-grid{display:grid;grid-template-columns:1fr;gap:12px;margin-top:12px}.brief-box{border-radius:7px;padding:18px 22px;border:1px solid;font-size:15px;line-height:1.7}.brief-box.req{background:#f3f7ff;border-color:#b9cdfd}.brief-box.res{background:#ebfbf4;border-color:#88e6bd}.brief-text{white-space:pre-wrap;word-break:break-word;margin-top:12px}.brief-chip{display:inline-block;margin:0 6px 6px 0;padding:2px 8px;border-radius:999px;background:#fff;border:1px solid #cddaf2;color:#435169;font-size:13px;font-weight:600}.role-line{margin:0 0 14px}.role-name{display:inline-block;min-width:74px;color:#657083;font-weight:700}.tool-label{display:inline-block;color:#475569;background:#eef2f7;border:1px solid #d8e0eb;border-radius:5px;padding:1px 7px;font-size:12px;font-weight:700;margin-right:6px}.empty-text{color:#4f5b6b}
    .ctx{margin-top:12px;border:1px solid #d9e2f1;border-radius:8px;background:#fbfdff;overflow:hidden}.ctx-tabs{display:flex;gap:6px;align-items:center;padding:8px;background:#f4f7fc;border-bottom:1px solid #d9e2f1;overflow-x:auto}.ctx-tab{border:1px solid transparent;background:transparent;color:#667284;border-radius:6px;padding:6px 10px;font-weight:700;font-size:13px;white-space:nowrap;cursor:pointer}.ctx-tab.active{background:#fff;color:#2f6fed;border-color:#c9d8f7;box-shadow:0 1px 2px rgba(21,35,60,.05)}.ctx-panel{display:none;padding:14px 16px}.ctx-panel.active{display:block}.ctx-summary{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}.ctx-stat{border:1px solid #dde6f2;border-radius:7px;background:#fff;padding:10px 12px}.ctx-stat-label{font-size:12px;color:#748094;font-weight:700}.ctx-stat-value{font-size:20px;font-weight:700;margin-top:2px;color:#20242c}.ctx-list{display:grid;gap:10px}.ctx-card{border:1px solid #dde6f2;border-radius:7px;background:#fff;padding:12px}.ctx-card-title{display:flex;gap:8px;align-items:center;flex-wrap:wrap;font-weight:800;color:#242a34}.ctx-card-sub{color:#7a8495;font-size:12px;margin-top:3px;word-break:break-all}.ctx-card-desc{margin-top:8px;color:#3d4654;white-space:pre-wrap}.ctx-param{display:inline-block;margin:8px 6px 0 0;padding:2px 7px;border-radius:5px;background:#f3f6fb;border:1px solid #dde6f2;color:#526074;font-size:12px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.ctx-pre{margin-top:8px;max-height:220px;overflow:auto;white-space:pre-wrap;word-break:break-word;font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:#344054;background:#f7f9fc;border:1px solid #e0e7f2;border-radius:6px;padding:10px}.ctx-kv{display:grid;grid-template-columns:150px 1fr;gap:8px;padding:7px 0;border-bottom:1px solid #edf1f7}.ctx-kv:last-child{border-bottom:0}.ctx-key{color:#687386;font-weight:700}.ctx-val{word-break:break-word}
    pre{margin:0;white-space:pre-wrap;word-break:break-word;max-height:520px;overflow:auto;font:13px/1.55 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.small{font-size:13px;color:#6b7484}.stats{display:flex;gap:10px;flex-wrap:wrap;margin-top:8px}.pill{background:#f7f9fc;border:1px solid var(--line);border-radius:999px;padding:3px 9px;color:#5d6675;font-size:13px}
  </style>
</head>
<body>
<main class="page">
  <div class="head">
    <div class="head-main"><a class="back" href="/">←</a><div><div class="title">Codex 详情</div><div class="sid">{{.Conversation.SessionID}}</div></div></div>
    <div class="mode-switch" role="tablist" aria-label="展示模式">
      <button type="button" class="mode-btn" data-mode="detail" role="tab" aria-selected="false">详细</button>
      <button type="button" class="mode-btn active" data-mode="brief" role="tab" aria-selected="true">简略</button>
    </div>
  </div>
  <section class="bar"><div id="viewer-label">简略 Viewer</div><div><a href="/api/conversations/{{.Conversation.ID}}">导出 JSON</a>　复制会话 ID</div></section>
  <section class="notice" id="trace-count">正在展示 {{len .Traces}} / {{.Conversation.TraceCount}} 条记录。</section>
  <div id="session-context"></div>
  <div id="trace-list">
    <section class="turn">加载中...</section>
  </div>
</main>
<script>
const conversationID = {{.Conversation.ID}};
const initialData = {conversation: {{toJSON .Conversation}}, traces: {{toJSON .Traces}}};
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const pretty = v => {
  if (!v) return '';
  try { return JSON.stringify(JSON.parse(v), null, 2); } catch (_) { return String(v); }
};
let viewMode = 'brief';
let latestData = initialData;
const parseJSON = v => {
  if (!v) return null;
  if (typeof v === 'object') return v;
  try { return JSON.parse(v); } catch (_) { return null; }
};
const stringify = v => {
  if (v === undefined || v === null) return '';
  if (typeof v === 'string') return v;
  try { return JSON.stringify(v, null, 2); } catch (_) { return String(v); }
};
function contentText(content){
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return stringify(content);
  return content.map(part => {
    if (typeof part === 'string') return part;
    if (!part || typeof part !== 'object') return '';
    if (['text','input_text','output_text'].includes(part.type) || typeof part.text === 'string') return part.text || '';
    if (part.type === 'tool_use') return '工具调用 ' + (part.name || part.id || '') + '\n' + stringify(part.input || {});
    if (part.type === 'tool_result') return '工具结果 ' + (part.tool_use_id || part.id || '') + '\n' + stringify(part.content || '');
    if (part.type && part.type.endsWith('_call')) return '工具调用 ' + (part.name || part.type) + '\n' + stringify(part.arguments || part);
    if (part.type && part.type.endsWith('_call_output')) return '工具结果 ' + (part.call_id || part.type) + '\n' + stringify(part.output || part);
    return stringify(part);
  }).filter(Boolean).join('\n');
}
function requestMessages(body){
  if (!body || typeof body !== 'object') return [];
  const source = Array.isArray(body.input) ? body.input : Array.isArray(body.messages) ? body.messages : [];
  return source.map(item => {
    if (!item || typeof item !== 'object') return null;
    if (item.type && item.type !== 'message' && !item.role) {
      return {role: item.type, text: contentText(item.content || item.output || item.text || item)};
    }
    return {role: item.role || 'user', text: contentText(item.content)};
  }).filter(item => item && item.text.trim());
}
function shortText(text, max = 900){
  text = String(text || '').trim();
  return text.length > max ? text.slice(0, max) + '\n...' : text;
}
function toolName(tool){
  if (!tool || typeof tool !== 'object') return '';
  if (tool.name) return tool.name;
  if (tool.function && tool.function.name) return tool.function.name;
  if (tool.type && tool.type !== 'function') return tool.type;
  return '';
}
function toolDetails(body){
  return Array.isArray(body?.tools) ? body.tools.map(tool => {
    const params = tool.parameters || tool.function?.parameters || {};
    const properties = params && typeof params === 'object' && params.properties && typeof params.properties === 'object' ? params.properties : {};
    return {
      name: toolName(tool) || 'unknown',
      type: tool.type || 'function',
      description: tool.description || tool.function?.description || '',
      required: Array.isArray(params.required) ? params.required : [],
      properties
    };
  }) : [];
}
function skillDetails(messages){
  const skills = [];
  messages.forEach(m => {
    const text = String(m.text || '');
    for (const match of text.matchAll(/<skill>\s*([\s\S]*?)<\/skill>/g)) {
      const raw = match[1] || '';
      const name = (raw.match(/<name>([^<]+)<\/name>/) || [,''])[1].trim();
      const path = (raw.match(/<path>([^<]+)<\/path>/) || [,''])[1].trim();
      const desc = (raw.match(/description:\s*([^\n]+)/) || raw.match(/<description>([\s\S]*?)<\/description>/) || [,''])[1].trim();
      if (name || path || desc) skills.push({name: name || path || 'skill', path, description: desc, raw});
    }
    skills.push(...availableSkillDetails(text));
  });
  const seen = new Set();
  return skills.filter(skill => {
    const key = skill.name + '|' + skill.path;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}
function availableSkillDetails(text){
  const sectionMatch = String(text || '').match(/### Available skills\s*([\s\S]*?)(?:\n### |\n## |<\/skills_instructions>|$)/);
  if (!sectionMatch) return [];
  const lines = sectionMatch[1].split(/\r?\n/);
  const items = [];
  let current = null;
  const flush = () => {
    if (current) items.push(current);
    current = null;
  };
  lines.forEach(line => {
    const start = line.match(/^\s*-\s+([^:]+):\s*(.*)$/);
    if (start) {
      flush();
      current = {name: start[1].trim(), raw: line.trim(), parts: [start[2].trim()]};
      return;
    }
    if (current && line.trim()) {
      current.raw += '\n' + line.trim();
      current.parts.push(line.trim());
    }
  });
  flush();
  return items.map(item => {
    const joined = item.parts.join(' ').replace(/\s+/g, ' ').trim();
    const pathMatch = joined.match(/\((?:file|source|path):\s*([^)]+)\)\s*$/i);
    const path = pathMatch ? pathMatch[1].trim() : '';
    const description = (pathMatch ? joined.slice(0, pathMatch.index) : joined).trim();
    return {name: item.name, path, description, raw: item.raw};
  }).filter(item => item.name);
}
function chips(items){
  return items.map(name => '<span class="brief-chip">' + esc(name) + '</span>').join('');
}
function injectedSections(messages){
  const tags = [
    ['permissions', 'permissions instructions', '权限'],
    ['environment', 'environment_context', '环境'],
    ['app', 'app-context', 'App'],
    ['collaboration', 'collaboration_mode', '协作'],
    ['skills', 'skills_instructions', '技能规则'],
    ['plugins', 'plugins_instructions', '插件规则'],
  ];
  const sections = [];
  messages.forEach(m => {
    const text = String(m.text || '');
    tags.forEach(([id, tag, label]) => {
      const start = '<' + tag + '>';
      const end = '</' + tag + '>';
      let pos = 0;
      while (true) {
        const i = text.indexOf(start, pos);
        if (i < 0) break;
        const j = text.indexOf(end, i + start.length);
        const content = j >= 0 ? text.slice(i + start.length, j) : text.slice(i + start.length);
        sections.push({id, label, role: m.role, content: content.trim()});
        pos = j >= 0 ? j + end.length : text.length;
      }
    });
  });
  return sections;
}
function mediaDetails(body){
  const out = [];
  const source = Array.isArray(body?.input) ? body.input : [];
  source.forEach((item, idx) => {
    const content = Array.isArray(item?.content) ? item.content : [];
    content.forEach((part, partIdx) => {
      if (part && typeof part === 'object' && (part.type === 'input_image' || part.type === 'image')) {
        const imageURL = typeof part.image_url === 'string' ? part.image_url : part.image_url?.url || part.source?.url || '';
        out.push({role: item.role || 'user', index: idx + 1, part: partIdx + 1, type: part.type, detail: part.detail || '', size: imageURL ? imageURL.length : 0});
      }
    });
  });
  return out;
}
function runtimeRows(body, t){
  const rows = [
    ['model', body?.model || t.Model || ''],
    ['tool_choice', stringify(body?.tool_choice)],
    ['parallel_tool_calls', body?.parallel_tool_calls],
    ['reasoning', stringify(body?.reasoning)],
    ['stream', body?.stream],
    ['store', body?.store],
    ['include', stringify(body?.include)],
    ['prompt_cache_key', body?.prompt_cache_key],
    ['text', stringify(body?.text)],
    ['request_bytes', t.RequestBytes || 0],
    ['response_bytes', t.ResponseBytes || 0],
  ];
  return rows.filter(([, value]) => value !== undefined && value !== null && String(value) !== '');
}
function renderKV(rows){
  if (!rows.length) return '<div class="empty-text">没有捕获到这类信息。</div>';
  return '<div class="ctx-list">' + rows.map(([k, v]) => '<div class="ctx-kv"><div class="ctx-key">' + esc(k) + '</div><div class="ctx-val">' + esc(String(v)) + '</div></div>').join('') + '</div>';
}
function renderToolCards(tools){
  if (!tools.length) return '<div class="empty-text">没有声明 tools。</div>';
  return '<div class="ctx-list">' + tools.map(tool => {
    const props = Object.entries(tool.properties || {});
    const params = props.map(([name, spec]) => '<span class="ctx-param">' + esc(name) + (tool.required.includes(name) ? ' *' : '') + (spec?.type ? ': ' + esc(spec.type) : '') + '</span>').join('');
    return '<div class="ctx-card"><div class="ctx-card-title">' + esc(tool.name) + '<span class="brief-chip">' + esc(tool.type) + '</span></div>' +
      (tool.description ? '<div class="ctx-card-desc">' + esc(tool.description) + '</div>' : '') +
      (params ? '<div>' + params + '</div>' : '') +
    '</div>';
  }).join('') + '</div>';
}
function renderSkillCards(skills){
  if (!skills.length) return '<div class="empty-text">没有注入 skill。</div>';
  return '<div class="ctx-list">' + skills.map(skill => '<div class="ctx-card"><div class="ctx-card-title">' + esc(skill.name) + '</div>' +
    (skill.path ? '<div class="ctx-card-sub">' + esc(skill.path) + '</div>' : '') +
    (skill.description ? '<div class="ctx-card-desc">' + esc(skill.description) + '</div>' : '') +
    '<div class="ctx-pre">' + esc(shortText(skill.raw, 1200)) + '</div></div>').join('') + '</div>';
}
function renderSectionCards(sections){
  if (!sections.length) return '<div class="empty-text">没有捕获到上下文注入块。</div>';
  return '<div class="ctx-list">' + sections.map(sec => '<div class="ctx-card"><div class="ctx-card-title">' + esc(sec.label) + '<span class="brief-chip">' + esc(sec.role) + '</span></div><div class="ctx-pre">' + esc(shortText(sec.content, 1600)) + '</div></div>').join('') + '</div>';
}
function renderMediaCards(media){
  if (!media.length) return '<div class="empty-text">没有图片或媒体输入。</div>';
  return '<div class="ctx-list">' + media.map(item => '<div class="ctx-card"><div class="ctx-card-title">' + esc(item.type) + '<span class="brief-chip">' + esc(item.role) + '</span></div><div class="ctx-card-desc">input #' + item.index + ', part #' + item.part + (item.detail ? ', detail ' + esc(item.detail) : '') + ', data ' + item.size.toLocaleString() + ' chars</div></div>').join('') + '</div>';
}
function renderSystemInstructions(body){
  const text = String(body?.instructions || '').trim();
  if (!text) return '<div class="empty-text">没有顶层 instructions。</div>';
  return '<div class="ctx-card"><div class="ctx-card-title">instructions<span class="brief-chip">' + esc(text.length.toLocaleString()) + ' chars</span></div><div class="ctx-pre">' + esc(shortText(text, 4000)) + '</div></div>';
}
function renderContextInspector(t, body){
  const messages = requestMessages(body);
  const tools = toolDetails(body);
  const mcpTools = tools.filter(tool => /mcp|resource|template/i.test(tool.name + ' ' + tool.description));
  const skills = skillDetails(messages);
  const sections = injectedSections(messages);
  const media = mediaDetails(body);
  const tabs = [
    ['overview', '概览', '<div class="ctx-summary">' +
      [['系统', body?.instructions ? String(body.instructions).length.toLocaleString() + ' chars' : '0'], ['消息', messages.length], ['Tools', tools.length], ['Skills', skills.length], ['MCP', mcpTools.length], ['上下文', sections.length], ['媒体', media.length]]
        .map(([label, value]) => '<div class="ctx-stat"><div class="ctx-stat-label">' + esc(label) + '</div><div class="ctx-stat-value">' + esc(value) + '</div></div>').join('') +
      '</div>'],
    ['system', '系统', renderSystemInstructions(body)],
    ['tools', 'Tools ' + tools.length, renderToolCards(tools)],
    ['skills', 'Skills ' + skills.length, renderSkillCards(skills)],
    ['mcp', 'MCP ' + mcpTools.length, renderToolCards(mcpTools)],
    ['context', '上下文 ' + sections.length, renderSectionCards(sections)],
    ['runtime', '运行参数', renderKV(runtimeRows(body, t))],
    ['media', '媒体 ' + media.length, renderMediaCards(media)],
  ];
  return '<div class="ctx" data-active-tab="overview"><div class="ctx-tabs">' +
    tabs.map(([id, label], idx) => '<button type="button" class="ctx-tab ' + (idx === 0 ? 'active' : '') + '" data-ctx-tab="' + id + '">' + esc(label) + '</button>').join('') +
    '</div>' + tabs.map(([id, , html], idx) => '<div class="ctx-panel ' + (idx === 0 ? 'active' : '') + '" data-ctx-panel="' + id + '">' + html + '</div>').join('') + '</div>';
}
function renderRoleLines(messages){
  if (!messages.length) return '<div class="empty-text">请求体里没有面向用户的消息。</div>';
  return messages.map(m => '<p class="role-line"><span class="role-name">' + esc(m.role) + '</span>' + esc(m.text) + '</p>').join('');
}
function sseEvents(t){
  const parsed = parseJSON(t.SSEEvents);
  return Array.isArray(parsed) ? parsed : [];
}
function eventData(ev){
  if (!ev || typeof ev !== 'object') return null;
  let data = Object.prototype.hasOwnProperty.call(ev, 'data') ? ev.data : ev;
  if (typeof data === 'string') data = parseJSON(data);
  return data && typeof data === 'object' ? data : null;
}
function normalizeOutputItems(items){
  if (!Array.isArray(items)) return [];
  const blocks = [];
  items.forEach(item => {
    if (!item || typeof item !== 'object') return;
    if (item.type === 'message' && Array.isArray(item.content)) {
      item.content.forEach(part => {
        const text = contentText([part]).trim();
        if (text) blocks.push(text);
      });
      return;
    }
    if (item.type === 'reasoning' && item.summary) {
      const text = Array.isArray(item.summary) ? item.summary.map(s => s && s.text || '').join('\n') : contentText(item.summary);
      if (text.trim()) blocks.push('[thinking]\n' + text);
      return;
    }
    if (item.type && item.type.endsWith('_call')) blocks.push('工具调用 ' + (item.name || item.type) + '\n' + stringify(item.arguments || item));
  });
  return blocks;
}
function responseText(t){
  const body = parseJSON(t.ResponseBody);
  const blocks = [];
  if (body && typeof body === 'object') {
    if (Array.isArray(body.output)) blocks.push(...normalizeOutputItems(body.output));
    if (Array.isArray(body.content)) blocks.push(contentText(body.content));
    if (body.error) blocks.push('错误\n' + stringify(body.error));
  }
  if (!blocks.length) {
    const done = sseEvents(t).filter(ev => ev.Event === 'response.completed' || ev.event === 'response.completed').pop();
    const data = eventData(done);
    if (Array.isArray(data?.response?.output)) blocks.push(...normalizeOutputItems(data.response.output));
  }
  if (!blocks.length) {
    const outputItems = sseEvents(t)
      .filter(ev => ev.Event === 'response.output_item.done' || ev.event === 'response.output_item.done')
      .map(eventData)
      .filter(data => data && data.item)
      .sort((a, b) => (a.output_index || 0) - (b.output_index || 0))
      .map(data => data.item);
    blocks.push(...normalizeOutputItems(outputItems));
  }
  const text = blocks.filter(Boolean).join('\n\n').trim();
  return text || '响应体里没有 assistant 文本。';
}
function briefTraceHTML(t){
  const body = parseJSON(t.RequestBody);
  const errorBox = t.Error ? '<div class="box err"><span class="tag">错误</span><pre>' + esc(t.Error) + '</pre></div>' : '';
  const messages = requestMessages(body);
  return '<section class="turn">' +
    '<div class="turn-head">▸ Turn ' + (t.SequenceNo || 0) + ' <span class="meta">' + (t.Status || 0) + ' ' + esc(t.Path) + '</span><a class="json-btn" href="/api/conversations/' + conversationID + '">JSON</a></div>' +
    '<div class="brief-grid">' +
      errorBox +
      '<div class="brief-box req"><span class="tag">请求</span><div class="brief-text">' + renderRoleLines(messages) + '</div></div>' +
      '<div class="brief-box res"><span class="tag">响应</span><div class="brief-text">' + esc(responseText(t)) + '</div></div>' +
    '</div>' +
  '</section>';
}
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
function syncModeUI(){
  document.getElementById('viewer-label').textContent = viewMode === 'brief' ? '简略 Viewer' : '详细 Viewer';
  document.querySelectorAll('.mode-btn').forEach(btn => {
    const active = btn.dataset.mode === viewMode;
    btn.classList.toggle('active', active);
    btn.setAttribute('aria-selected', active ? 'true' : 'false');
  });
}
function renderTraces(traces){
  syncModeUI();
  const context = document.getElementById('session-context');
  if (context) {
    const first = (traces || [])[0];
    context.innerHTML = viewMode === 'brief' && first ? renderContextInspector(first, parseJSON(first.RequestBody)) : '';
  }
  const renderer = viewMode === 'brief' ? briefTraceHTML : traceHTML;
  document.getElementById('trace-list').innerHTML = (traces || []).map(renderer).join('') || '<section class="turn">暂无 trace。</section>';
}
document.querySelectorAll('.mode-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    viewMode = btn.dataset.mode || 'detail';
    if (latestData) renderTraces(latestData.traces || []);
    else refreshDetail(true).catch(() => {});
  });
});
document.addEventListener('click', event => {
  const tab = event.target.closest('.ctx-tab');
  if (!tab) return;
  const root = tab.closest('.ctx');
  if (!root) return;
  const name = tab.dataset.ctxTab;
  root.querySelectorAll('.ctx-tab').forEach(item => item.classList.toggle('active', item === tab));
  root.querySelectorAll('.ctx-panel').forEach(panel => panel.classList.toggle('active', panel.dataset.ctxPanel === name));
});
let lastPayload = '';
async function refreshDetail(forceRender){
  const rsp = await fetch('/api/conversations/' + conversationID, {cache:'no-store'});
  if (!rsp.ok) return;
  const data = await rsp.json();
  latestData = data;
  const payload = JSON.stringify(data);
  if (payload === lastPayload && !forceRender) return;
  lastPayload = payload;
  document.getElementById('trace-count').textContent = '正在展示 ' + (data.traces || []).length + ' / ' + ((data.conversation || {}).TraceCount || 0) + ' 条记录。';
  renderTraces(data.traces || []);
}
lastPayload = JSON.stringify(initialData);
renderTraces(initialData.traces || []);
setInterval(() => refreshDetail().catch(() => {}), 1000);
</script>
</body>
</html>
{{end}}
`
