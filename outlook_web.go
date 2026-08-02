package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

func (p *proxyServer) handleOutlook(w http.ResponseWriter, r *http.Request) {
	if err := baseTemplate.ExecuteTemplate(w, "outlook", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *proxyServer) handleAPIOutlookAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := p.store.ListOutlookAccounts(r.Context())
	writeJSON(w, accounts, err)
}

// handleAPIOutlookValidEmails 返回「有 access token 且未过期」的所有账号邮箱。
// 过期时间由驱动按 loc=Asia/Shanghai 解析成中国时区时刻,在 Go 里与 time.Now() 比较(避开 MySQL NOW() 的 UTC 时区坑)。
func (p *proxyServer) handleAPIOutlookValidEmails(w http.ResponseWriter, r *http.Request) {
	accounts, err := p.store.ListOutlookAccounts(r.Context())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	now := time.Now()
	emails := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if a.AccessTokenLen > 0 && !a.AccessTokenExpiresAt.IsZero() && a.AccessTokenExpiresAt.After(now) {
			emails = append(emails, a.Email)
		}
	}
	writeJSON(w, map[string]any{"ok": true, "count": len(emails), "emails": emails}, nil)
}

// handleAPIOutlookUnregisteredEmails 返回「还没注册 GPT 账号」的邮箱列表(registered_gpt_account=0),
// 供批量注册流程挑下一个可用邮箱。Codex 已登录的邮箱也视为已注册,不会出现在这里。
// 可选 ?valid=1 只保留 access token 未过期的邮箱(判定同 valid-emails)。
// 同一邮箱可能有多行(唯一键是 email+client_id+scope),这里按邮箱去重,保留最近创建的那条。
func (p *proxyServer) handleAPIOutlookUnregisteredEmails(w http.ResponseWriter, r *http.Request) {
	accounts, err := p.store.ListOutlookAccounts(r.Context())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	onlyValid := r.URL.Query().Get("valid") == "1"
	now := time.Now()
	seen := make(map[string]bool, len(accounts))
	emails := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if a.RegisteredGPTAccount || a.HasCodexAccount || a.Email == "" || seen[a.Email] {
			continue
		}
		if onlyValid && !(a.AccessTokenLen > 0 && !a.AccessTokenExpiresAt.IsZero() && a.AccessTokenExpiresAt.After(now)) {
			continue
		}
		seen[a.Email] = true
		emails = append(emails, a.Email)
	}
	writeJSON(w, map[string]any{"ok": true, "count": len(emails), "emails": emails}, nil)
}

type outlookCredentialInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleAPIOutlookAccountCreate 手动新增账号:只收邮箱 + 密码,建一条待登录记录。
func (p *proxyServer) handleAPIOutlookAccountCreate(w http.ResponseWriter, r *http.Request) {
	var in outlookCredentialInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id, err := p.store.CreateOutlookAccount(r.Context(), in.Email, in.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id}, nil)
}

// handleAPIOutlookAccountCredentials 返回某行的邮箱 + 密码,供编辑弹窗回填。
func (p *proxyServer) handleAPIOutlookAccountCredentials(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	email, password, err := p.store.GetOutlookCredentials(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"email": email, "password": password}, nil)
}

// handleAPIOutlookAccountUpdate 修改某行的邮箱 + 密码。
func (p *proxyServer) handleAPIOutlookAccountUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	var in outlookCredentialInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := p.store.UpdateOutlookCredentials(r.Context(), id, in.Email, in.Password); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func (p *proxyServer) handleAPIOutlookAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	if err := p.store.DeleteOutlookAccount(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

// handleAPIOutlookAccountRefresh 同步刷新单个 Outlook 账号的 access_token + refresh_token:
// 用库里保存的登录会话 cookie 走 authorize?prompt=none 的 PKCE 静默授权换取新 token,
// 并把续期后的 cookie 一并写回 outlook_login_tokens,完成后回读该行返回给前端。
// 纯 Go 实现(见 refreshOutlookToken),不打开 Chrome、不调用任何外部脚本、不需要重新登录。
func (p *proxyServer) handleAPIOutlookAccountRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	account, err := p.refreshOutlookToken(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "account": account}, nil)
}

// handleAPIOutlookRefreshAll 一键刷新全部:立即返回,刷新在后台并发跑(等价于逐行点「刷新 Token」),
// 前端轮询 GET /api/outlook/refresh-all 看进度。已有任务在跑时返回 409。
func (p *proxyServer) handleAPIOutlookRefreshAll(w http.ResponseWriter, r *http.Request) {
	total, started, err := p.startRefreshAllOutlookTokens(r.Context())
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if !started {
		http.Error(w, "已有刷新任务在进行中", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "total": total}, nil)
}

func (p *proxyServer) handleAPIOutlookRefreshAllStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, p.outlookJob.snapshot(), nil)
}

const outlookHTML = `
{{define "outlook"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>OUTLOOK 账号 · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--red:#c94040;--purple:#6750d8}
    *{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}.app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;align-self:flex-start;height:100vh;transition:width .16s ease,padding .16s ease}.sidebar .brand-row{display:flex;align-items:center;gap:8px;padding:6px 2px 18px 10px}.sidebar .brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;flex:1;white-space:nowrap;overflow:hidden}.sidebar-toggle{width:30px;height:30px;padding:0;border-radius:7px;border:1px solid var(--line);background:#fff;color:#667284;font-weight:800}.app.sidebar-collapsed .sidebar{width:58px;padding-left:10px;padding-right:10px}.app.sidebar-collapsed .brand{display:none}.app.sidebar-collapsed .nav-item{padding:10px 0;text-align:center;font-size:0}.app.sidebar-collapsed .nav-item::first-letter{font-size:15px}.app.sidebar-collapsed .sidebar-toggle{transform:rotate(180deg)}
    .nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px;text-decoration:none}.nav-item:hover{background:#eef2fa}.nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:22px 28px 56px}.top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}button{height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 14px;font:inherit;color:var(--text);cursor:pointer;transition:transform .06s ease,box-shadow .12s ease,background .12s ease}
    button:hover:not(:disabled){background:#f3f6fc}button:active:not(:disabled){transform:translateY(1px) scale(.985);box-shadow:inset 0 2px 7px rgba(20,30,50,.16)}button:disabled{opacity:.6;cursor:not-allowed}
    button.busy::before{content:'';display:inline-block;width:11px;height:11px;margin-right:7px;vertical-align:-1px;border:2px solid currentColor;border-top-color:transparent;border-radius:50%;animation:spin .7s linear infinite}@keyframes spin{to{transform:rotate(360deg)}}.panel{margin-top:18px;background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05);overflow-x:auto}
    .notice{margin-top:12px;color:var(--muted)}
    table{width:100%;min-width:1080px;border-collapse:collapse}th,td{padding:15px 18px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle;white-space:nowrap}tbody tr:last-child td{border-bottom:0}th{font-size:12px;color:#606a7a;font-weight:600;background:#fbfcfe}
    tbody tr.row-link{cursor:pointer}tbody tr.row-link:hover{background:#fafcff}
    .email{font-weight:600}.sub{font-size:12px;color:var(--muted)}.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:#526074}
    .pill{display:inline-block;border-radius:6px;padding:3px 9px;font-size:12px;font-weight:600}.st-ok{color:var(--green);background:#e9f7ef;border:1px solid #bfe6cf}.st-bad{color:var(--red);background:#fdecec;border:1px solid #f2c2c2}.st-none{color:var(--muted);background:#eef1f6;border:1px solid #e0e5ee}
    .gpt-yes{color:var(--green);background:#e9f7ef;border:1px solid #bfe6cf}.gpt-no{color:var(--muted);background:#eef1f6;border:1px solid #e0e5ee}
    .token-ok{color:#137a4d}.token-warn{color:#b06d12;font-weight:600}.token-bad{color:var(--red);font-weight:600}.small{font-size:12px;color:var(--muted)}
    /* 操作列 4 个按钮排成 2×2 等宽网格,比一字排开窄得多,也不会参差换行 */
    td.actions{width:206px;min-width:206px}.act-grid{display:grid;grid-template-columns:repeat(2,1fr);gap:6px;width:194px}
    .act-grid>button{display:inline-flex;align-items:center;justify-content:center;width:100%;height:32px;margin:0;padding:0 8px;font-size:13px;white-space:nowrap}
    .login-btn{color:var(--green);border-color:#bfe6cf;background:#f1fbf5}.login-btn:hover:not(:disabled){background:#e6f7ee}
    .refresh-btn{color:var(--blue);border-color:#b9ccf5;background:#f4f8ff}.refresh-btn:hover{background:#e9f1ff}.edit-btn{color:#5a4bc4;border-color:#cfc7f2;background:#f6f4ff}.edit-btn:hover{background:#efeaff}.delete-btn{color:var(--red);border-color:#f0b3b3;background:#fff6f6}.delete-btn:hover{background:#fff1f1}
    .send-btn{color:#b06d12;border-color:#f0d9ab;background:#fffaf0}.send-btn:hover:not(:disabled){background:#fff4e2}
    .signup-btn{color:#0f8548;border-color:#bfe6cf;background:#f1fbf5}.signup-btn:hover:not(:disabled){background:#e6f7ee}
    .gptlogin-btn{color:#5a4bc4;border-color:#cfc7f2;background:#f6f4ff}.gptlogin-btn:hover:not(:disabled){background:#efeaff}
    .empty{padding:28px;color:var(--muted);text-align:center}
    .btn-primary{background:var(--blue);border-color:var(--blue);color:#fff;font-weight:600}.btn-primary:hover{background:#265fd0}
    .modal-mask{display:none;position:fixed;inset:0;background:rgba(20,28,45,.42);align-items:center;justify-content:center;z-index:50}.modal-mask.show{display:flex}
    .modal{background:#fff;border-radius:10px;width:400px;max-width:92vw;padding:22px 24px;box-shadow:0 12px 40px rgba(20,30,50,.25)}.modal h2{margin:0 0 4px;font-size:16px}.modal .sub{margin-bottom:16px}
    .field{display:grid;gap:6px;margin-bottom:14px}.field label{font-size:12px;color:#4b5563;font-weight:600}.field input{width:100%}
    .modal.send-modal{width:560px}.field textarea{width:100%;font:inherit;padding:8px 10px;border:1px solid var(--line);border-radius:7px;resize:vertical;min-height:120px}
    .modal-actions{display:flex;justify-content:flex-end;gap:10px;margin-top:6px}
    .login-box{display:none;padding:16px 20px;border-bottom:1px solid var(--line);background:#f8fbff}.login-box.show{display:block}
    .count-badge{margin-left:10px;font-size:13px;font-weight:600;color:var(--muted);background:#eef1f6;border-radius:999px;padding:3px 11px;vertical-align:2px}
    .login-title{font-weight:600}.login-detail{margin-top:4px}.login-box .modal-actions{justify-content:flex-start;margin-top:10px}.login-box button{height:32px;padding:0 12px;font-size:13px}
  </style>
</head>
<body>
<div class="app">
  <aside class="sidebar">
    <div class="brand-row"><div class="brand">AGENT-GO-PROXY</div><button class="sidebar-toggle" id="sidebar-toggle" type="button" title="收起/展开侧栏">‹</button></div>
    <nav>
      <a class="nav-item" href="/">Dashboard</a>
      <a class="nav-item" href="/routes">路由</a>
      <a class="nav-item" href="/chains">链式代理</a>
      <a class="nav-item" href="/openai">OPENAI</a>
      <a class="nav-item active" href="/outlook">OUTLOOK</a>
      <a class="nav-item" href="/api-keys">API Key</a>
      <a class="nav-item" href="/herosms">HeroSMS</a>
    </nav>
  </aside>
  <main class="page">
    <div class="top"><h1>OUTLOOK · 已登录账号<span class="count-badge" id="account-count"></span></h1><button class="btn-primary" id="login-btn" type="button">+ 登录账号</button><button id="add-btn" type="button">+ 新增账号</button><button id="refresh-all-btn" type="button" title="逐个刷新所有有 token 的账号(后台异步执行)">刷新全部 Token</button></div>
    <div class="notice">点「+ 登录账号」会弹出一个独立 profile 的 Chrome 窗口，你手动登录完成后自动抓取 cookie 与 token 入库（不会用你日常的 Chrome，也不会碰它的 cookie）。数据存在 <span class="mono">outlook_login_tokens</span> 表，与 outlook-login-automation skill 共用。点「刷新 Token」用库里 cookie 走 authorize?prompt=none 静默流程同时刷新 access / refresh token，无需重新登录。</div>
    <section class="panel">
      <div class="login-box" id="login-box">
        <div class="login-title" id="login-title-text">正在等待登录…</div>
        <div class="small login-detail" id="login-status"></div>
        <div class="modal-actions"><button id="login-cancel" type="button">取消登录</button></div>
      </div>
      <div class="login-box" id="gpt-box">
        <div class="login-title" id="gpt-title">GPT 任务</div>
        <div class="small login-detail" id="gpt-status"></div>
        <div class="modal-actions"><button id="gpt-cancel" type="button">取消任务</button></div>
      </div>
      <table>
        <thead><tr><th>账号</th><th>Codex</th><th>套餐/租户</th><th>Access Token 过期</th><th>Refresh Token 过期</th><th>Cookie</th><th>刷新状态</th><th>创建时间</th><th>操作</th></tr></thead>
        <tbody id="account-rows"></tbody>
      </table>
      <div class="empty" id="empty">暂无已登录的 Outlook 账号。点右上角「+ 登录账号」开始。</div>
    </section>
  </main>
</div>
<div class="modal-mask" id="modal-mask">
  <div class="modal">
    <h2 id="modal-title">新增账号</h2>
    <div class="sub small" id="modal-sub">输入邮箱与密码。密码以明文保存在数据库,供后续自动登录复用。</div>
    <div class="field"><label for="f-email">邮箱</label><input id="f-email" type="text" placeholder="user@outlook.com" autocomplete="off"></div>
    <div class="field"><label for="f-password">密码</label><input id="f-password" type="text" placeholder="登录密码" autocomplete="off"></div>
    <div class="modal-actions"><button id="modal-cancel" type="button">取消</button><button class="btn-primary" id="modal-save" type="button">保存</button></div>
  </div>
</div>
<div class="modal-mask" id="send-mask">
  <div class="modal send-modal">
    <h2>发邮件</h2>
    <div class="sub small" id="send-from"></div>
    <div class="field"><label for="s-to">收件人</label><input id="s-to" type="text" placeholder="多个地址用逗号分隔" autocomplete="off"></div>
    <div class="field"><label for="s-subject">主题</label><input id="s-subject" type="text" autocomplete="off"></div>
    <div class="field"><label for="s-body">正文</label><textarea id="s-body" placeholder="纯文本正文"></textarea></div>
    <div class="modal-actions"><button id="send-cancel" type="button">取消</button><button class="btn-primary" id="send-submit" type="button">发送</button></div>
  </div>
</div>
<script>
const appEl=document.querySelector('.app');
if(localStorage.getItem('sidebarCollapsed')==='1')appEl.classList.add('sidebar-collapsed');
document.getElementById('sidebar-toggle').addEventListener('click',()=>{appEl.classList.toggle('sidebar-collapsed');localStorage.setItem('sidebarCollapsed',appEl.classList.contains('sidebar-collapsed')?'1':'0')});
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const fmtTime=v=>v?new Date(v).toLocaleString('zh-CN',{timeZone:'Asia/Shanghai',hour12:false}):'';
// tokenText 按过期时间着色:已过期红、1 小时内到期黄、其余绿;时间未知(零值/空)显示「未知」。
function tokenText(v){
  const at=v?new Date(v):null;
  if(!at||isNaN(at.getTime())||at.getFullYear()<2000)return '<span class="small">未知</span>';
  const left=at.getTime()-Date.now();
  const cls=left<=0?'token-bad':(left<3600000?'token-warn':'token-ok');
  const tip=left<=0?'（已过期）':'';
  return '<span class="'+cls+'">'+esc(fmtTime(v))+tip+'</span>';
}
// needsLogin 判断这行是否还需要重新登录。重新登录只在静默刷新救不回来时才有意义,
// 所以「有 token + 没过期 + 最近一次刷新没失败」的健康账号不显示「登录」按钮。
// 刷新失败会写 last_refresh_error,所以刷失败的行会自动冒出登录按钮,正好接上。
function needsLogin(item){
  if(item.last_refresh_error)return true;
  if(!item.access_token_len)return true;
  const at=item.access_token_expires_at?new Date(item.access_token_expires_at):null;
  if(!at||isNaN(at.getTime())||at.getFullYear()<2000)return true;
  return at.getTime()<=Date.now();
}
function statusPill(item){
  if(item.last_refresh_error)return '<span class="pill st-bad" title="'+esc(item.last_refresh_error)+'">失败</span>';
  const s=item.last_refresh_status||'';
  if(!s)return '<span class="pill st-none">-</span>';
  return '<span class="pill st-ok">'+esc(s)+'</span>';
}
async function loadAccounts(){
  const rsp=await fetch('/api/outlook/accounts',{cache:'no-store'});if(!rsp.ok)throw new Error(await rsp.text());
  const items=await rsp.json();const rows=document.getElementById('account-rows');
  rows.innerHTML=(items||[]).map(item=>
    '<tr class="row-link" data-href="/outlook/accounts/'+item.id+'">'+
    '<td><div class="email">'+esc(item.email||'-')+'</div>'+(item.display_name?'<div class="sub">'+esc(item.display_name)+'</div>':'')+'</td>'+
    '<td>'+((item.has_codex_account||item.has_gpt_account)?'<span class="pill gpt-yes">是</span>':'<span class="pill gpt-no">否</span>')+'</td>'+
    '<td><div class="sub">'+esc(item.token_type||'-')+'</div><div class="sub mono" title="'+esc(item.tenant_id||'')+'">'+esc((item.tenant_id||'').slice(0,12)||'-')+'</div></td>'+
    '<td>'+tokenText(item.access_token_expires_at)+'</td>'+
    '<td>'+tokenText(item.refresh_token_expires_at)+'</td>'+
    '<td class="sub">'+esc(item.cookie_count||0)+'</td>'+
    '<td>'+statusPill(item)+'</td>'+
    '<td class="sub">'+esc(fmtTime(item.created_at))+'</td>'+
    '<td class="actions"><div class="act-grid">'+
      (needsLogin(item)?'<button class="login-btn" data-login-id="'+item.id+'" type="button"'+(item.has_password?' title="用这行保存的邮箱+密码自动登录"':' disabled title="没有保存密码，请先用「修改」填写密码"')+'>登录</button>':'')+
      '<button class="send-btn" data-send-id="'+item.id+'" data-email="'+esc(item.email||'')+'" type="button"'+(item.access_token_len?' title="用这个邮箱发一封邮件"':' disabled title="没有 access token，先登录或刷新 Token"')+'>发邮件</button>'+
      ((item.registered_gpt_account||item.has_codex_account||item.has_gpt_account)?'':'<button class="signup-btn" data-signup-id="'+item.id+'" type="button" title="用这个邮箱自动注册 ChatGPT 账号(CDP 自动化)">注册GPT</button>')+
      '<button class="gptlogin-btn" data-gptlogin-id="'+item.id+'" type="button" title="把这个邮箱的 GPT 账号登进 Codex(含手机验证)">登录Codex</button>'+
      '<button class="refresh-btn" data-refresh-id="'+item.id+'" type="button" title="刷新 access + refresh token">刷新 Token</button>'+
      '<button class="edit-btn" data-edit-id="'+item.id+'" type="button" title="修改邮箱/密码">修改</button>'+
      '<button class="delete-btn" data-id="'+item.id+'" type="button">删除</button>'+
    '</div></td>'+
    '</tr>').join('');
  document.getElementById('empty').style.display=items&&items.length?'none':'block';
  const total=(items||[]).length;
  document.getElementById('account-count').textContent=total?'共 '+total+' 个账号':'';
}
// ===== 刷新全部 Token(后台异步 + 轮询进度,页面不卡住)=====
const refreshAllBtn=document.getElementById('refresh-all-btn');
let refreshAllTimer=null;
function setRefreshAllLabel(text,busy){
  refreshAllBtn.textContent=text;
  refreshAllBtn.disabled=!!busy;
  refreshAllBtn.classList.toggle('busy',!!busy);
}
// pollRefreshAll 每 2s 拉一次进度,同时重载列表让过期时间边刷边更新;跑完提示结果。
async function pollRefreshAll(announce){
  let st;
  try{
    const rsp=await fetch('/api/outlook/refresh-all',{cache:'no-store'});
    if(!rsp.ok)throw new Error(await rsp.text());
    st=await rsp.json();
  }catch(err){setRefreshAllLabel('刷新全部 Token',false);return}
  await loadAccounts().catch(()=>{});
  if(st.running){
    setRefreshAllLabel('刷新中 '+st.done+'/'+st.total+'…',true);
    refreshAllTimer=setTimeout(()=>pollRefreshAll(announce),2000);
    return;
  }
  refreshAllTimer=null;
  setRefreshAllLabel('刷新全部 Token',false);
  if(announce&&st.total){
    const msg='刷新完成：成功 '+st.ok+' 个，失败 '+st.failed+' 个。';
    if(st.failed)alert(msg+'\n\n'+(st.errors||[]).join('\n'));
  }
}
refreshAllBtn.addEventListener('click',async()=>{
  if(refreshAllTimer)return;
  setRefreshAllLabel('启动中…',true);
  try{
    const rsp=await fetch('/api/outlook/accounts/refresh-all',{method:'POST'});
    if(!rsp.ok&&rsp.status!==409)throw new Error(await rsp.text());
    if(rsp.ok){
      const r=await rsp.json();
      if(!r.total){setRefreshAllLabel('刷新全部 Token',false);alert('没有可刷新的账号（都还没有 access token）。');return}
    }
  }catch(err){setRefreshAllLabel('刷新全部 Token',false);alert('发起刷新失败：'+err.message);return}
  pollRefreshAll(true);
});
// 页面打开时若已有任务在跑(比如刚刷新过页面),接上进度继续轮询。
fetch('/api/outlook/refresh-all',{cache:'no-store'}).then(r=>r.json()).then(st=>{
  if(st&&st.running){setRefreshAllLabel('刷新中 '+st.done+'/'+st.total+'…',true);refreshAllTimer=setTimeout(()=>pollRefreshAll(false),2000)}
}).catch(()=>{});
// 整行点击跳转到该账号的邮件页(点到操作按钮时不跳转)。
document.getElementById('account-rows').addEventListener('click',event=>{
  if(event.target.closest('button,a,input,select'))return;
  const row=event.target.closest('tr[data-href]');if(row)location.href=row.dataset.href;
});
// 刷新 Token:同步等静默刷新跑完(内部 90s 超时)。**无论成败都重载列表**:
// 失败时后端已把 last_refresh_status='error'/last_refresh_error 落库了,重载后状态栏
// 才会变「失败」、needsLogin 才会把「登录」按钮亮出来。只在成功时重载会让失败行一直
// 停在旧状态,用户既看不出刷失败、也等不到登录按钮。
document.getElementById('account-rows').addEventListener('click',async event=>{
  const button=event.target.closest('.refresh-btn');if(!button)return;
  const label=button.textContent;button.disabled=true;button.classList.add('busy');button.textContent='刷新中…';
  try{
    const rsp=await fetch('/api/outlook/accounts/'+button.dataset.refreshId+'/refresh',{method:'POST'});
    if(!rsp.ok)throw new Error(await rsp.text());
  }catch(err){alert('刷新 Token 失败：'+err.message)}
  finally{
    button.disabled=false;button.classList.remove('busy');button.textContent=label;
    await loadAccounts().catch(()=>{});
  }
});
document.getElementById('account-rows').addEventListener('click',async event=>{
  const button=event.target.closest('.delete-btn');if(!button||!confirm('确认删除这条 Outlook 登录记录（含 token 与 cookie）吗？'))return;
  const rsp=await fetch('/api/outlook/accounts/'+button.dataset.id,{method:'DELETE'});
  if(!rsp.ok){alert('删除失败：'+await rsp.text());return}
  loadAccounts().catch(()=>{});
});
// ===== 发邮件弹窗(用该行账号的 token 发,后端遇 401 会自动刷一次)=====
const sendMask=document.getElementById('send-mask');
const sTo=document.getElementById('s-to'),sSubject=document.getElementById('s-subject'),sBody=document.getElementById('s-body');
let sendingId=null;
function closeSend(){sendMask.classList.remove('show');sendingId=null}
document.getElementById('account-rows').addEventListener('click',event=>{
  const button=event.target.closest('.send-btn');if(!button||button.disabled)return;
  sendingId=button.dataset.sendId;
  document.getElementById('send-from').textContent='以 '+(button.dataset.email||'该账号')+' 的身份发送，会保存到「已发送邮件」。';
  sTo.value='';sSubject.value='';sBody.value='';
  sendMask.classList.add('show');sTo.focus();
});
document.getElementById('send-cancel').addEventListener('click',closeSend);
sendMask.addEventListener('click',e=>{if(e.target===sendMask)closeSend()});
document.addEventListener('keydown',e=>{if(e.key==='Escape'&&sendMask.classList.contains('show'))closeSend()});
document.getElementById('send-submit').addEventListener('click',async()=>{
  if(!sendingId)return;
  const to=sTo.value.trim();
  if(!to){alert('请填写收件人');sTo.focus();return}
  const button=document.getElementById('send-submit');const label=button.textContent;
  button.disabled=true;button.classList.add('busy');button.textContent='发送中…';
  try{
    const rsp=await fetch('/api/outlook/accounts/'+sendingId+'/send',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({to,subject:sSubject.value,body:sBody.value})});
    if(!rsp.ok)throw new Error(await rsp.text());
    const r=await rsp.json();
    closeSend();
    alert('已发送给：'+((r.to||[]).join('、')||to));
  }catch(err){alert('发送失败：'+err.message)}
  finally{button.disabled=false;button.classList.remove('busy');button.textContent=label}
});
// ===== 新增 / 修改 / 登录 弹窗(三种模式共用一个框)=====
let editingId=null;  // null=新增,否则=修改的行 id
let modalMode='add'; // add / edit / login
const MODAL_TEXT={
  add:{title:'新增账号',sub:'输入邮箱与密码。密码以明文保存在数据库,供后续自动登录复用。',save:'保存',email:'邮箱',password:'密码'},
  edit:{title:'修改账号',sub:'修改邮箱与密码,保存后立即生效。',save:'保存',email:'邮箱',password:'密码'},
  login:{title:'登录 Outlook 账号',sub:'会弹出一个独立 profile 的 Chrome 窗口,请在里面手动完成登录。两项都可留空:邮箱留空则登录后从 id_token 自动识别,密码填了会一并保存供后续复用。',save:'开始登录',email:'邮箱(可选)',password:'密码(可选)'},
};
const mask=document.getElementById('modal-mask');
const fEmail=document.getElementById('f-email'),fPassword=document.getElementById('f-password');
function openModal(mode,id){
  modalMode=mode||'add';
  editingId=id||null;
  const t=MODAL_TEXT[modalMode]||MODAL_TEXT.add;
  document.getElementById('modal-title').textContent=t.title;
  document.getElementById('modal-sub').textContent=t.sub;
  document.getElementById('modal-save').textContent=t.save;
  document.querySelector('label[for="f-email"]').textContent=t.email;
  document.querySelector('label[for="f-password"]').textContent=t.password;
  mask.classList.add('show');fEmail.focus();
}
function closeModal(){mask.classList.remove('show');fEmail.value='';fPassword.value='';editingId=null;modalMode='add'}
document.getElementById('add-btn').addEventListener('click',()=>{fEmail.value='';fPassword.value='';openModal('add',null)});
document.getElementById('login-btn').addEventListener('click',()=>{fEmail.value='';fPassword.value='';openModal('login',null)});
document.getElementById('modal-cancel').addEventListener('click',closeModal);
mask.addEventListener('click',e=>{if(e.target===mask)closeModal()});
document.addEventListener('keydown',e=>{if(e.key==='Escape'&&mask.classList.contains('show'))closeModal()});
// 点「修改」:先拉当前邮箱/密码回填,再打开弹窗
document.getElementById('account-rows').addEventListener('click',async event=>{
  const button=event.target.closest('.edit-btn');if(!button)return;
  try{
    const rsp=await fetch('/api/outlook/accounts/'+button.dataset.editId+'/credentials',{cache:'no-store'});
    if(!rsp.ok)throw new Error(await rsp.text());
    const c=await rsp.json();
    fEmail.value=c.email||'';fPassword.value=c.password||'';
    openModal('edit',button.dataset.editId);
  }catch(err){alert('读取账号信息失败：'+err.message)}
});
document.getElementById('modal-save').addEventListener('click',async()=>{
  const email=fEmail.value.trim(),password=fPassword.value;
  if(modalMode==='login'){
    const btn=document.getElementById('modal-save');btn.disabled=true;btn.textContent='启动中…';
    try{closeModal();await startLogin(email,password)}
    catch(err){alert('启动登录失败：'+err.message)}
    finally{btn.disabled=false;btn.textContent='开始登录'}
    return;
  }
  if(!email){alert('请输入邮箱');fEmail.focus();return}
  const save=document.getElementById('modal-save');const label=save.textContent;save.disabled=true;save.textContent='保存中…';
  try{
    const url=editingId?'/api/outlook/accounts/'+editingId:'/api/outlook/accounts';
    const method=editingId?'PUT':'POST';
    const rsp=await fetch(url,{method,headers:{'Content-Type':'application/json'},body:JSON.stringify({email,password})});
    if(!rsp.ok)throw new Error(await rsp.text());
    closeModal();await loadAccounts();
  }catch(err){alert('保存失败：'+err.message)}
  finally{save.disabled=false;save.textContent=label}
});
// ===== 手动登录:拉起独立 Chrome 窗口,登完自动抓 cookie+token =====
const loginBox=document.getElementById('login-box');
let loginPollTimer=null,currentLoginId=null;
// 先写内容、最后才让框可见。反过来的话,中间任何一步抛异常都会在页面上
// 留下一个「有边框但没字」的空壳,反而看不出发生了什么。
function showLoginBox(title,detail,busy){
  const t=document.getElementById('login-title-text');
  const s=document.getElementById('login-status');
  const c=document.getElementById('login-cancel');
  if(t)t.textContent=title||'登录';
  if(s)s.textContent=detail||'';
  if(c)c.style.display=busy?'':'none';
  loginBox.classList.add('show');
}
function hideLoginBox(){loginBox.classList.remove('show')}
// 手动登录:顶部「+ 登录账号」,自己在窗口里从头登。
async function startLogin(email,password){
  const rsp=await fetch('/api/outlook/logins',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({email,password})});
  if(!rsp.ok)throw new Error(await rsp.text());
  trackLogin(await rsp.json(),'正在等待登录…');
}
// 自动登录:行内「登录」,用该行存的邮箱+密码自动填表;卡住(验证码/两步验证)会转人工,
// 窗口留着不关,状态框里显示原因,你接管完成后照样自动抓 token。
async function startAutoLogin(id){
  const rsp=await fetch('/api/outlook/accounts/'+id+'/login',{method:'POST'});
  if(!rsp.ok)throw new Error(await rsp.text());
  trackLogin(await rsp.json(),'正在自动登录…');
}
function trackLogin(st,title){
  currentLoginId=st.id;
  showLoginBox(title,st.message||'',true);
  pollLogin();
}
// 轮询后端任务状态:waiting(等你在窗口里登录)→ capturing(抓 cookie 换 token)→ 终态
function pollLogin(){
  clearTimeout(loginPollTimer);
  loginPollTimer=setTimeout(async()=>{
    if(!currentLoginId)return;
    try{
      const rsp=await fetch('/api/outlook/logins/'+currentLoginId,{cache:'no-store'});
      if(!rsp.ok)throw new Error(await rsp.text());
      const st=await rsp.json();
      if(st.state==='waiting'||st.state==='capturing'){
        // 进行中的具体进度全在 message 里(正在填写邮箱 / 遇到验证码请接管 …),标题只分两段。
        showLoginBox(st.state==='waiting'?'登录进行中…':'登录成功，正在抓取 token…',st.message||'',true);
        pollLogin();return;
      }
      currentLoginId=null;
      if(st.state==='completed'){
        showLoginBox('登录成功','已保存 '+(st.email||'该账号')+' 的 cookie 与 token。',false);
        await loadAccounts();
        setTimeout(hideLoginBox,5000);
      }else if(st.state==='cancelled'){hideLoginBox()}
      else{
        // 兜底到 message、再兜底到状态码,保证这一行永远不会是空的。
        showLoginBox('登录失败',st.error||st.message||('后端返回状态 '+(st.state||'未知')+'，未给出原因'),false);
      }
    }catch(err){currentLoginId=null;showLoginBox('登录状态查询失败',(err&&err.message)||String(err),false)}
  },1500);
}
document.getElementById('account-rows').addEventListener('click',async event=>{
  const button=event.target.closest('.login-btn');if(!button||button.disabled)return;
  const label=button.textContent;button.disabled=true;button.classList.add('busy');button.textContent='启动中…';
  try{await startAutoLogin(button.dataset.loginId)}
  catch(err){alert('启动自动登录失败：'+err.message)}
  finally{button.disabled=false;button.classList.remove('busy');button.textContent=label}
});
document.getElementById('login-cancel').addEventListener('click',async()=>{
  const id=currentLoginId;currentLoginId=null;clearTimeout(loginPollTimer);
  if(id)await fetch('/api/outlook/logins/'+id+'/cancel',{method:'POST'}).catch(()=>{});
  hideLoginBox();
});
// ===== GPT 注册 / 登录任务(CDP 自动化,后台跑,轮询状态)=====
const gptBox=document.getElementById('gpt-box');
let gptTimer=null,gptId=null,gptKind=null;
function showGptBox(title,detail){document.getElementById('gpt-title').textContent=title;document.getElementById('gpt-status').textContent=detail||'';gptBox.classList.add('show')}
function hideGptBox(){gptBox.classList.remove('show')}
const GPT_LABEL={signup:'注册 GPT',login:'登录 Codex'};
const GPT_DONE={completed:'完成',failed:'失败',human_verification:'需人工(人机验证)',cancelled:'已取消'};
async function startGptTask(kind,id){
  const url=kind==='signup'?('/api/outlook/accounts/'+id+'/gpt-signup'):('/api/outlook/accounts/'+id+'/gpt-login');
  const rsp=await fetch(url,{method:'POST'});
  if(!rsp.ok)throw new Error(await rsp.text());
  const st=await rsp.json();
  gptId=st.id;gptKind=kind;
  showGptBox(GPT_LABEL[kind]+'…',st.message||'启动中…');
  pollGpt();
}
function pollGpt(){
  clearTimeout(gptTimer);
  gptTimer=setTimeout(async()=>{
    if(!gptId)return;
    try{
      const rsp=await fetch('/api/'+(gptKind==='signup'?'gpt-signup':'gpt-login')+'/'+gptId,{cache:'no-store'});
      if(!rsp.ok)throw new Error(await rsp.text());
      const st=await rsp.json();
      if(st.state==='running'){showGptBox(GPT_LABEL[gptKind]+'进行中…',st.message||'');pollGpt();return}
      gptId=null;
      const label=GPT_DONE[st.state]||st.state;
      showGptBox(GPT_LABEL[gptKind]+'：'+label, st.error||st.message||st.final_url||'');
      await loadAccounts().catch(()=>{});
      if(st.state==='completed')setTimeout(hideGptBox,6000);
    }catch(err){gptId=null;showGptBox('任务状态查询失败',(err&&err.message)||String(err))}
  },1500);
}
document.getElementById('account-rows').addEventListener('click',async event=>{
  const s=event.target.closest('.signup-btn'),l=event.target.closest('.gptlogin-btn');
  const btn=s||l;if(!btn||btn.disabled)return;
  if(gptId){alert('已有一个 GPT 任务在进行中，先等它结束。');return}
  const label=btn.textContent;btn.disabled=true;btn.classList.add('busy');btn.textContent='启动中…';
  try{await startGptTask(s?'signup':'login',btn.dataset.signupId||btn.dataset.gptloginId)}
  catch(err){alert((s?'注册':'登录')+'启动失败：'+err.message)}
  finally{btn.disabled=false;btn.classList.remove('busy');btn.textContent=label}
});
document.getElementById('gpt-cancel').addEventListener('click',async()=>{
  const id=gptId,kind=gptKind;gptId=null;clearTimeout(gptTimer);
  if(id)await fetch('/api/'+(kind==='signup'?'gpt-signup':'gpt-login')+'/'+id+'/cancel',{method:'POST'}).catch(()=>{});
  hideGptBox();
});
loadAccounts().catch(err=>alert('加载账号失败：'+err.message));
</script>
</body>
</html>
{{end}}
`
