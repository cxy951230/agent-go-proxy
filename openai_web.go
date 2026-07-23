package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (p *proxyServer) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	if err := baseTemplate.ExecuteTemplate(w, "openai", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *proxyServer) handleOpenAIAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	account, err := p.store.GetOpenAIAccount(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := baseTemplate.ExecuteTemplate(w, "openai-account", account); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *proxyServer) handleAPIOpenAIAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := p.store.ListOpenAIAccounts(r.Context())
	writeJSON(w, accounts, err)
}

func (p *proxyServer) handleAPIOpenAIAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	if err := p.store.DeleteOpenAIAccount(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func (p *proxyServer) handleAPIOpenAIAccountRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	// 同步刷新(补齐 Token 过期时间→按需刷新凭证→查额度),等完成再返回,
	// 让前端能像「模型」按钮那样显示加载态、拿到结果后再重载列表。Refresh 内部自带 30s 超时。
	p.openaiLogins.Refresh(id)
	writeJSON(w, map[string]any{"ok": true}, nil)
}

// handleAPIOpenAIAccountModels 同步拉取模型目录:调用方要立刻拿到结果填下拉框,
// 所以这里不像额度刷新那样异步。
func (p *proxyServer) handleAPIOpenAIAccountModels(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	if err := p.openaiLogins.RefreshModels(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	account, err := p.store.GetOpenAIAccount(r.Context(), id)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "models": account.Models, "models_at": account.ModelsAt}, nil)
}

// handleAPIOpenAIAccountSettings 保存页面选择的模型 / 推理强度 / 速度档位。
func (p *proxyServer) handleAPIOpenAIAccountSettings(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	var input struct {
		Model           string `json:"selected_model"`
		ReasoningEffort string `json:"selected_reasoning_effort"`
		ServiceTier     string `json:"selected_service_tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := p.store.UpdateOpenAIAccountSettings(r.Context(), id, input.Model, input.ReasoningEffort, input.ServiceTier); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "account not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func (p *proxyServer) handleAPIOpenAILoginStart(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if len(input.Name) > 128 {
		http.Error(w, "账号名称过长", http.StatusBadRequest)
		return
	}
	status, err := p.openaiLogins.Start(input.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, status, nil)
}

func (p *proxyServer) handleAPIOpenAILoginStatus(w http.ResponseWriter, r *http.Request) {
	status, ok := p.openaiLogins.Status(chi.URLParam(r, "id"))
	if !ok {
		http.Error(w, "login not found", http.StatusNotFound)
		return
	}
	writeJSON(w, status, nil)
}

func (p *proxyServer) handleAPIOpenAILoginCancel(w http.ResponseWriter, r *http.Request) {
	if err := p.openaiLogins.Cancel(chi.URLParam(r, "id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

const openAIHTML = `
{{define "openai"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>OPENAI 账号 · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--red:#c94040;--purple:#6750d8}
    *{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}.app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;align-self:flex-start;height:100vh;transition:width .16s ease,padding .16s ease}.sidebar .brand-row{display:flex;align-items:center;gap:8px;padding:6px 2px 18px 10px}.sidebar .brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;flex:1;white-space:nowrap;overflow:hidden}.sidebar-toggle{width:30px;height:30px;padding:0;border-radius:7px;border:1px solid var(--line);background:#fff;color:#667284;font-weight:800}.app.sidebar-collapsed .sidebar{width:58px;padding-left:10px;padding-right:10px}.app.sidebar-collapsed .brand{display:none}.app.sidebar-collapsed .nav-item{padding:10px 0;text-align:center;font-size:0}.app.sidebar-collapsed .nav-item::first-letter{font-size:15px}.app.sidebar-collapsed .sidebar-toggle{transform:rotate(180deg)}
    .nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px;text-decoration:none}.nav-item:hover{background:#eef2fa}.nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:22px 28px 56px}.top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}button,input{height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 14px;font:inherit;color:var(--text)}button{cursor:pointer}.btn-primary{background:var(--blue);border-color:var(--blue);color:#fff;font-weight:600}.btn-primary:hover{background:#265fd0}.panel{margin-top:18px;background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05);overflow-x:auto}
    tbody tr.row-link{cursor:pointer}tbody tr.row-link:hover{background:#fafcff}
    .login-box{display:none;padding:18px 22px;border-bottom:1px solid var(--line);background:#f8fbff}.login-box.show{display:block}.login-title{font-weight:600}.login-url{display:block;margin-top:8px;color:var(--blue);word-break:break-all}.small{font-size:12px;color:var(--muted)}table{width:100%;min-width:1180px;border-collapse:collapse}th,td{padding:16px 18px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle;white-space:nowrap}tbody tr:last-child td{border-bottom:0}th{font-size:12px;color:#606a7a;font-weight:600;background:#fbfcfe}.pill{display:inline-block;border-radius:6px;padding:3px 9px;font-size:13px;font-weight:600}.plan{color:var(--purple);background:#f4f1ff;border:1px solid #ddd4ff}.account-id{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;color:#526074;line-height:1.7}.delete-btn{height:32px;color:var(--red);border-color:#f0b3b3;background:#fff6f6}.refresh-btn{height:32px;margin-right:6px}.usage{font-size:12px;line-height:1.5;color:#526074}.empty{padding:28px;color:var(--muted);text-align:center}.notice{margin-top:12px;color:var(--muted)}
    /* 操作列:4 个按钮排成 2×2 等宽网格,比一字排开窄得多,也不会参差换行 */
    td.actions{white-space:nowrap;width:140px;min-width:140px}
    .act-grid{display:grid;grid-template-columns:repeat(2,1fr);gap:6px;width:128px}
    .act-grid>a,.act-grid>button{display:inline-flex;align-items:center;justify-content:center;
      width:100%;height:30px;margin:0;padding:0 6px;border:1px solid var(--line);border-radius:7px;
      background:#fff;color:var(--text);text-decoration:none;font:inherit;font-size:13px;
      white-space:nowrap}
    .act-grid>a:hover,.act-grid>button:hover{border-color:#b8c7dc;background:#f7f9fc}
    .act-grid>.delete-btn{color:var(--red);border-color:#f0b3b3;background:#fff6f6}
    .act-grid>.delete-btn:hover{background:#fff1f1;border-color:#e07d7d}
    .cfg-stack{display:grid;gap:6px;min-width:210px}
    .cfg-row{display:flex;gap:8px}
    .cfg-item{display:grid;gap:2px;font-size:11px;color:var(--muted);font-weight:600}
    .cfg-select{height:30px;width:100%;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 6px;font:inherit;font-size:12px;color:var(--text)}
    .cfg-stack>.cfg-select{font-size:13px;height:32px}
    .cfg-select:hover{border-color:#b8c7dc}
    .token-ok{color:#137a4d}.token-warn{color:#b06d12;font-weight:600}.token-bad{color:var(--red);font-weight:600}
    .mini-quota{min-width:200px}.mini-line{font-size:12px;font-weight:600;white-space:nowrap}.mini-label{display:inline-block;color:#4b5563;font-weight:600;font-size:11px;padding:1px 8px;margin-right:6px;border-radius:999px;background:#eef1f6}.mini-progress{height:7px;border-radius:5px;background:#e8edf5;overflow:hidden;margin:8px 0 0;max-width:200px}.mini-progress:last-child{margin-bottom:0}.mini-progress span{display:block;height:100%;background:var(--blue)}.detail-btn{display:inline-flex;height:32px;align-items:center;padding:0 12px;margin-right:6px;border:1px solid var(--line);border-radius:7px;color:var(--text);text-decoration:none;background:#fff}
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
      <a class="nav-item active" href="/openai">OPENAI</a>
      <a class="nav-item" href="/api-keys">API Key</a>
    </nav>
  </aside>
  <main class="page">
    <div class="top"><h1>OPENAI · GPT 账号</h1><button class="btn-primary" id="login-btn" type="button">+ 登录 GPT 账号</button></div>
    <div class="notice">登录由 Codex Bridge 发起。鉴权信息直接保存到 MySQL，页面和列表 API 不返回 token。</div>
    <section class="panel">
      <div class="login-box" id="login-box">
        <div class="login-title" id="login-title">等待浏览器登录</div>
        <a class="login-url" id="login-url" target="_blank" rel="noreferrer"></a>
        <div class="small" id="login-status"></div>
      </div>
      <table>
        <thead><tr><th>名称</th><th>邮箱</th><th>ChatGPT Account ID</th><th>套餐</th><th>额度使用</th><th>Token 过期</th><th>模型配置</th><th>操作</th></tr></thead>
        <tbody id="account-rows"></tbody>
      </table>
      <div class="empty" id="empty">暂无 GPT 账号，点击右上角开始登录。</div>
    </section>
  </main>
</div>
<script>
const appEl=document.querySelector('.app');
if(localStorage.getItem('sidebarCollapsed')==='1')appEl.classList.add('sidebar-collapsed');
document.getElementById('sidebar-toggle').addEventListener('click',()=>{appEl.classList.toggle('sidebar-collapsed');localStorage.setItem('sidebarCollapsed',appEl.classList.contains('sidebar-collapsed')?'1':'0')});
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const short=v=>{v=String(v||'');return v.length>22?v.slice(0,12)+'…'+v.slice(-6):v};
const fmtTime=v=>v?new Date(v).toLocaleString('zh-CN',{hour12:false}):'';
async function loadAccounts(){
  const rsp=await fetch('/api/openai/accounts',{cache:'no-store'});if(!rsp.ok)throw new Error(await rsp.text());
  const items=await rsp.json();const rows=document.getElementById('account-rows');
  rows.innerHTML=(items||[]).map(item=>'<tr class="row-link" data-href="/stats/tokens?dim=account&id='+item.id+'&name='+encodeURIComponent(item.name||'')+'"><td>'+esc(item.name||'-')+'</td><td>'+esc(item.email||'-')+'</td><td><span class="account-id" title="'+esc(item.account_id)+'">'+esc(short(item.account_id))+'</span></td><td><span class="pill plan">'+esc(item.plan_type||'unknown')+'</span></td><td class="usage">'+usageText(item)+'</td><td>'+tokenText(item)+'</td>'+settingsCells(item)+'<td class="actions"><div class="act-grid"><a class="detail-btn" href="/openai/accounts/'+item.id+'">详情</a><button class="refresh-btn" data-refresh-id="'+item.id+'" type="button" title="刷新额度与 Token">额度</button><button class="models-btn" data-models-id="'+item.id+'" type="button" title="重新拉取可用模型">模型</button><button class="delete-btn" data-id="'+item.id+'" type="button">删除</button></div></td></tr>').join('');
  document.getElementById('empty').style.display=items&&items.length?'none':'block';
}
// tokenText 展示 access_token 过期时间:已过期/即将过期(1小时内)标红或标黄;
// refresh_error 非空说明刷新被上游永久拒绝,必须重新登录。
function tokenText(item){
  if(item.refresh_error)return '<span class="token-bad" title="'+esc(item.refresh_error)+'">需重新登录</span>';
  // Go 的 time.Time 零值会被序列化成 0001-01-01,omitempty 对结构体不生效,
  // 所以这里按年份兜底判断「未知」,不能只判空。
  const at=item.token_expires_at?new Date(item.token_expires_at):null;
  if(!at||isNaN(at.getTime())||at.getFullYear()<2000)return '<span class="small">未知</span>';
  const left=at.getTime()-Date.now();
  const cls=left<=0?'token-bad':(left<3600000?'token-warn':'token-ok');
  return '<span class="'+cls+'">'+esc(fmtTime(item.token_expires_at))+'</span>';
}
// modelList 取缓存的模型目录,只保留 picker 里可见的(与 CLI /model 一致)。
function modelList(item){
  const models=(item.models&&item.models.models)||[];
  return models.filter(m=>m&&m.show_in_picker!==false);
}
function currentModel(item){
  const models=modelList(item);
  if(!models.length)return null;
  return models.find(m=>m.model===item.selected_model)
    ||models.find(m=>m.is_default)
    ||models[0];
}
const EFFORT_LABEL={none:'无',minimal:'极低',low:'低',medium:'中',high:'高',xhigh:'极高',max:'最大',ultra:'Ultra'};
// 速度档位:目录里的 service_tiers 之外,总有一个 "default"(标准)。
// 该账号/模型没有额外档位时就只有标准一项。
function tierOptions(model){
  const tiers=[{id:'default',name:'标准'}];
  (model&&model.service_tiers||[]).forEach(t=>{if(t&&t.id&&t.id!=='default')tiers.push({id:t.id,name:t.name||t.id})});
  return tiers;
}
function selectHTML(id,field,options,selected,disabled){
  if(disabled)return '<span class="small">未拉取</span>';
  if(!options.length)return '<span class="small">-</span>';
  return '<select class="cfg-select" data-account-id="'+id+'" data-field="'+field+'">'+
    options.map(o=>'<option value="'+esc(o.value)+'"'+(o.value===selected?' selected':'')+'>'+esc(o.label)+'</option>').join('')+'</select>';
}
// settingsCells 渲染「模型配置」单元格:模型在上,推理强度与速度并排在下。
// 后两者的可选项取决于当前选中的模型,所以换模型后要重新渲染整行。
function settingsCells(item){
  const models=modelList(item);
  if(!models.length)return '<td><span class="small">未拉取，点「拉取模型」</span></td>';
  const model=currentModel(item);
  const modelOpts=models.map(m=>({value:m.model,label:m.display_name||m.model}));
  const effortOpts=((model&&model.supported_reasoning_efforts)||[]).map(e=>({value:e.effort,label:EFFORT_LABEL[e.effort]||e.effort}));
  const tierOpts=tierOptions(model).map(t=>({value:t.id,label:t.name}));
  const effort=item.selected_reasoning_effort||(model&&model.default_reasoning_effort)||'';
  const tier=item.selected_service_tier||(model&&model.default_service_tier)||'default';
  return '<td><div class="cfg-stack">'+
    selectHTML(item.id,'selected_model',modelOpts,model?model.model:'',false)+
    '<div class="cfg-row">'+
      '<label class="cfg-item"><span>推理</span>'+selectHTML(item.id,'selected_reasoning_effort',effortOpts,effort,false)+'</label>'+
      '<label class="cfg-item"><span>速度</span>'+selectHTML(item.id,'selected_service_tier',tierOpts,tier,false)+'</label>'+
    '</div>'+
  '</div></td>';
}
function usageText(item){
  if(item.status_error)return '<span title="'+esc(item.status_error)+'">查询失败</span>';
  const status=item.status||{};const usage=status.usage||status;const rate=usage.rate_limit||usage.rate_limits||status.rate_limit||status.rate_limits||{};const primary=rate.primary_window||rate.primaryWindow||{};
  const secondary=rate.secondary_window||rate.secondaryWindow;const windows=[primary,secondary].filter(window=>window&&window.used_percent!==undefined);
  if(!windows.length)return '<span class="small">未查询</span>';
  return '<div class="mini-quota">'+windows.map(window=>{const used=Number(window.used_percent??0);return '<div class="mini-line"><span class="mini-label">'+esc(windowLabel(window))+'</span><span>已用 '+esc(used)+'% · 剩余 '+esc(Math.max(0,100-used))+'%</span></div><div class="mini-progress"><span style="width:'+Math.min(100,Math.max(0,used))+'%"></span></div>'}).join('')+'</div>';
}
function windowLabel(window){const seconds=Number(window.limit_window_seconds);if(seconds>=2419200&&seconds<=2764800)return '月';if(seconds>=518400&&seconds<=691200)return '周';if(seconds>=14400&&seconds<=21600)return '5小时';if(seconds>=3600&&seconds%3600===0)return seconds/3600+'小时';if(seconds>=86400&&seconds%86400===0)return seconds/86400+'天';return seconds?Math.round(seconds/3600)+'小时':'-'}
async function beginLogin(){
  const name=prompt('账号名称（可选）','');if(name===null)return;
  const button=document.getElementById('login-btn');button.disabled=true;
  try{
    const rsp=await fetch('/api/openai/logins',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:name.trim()})});
    if(!rsp.ok)throw new Error(await rsp.text());const task=await rsp.json();
    const box=document.getElementById('login-box');const link=document.getElementById('login-url');box.classList.add('show');link.href=task.auth_url;link.textContent=task.auth_url;document.getElementById('login-status').textContent='请在浏览器中完成 ChatGPT 登录…';
    window.open(task.auth_url,'_blank','noopener');pollLogin(task.id);
  }catch(err){alert('启动登录失败：'+err.message);button.disabled=false}
}
async function pollLogin(id){
  try{
    const rsp=await fetch('/api/openai/logins/'+encodeURIComponent(id),{cache:'no-store'});if(!rsp.ok)throw new Error(await rsp.text());const task=await rsp.json();
    if(task.state==='waiting'){setTimeout(()=>pollLogin(id),1000);return}
    document.getElementById('login-btn').disabled=false;
    if(task.state==='completed'){const box=document.getElementById('login-box');const link=document.getElementById('login-url');box.classList.remove('show');link.removeAttribute('href');link.textContent='';document.getElementById('login-status').textContent='';await loadAccounts();return}
    document.getElementById('login-title').textContent='登录未完成';document.getElementById('login-status').textContent=task.error||task.state;
  }catch(err){document.getElementById('login-btn').disabled=false;document.getElementById('login-status').textContent='查询登录状态失败：'+err.message}
}
document.getElementById('login-btn').addEventListener('click',beginLogin);
document.getElementById('account-rows').addEventListener('click',async event=>{const button=event.target.closest('.delete-btn');if(!button||!confirm('确认删除这个 GPT 账号及其数据库鉴权信息吗？'))return;const rsp=await fetch('/api/openai/accounts/'+button.dataset.id,{method:'DELETE'});if(!rsp.ok){alert('删除失败：'+await rsp.text());return}loadAccounts()});
// 整行点击跳转到该账号的 token 消耗统计页(点到按钮/链接/下拉等交互控件时不跳转)。
document.getElementById('account-rows').addEventListener('click',event=>{if(event.target.closest('a,button,input,select'))return;const row=event.target.closest('tr[data-href]');if(row)location.href=row.dataset.href});
document.getElementById('account-rows').addEventListener('click',async event=>{
  const button=event.target.closest('.refresh-btn');if(!button)return;
  const label=button.textContent;button.disabled=true;button.textContent='…';
  try{
    const rsp=await fetch('/api/openai/accounts/'+button.dataset.refreshId+'/refresh',{method:'POST'});
    if(!rsp.ok)throw new Error(await rsp.text());
    await loadAccounts();
  }catch(err){alert('刷新额度失败：'+err.message)}
  finally{button.disabled=false;button.textContent=label}
});
// 拉取模型目录:同步等结果,拿到后重渲染以填充三个下拉。
document.getElementById('account-rows').addEventListener('click',async event=>{
  const button=event.target.closest('.models-btn');if(!button)return;
  const label=button.textContent;button.disabled=true;button.textContent='…';
  try{
    const rsp=await fetch('/api/openai/accounts/'+button.dataset.modelsId+'/models',{method:'POST'});
    if(!rsp.ok)throw new Error(await rsp.text());
    await loadAccounts();
  }catch(err){alert('拉取模型失败：'+err.message)}
  finally{button.disabled=false;button.textContent=label}
});
// 三个下拉改动即保存。换模型会让推理强度/速度的可选项变化,所以保存后重新加载整行。
document.getElementById('account-rows').addEventListener('change',async event=>{
  const select=event.target.closest('.cfg-select');if(!select)return;
  const row=select.closest('tr');
  const value=field=>{const el=row.querySelector('.cfg-select[data-field="'+field+'"]');return el?el.value:''};
  const payload={
    selected_model:value('selected_model'),
    selected_reasoning_effort:value('selected_reasoning_effort'),
    selected_service_tier:value('selected_service_tier')
  };
  // 换模型时清空另两项,让页面回落到新模型自己的默认值。
  if(select.dataset.field==='selected_model'){payload.selected_reasoning_effort='';payload.selected_service_tier=''}
  const rsp=await fetch('/api/openai/accounts/'+select.dataset.accountId+'/settings',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});
  if(!rsp.ok){alert('保存失败：'+await rsp.text());return}
  loadAccounts().catch(()=>{});
});
loadAccounts().catch(err=>alert('加载账号失败：'+err.message));
</script>
</body>
</html>
{{end}}
`

const openAIAccountHTML = `
{{define "openai-account"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
  <title>GPT 账号额度详情 · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--red:#c94040;--purple:#6750d8}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif}.app{display:flex;min-height:100vh}.sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;height:100vh}.brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;padding:12px}.nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px;text-decoration:none}.nav-item:hover{background:#eef2fa}.nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}.page{flex:1;min-width:0;padding:22px 28px 56px}.top{display:flex;align-items:center;gap:14px}.top h1{font-size:18px;margin:0;flex:1}.back{color:var(--blue);text-decoration:none}.account-summary{margin-top:18px;padding:16px 18px;background:#fff;border:1px solid var(--line);border-radius:8px;display:flex;gap:28px;flex-wrap:wrap}.summary-item span{display:block;font-size:12px;color:var(--muted)}.summary-item strong{display:block;margin-top:3px}.quota-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:14px;margin-top:16px}.quota-card{border:1px solid var(--line);border-radius:8px;padding:15px 17px;background:#fff;min-width:0}.quota-card h2{font-size:14px;margin:0 0 10px}.kv{display:grid;grid-template-columns:minmax(105px,auto) minmax(0,1fr);gap:6px 12px;font-size:12px}.kv .key{color:var(--muted)}.kv div:nth-child(even){overflow-wrap:anywhere}.progress{height:9px;border-radius:6px;background:#e5eaf3;overflow:hidden;margin:10px 0}.progress span{display:block;height:100%;background:var(--blue)}.window-title{font-size:17px;font-weight:700}.window-label{float:right;font-size:12px;color:var(--muted);font-weight:600}.credit{padding:10px 0;border-top:1px solid #e8ebf1}.credit:first-of-type{border-top:0}.raw-status{margin-top:16px;background:#fff;border:1px solid var(--line);border-radius:8px;padding:14px 16px}.raw-status summary{cursor:pointer;color:var(--blue);font-weight:600}.raw-status pre{white-space:pre-wrap;word-break:break-word;max-height:520px;overflow:auto;background:#111827;color:#d7e0ee;padding:14px;border-radius:7px;font-size:11px}.error{color:var(--red)}
  </style>
</head>
<body><div class="app"><aside class="sidebar"><div class="brand">AGENT-GO-PROXY</div><nav><a class="nav-item" href="/">Dashboard</a><a class="nav-item" href="/routes">路由</a><a class="nav-item" href="/chains">链式代理</a><a class="nav-item active" href="/openai">OPENAI</a><a class="nav-item" href="/api-keys">API Key</a></nav></aside><main class="page"><div class="top"><a class="back" href="/openai">← 返回账号列表</a><h1>GPT 账号额度详情</h1></div><div id="content"></div></main></div>
<script>
const account={{toJSON .}};
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const yesNo=value=>value===true?'是':value===false?'否':value??'-';
const valueText=value=>value===null||value===undefined?'-':typeof value==='object'?JSON.stringify(value):String(value);
const kv=entries=>'<div class="kv">'+entries.map(([key,value])=>'<div class="key">'+esc(key)+'</div><div>'+esc(valueText(value))+'</div>').join('')+'</div>';
const durationText=seconds=>{seconds=Number(seconds);if(!Number.isFinite(seconds))return '-';const days=Math.floor(seconds/86400),hours=Math.floor(seconds%86400/3600),minutes=Math.floor(seconds%3600/60);return (days?days+'天 ':'')+(hours?hours+'小时 ':'')+minutes+'分钟'};
const fmtTime=value=>value?new Date(value).toLocaleString('zh-CN',{hour12:false}):'-';
function windowLabel(window){const seconds=Number(window.limit_window_seconds);if(seconds>=2419200&&seconds<=2764800)return '月';if(seconds>=518400&&seconds<=691200)return '周';if(seconds>=14400&&seconds<=21600)return '5小时';if(seconds>=3600&&seconds%3600===0)return seconds/3600+'小时';if(seconds>=86400&&seconds%86400===0)return seconds/86400+'天';return seconds?Math.round(seconds/3600)+'小时':'-'}
function windowCard(title,window){if(!window)return '<section class="quota-card"><h2>'+esc(title)+'</h2><span style="color:var(--muted)">无此额度窗口</span></section>';const used=Number(window.used_percent??0),resetAt=window.reset_at?new Date(Number(window.reset_at)*1000).toLocaleString('zh-CN',{hour12:false}):'-';return '<section class="quota-card"><h2>'+esc(title)+'</h2><div class="window-title">已用 '+esc(used)+'% · 剩余 '+esc(Math.max(0,100-used))+'% <span class="window-label">'+esc(windowLabel(window))+'</span></div><div class="progress"><span style="width:'+Math.min(100,Math.max(0,used))+'%"></span></div>'+kv([['周期',durationText(window.limit_window_seconds)],['距离重置',durationText(window.reset_after_seconds)],['重置时间',resetAt],['窗口秒数',window.limit_window_seconds],['剩余秒数',window.reset_after_seconds]])+'</section>'}
function render(){const status=account.status||{},usage=status.usage||{},rate=usage.rate_limit||{},credits=usage.credits||{},resets=usage.rate_limit_reset_credits||{},spend=usage.spend_control||{},detail=status.rate_limit_reset_credit_details||{};if(account.status_error){document.getElementById('content').innerHTML='<div class="account-summary error">额度查询失败：'+esc(account.status_error)+'</div>';return}const resetList=(detail.credits||[]).map((credit,index)=>'<div class="credit"><strong>'+esc(credit.title||('重置额度 '+(index+1)))+'</strong>'+kv([['状态',credit.status],['类型',credit.reset_type],['支持当前套餐',yesNo(credit.is_supported_by_plan)],['授予时间',fmtTime(credit.granted_at)],['到期时间',fmtTime(credit.expires_at)],['兑换开始',fmtTime(credit.redeem_started_at)],['兑换完成',fmtTime(credit.redeemed_at)],['说明',credit.description],['来源',credit.profile_user_id],['ID',credit.id]])+'</div>').join('');document.getElementById('content').innerHTML='<div class="account-summary"><div class="summary-item"><span>名称</span><strong>'+esc(account.name||'-')+'</strong></div><div class="summary-item"><span>邮箱</span><strong>'+esc(account.email||usage.email||'-')+'</strong></div><div class="summary-item"><span>套餐</span><strong>'+esc(account.plan_type||usage.plan_type||'-')+'</strong></div><div class="summary-item"><span>ChatGPT Account ID</span><strong>'+esc(account.account_id||'-')+'</strong></div><div class="summary-item"><span>额度更新时间</span><strong>'+esc(fmtTime(account.status_at))+'</strong></div></div><div class="quota-grid">'+windowCard('主额度窗口',rate.primary_window)+windowCard('次额度窗口',rate.secondary_window)+'<section class="quota-card"><h2>额度状态</h2>'+kv([['允许调用',yesNo(rate.allowed)],['是否耗尽',yesNo(rate.limit_reached)],['耗尽类型',usage.rate_limit_reached_type],['可用重置次数',resets.available_count],['当前套餐可用重置',resets.applicable_available_count],['额外额度',usage.additional_rate_limits],['Code Review 额度',usage.code_review_rate_limit]])+'</section><section class="quota-card"><h2>Credits / 余额</h2>'+kv([['有余额',yesNo(credits.has_credits)],['余额',credits.balance],['无限额度',yesNo(credits.unlimited)],['超额限制已触发',yesNo(credits.overage_limit_reached)],['预估云端消息',credits.approx_cloud_messages],['预估本地消息',credits.approx_local_messages]])+'</section><section class="quota-card"><h2>消费控制与活动</h2>'+kv([['消费上限已触发',yesNo(spend.reached)],['个人上限',spend.individual_limit],['促销信息',usage.promo]])+'</section><section class="quota-card"><h2>后端账号信息</h2>'+kv([['邮箱',usage.email],['套餐',usage.plan_type],['Account ID',usage.account_id],['User ID',usage.user_id]])+'</section><section class="quota-card"><h2>可用重置额度（'+esc(detail.available_count??0)+'）</h2>'+(resetList||'<span style="color:var(--muted)">无可用重置额度</span>')+kv([['累计获得',detail.total_earned_count]])+'</section></div><details class="raw-status"><summary>查看 Bridge /status 完整原始数据</summary><pre>'+esc(JSON.stringify(status,null,2))+'</pre></details>'}
render();
</script></body></html>
{{end}}
`
