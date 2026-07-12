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
}).Parse(indexHTML + detailHTML + routesHTML + chainsHTML))

func (p *proxyServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	month := strings.TrimSpace(r.URL.Query().Get("month"))
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
	const pageSize = 10
	conversations, err := p.store.ListConversationsPage(r.Context(), query, status, month, date, agent, accountID, pageSize+1, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hasMore := len(conversations) > pageSize
	if hasMore {
		conversations = conversations[:pageSize]
	}
	subagentLinks, err := p.store.SubagentLinks(r.Context(), conversations)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filterOptions, err := p.store.FilterOptions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	conversationCount, traceCount, inputTokens, outputTokens, cachedTokens, err := p.store.Stats(r.Context(), query, status, month, date, agent, accountID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Now":               time.Now(),
		"Query":             query,
		"Status":            status,
		"Month":             month,
		"Date":              date,
		"Agent":             agent,
		"AccountID":         accountID,
		"FilterOptions":     filterOptions,
		"Conversations":     conversations,
		"SubagentLinks":     subagentLinks,
		"HasMore":           hasMore,
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
	limit, offset := paginationParams(r)
	conversations, err := p.store.ListConversationsPage(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("status"), r.URL.Query().Get("month"), r.URL.Query().Get("date"), r.URL.Query().Get("agent"), r.URL.Query().Get("account_id"), limit, offset)
	writeJSON(w, conversations, err)
}

func (p *proxyServer) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	status := r.URL.Query().Get("status")
	month := r.URL.Query().Get("month")
	date := r.URL.Query().Get("date")
	agent := r.URL.Query().Get("agent")
	accountID := r.URL.Query().Get("account_id")
	limit, offset := paginationParams(r)
	conversations, err := p.store.ListConversationsPage(r.Context(), query, status, month, date, agent, accountID, limit+1, offset)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	hasMore := len(conversations) > limit
	if hasMore {
		conversations = conversations[:limit]
	}
	subagentLinks, err := p.store.SubagentLinks(r.Context(), conversations)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	filterOptions, err := p.store.FilterOptions(r.Context())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	conversationCount, traceCount, inputTokens, outputTokens, cachedTokens, err := p.store.Stats(r.Context(), query, status, month, date, agent, accountID)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{
		"now":                time.Now(),
		"conversations":      conversations,
		"subagent_links":     subagentLinks,
		"limit":              limit,
		"offset":             offset,
		"has_more":           hasMore,
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

func paginationParams(r *http.Request) (limit, offset int) {
	limit = 10
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 100 {
		limit = 100
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
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

func (p *proxyServer) handleRoutes(w http.ResponseWriter, r *http.Request) {
	if err := baseTemplate.ExecuteTemplate(w, "routes", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *proxyServer) handleChains(w http.ResponseWriter, r *http.Request) {
	if err := baseTemplate.ExecuteTemplate(w, "chains", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *proxyServer) handleAPIRoutesList(w http.ResponseWriter, r *http.Request) {
	routes, err := p.store.ListAPIRoutes(r.Context())
	writeJSON(w, routes, err)
}

func (p *proxyServer) handleAPIRouteCreate(w http.ResponseWriter, r *http.Request) {
	var route APIRoute
	if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id, err := p.store.CreateAPIRoute(r.Context(), route)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id}, nil)
}

func (p *proxyServer) handleAPIRouteUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad route id", http.StatusBadRequest)
		return
	}
	var route APIRoute
	if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := p.store.UpdateAPIRoute(r.Context(), id, route); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "route not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func (p *proxyServer) handleAPIRouteDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad route id", http.StatusBadRequest)
		return
	}
	if err := p.store.DeleteAPIRoute(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "route not found", http.StatusNotFound)
			return
		}
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func (p *proxyServer) handleAPIRouteToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad route id", http.StatusBadRequest)
		return
	}
	enabled, err := p.store.ToggleAPIRoute(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "route not found", http.StatusNotFound)
			return
		}
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "enabled": enabled}, nil)
}

func (p *proxyServer) handleAPIChainsList(w http.ResponseWriter, r *http.Request) {
	chains, err := p.store.ListChainProxies(r.Context())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	routes, err := p.store.ListAPIRoutes(r.Context())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"chains": chains, "routes": routes}, nil)
}

func (p *proxyServer) handleAPIChainCreate(w http.ResponseWriter, r *http.Request) {
	var chain ChainProxy
	if err := json.NewDecoder(r.Body).Decode(&chain); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id, err := p.store.CreateChainProxy(r.Context(), chain)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id}, nil)
}

func (p *proxyServer) handleAPIChainUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad chain id", http.StatusBadRequest)
		return
	}
	var chain ChainProxy
	if err := json.NewDecoder(r.Body).Decode(&chain); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := p.store.UpdateChainProxy(r.Context(), id, chain); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "chain not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func (p *proxyServer) handleAPIChainToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad chain id", http.StatusBadRequest)
		return
	}
	enabled, err := p.store.ToggleChainProxy(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "chain not found", http.StatusNotFound)
			return
		}
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "enabled": enabled}, nil)
}

func (p *proxyServer) handleAPIChainDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad chain id", http.StatusBadRequest)
		return
	}
	if err := p.store.DeleteChainProxy(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "chain not found", http.StatusNotFound)
			return
		}
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
    .app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;align-self:flex-start;height:100vh;transition:width .16s ease,padding .16s ease}
    .sidebar .brand-row{display:flex;align-items:center;gap:8px;padding:6px 2px 18px 10px}.sidebar .brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;flex:1;white-space:nowrap;overflow:hidden}
    .sidebar-toggle{width:30px;height:30px;padding:0;border-radius:7px;border:1px solid var(--line);background:#fff;color:#667284;font-weight:800}
    .app.sidebar-collapsed .sidebar{width:58px;padding-left:10px;padding-right:10px}.app.sidebar-collapsed .brand{display:none}.app.sidebar-collapsed .nav-item{padding:10px 0;text-align:center;font-size:0}.app.sidebar-collapsed .nav-item::first-letter{font-size:15px}.app.sidebar-collapsed .sidebar-toggle{transform:rotate(180deg)}
    .nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px}
    .nav-item:hover{background:#eef2fa}
    .nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:22px 28px 56px}.top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}.clock{color:#d95252;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
    .filters{display:grid;grid-template-columns:150px 150px 150px 150px 190px 1fr auto;gap:12px;margin-top:16px}
    select,input,button{height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 12px;font:inherit;color:var(--text)}
    button{cursor:pointer}.stats,.table{background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05)}
    .stats{display:flex;gap:36px;padding:22px 26px;margin-top:16px}.stat-label{font-size:12px;font-weight:600;color:var(--muted);letter-spacing:.02em}.stat-value{font-size:28px;font-weight:600;margin-top:4px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
    .table{margin-top:18px;overflow-x:auto}table{width:100%;min-width:1360px;border-collapse:collapse;table-layout:fixed}th,td{padding:14px 14px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle}th{font-size:12px;color:#606a7a;font-weight:600;letter-spacing:.04em;background:#fbfcfe}tr:last-child td{border-bottom:0}tbody tr.row-link{cursor:pointer}tbody tr.row-link:hover{background:#fafcff}tbody tr.child-row{background:#fbf9ff}tbody tr.child-row:hover{background:#f7f3ff}
    th:nth-child(1),td:nth-child(1){width:42px}th:nth-child(2),td:nth-child(2){width:128px}th:nth-child(3),td:nth-child(3){width:285px}th:nth-child(4),td:nth-child(4){width:96px}th:nth-child(5),td:nth-child(5){width:90px}th:nth-child(6),td:nth-child(6){width:58px}th:nth-child(7),td:nth-child(7){width:170px}th:nth-child(8),td:nth-child(8){width:86px}th:nth-child(9),td:nth-child(9){width:90px}th:nth-child(10),td:nth-child(10){width:70px}th:nth-child(11),td:nth-child(11){width:74px}th:nth-child(12),td:nth-child(12){width:82px}
    .prompt{font-size:14px;color:#242832}.sid{display:block;margin-top:4px;color:#8a93a3;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}
    .account-btn,.tag-btn{height:auto;max-width:104px;padding:3px 9px;border-radius:6px;border:1px solid #b8e2df;background:#edfafa;color:#0f766e;text-align:left;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;box-shadow:none;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px;font-weight:600}.account-btn:hover,.tag-btn:hover{background:#e2f7f4;border-color:#81cfc7;color:#0b5d56}.tag-btn{max-width:82px;border-color:#d8e0eb;background:#f7f9fc;color:#526074;font-family:inherit}
    .pill{display:inline-block;border-radius:6px;padding:3px 9px;font-size:13px;font-weight:600;white-space:nowrap}.model{color:var(--purple);background:#f4f1ff;border:1px solid #ddd4ff}.agent{color:#2469e8;background:#edf4ff;border:1px solid #c6d9ff}.ok{color:var(--green);background:#ecfbf2;border:1px solid #a9e2bf}.live{color:#fff;background:#f5821f;border:1px solid #f5821f}.error{color:var(--red);background:#fff1f1;border:1px solid #f0b3b3}.token{color:var(--orange);background:#fff8ee;border:1px solid #ffd09a;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.token-stack{display:inline-grid;gap:2px;line-height:1.35;min-width:92px;text-align:right}.tree-cell{padding-left:10px;padding-right:4px}.tree-toggle{width:26px;height:26px;padding:0;border-radius:6px;border:1px solid #d8e0eb;background:#fff;color:#667284;font-weight:800;line-height:1}.tree-toggle:hover{border-color:#b8c7dc;background:#f7f9fc}.child-mark{display:inline-block;color:#8a7ad8;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-weight:700}
    a{color:var(--blue);text-decoration:none}.small{font-size:12px;color:#8a93a3}.action{display:inline-flex;align-items:center;justify-content:center;min-width:48px;height:30px;border:1px solid var(--line);padding:0 10px;border-radius:7px;background:#fff;font-weight:500;white-space:nowrap}.delete-btn{color:var(--red);border-color:#f0b3b3;background:#fff6f6}.delete-btn:hover{background:#fff1f1;border-color:#e07d7d}.load-status{padding:12px 16px;text-align:center;color:#7b8494;border-top:1px solid var(--line);background:#fbfcfe}
  </style>
</head>
<body>
<div class="app">
  <aside class="sidebar">
    <div class="brand-row"><div class="brand">AGENT-GO-PROXY</div><button class="sidebar-toggle" id="sidebar-toggle" type="button" title="收起/展开侧栏">‹</button></div>
    <nav>
      <a class="nav-item active" href="/">Dashboard</a>
      <a class="nav-item" href="/routes">路由</a>
      <a class="nav-item" href="/chains">链式代理</a>
    </nav>
  </aside>
  <main class="page">
  <div class="top"><h1>对话日志</h1><div class="clock" id="clock">{{fmtTime .Now}}</div></div>
  <form class="filters" method="get">
    <select name="month" id="month-filter">
      <option value="all">全部月份</option>
      {{range .FilterOptions.Months}}<option value="{{.}}" {{if eq $.Month .}}selected{{end}}>{{.}}</option>{{end}}
    </select>
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
      <thead><tr><th></th><th>时间</th><th>首条用户 PROMPT</th><th>账号</th><th>标签</th><th>TRACE</th><th>Token<br>输入/输出/缓存</th><th>模型</th><th>AGENT</th><th>耗时(分)</th><th>状态</th><th>操作</th></tr></thead>
      <tbody id="conversation-rows">
      {{range .Conversations}}
        <tr class="row-link" data-href="/conversations/{{.ID}}">
          <td class="tree-cell"></td>
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
        <tr><td colspan="12">暂无数据。发起一次 Codex 请求后这里会出现新会话。</td></tr>
      {{end}}
      </tbody>
    </table>
    <div class="load-status" id="load-status">{{if .HasMore}}向下滚动加载更多{{else}}已加载全部记录{{end}}</div>
  </section>
  </main>
</div>
<script>
const initialConversations = {{toJSON .Conversations}};
const initialSubagentLinks = {{toJSON .SubagentLinks}};
const pageSize = 10;
let nextOffset = (initialConversations || []).length;
let hasMore = {{if .HasMore}}true{{else}}false{{end}};
let loadingMore = false;
const appEl = document.querySelector('.app');
if (localStorage.getItem('sidebarCollapsed') === '1') appEl.classList.add('sidebar-collapsed');
document.getElementById('sidebar-toggle').addEventListener('click', () => {
  appEl.classList.toggle('sidebar-collapsed');
  localStorage.setItem('sidebarCollapsed', appEl.classList.contains('sidebar-collapsed') ? '1' : '0');
});
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
let latestConversations = initialConversations || [];
let latestSubagentLinks = initialSubagentLinks || {};
const expandedParents = new Set();
const fmtMinutes = v => {
  v = Math.max(0, Number(v || 0));
  return v >= 10 ? Math.round(v).toString() : v.toFixed(1);
};
function statusClass(status){
  if (status === 'LIVE') return 'live';
  if (status === 'ERROR') return 'error';
  return 'ok';
}
function childSessionSet(links){
  const out = new Set();
  Object.values(links || {}).forEach(children => (children || []).forEach(id => out.add(id)));
  return out;
}
function conversationRowHTML(item, opts = {}){
  const children = opts.children || [];
  const isChild = !!opts.isChild;
  const missing = !!opts.missing;
  const expanded = expandedParents.has(item.SessionID);
  const tree = children.length
    ? '<button class="tree-toggle" type="button" data-parent-session="' + esc(item.SessionID || '') + '" aria-expanded="' + (expanded ? 'true' : 'false') + '">' + (expanded ? '▾' : '▸') + '</button>'
    : isChild ? '<span class="child-mark">└</span>' : '';
  return '<tr class="' + (missing ? '' : 'row-link ') + (isChild ? 'child-row' : '') + '"' + (missing ? '' : ' data-href="/conversations/' + item.ID + '"') + '>' +
    '<td class="tree-cell">' + tree + '</td>' +
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
    '<td>' + (missing ? '<span class="small">等待入库</span>' : '<button class="action delete-btn" data-delete-id="' + item.ID + '" data-session-id="' + esc(item.SessionID || '') + '">删除</button>') + '</td>' +
  '</tr>';
}
function missingSubagentRow(sessionID){
  return {
    ID: '',
    SessionID: sessionID,
    UpdatedAt: '',
    FirstPrompt: 'Subagent 已启动，暂未捕获到会话请求。',
    AccountID: '',
    AccountName: '',
    Tags: '',
    TraceCount: 0,
    InputTokens: 0,
    OutputTokens: 0,
    CachedTokens: 0,
    Model: 'unknown',
    Agent: 'Codex',
    DurationMin: 0,
    Status: 'LIVE'
  };
}
function renderConversationRows(conversations, links){
  latestConversations = conversations || [];
  latestSubagentLinks = links || {};
  const rows = document.getElementById('conversation-rows');
  const bySession = new Map(latestConversations.map(item => [item.SessionID, item]));
  const children = childSessionSet(latestSubagentLinks);
  const html = [];
  latestConversations.forEach(item => {
    if (children.has(item.SessionID)) return;
    const childIDs = latestSubagentLinks[item.SessionID] || [];
    html.push(conversationRowHTML(item, {children: childIDs}));
    if (expandedParents.has(item.SessionID)) {
      childIDs.forEach(id => {
        const child = bySession.get(id);
        html.push(conversationRowHTML(child || missingSubagentRow(id), {isChild: true, missing: !child}));
      });
    }
  });
  rows.innerHTML = html.join('') || '<tr><td colspan="12">暂无数据。发起一次 Codex 请求后这里会出现新会话。</td></tr>';
  updateLoadStatus();
}
function updateLoadStatus(){
  const el = document.getElementById('load-status');
  if (!el) return;
  el.textContent = loadingMore ? '加载中...' : (hasMore ? '向下滚动加载更多' : '已加载全部记录');
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
  syncSelectOptions(document.getElementById('month-filter'), '全部月份', options.Months || [], v => v, v => v);
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
  const message = '确认删除这个会话吗？\\n\\n' + (sessionID ? ('Session: ' + sessionID + '\\n') : '') + '会话、所有 trace，以及它启动的 subagent 会话都会被删除。';
  if (!confirm(message)) return;
  const rsp = await fetch('/api/conversations/' + encodeURIComponent(conversationID), {method: 'DELETE'});
  if (!rsp.ok) {
    alert('删除失败：' + await rsp.text());
    return;
  }
  refreshDashboard().catch(() => {});
}
document.getElementById('conversation-rows').addEventListener('click', event => {
  const treeButton = event.target.closest('.tree-toggle');
  if (treeButton) {
    event.preventDefault();
    event.stopPropagation();
    const session = treeButton.dataset.parentSession || '';
    if (expandedParents.has(session)) expandedParents.delete(session);
    else expandedParents.add(session);
    renderConversationRows(latestConversations, latestSubagentLinks);
    return;
  }
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
  await fetchDashboardPage({offset: 0, replace: true});
}
function dashboardQuery(offset){
  const form = document.querySelector('.filters');
  const qs = new URLSearchParams(new FormData(form));
  qs.set('limit', pageSize);
  qs.set('offset', offset);
  return qs;
}
function mergeFirstPage(firstPage){
  const firstIDs = new Set((firstPage || []).map(item => item.ID));
  const rest = latestConversations.filter(item => !firstIDs.has(item.ID));
  return (firstPage || []).concat(rest);
}
async function fetchDashboardPage({offset, replace}){
  const qs = dashboardQuery(offset);
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
  const page = data.conversations || [];
  hasMore = !!data.has_more;
  if (replace) {
    latestConversations = mergeFirstPage(page);
    nextOffset = Math.max(pageSize, latestConversations.length);
    renderConversationRows(latestConversations, Object.assign({}, latestSubagentLinks, data.subagent_links || {}));
  } else {
    const seen = new Set(latestConversations.map(item => item.ID));
    const fresh = page.filter(item => !seen.has(item.ID));
    latestConversations = latestConversations.concat(fresh);
    latestSubagentLinks = Object.assign({}, latestSubagentLinks, data.subagent_links || {});
    nextOffset += page.length;
    renderConversationRows(latestConversations, latestSubagentLinks);
  }
}
async function loadMore(){
  if (!hasMore || loadingMore) return;
  loadingMore = true;
  updateLoadStatus();
  try {
    await fetchDashboardPage({offset: nextOffset, replace: false});
  } finally {
    loadingMore = false;
    updateLoadStatus();
  }
}
renderConversationRows(initialConversations || [], initialSubagentLinks || {});
window.addEventListener('scroll', () => {
  if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 260) {
    loadMore().catch(() => {});
  }
});
setInterval(() => refreshDashboard().catch(() => {}), 3000);
</script>
</body>
</html>
{{end}}
`

const chainsHTML = `
{{define "chains"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>链式代理 · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--red:#c94040;--purple:#6750d8}
    *{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
    .app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;align-self:flex-start;height:100vh;transition:width .16s ease,padding .16s ease}
    .sidebar .brand-row{display:flex;align-items:center;gap:8px;padding:6px 2px 18px 10px}.sidebar .brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;flex:1;white-space:nowrap;overflow:hidden}
    .sidebar-toggle{width:30px;height:30px;padding:0;border-radius:7px;border:1px solid var(--line);background:#fff;color:#667284;font-weight:800}
    .app.sidebar-collapsed .sidebar{width:58px;padding-left:10px;padding-right:10px}.app.sidebar-collapsed .brand{display:none}.app.sidebar-collapsed .nav-item{padding:10px 0;text-align:center;font-size:0}.app.sidebar-collapsed .nav-item::first-letter{font-size:15px}.app.sidebar-collapsed .sidebar-toggle{transform:rotate(180deg)}
    .nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px}
    .nav-item:hover{background:#eef2fa}
    .nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:22px 28px 56px}
    .top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}
    button{cursor:pointer;height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 14px;font:inherit;color:var(--text)}
    .btn-primary{background:var(--blue);border-color:var(--blue);color:#fff;font-weight:600}.btn-primary:hover{background:#265fd0}
    .table{margin-top:18px;background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05);overflow-x:auto}
    table{width:100%;min-width:900px;border-collapse:collapse}th,td{padding:14px 16px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle}th{font-size:12px;color:#606a7a;font-weight:600;letter-spacing:.04em;background:#fbfcfe}tr:last-child td{border-bottom:0}
    .muted{color:var(--muted)}.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}
    .tag{display:inline-block;border-radius:6px;padding:3px 9px;font-size:12px;font-weight:600;color:#2469e8;background:#edf4ff;border:1px solid #c6d9ff}.pill{display:inline-block;border-radius:6px;padding:3px 9px;font-size:13px;font-weight:600;color:var(--purple);background:#f4f1ff;border:1px solid #ddd4ff}
    .route-stack{display:grid;gap:6px}.route-chip{display:inline-flex;align-items:center;gap:8px;max-width:100%;padding:5px 9px;border:1px solid #d8e0eb;border-radius:7px;background:#f8fafc}.order{display:inline-grid;place-items:center;width:22px;height:22px;border-radius:50%;background:#2f6fed;color:#fff;font-size:12px;font-weight:800}.route-name{font-weight:700}.route-model{color:#6b55d9}
    .action{display:inline-flex;align-items:center;justify-content:center;min-width:44px;height:30px;border:1px solid var(--line);padding:0 12px;border-radius:7px;background:#fff;font-weight:500}.action + .action{margin-left:6px}
    .delete-btn{color:var(--red);border-color:#f0b3b3;background:#fff6f6}.delete-btn:hover{background:#fff1f1;border-color:#e07d7d}
    .switch{display:inline-flex;align-items:center;justify-content:center;min-width:60px;height:30px;padding:0 16px;border-radius:16px;border:1px solid var(--line);background:#fff;color:var(--muted);font-weight:700;font-size:12px;letter-spacing:.06em;cursor:pointer}.switch.on{background:var(--green);border-color:var(--green);color:#fff}
    .overlay{display:none;position:fixed;inset:0;background:rgba(22,28,40,.4);align-items:center;justify-content:center;z-index:20}.overlay.show{display:flex}
    .modal{width:620px;max-width:calc(100vw - 32px);background:#fff;border-radius:12px;box-shadow:0 12px 40px rgba(20,30,50,.24);padding:24px}.modal h2{margin:0 0 18px;font-size:16px}
    .field{margin-bottom:14px}.field-row{display:flex;gap:12px}.field-row .field{flex:1}.field label{display:block;font-size:12px;font-weight:600;color:var(--muted);margin-bottom:6px}.field input,.field select{width:100%;height:40px;border:1px solid var(--line);border-radius:7px;padding:0 12px;font:inherit;color:var(--text);background:#fff}
    .route-picker{max-height:310px;overflow:auto;border:1px solid var(--line);border-radius:8px;background:#fbfcfe}.route-option{display:grid;grid-template-columns:34px 1fr auto;gap:10px;align-items:center;width:100%;height:auto;border:0;border-bottom:1px solid var(--line);border-radius:0;background:transparent;padding:12px 14px;text-align:left}.route-option:last-child{border-bottom:0}.route-option.selected{background:#eaf1ff}.rank{display:grid;place-items:center;width:26px;height:26px;border-radius:50%;border:1px solid #c7d7fb;color:#2f6fed;font-weight:800}.route-option:not(.selected) .rank{color:#a0a8b7;border-color:#d8e0eb}.route-meta{color:#697386;font-size:12px;margin-top:3px;word-break:break-all}.selected-list{min-height:28px;color:#596474}.selected-chip{display:inline-flex;align-items:center;gap:8px;margin:0 6px 6px 0}.selected-remove{display:inline-grid;place-items:center;width:20px;height:20px;padding:0;border:1px solid #cfd8e7;border-radius:50%;background:#fff;color:#7b8494;font-weight:800;line-height:1}.selected-remove:hover{border-color:#e07d7d;color:#c94040;background:#fff6f6}.empty{padding:38px 16px;text-align:center;color:var(--muted)}.err{color:var(--red);font-size:13px;min-height:18px;margin-top:4px}.modal-actions{display:flex;justify-content:flex-end;gap:10px;margin-top:22px}
  </style>
</head>
<body>
<div class="app">
  <aside class="sidebar">
    <div class="brand-row"><div class="brand">AGENT-GO-PROXY</div><button class="sidebar-toggle" id="sidebar-toggle" type="button" title="收起/展开侧栏">‹</button></div>
    <nav>
      <a class="nav-item" href="/">Dashboard</a>
      <a class="nav-item" href="/routes">路由</a>
      <a class="nav-item active" href="/chains">链式代理</a>
    </nav>
  </aside>
  <main class="page">
    <div class="top"><h1>链式代理</h1><button class="btn-primary" id="add-btn">+ 添加链式代理</button></div>
    <section class="table">
      <table>
        <thead><tr><th>名称</th><th>API</th><th>链路顺序</th><th style="width:90px">启用</th><th style="width:150px">操作</th></tr></thead>
        <tbody id="chain-rows"></tbody>
      </table>
      <div class="empty" id="empty-tip" style="display:none">还没有链式代理。点击右上角添加一个。</div>
    </section>
  </main>
</div>

<div class="overlay" id="modal-overlay">
  <div class="modal">
    <h2 id="modal-title">添加链式代理</h2>
    <div class="field-row">
      <div class="field"><label>名称</label><input id="f-name" placeholder="便于识别的名称，可留空"></div>
      <div class="field"><label>API 类型</label><select id="f-api-style"></select></div>
    </div>
    <div class="field">
      <label>选择路由</label>
      <div class="route-picker" id="route-picker"></div>
    </div>
    <div class="field">
      <label>当前顺序</label>
      <div class="selected-list" id="selected-list">未选择路由</div>
    </div>
    <div class="err" id="modal-err"></div>
    <div class="modal-actions">
      <button id="cancel-btn">取消</button>
      <button class="btn-primary" id="save-btn">保存</button>
    </div>
  </div>
</div>

<script>
const appEl = document.querySelector('.app');
if (localStorage.getItem('sidebarCollapsed') === '1') appEl.classList.add('sidebar-collapsed');
document.getElementById('sidebar-toggle').addEventListener('click', () => {
  appEl.classList.toggle('sidebar-collapsed');
  localStorage.setItem('sidebarCollapsed', appEl.classList.contains('sidebar-collapsed') ? '1' : '0');
});
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const STYLES = [{value:'openai', label:'OpenAI'}, {value:'anthropic', label:'Anthropic'}];
const styleLabel = v => (STYLES.find(s => s.value === v) || {}).label || v || '';
let chains = [];
let routes = [];
let editingId = null;
let selectedRouteIDs = [];
const overlay = document.getElementById('modal-overlay');
const errBox = document.getElementById('modal-err');
const styleSelect = document.getElementById('f-api-style');

function fillStyleSelect(){
  styleSelect.innerHTML = STYLES.map(s => '<option value="' + esc(s.value) + '">' + esc(s.label) + '</option>').join('');
}
function routeLabel(route){
  return route.name || route.model || route.base_url || ('Route #' + route.id);
}
function routesForStyle(style){
  return routes.filter(r => r.api_style === style);
}
function selectedRoutes(){
  const byID = new Map(routes.map(r => [String(r.id), r]));
  return selectedRouteIDs.map(id => byID.get(String(id))).filter(Boolean);
}
function renderRows(){
  const tbody = document.getElementById('chain-rows');
  const empty = document.getElementById('empty-tip');
  if (!chains.length){
    tbody.innerHTML = '';
    empty.style.display = 'block';
    return;
  }
  empty.style.display = 'none';
  tbody.innerHTML = chains.map(chain => {
    const routeHTML = (chain.routes || []).map((r, idx) =>
      '<span class="route-chip"><span class="order">' + (idx + 1) + '</span><span><span class="route-name">' + esc(routeLabel(r)) + '</span>' +
      (r.model ? ' <span class="route-model">' + esc(r.model) + '</span>' : '') +
      '<div class="route-meta">' + esc(r.base_url || '') + '</div></span></span>'
    ).join('');
    return '<tr>' +
      '<td>' + (chain.name ? esc(chain.name) : '<span class="muted">未命名</span>') + '</td>' +
      '<td><span class="tag">' + esc(styleLabel(chain.api_style)) + '</span></td>' +
      '<td><div class="route-stack">' + (routeHTML || '<span class="muted">无路由</span>') + '</div></td>' +
      '<td><button class="switch' + (chain.enabled ? ' on' : '') + '" data-toggle="' + chain.id + '">' + (chain.enabled ? 'ON' : 'OFF') + '</button></td>' +
      '<td><button class="action" data-edit="' + chain.id + '">编辑</button><button class="action delete-btn" data-delete="' + chain.id + '">删除</button></td>' +
    '</tr>';
  }).join('');
}
function renderPicker(){
  const picker = document.getElementById('route-picker');
  const style = styleSelect.value;
  const list = routesForStyle(style);
  if (!list.length) {
    picker.innerHTML = '<div class="empty">这个 API 类型下还没有路由配置。</div>';
    document.getElementById('selected-list').textContent = '未选择路由';
    return;
  }
  picker.innerHTML = list.map(route => {
    const idx = selectedRouteIDs.indexOf(route.id);
    const selected = idx >= 0;
    return '<button type="button" class="route-option' + (selected ? ' selected' : '') + '" data-route-id="' + route.id + '">' +
      '<span class="rank">' + (selected ? idx + 1 : '+') + '</span>' +
      '<span><strong>' + esc(routeLabel(route)) + '</strong><div class="route-meta">' + esc(route.base_url || '') + (route.model ? ' · ' + esc(route.model) : '') + '</div></span>' +
      '<span class="pill">' + esc(route.protocol || '-') + '</span>' +
    '</button>';
  }).join('');
  const picked = selectedRoutes();
  document.getElementById('selected-list').innerHTML = picked.length
    ? picked.map((r, idx) => '<span class="route-chip selected-chip"><span class="order">' + (idx + 1) + '</span>' + esc(routeLabel(r)) + '<button type="button" class="selected-remove" data-remove-route-id="' + r.id + '" title="移除">×</button></span>').join(' ')
    : '未选择路由';
}
async function loadData(){
  const rsp = await fetch('/api/chains', {cache:'no-store'});
  if (!rsp.ok) return;
  const data = await rsp.json();
  chains = data.chains || [];
  routes = data.routes || [];
  renderRows();
}
function openModal(chain){
  editingId = chain ? chain.id : null;
  document.getElementById('modal-title').textContent = chain ? '编辑链式代理' : '添加链式代理';
  document.getElementById('f-name').value = chain ? (chain.name || '') : '';
  fillStyleSelect();
  styleSelect.value = (chain && chain.api_style) || 'openai';
  selectedRouteIDs = chain ? (chain.route_ids || []).slice() : [];
  selectedRouteIDs = selectedRouteIDs.filter(id => routes.some(r => r.id === id && r.api_style === styleSelect.value));
  errBox.textContent = '';
  renderPicker();
  overlay.classList.add('show');
}
function closeModal(){
  overlay.classList.remove('show');
  editingId = null;
  selectedRouteIDs = [];
}
async function saveChain(){
  const payload = {
    name: document.getElementById('f-name').value.trim(),
    api_style: styleSelect.value,
    route_ids: selectedRouteIDs
  };
  if (!payload.route_ids.length){ errBox.textContent = '请至少选择一个路由'; return; }
  const url = editingId ? '/api/chains/' + editingId : '/api/chains';
  const method = editingId ? 'PUT' : 'POST';
  const rsp = await fetch(url, {method, headers:{'Content-Type':'application/json'}, body: JSON.stringify(payload)});
  if (!rsp.ok){ errBox.textContent = '保存失败：' + await rsp.text(); return; }
  closeModal();
  loadData().catch(() => {});
}
async function toggleChain(id){
  const rsp = await fetch('/api/chains/' + id + '/toggle', {method:'POST'});
  if (!rsp.ok){ alert('切换失败：' + await rsp.text()); return; }
  loadData().catch(() => {});
}
async function deleteChain(id){
  if (!confirm('确认删除这个链式代理吗？')) return;
  const rsp = await fetch('/api/chains/' + id, {method:'DELETE'});
  if (!rsp.ok){ alert('删除失败：' + await rsp.text()); return; }
  loadData().catch(() => {});
}
document.getElementById('add-btn').addEventListener('click', () => openModal(null));
document.getElementById('cancel-btn').addEventListener('click', closeModal);
document.getElementById('save-btn').addEventListener('click', () => saveChain().catch(err => errBox.textContent = String(err)));
styleSelect.addEventListener('change', () => {
  selectedRouteIDs = [];
  renderPicker();
});
document.getElementById('route-picker').addEventListener('click', event => {
  const btn = event.target.closest('[data-route-id]');
  if (!btn) return;
  const id = Number(btn.dataset.routeId);
  const idx = selectedRouteIDs.indexOf(id);
  if (idx >= 0) selectedRouteIDs.splice(idx, 1);
  else selectedRouteIDs.push(id);
  renderPicker();
});
document.getElementById('selected-list').addEventListener('click', event => {
  const btn = event.target.closest('[data-remove-route-id]');
  if (!btn) return;
  const id = Number(btn.dataset.removeRouteId);
  selectedRouteIDs = selectedRouteIDs.filter(value => value !== id);
  renderPicker();
});
document.getElementById('chain-rows').addEventListener('click', event => {
  const toggleBtn = event.target.closest('[data-toggle]');
  if (toggleBtn){ toggleChain(toggleBtn.dataset.toggle).catch(() => {}); return; }
  const editBtn = event.target.closest('[data-edit]');
  if (editBtn){ const chain = chains.find(x => String(x.id) === editBtn.dataset.edit); if (chain) openModal(chain); return; }
  const delBtn = event.target.closest('[data-delete]');
  if (delBtn){ deleteChain(delBtn.dataset.delete).catch(() => {}); }
});
overlay.addEventListener('click', e => { if (e.target === overlay) closeModal(); });
loadData().catch(() => {});
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
    .brief-grid{display:grid;grid-template-columns:1fr;gap:12px;margin-top:12px}.brief-box{border-radius:7px;padding:18px 22px;border:1px solid;font-size:15px;line-height:1.7}.brief-box.req{background:#f3f7ff;border-color:#b9cdfd}.brief-box.res{background:#ebfbf4;border-color:#88e6bd}.brief-box.subagent{background:#f7f3ff;border-color:#cbb8ff}.brief-box.subagent .tag{background:#7c3aed}.brief-text{white-space:pre-wrap;word-break:break-word;margin-top:12px}.brief-chip{display:inline-block;margin:0 6px 6px 0;padding:2px 8px;border-radius:999px;background:#fff;border:1px solid #cddaf2;color:#435169;font-size:13px;font-weight:600}.role-line{margin:0 0 14px}.role-name{display:inline-block;min-width:74px;color:#657083;font-weight:700}.tool-label{display:inline-block;color:#475569;background:#eef2f7;border:1px solid #d8e0eb;border-radius:5px;padding:1px 7px;font-size:12px;font-weight:700;margin-right:6px}.empty-text{color:#4f5b6b}
    .ctx{margin-top:12px;border:1px solid #d9e2f1;border-radius:8px;background:#fbfdff;overflow:hidden}.ctx-tabs{display:flex;gap:6px;align-items:center;padding:8px;background:#f4f7fc;border-bottom:1px solid #d9e2f1;overflow-x:auto}.ctx-tab{border:1px solid transparent;background:transparent;color:#667284;border-radius:6px;padding:6px 10px;font-weight:700;font-size:13px;white-space:nowrap;cursor:pointer}.ctx-tab.active{background:#fff;color:#2f6fed;border-color:#c9d8f7;box-shadow:0 1px 2px rgba(21,35,60,.05)}.ctx-panel{display:none;padding:14px 16px}.ctx-panel.active{display:block}.ctx-summary{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:10px}.ctx-stat{border:1px solid #dde6f2;border-radius:7px;background:#fff;padding:10px 12px}.ctx-stat-label{font-size:12px;color:#748094;font-weight:700}.ctx-stat-value{font-size:20px;font-weight:700;margin-top:2px;color:#20242c}.ctx-list{display:grid;gap:10px}.ctx-card{border:1px solid #dde6f2;border-radius:7px;background:#fff;padding:12px}.ctx-card.subagent-card{border-color:#d8c7ff;background:#fcfaff}.ctx-card-title{display:flex;gap:8px;align-items:center;flex-wrap:wrap;font-weight:800;color:#242a34}.ctx-card-sub{color:#7a8495;font-size:12px;margin-top:3px;word-break:break-all}.ctx-card-desc{margin-top:8px;color:#3d4654;white-space:pre-wrap}.ctx-param{display:inline-block;margin:8px 6px 0 0;padding:2px 7px;border-radius:5px;background:#f3f6fb;border:1px solid #dde6f2;color:#526074;font-size:12px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.ctx-pre{margin-top:8px;max-height:220px;overflow:auto;white-space:pre-wrap;word-break:break-word;font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:#344054;background:#f7f9fc;border:1px solid #e0e7f2;border-radius:6px;padding:10px}.ctx-kv{display:grid;grid-template-columns:150px 1fr;gap:8px;padding:7px 0;border-bottom:1px solid #edf1f7}.ctx-kv:last-child{border-bottom:0}.ctx-key{color:#687386;font-weight:700}.ctx-val{word-break:break-word}.subagent-detail{margin-top:8px}.subagent-kv{display:grid;grid-template-columns:92px 1fr;gap:4px 10px;margin-top:8px;font-size:13px}.subagent-kv div:nth-child(odd){color:#687386;font-weight:700}.subagent-task{margin-top:10px;padding:10px;border:1px solid #e1d8ff;border-radius:6px;background:#fff;white-space:pre-wrap;word-break:break-word}
    pre{margin:0;white-space:pre-wrap;word-break:break-word;max-height:520px;overflow:auto;font:13px/1.55 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}.small{font-size:13px;color:#6b7484}.stats{display:flex;gap:10px;flex-wrap:wrap;margin-top:8px}.pill{background:#f7f9fc;border:1px solid var(--line);border-radius:999px;padding:3px 9px;color:#5d6675;font-size:13px}
  </style>
</head>
<body>
<main class="page">
  <div class="head">
    <div class="head-main"><a class="back" href="/">←</a><div><div class="title">{{.Conversation.Agent}} 详情</div><div class="sid">{{.Conversation.SessionID}}</div></div></div>
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
// looksInjectedBlock 判断一段文本是否是注入的上下文块(权限/环境/技能规则等),
// 这些不算系统提示,单独在「上下文」tab 展示。
function looksInjectedBlock(text){
  const t = String(text || '').trim();
  return /^<(?:permissions instructions|environment_context|app-context|collaboration_mode|skills_instructions|plugins_instructions|user_instructions)>/.test(t) || t.startsWith('# AGENTS.md');
}
// systemText 取系统提示:老 Codex 用顶层 instructions,Claude 用顶层 system;
// 新版 Codex 把系统提示放在 input 的 developer/system 消息里(排除 additional_tools 与注入上下文块)。
function systemText(body){
  if (!body || typeof body !== 'object') return '';
  if (body.instructions) return String(body.instructions);
  if (typeof body.system === 'string') return body.system;
  if (Array.isArray(body.system)) return contentText(body.system);
  if (Array.isArray(body.input)) {
    const parts = [];
    body.input.forEach(item => {
      if (!item || typeof item !== 'object' || item.type === 'additional_tools') return;
      if (item.role !== 'developer' && item.role !== 'system') return;
      const text = contentText(item.content).trim();
      if (text && !looksInjectedBlock(text)) parts.push(text);
    });
    if (parts.length) return parts.join('\n\n');
  }
  return '';
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
// collectRawTools 收集所有工具定义:老格式在顶层 tools;新版 Codex 在 input 里
// type=additional_tools 的条目。namespace 分组会展开成带前缀名的子工具。
function collectRawTools(body){
  const out = [];
  const push = arr => {
    (Array.isArray(arr) ? arr : []).forEach(tool => {
      if (!tool || typeof tool !== 'object') return;
      if (tool.type === 'namespace' && Array.isArray(tool.tools)) {
        tool.tools.forEach(sub => {
          if (sub && typeof sub === 'object') out.push(Object.assign({}, sub, {name: (tool.name ? tool.name + '.' : '') + (toolName(sub) || 'unknown')}));
        });
      } else {
        out.push(tool);
      }
    });
  };
  if (body && Array.isArray(body.tools)) push(body.tools);
  if (body && Array.isArray(body.input)) {
    body.input.forEach(item => {
      if (item && item.type === 'additional_tools') push(item.tools);
    });
  }
  return out;
}
function toolDetails(body){
  return collectRawTools(body).map(tool => {
    const params = tool.parameters || tool.input_schema || tool.function?.parameters || {};
    const properties = params && typeof params === 'object' && params.properties && typeof params.properties === 'object' ? params.properties : {};
    return {
      name: toolName(tool) || 'unknown',
      type: tool.type || 'function',
      description: tool.description || tool.function?.description || '',
      required: Array.isArray(params.required) ? params.required : [],
      properties
    };
  });
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
  // Codex 在 input[],Claude 在 messages[];两者都扫
  const source = Array.isArray(body?.input) ? body.input : Array.isArray(body?.messages) ? body.messages : [];
  source.forEach((item, idx) => {
    const content = Array.isArray(item?.content) ? item.content : [];
    content.forEach((part, partIdx) => {
      if (part && typeof part === 'object' && (part.type === 'input_image' || part.type === 'image')) {
        // Codex: image_url 字符串/对象;Claude: source.data(base64)或 source.url
        const imageURL = typeof part.image_url === 'string' ? part.image_url : part.image_url?.url || part.source?.url || part.source?.data || '';
        out.push({role: item.role || 'user', index: idx + 1, part: partIdx + 1, type: part.type, detail: part.detail || part.source?.media_type || '', size: imageURL ? imageURL.length : 0});
      }
    });
  });
  return out;
}
function parseMaybeJSON(value){
  let current = value;
  for (let i = 0; i < 3; i++) {
    if (typeof current !== 'string') return current;
    const trimmed = current.trim();
    if (!trimmed || !/^[\[{"]/.test(trimmed)) return current;
    try { current = JSON.parse(trimmed); } catch (_) { return current; }
  }
  return current;
}
function extractBalancedObject(text, start){
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = start; i < text.length; i++) {
    const ch = text[i];
    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (ch === '\\') {
        escaped = true;
      } else if (ch === '"') {
        inString = false;
      }
      continue;
    }
    if (ch === '"') {
      inString = true;
      continue;
    }
    if (ch === '{') depth++;
    if (ch === '}') {
      depth--;
      if (depth === 0) return text.slice(start, i + 1);
    }
  }
  return '';
}
function taskFromSubagentMessage(message){
  const text = String(message || '').trim();
  const candidates = [];
  let pos = 0;
  while (true) {
    const start = text.indexOf('{', pos);
    if (start < 0) break;
    const raw = extractBalancedObject(text, start);
    if (!raw) break;
    const parsed = parseJSON(raw);
    if (parsed && typeof parsed === 'object' && (parsed.summary || parsed.action || parsed.target || parsed.page_location)) {
      return parsed;
    }
    candidates.push(raw);
    pos = start + Math.max(raw.length, 1);
  }
  return null;
}
function normalizeList(value){
  if (!value) return '';
  if (Array.isArray(value)) return value.filter(v => v !== undefined && v !== null && String(v).trim() !== '').join(' / ');
  if (typeof value === 'object') return stringify(value);
  return String(value);
}
function subagentTitle(agent, idx){
  const task = agent.task || {};
  return task.summary || agent.nickname || agent.agent_id || ('Subagent #' + (idx + 1));
}
function subagentRows(agent){
  const task = agent.task || {};
  const target = task.target || {};
  const page = task.page_location || {};
  return [
    ['Agent', [agent.nickname, agent.agent_id].filter(Boolean).join(' · ')],
    ['类型', agent.agent_type || ''],
    ['Call ID', agent.call_id || ''],
    ['Turn', agent.turn || ''],
    ['页面', normalizeList(page.breadcrumb || page.menu_leaf || page)],
    ['动作', task.action || ''],
    ['目标', normalizeList(target.element || target.labels || target)],
    ['锚点', normalizeList(task.search_anchors)],
    ['范围', normalizeList([task.scope, task.source].filter(Boolean))],
    ['上下文', agent.fork_context === undefined ? '' : 'fork_context=' + String(agent.fork_context)],
  ].filter(([, v]) => v !== undefined && v !== null && String(v).trim() !== '');
}
function renderSubagentDetail(agent, idx, compact){
  const rows = subagentRows(agent);
  const task = agent.task || null;
  const fallbackMessage = !task && agent.message ? shortText(agent.message, compact ? 900 : 1600) : '';
  return '<div class="' + (compact ? 'subagent-detail' : 'ctx-card subagent-card') + '">' +
    (compact ? '' : '<div class="ctx-card-title">启动 Subagent #' + (idx + 1) + '<span class="brief-chip">' + esc(agent.nickname || 'worker') + '</span></div>') +
    '<div class="subagent-kv">' + rows.map(([k, v]) => '<div>' + esc(k) + '</div><div>' + esc(v) + '</div>').join('') + '</div>' +
    (task ? '<div class="subagent-task">' + esc(stringify(task)) + '</div>' : '') +
    (fallbackMessage ? '<div class="subagent-task">' + esc(fallbackMessage) + '</div>' : '') +
  '</div>';
}
function renderSubagentCards(subagents){
  if (!subagents.length) return '<div class="empty-text">没有捕获到 spawn_agent 调用。</div>';
  return '<div class="ctx-list">' + subagents.map((agent, idx) => renderSubagentDetail(agent, idx, false)).join('') + '</div>';
}
function mergeSubagentOutput(agent, output){
  const parsed = parseMaybeJSON(output?.output ?? output);
  const value = parsed && typeof parsed === 'object' ? parsed : {};
  if (value.agent_id && !agent.agent_id) agent.agent_id = value.agent_id;
  if (value.nickname && !agent.nickname) agent.nickname = value.nickname;
}
function collectSubagentItem(state, item, fallbackKey){
  if (!item || typeof item !== 'object') return null;
  if (item.type === 'function_call_output') {
    const key = item.call_id || fallbackKey;
    const agent = state.byCallID.get(key) || state.byKey.get(key);
    if (agent) mergeSubagentOutput(agent, item);
    return agent || null;
  }
  if (item.type !== 'function_call' || item.name !== 'spawn_agent') return null;
  const key = item.call_id || item.id || fallbackKey || ('spawn-' + state.items.length);
  let agent = state.byKey.get(key) || state.byCallID.get(item.call_id || '');
  if (!agent) {
    agent = {call_id: item.call_id || '', item_id: item.id || '', output_index: item.output_index, raw_arguments: '', turn: ''};
    state.items.push(agent);
  }
  if (item.call_id) state.byCallID.set(item.call_id, agent);
  if (item.id) state.byItemID.set(item.id, agent);
  if (item.output_index !== undefined) state.byOutputIndex.set(String(item.output_index), agent);
  state.byKey.set(key, agent);
  if (item.arguments !== undefined && item.arguments !== '') applySubagentArguments(agent, item.arguments);
  return agent;
}
function applySubagentArguments(agent, raw){
  agent.raw_arguments = typeof raw === 'string' ? raw : stringify(raw);
  const args = parseMaybeJSON(raw);
  if (!args || typeof args !== 'object') return;
  agent.agent_type = args.agent_type || agent.agent_type || '';
  agent.fork_context = args.fork_context;
  agent.message = args.message || agent.message || '';
  agent.model = args.model || agent.model || '';
  agent.reasoning_effort = args.reasoning_effort || agent.reasoning_effort || '';
  agent.task = taskFromSubagentMessage(agent.message);
}
function findSubagentByEvent(state, data){
  return state.byCallID.get(data?.call_id || '') ||
    state.byItemID.get(data?.item_id || '') ||
    state.byOutputIndex.get(String(data?.output_index ?? '')) ||
    null;
}
function subagentsFromTrace(t){
  const state = {items: [], byKey: new Map(), byCallID: new Map(), byItemID: new Map(), byOutputIndex: new Map()};
  const body = parseJSON(t.ResponseBody);
  if (Array.isArray(body?.output)) {
    body.output.forEach((item, idx) => collectSubagentItem(state, item, 'body-' + idx));
  }
  sseEvents(t).forEach(ev => {
    const name = ev.Event || ev.event || '';
    const data = eventData(ev);
    if (!data || typeof data !== 'object') return;
    if (data.item) collectSubagentItem(state, data.item, name + '-' + (data.output_index ?? state.items.length));
    if (name === 'response.function_call_arguments.done' || data.type === 'response.function_call_arguments.done') {
      const agent = findSubagentByEvent(state, data);
      if (!agent) return;
      applySubagentArguments(agent, data.arguments);
    }
    if (Array.isArray(data?.response?.output)) {
      data.response.output.forEach((item, idx) => collectSubagentItem(state, item, 'completed-' + idx));
    }
  });
  return state.items.map(agent => ({...agent, turn: t.SequenceNo || ''})).filter(agent => agent.raw_arguments || agent.message || agent.agent_id);
}
function allSubagents(traces){
  return (traces || []).flatMap(t => subagentsFromTrace(t));
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
    ['client_metadata', stringify(body?.client_metadata)],
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
  const text = systemText(body).trim();
  if (!text) return '<div class="empty-text">没有顶层系统提示。</div>';
  return '<div class="ctx-card"><div class="ctx-card-title">instructions<span class="brief-chip">' + esc(text.length.toLocaleString()) + ' chars</span></div><div class="ctx-pre">' + esc(shortText(text, 4000)) + '</div></div>';
}
function renderContextInspector(t, body, traces){
  const messages = requestMessages(body);
  const tools = toolDetails(body);
  const mcpTools = tools.filter(tool => /mcp|resource|template/i.test(tool.name + ' ' + tool.description));
  const skills = skillDetails(messages);
  const sections = injectedSections(messages);
  const media = mediaDetails(body);
  const subagents = allSubagents(traces || []);
  const tabs = [
    ['overview', '概览', '<div class="ctx-summary">' +
      [['系统', systemText(body) ? systemText(body).length.toLocaleString() + ' chars' : '0'], ['消息', messages.length], ['Tools', tools.length], ['Skills', skills.length], ['MCP', mcpTools.length], ['上下文', sections.length], ['Subagents', subagents.length], ['媒体', media.length]]
        .map(([label, value]) => '<div class="ctx-stat"><div class="ctx-stat-label">' + esc(label) + '</div><div class="ctx-stat-value">' + esc(value) + '</div></div>').join('') +
      '</div>'],
    ['system', '系统', renderSystemInstructions(body)],
    ['tools', 'Tools ' + tools.length, renderToolCards(tools)],
    ['skills', 'Skills ' + skills.length, renderSkillCards(skills)],
    ['mcp', 'MCP ' + mcpTools.length, renderToolCards(mcpTools)],
    ['context', '上下文 ' + sections.length, renderSectionCards(sections)],
    ['subagents', 'Subagents ' + subagents.length, renderSubagentCards(subagents)],
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
  if (!blocks.length) blocks.push(...claudeStreamBlocks(t));
  const text = blocks.filter(Boolean).join('\n\n').trim();
  return text || '响应体里没有 assistant 文本。';
}
// claudeStreamBlocks 把 Anthropic 流式 SSE 还原成文本:正文走 text_delta,
// 思考走 thinking_delta,工具调用按 content_block_start 的 tool_use 名字标注。
function claudeStreamBlocks(t){
  const events = sseEvents(t);
  const parts = {};
  let order = [];
  events.forEach(ev => {
    const name = ev.Event || ev.event;
    const d = eventData(ev);
    if (!d) return;
    const idx = typeof d.index === 'number' ? d.index : 0;
    if (!(idx in parts)) { parts[idx] = {text: '', label: ''}; order.push(idx); }
    if (name === 'content_block_start' && d.content_block) {
      const cb = d.content_block;
      if (cb.type === 'tool_use') parts[idx].label = '工具调用 ' + (cb.name || cb.id || '');
      else if (cb.type === 'thinking') parts[idx].label = '[thinking]';
    } else if (name === 'content_block_delta' && d.delta) {
      if (d.delta.type === 'text_delta') parts[idx].text += d.delta.text || '';
      else if (d.delta.type === 'thinking_delta') parts[idx].text += d.delta.thinking || '';
      else if (d.delta.type === 'input_json_delta') parts[idx].text += d.delta.partial_json || '';
    }
  });
  return order.map(i => {
    const p = parts[i];
    const body = (p.label ? p.label + '\n' : '') + p.text;
    return body.trim();
  }).filter(Boolean);
}
function briefTraceHTML(t){
  const body = parseJSON(t.RequestBody);
  const errorBox = t.Error ? '<div class="box err"><span class="tag">错误</span><pre>' + esc(t.Error) + '</pre></div>' : '';
  const messages = requestMessages(body);
  const subagents = subagentsFromTrace(t);
  const subagentBoxes = subagents.map((agent, idx) =>
    '<div class="brief-box subagent"><span class="tag">启动 Subagent</span><div class="brief-text"><strong>' + esc(subagentTitle(agent, idx)) + '</strong>' + renderSubagentDetail(agent, idx, true) + '</div></div>'
  ).join('');
  return '<section class="turn">' +
    '<div class="turn-head">▸ Turn ' + (t.SequenceNo || 0) + ' <span class="meta">' + (t.Status || 0) + ' ' + esc(t.Path) + '</span><a class="json-btn" href="/api/conversations/' + conversationID + '">JSON</a></div>' +
    '<div class="brief-grid">' +
      errorBox +
      '<div class="brief-box req"><span class="tag">请求</span><div class="brief-text">' + renderRoleLines(messages) + '</div></div>' +
      '<div class="brief-box res"><span class="tag">响应</span><div class="brief-text">' + esc(responseText(t)) + '</div></div>' +
      subagentBoxes +
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
    // 用请求体最大的 trace 作为上下文来源:Claude Code 首个请求常是几百字节的
    // quota/warmup 探测(无 system/tools/messages),直接取 traces[0] 会全是 0。
    const first = (traces || []).slice().sort((a, b) => String(b.RequestBody || '').length - String(a.RequestBody || '').length)[0];
    context.innerHTML = viewMode === 'brief' && first ? renderContextInspector(first, parseJSON(first.RequestBody), traces || []) : '';
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

const routesHTML = `
{{define "routes"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>路由 · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--orange:#d46f1e;--red:#c94040;--purple:#6750d8}
    *{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
    .app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;align-self:flex-start;height:100vh;transition:width .16s ease,padding .16s ease}
    .sidebar .brand-row{display:flex;align-items:center;gap:8px;padding:6px 2px 18px 10px}.sidebar .brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;flex:1;white-space:nowrap;overflow:hidden}
    .sidebar-toggle{width:30px;height:30px;padding:0;border-radius:7px;border:1px solid var(--line);background:#fff;color:#667284;font-weight:800}
    .app.sidebar-collapsed .sidebar{width:58px;padding-left:10px;padding-right:10px}.app.sidebar-collapsed .brand{display:none}.app.sidebar-collapsed .nav-item{padding:10px 0;text-align:center;font-size:0}.app.sidebar-collapsed .nav-item::first-letter{font-size:15px}.app.sidebar-collapsed .sidebar-toggle{transform:rotate(180deg)}
    .nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px}
    .nav-item:hover{background:#eef2fa}
    .nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:22px 28px 56px}
    .top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}
    button{cursor:pointer;height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 14px;font:inherit;color:var(--text)}
    .btn-primary{background:var(--blue);border-color:var(--blue);color:#fff;font-weight:600}
    .btn-primary:hover{background:#265fd0}
    .table{margin-top:18px;background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05);overflow-x:auto}
    table{width:100%;min-width:820px;border-collapse:collapse}
    th,td{padding:14px 16px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle}
    th{font-size:12px;color:#606a7a;font-weight:600;letter-spacing:.04em;background:#fbfcfe}
    tr:last-child td{border-bottom:0}
    .mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}
    .muted{color:var(--muted)}
    .pill{display:inline-block;border-radius:6px;padding:3px 9px;font-size:13px;font-weight:600;color:var(--purple);background:#f4f1ff;border:1px solid #ddd4ff}
    .tag{display:inline-block;border-radius:6px;padding:3px 9px;font-size:12px;font-weight:600;color:#2469e8;background:#edf4ff;border:1px solid #c6d9ff}
    .action{display:inline-flex;align-items:center;justify-content:center;min-width:44px;height:30px;border:1px solid var(--line);padding:0 12px;border-radius:7px;background:#fff;font-weight:500}
    .action + .action{margin-left:6px}
    .delete-btn{color:var(--red);border-color:#f0b3b3;background:#fff6f6}.delete-btn:hover{background:#fff1f1;border-color:#e07d7d}
    .switch{display:inline-flex;align-items:center;justify-content:center;min-width:60px;height:30px;padding:0 16px;border-radius:16px;border:1px solid var(--line);background:#fff;color:var(--muted);font-weight:700;font-size:12px;letter-spacing:.06em;cursor:pointer}
    .switch:hover{border-color:#b8c7dc}
    .switch.on{background:var(--green);border-color:var(--green);color:#fff}
    .switch.on:hover{background:#0f8548}
    .overlay{display:none;position:fixed;inset:0;background:rgba(22,28,40,.4);align-items:center;justify-content:center;z-index:20}
    .overlay.show{display:flex}
    .modal{width:460px;max-width:calc(100vw - 32px);background:#fff;border-radius:12px;box-shadow:0 12px 40px rgba(20,30,50,.24);padding:24px}
    .modal h2{margin:0 0 18px;font-size:16px}
    .field{margin-bottom:14px}
    .field-row{display:flex;gap:12px}.field-row .field{flex:1;margin-bottom:14px}
    .field label{display:block;font-size:12px;font-weight:600;color:var(--muted);margin-bottom:6px}
    .field input,.field select{width:100%;height:40px;border:1px solid var(--line);border-radius:7px;padding:0 12px;font:inherit;color:var(--text);background:#fff}
    .modal-actions{display:flex;justify-content:flex-end;gap:10px;margin-top:22px}
    .empty{padding:38px 16px;text-align:center;color:var(--muted)}
    .err{color:var(--red);font-size:13px;min-height:18px;margin-top:4px}
  </style>
</head>
<body>
<div class="app">
  <aside class="sidebar">
    <div class="brand-row"><div class="brand">AGENT-GO-PROXY</div><button class="sidebar-toggle" id="sidebar-toggle" type="button" title="收起/展开侧栏">‹</button></div>
    <nav>
      <a class="nav-item" href="/">Dashboard</a>
      <a class="nav-item active" href="/routes">路由</a>
      <a class="nav-item" href="/chains">链式代理</a>
    </nav>
  </aside>
  <main class="page">
    <div class="top"><h1>路由 · 第三方 API 配置</h1><button class="btn-primary" id="add-btn">+ 添加配置</button></div>
    <section class="table">
      <table>
        <thead><tr><th>名称</th><th>Base URL</th><th>API</th><th>接口协议</th><th>Model</th><th>API Key</th><th style="width:90px">启用</th><th style="width:150px">操作</th></tr></thead>
        <tbody id="route-rows"></tbody>
      </table>
      <div class="empty" id="empty-tip" style="display:none">还没有配置。点击右上角「添加配置」新建一个。</div>
    </section>
  </main>
</div>

<div class="overlay" id="modal-overlay">
  <div class="modal">
    <h2 id="modal-title">添加配置</h2>
    <div class="field"><label>名称</label><input id="f-name" placeholder="便于识别的名称，可留空"></div>
    <div class="field"><label>Base URL</label><input id="f-base-url" placeholder="https://api.example.com/v1"></div>
    <div class="field-row">
      <div class="field"><label>API</label><select id="f-api-style"></select></div>
      <div class="field"><label>接口协议</label><select id="f-protocol"></select></div>
    </div>
    <div class="field"><label>Model</label><input id="f-model" placeholder="gpt-4o / claude-sonnet-... 可留空"></div>
    <div class="field"><label>API Key</label><input id="f-api-key" placeholder="sk-..." autocomplete="off"></div>
    <div class="err" id="modal-err"></div>
    <div class="modal-actions">
      <button id="cancel-btn">取消</button>
      <button class="btn-primary" id="save-btn">保存</button>
    </div>
  </div>
</div>

<script>
const appEl = document.querySelector('.app');
if (localStorage.getItem('sidebarCollapsed') === '1') appEl.classList.add('sidebar-collapsed');
document.getElementById('sidebar-toggle').addEventListener('click', () => {
  appEl.classList.toggle('sidebar-collapsed');
  localStorage.setItem('sidebarCollapsed', appEl.classList.contains('sidebar-collapsed') ? '1' : '0');
});
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const maskKey = k => {
  k = String(k || '');
  if (!k) return '<span class="muted">—</span>';
  if (k.length <= 8) return esc('••••' + k.slice(-2));
  return esc(k.slice(0, 4) + '••••••' + k.slice(-4));
};
// API 风格 → 展示名。接口协议按风格联动(需与后端 store.go routeProtocols 保持一致)。
const STYLES = [{value:'openai', label:'OpenAI'}, {value:'anthropic', label:'Anthropic'}];
const PROTOCOLS = {
  openai: [
    {value:'chat_completions', label:'Chat Completions API'},
    {value:'responses', label:'Responses API'},
  ],
  anthropic: [
    {value:'messages', label:'Messages API'},
    {value:'chat_completions', label:'Chat Completions API'},
  ],
};
const styleLabel = v => (STYLES.find(s => s.value === v) || {}).label || v || '';
const protocolLabel = (style, v) => ((PROTOCOLS[style] || []).find(p => p.value === v) || {}).label || v || '';
let routes = [];
let editingId = null;
const overlay = document.getElementById('modal-overlay');
const errBox = document.getElementById('modal-err');

function renderRows(){
  const tbody = document.getElementById('route-rows');
  const empty = document.getElementById('empty-tip');
  if (!routes.length){
    tbody.innerHTML = '';
    empty.style.display = 'block';
    return;
  }
  empty.style.display = 'none';
  tbody.innerHTML = routes.map(r =>
    '<tr>' +
      '<td>' + (r.name ? esc(r.name) : '<span class="muted">未命名</span>') + '</td>' +
      '<td class="mono">' + esc(r.base_url || '') + '</td>' +
      '<td>' + (r.api_style ? '<span class="tag">' + esc(styleLabel(r.api_style)) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (r.protocol ? esc(protocolLabel(r.api_style, r.protocol)) : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (r.model ? '<span class="pill">' + esc(r.model) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="mono">' + maskKey(r.api_key) + '</td>' +
      '<td><button class="switch' + (r.enabled ? ' on' : '') + '" data-toggle="' + r.id + '">' + (r.enabled ? 'ON' : 'OFF') + '</button></td>' +
      '<td>' +
        '<button class="action" data-edit="' + r.id + '">编辑</button>' +
        '<button class="action delete-btn" data-delete="' + r.id + '">删除</button>' +
      '</td>' +
    '</tr>'
  ).join('');
}

function fillSelect(select, options){
  select.innerHTML = options.map(o => '<option value="' + esc(o.value) + '">' + esc(o.label) + '</option>').join('');
}
// 接口协议选项跟随 API 风格联动;keepValue 用于编辑回填时保留已保存的协议。
function syncProtocolOptions(keepValue){
  const style = document.getElementById('f-api-style').value;
  const protoSelect = document.getElementById('f-protocol');
  fillSelect(protoSelect, PROTOCOLS[style] || []);
  if (keepValue && (PROTOCOLS[style] || []).some(p => p.value === keepValue)) {
    protoSelect.value = keepValue;
  }
}

async function loadRoutes(){
  const rsp = await fetch('/api/routes', {cache:'no-store'});
  if (!rsp.ok) return;
  routes = await rsp.json() || [];
  renderRows();
}

function openModal(route){
  editingId = route ? route.id : null;
  document.getElementById('modal-title').textContent = route ? '编辑配置' : '添加配置';
  document.getElementById('f-name').value = route ? (route.name || '') : '';
  document.getElementById('f-base-url').value = route ? (route.base_url || '') : '';
  document.getElementById('f-model').value = route ? (route.model || '') : '';
  document.getElementById('f-api-key').value = route ? (route.api_key || '') : '';
  const styleSelect = document.getElementById('f-api-style');
  fillSelect(styleSelect, STYLES);
  styleSelect.value = (route && route.api_style) || 'openai';
  syncProtocolOptions(route ? route.protocol : '');
  errBox.textContent = '';
  overlay.classList.add('show');
  document.getElementById('f-base-url').focus();
}
function closeModal(){ overlay.classList.remove('show'); editingId = null; }

async function saveRoute(){
  const payload = {
    name: document.getElementById('f-name').value.trim(),
    base_url: document.getElementById('f-base-url').value.trim(),
    api_style: document.getElementById('f-api-style').value,
    protocol: document.getElementById('f-protocol').value,
    model: document.getElementById('f-model').value.trim(),
    api_key: document.getElementById('f-api-key').value.trim()
  };
  if (!payload.base_url){ errBox.textContent = 'Base URL 不能为空'; return; }
  const url = editingId ? '/api/routes/' + editingId : '/api/routes';
  const method = editingId ? 'PUT' : 'POST';
  const rsp = await fetch(url, {method, headers:{'Content-Type':'application/json'}, body: JSON.stringify(payload)});
  if (!rsp.ok){ errBox.textContent = '保存失败：' + await rsp.text(); return; }
  closeModal();
  loadRoutes().catch(() => {});
}

async function toggleRoute(id){
  const rsp = await fetch('/api/routes/' + id + '/toggle', {method:'POST'});
  if (!rsp.ok){ alert('切换失败：' + await rsp.text()); return; }
  loadRoutes().catch(() => {});
}

async function deleteRoute(id){
  if (!confirm('确认删除这个配置吗？')) return;
  const rsp = await fetch('/api/routes/' + id, {method:'DELETE'});
  if (!rsp.ok){ alert('删除失败：' + await rsp.text()); return; }
  loadRoutes().catch(() => {});
}

document.getElementById('add-btn').addEventListener('click', () => openModal(null));
document.getElementById('cancel-btn').addEventListener('click', closeModal);
document.getElementById('save-btn').addEventListener('click', () => saveRoute().catch(err => errBox.textContent = String(err)));
document.getElementById('f-api-style').addEventListener('change', () => syncProtocolOptions());
overlay.addEventListener('click', e => { if (e.target === overlay) closeModal(); });
document.getElementById('route-rows').addEventListener('click', e => {
  const toggleBtn = e.target.closest('[data-toggle]');
  if (toggleBtn){ toggleRoute(toggleBtn.dataset.toggle).catch(() => {}); return; }
  const editBtn = e.target.closest('[data-edit]');
  if (editBtn){ const r = routes.find(x => String(x.id) === editBtn.dataset.edit); if (r) openModal(r); return; }
  const delBtn = e.target.closest('[data-delete]');
  if (delBtn){ deleteRoute(delBtn.dataset.delete).catch(() => {}); }
});
loadRoutes().catch(() => {});
</script>
</body>
</html>
{{end}}
`
