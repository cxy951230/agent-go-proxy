package main

const herosmsHTML = `
{{define "herosms"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>HeroSMS 接码 · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--orange:#d46f1e;--red:#c94040;--purple:#6750d8}
    *{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}.app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;align-self:flex-start;height:100vh;transition:width .16s ease,padding .16s ease}.sidebar .brand-row{display:flex;align-items:center;gap:8px;padding:6px 2px 18px 10px}.sidebar .brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;flex:1;white-space:nowrap;overflow:hidden}.sidebar-toggle{width:30px;height:30px;padding:0;border-radius:7px;border:1px solid var(--line);background:#fff;color:#667284;font-weight:800}.app.sidebar-collapsed .sidebar{width:58px;padding-left:10px;padding-right:10px}.app.sidebar-collapsed .brand{display:none}.app.sidebar-collapsed .nav-item{padding:10px 0;text-align:center;font-size:0}.app.sidebar-collapsed .nav-item::first-letter{font-size:15px}.app.sidebar-collapsed .sidebar-toggle{transform:rotate(180deg)}
    .nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px;text-decoration:none}.nav-item:hover{background:#eef2fa}.nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:22px 28px 56px}.top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}
    button,input,select{height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 14px;font:inherit;color:var(--text)}button{cursor:pointer;transition:transform .06s ease,box-shadow .12s ease,background .12s ease}button:hover:not(:disabled){background:#f3f6fc}button:active:not(:disabled){transform:translateY(1px) scale(.985)}button:disabled{opacity:.5;cursor:not-allowed}
    button.busy::before{content:'';display:inline-block;width:11px;height:11px;margin-right:7px;vertical-align:-1px;border:2px solid currentColor;border-top-color:transparent;border-radius:50%;animation:spin .7s linear infinite}@keyframes spin{to{transform:rotate(360deg)}}
    .btn-primary{background:var(--blue);border-color:var(--blue);color:#fff;font-weight:600}.btn-primary:hover:not(:disabled){background:#265fd0}
    .balance-pill{font-size:13px;font-weight:600;color:var(--green);background:#e9f7ef;border:1px solid #bfe6cf;border-radius:999px;padding:5px 13px}
    .panel{margin-top:18px;background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05);overflow-x:auto}
    .panel-head{display:flex;align-items:center;gap:10px;padding:14px 18px;border-bottom:1px solid var(--line);flex-wrap:wrap}.panel-head h2{font-size:14px;margin:0;font-weight:600}.panel-head .small,.small{font-size:12px;color:var(--muted)}
    .step{display:inline-flex;align-items:center;justify-content:center;width:22px;height:22px;border-radius:50%;background:var(--blue);color:#fff;font-size:12px;font-weight:700;margin-right:8px}
    .cfg-row{display:flex;gap:10px;align-items:center;padding:16px 18px;flex-wrap:wrap}.cfg-row input{flex:1;min-width:280px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}
    .row{display:flex;gap:10px;align-items:center;padding:16px 18px;flex-wrap:wrap}
    .filter{height:38px;width:120px;padding:0 10px}
    #svc-search{height:38px;width:220px}#svc-select{height:38px;min-width:260px;max-width:420px}
    table{width:100%;border-collapse:collapse;min-width:680px}th,td{padding:12px 18px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle;white-space:nowrap}tbody tr:last-child td{border-bottom:0}th{font-size:12px;color:#606a7a;font-weight:600;background:#fbfcfe}
    tbody tr:hover{background:#fafcff}tbody tr.sel{background:#eff5ff}
    .mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px;color:#526074}
    .muted{color:var(--muted)}.price{font-weight:600;color:var(--orange)}
    .pick-btn{height:30px;padding:0 12px;color:var(--blue);border-color:#b9ccf5;background:#f4f8ff;font-size:13px}
    .empty{padding:26px;text-align:center;color:var(--muted)}
    .notice{margin-top:12px;color:var(--muted);font-size:13px}
    .buybar{display:flex;gap:12px;align-items:center;padding:14px 18px;background:#f7faff;border-bottom:1px solid var(--line);flex-wrap:wrap}
    .buybar.hidden{display:none}.buybar .qty{width:70px;height:38px;text-align:center}
    /* tab 切换 */
    .tabs{display:flex;gap:2px;margin-top:18px;border-bottom:1px solid var(--line)}
    .tab{height:auto;padding:10px 18px;border:none;border-bottom:2px solid transparent;background:none;font-weight:600;color:var(--muted);border-radius:0}
    .tab:hover{background:none;color:var(--text)}.tab.active{color:var(--blue);border-bottom-color:var(--blue)}
    .tab .cnt{display:inline-block;min-width:18px;margin-left:6px;padding:0 6px;font-size:11px;line-height:18px;border-radius:999px;background:#eef1f6;color:#4b5563;font-weight:700}
    .tab.active .cnt{background:#e6efff;color:var(--blue)}
    .tabpane{display:none}.tabpane.active{display:block}
    /* 我的号码 */
    .act-row{display:grid;grid-template-columns:150px 1fr 130px 200px;gap:12px;align-items:center;padding:12px 18px;border-bottom:1px solid var(--line)}
    .act-row:last-child{border-bottom:0}
    .code-box{font-size:18px;font-weight:700;letter-spacing:2px;color:var(--green)}
    .act-status{font-size:12px;color:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
    .act-btns{display:flex;gap:6px;justify-content:flex-end}.act-btns button{height:32px;padding:0 10px;font-size:13px}
    .cancel-btn{color:var(--red);border-color:#f0b3b3;background:#fff6f6}.cancel-btn:hover:not(:disabled){background:#fff1f1}
    .finish-btn{color:var(--green);border-color:#bfe6cf;background:#f1fbf5}
    .tag{display:inline-block;font-size:11px;font-weight:600;padding:2px 8px;border-radius:999px;background:#eef1f6;color:#4b5563}
    .tag.ok{background:#e9f7ef;color:var(--green)}.tag.cancel{background:#fdecec;color:var(--red)}
    .country-chips{display:flex;gap:8px;align-items:center;flex-wrap:wrap;min-height:32px}.country-chip{display:inline-flex;align-items:center;gap:7px;height:30px;padding:0 10px;border-radius:999px;background:#eef5ff;color:var(--blue);border:1px solid #bfd0f7;font-weight:600}.country-chip button{height:20px;width:20px;padding:0;border-radius:50%;border:0;background:#dfe9ff;color:var(--blue);font-weight:800;line-height:20px}.limit-btn.on{color:var(--green);border-color:#bfe6cf;background:#f1fbf5}
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
      <a class="nav-item" href="/outlook">OUTLOOK</a>
      <a class="nav-item" href="/api-keys">API Key</a>
      <a class="nav-item active" href="/herosms">HeroSMS</a>
    </nav>
  </aside>
  <main class="page">
    <div class="top"><h1>HeroSMS · 接码</h1><span class="balance-pill" id="balance">余额 —</span><button id="balance-btn" type="button" title="刷新账户余额">刷新余额</button></div>
    <div class="notice">买号真实扣费。「我的号码」里是你买过的号（存在本地，切菜单/刷新都在），每 3 秒自动拉验证码；收到码用完点「完成」，不用了点「取消」（满 2 分钟才可取消）。</div>

    <section class="panel">
      <div class="panel-head"><h2>API Key 配置</h2><span class="small" id="cfg-hint"></span></div>
      <div class="cfg-row">
        <input id="cfg-key" type="text" placeholder="HeroSMS API Key" autocomplete="off">
        <button class="btn-primary" id="cfg-save" type="button">保存</button>
        <button id="cfg-reset" type="button" title="清空为使用 skill 内置默认 Key">恢复默认</button>
      </div>
      <div class="cfg-row" style="margin-top:12px">
        <span class="small" style="min-width:150px">Codex 登录买号最高单价</span>
        <input id="cfg-gpt-price" type="number" min="0.0001" step="0.0001" placeholder="0.2000" style="max-width:220px">
        <span class="small">OUTLOOK 页「登录 Codex」自动买号时不会超过这个价格。</span>
      </div>
      <div class="cfg-row" style="margin-top:12px">
        <span class="small" style="min-width:150px">Codex 登录库存限制最小值</span>
        <input id="cfg-gpt-min-count" type="number" min="0" step="100" placeholder="2000" style="max-width:220px">
        <span class="small">OUTLOOK 页「登录 Codex」自动买号时只选库存大于这个值的国家/价格。</span>
      </div>
      <div class="cfg-row" style="margin-top:12px">
        <span class="small" style="min-width:150px">Codex 登录限定国家</span>
        <div id="cfg-gpt-countries" class="country-chips"><span class="small">未限定，按原逻辑自动选择。</span></div>
        <span class="small">在「买号 / 接码」的国家列表点「限定」加入；每个国家最多试 3 个，全部试完即结束。</span>
      </div>
    </section>

    <div class="tabs">
      <button class="tab active" data-tab="numbers" type="button">我的号码<span class="cnt" id="num-cnt">0</span></button>
      <button class="tab" data-tab="buy" type="button">买号 / 接码</button>
      <button class="tab" data-tab="logs" type="button">购买记录</button>
      <button class="tab" data-tab="blacklist" type="button">国家拉黑</button>
    </div>

    <!-- 我的号码 -->
    <div class="tabpane active" id="pane-numbers">
      <section class="panel">
        <div class="panel-head"><h2>我的号码</h2><span class="small">买过的号在这（本地保存）。收到码用完点「完成」，不用点「取消」（满 120s）。</span><span style="flex:1"></span><button id="clear-done" type="button" title="移除已完成/已取消的记录">清除已结束</button></div>
        <div id="act-list"></div>
        <div class="empty" id="act-empty">还没有买过号。去「买号 / 接码」tab 买一个。</div>
      </section>
    </div>

    <!-- 买号 / 接码 -->
    <div class="tabpane" id="pane-buy">
      <section class="panel">
        <div class="panel-head"><h2><span class="step">1</span>选服务</h2></div>
        <div class="row">
          <input id="svc-search" type="text" placeholder="搜服务名，如 openai / google">
          <select id="svc-select"><option value="">加载服务列表…</option></select>
          <span class="small" id="svc-count"></span>
        </div>
      </section>
      <section class="panel">
        <div class="panel-head">
          <h2><span class="step">2</span>查价选国家</h2>
          <span class="small">最高单价</span><input class="filter" id="f-price" type="number" step="0.01" value="0.2">
          <span class="small">库存 &gt;</span><input class="filter" id="f-count" type="number" step="100" value="2000">
          <button id="offers-btn" type="button">查询价格</button>
        </div>
        <div class="buybar hidden" id="buybar">
          <span class="step">3</span>
          <span>已选：<b id="bb-country">—</b> <span class="mono" id="bb-cid"></span> · <span class="price" id="bb-price"></span></span>
          <span class="small">数量</span><input class="qty" id="bb-qty" type="number" min="1" max="10" value="1">
          <button class="btn-primary" id="bb-buy" type="button">购买</button>
          <span class="small" id="bb-hint"></span>
        </div>
        <table>
          <thead><tr><th>国家</th><th>ID</th><th>单价(USD)</th><th>库存</th><th>操作</th></tr></thead>
          <tbody id="offer-rows"></tbody>
        </table>
        <div class="empty" id="offer-empty">先选服务，再点「查询价格」。</div>
      </section>
    </div>

    <!-- 购买记录 -->
    <div class="tabpane" id="pane-logs">
      <section class="panel">
        <div class="panel-head">
          <h2>购买记录</h2><span class="small">只记录登录/接码流程里实际使用过的号码；只买了但没试过的号码不会出现在这里。</span><span style="flex:1"></span>
          <select id="log-svc" class="filter"><option value="">全部业务</option></select>
          <button id="log-refresh" type="button">刷新</button>
        </div>
        <table>
          <thead><tr><th>时间</th><th>业务</th><th>国家</th><th>号码</th><th>费用</th><th>结果</th><th>原因</th><th>来源</th></tr></thead>
          <tbody id="log-rows"></tbody>
        </table>
        <div class="empty" id="log-empty">暂无记录。</div>
      </section>
    </div>

    <!-- 国家拉黑 -->
    <div class="tabpane" id="pane-blacklist">
      <section class="panel">
        <div class="panel-head"><h2><span class="step">1</span>选择业务</h2><span class="small">拉黑后，下次购买这个业务的号码时不会选择该国家。</span></div>
        <div class="row">
          <input id="bl-svc-search" type="text" placeholder="搜服务名，如 openai / google">
          <select id="bl-svc-select"><option value="">加载服务列表…</option></select>
          <button id="bl-load" type="button">查询国家</button>
          <span class="small" id="bl-count"></span>
        </div>
      </section>
      <section class="panel">
        <div class="panel-head"><h2><span class="step">2</span>国家列表</h2><span class="small">这里会展示该业务可用国家，包括已拉黑国家。</span></div>
        <table>
          <thead><tr><th>国家</th><th>ID</th><th>单价(USD)</th><th>库存</th><th>状态</th><th>操作</th></tr></thead>
          <tbody id="bl-rows"></tbody>
        </table>
        <div class="empty" id="bl-empty">先选择业务，再点「查询国家」。</div>
      </section>
    </div>

  </main>
</div>
<script>
const appEl=document.querySelector('.app');
if(localStorage.getItem('sidebarCollapsed')==='1')appEl.classList.add('sidebar-collapsed');
document.getElementById('sidebar-toggle').addEventListener('click',()=>{appEl.classList.toggle('sidebar-collapsed');localStorage.setItem('sidebarCollapsed',appEl.classList.contains('sidebar-collapsed')?'1':'0')});
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
async function jpost(url,body){const r=await fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body||{})});if(!r.ok)throw new Error(await r.text());return r.json()}
async function jget(url){const r=await fetch(url,{cache:'no-store'});if(!r.ok)throw new Error(await r.text());return r.json()}

// ===== tab 切换 =====
function switchTab(name){
  document.querySelectorAll('.tab').forEach(t=>t.classList.toggle('active',t.dataset.tab===name));
  document.querySelectorAll('.tabpane').forEach(p=>p.classList.toggle('active',p.id==='pane-'+name));
}
document.querySelectorAll('.tab').forEach(t=>t.addEventListener('click',()=>switchTab(t.dataset.tab)));

// ===== 配置 =====
let gptCountryLimits=[];
function countryKey(v){return String(v??'').trim()}
function countryLabel(c){return c.name?c.name+' ('+c.id+')':c.id}
function serializeCountryLimits(){return gptCountryLimits.map(c=>c.id).join(',')}
function renderCountryLimits(){
  const box=document.getElementById('cfg-gpt-countries');
  if(!box)return;
  if(!gptCountryLimits.length){box.innerHTML='<span class="small">未限定，按原逻辑自动选择。</span>';return}
  box.innerHTML=gptCountryLimits.map((c,i)=>'<span class="country-chip">'+esc(countryLabel(c))+'<button type="button" data-rm-limit="'+i+'" title="取消限定">×</button></span>').join('');
  document.querySelectorAll('.limit-btn').forEach(btn=>btn.classList.toggle('on',gptCountryLimits.some(c=>c.id===btn.dataset.cid)));
}
function setCountryLimitsFromString(raw){
  gptCountryLimits=[];
  String(raw||'').split(',').map(x=>x.trim()).filter(Boolean).forEach(id=>{
    if(!gptCountryLimits.some(c=>c.id===id))gptCountryLimits.push({id,name:''});
  });
  renderCountryLimits();
}
async function saveHeroSMSConfig(){
  return jpost('/api/herosms/config',{api_key:document.getElementById('cfg-key').value.trim(),gpt_login_max_price:Number(document.getElementById('cfg-gpt-price').value||0.2),gpt_login_min_count:Math.max(0,parseInt(document.getElementById('cfg-gpt-min-count').value,10)||0),gpt_login_countries:serializeCountryLimits()});
}
function toggleCountryLimit(o){
  const id=countryKey(o.country_id), name=String(o.country_name||'').trim(); if(!id)return;
  const idx=gptCountryLimits.findIndex(c=>c.id===id);
  if(idx>=0)gptCountryLimits.splice(idx,1); else gptCountryLimits.push({id,name});
  renderCountryLimits();
  saveHeroSMSConfig().catch(e=>alert('保存限定国家失败：'+e.message));
}
document.addEventListener('click',event=>{
  const rm=event.target.closest('[data-rm-limit]'); if(!rm)return;
  gptCountryLimits.splice(+rm.dataset.rmLimit,1);renderCountryLimits();saveHeroSMSConfig().catch(e=>alert('保存限定国家失败：'+e.message));
});
async function loadConfig(){
  try{const c=await jget('/api/herosms/config');
    document.getElementById('cfg-key').value=c.api_key||'';
    document.getElementById('cfg-hint').textContent=c.is_default?'当前用的是 skill 内置默认 Key':'已使用自定义 Key';
    const minCount=Number.isFinite(Number(c.gpt_login_min_count))?Number(c.gpt_login_min_count):2000;
    document.getElementById('cfg-gpt-price').value=Number(c.gpt_login_max_price||0.2).toFixed(4);
    document.getElementById('cfg-gpt-min-count').value=String(minCount);
    setCountryLimitsFromString(c.gpt_login_countries||'');
    const fCount=document.getElementById('f-count');if(fCount&&!fCount.dataset.touched)fCount.value=String(minCount);
  }catch(e){}
}
document.getElementById('cfg-save').addEventListener('click',async()=>{
  const btn=document.getElementById('cfg-save');btn.disabled=true;
  try{await saveHeroSMSConfig();await loadConfig();await loadBalance();loadServices()}
  catch(e){alert('保存失败：'+e.message)}finally{btn.disabled=false}
});
document.getElementById('cfg-reset').addEventListener('click',async()=>{
  if(!confirm('清空自定义 Key，恢复使用 skill 内置默认 Key？'))return;
  try{document.getElementById('cfg-key').value='';await saveHeroSMSConfig();await loadConfig();await loadBalance();loadServices()}catch(e){alert('失败：'+e.message)}
});

// ===== 余额 =====
async function loadBalance(){
  const el=document.getElementById('balance');
  try{const b=await jget('/api/herosms/balance');el.textContent='余额 $'+Number(b.balance||0).toFixed(4)}
  catch(e){el.textContent='余额 获取失败'}
}
document.getElementById('balance-btn').addEventListener('click',loadBalance);

// ===== 我的号码(MySQL 持久化；页面手买 + 登录 Codex 自动化共用) =====
const CANCEL_MIN_MS=120000;
let activations=[];
function actBoughtMS(a){const t=Date.parse(a.bought_at||a.BoughtAt||'');return Number.isFinite(t)?t:Date.now()}
function statusTag(a){
  if(a.status==='cancelled')return '<span class="tag cancel">已取消</span>';
  if(a.status==='finished')return '<span class="tag ok">已完成</span>';
  if(a.code)return '<span class="tag ok">已收码</span>';
  return '<span class="tag">等待中</span>';
}
function renderActs(){
  document.getElementById('num-cnt').textContent=activations.length;
  document.getElementById('act-empty').style.display=activations.length?'none':'block';
  document.getElementById('act-list').innerHTML=activations.map((a,i)=>{
    const ended=a.status==='cancelled'||a.status==='finished';
    const left=Math.max(0,Math.ceil((CANCEL_MIN_MS-(Date.now()-actBoughtMS(a)))/1000));
    const canCancel=!ended&&left<=0;
    const cancelLabel=ended?'—':(left>0?('取消('+left+'s)'):'取消');
    const source=a.source==='gpt_login'?'登录Codex':(a.source||'手动');
    const statusText=a.statusText||a.last_raw||'已购买，等待短信…';
    return '<div class="act-row">'+
      '<div>'+statusTag(a)+'<div class="mono" style="margin-top:4px">'+esc(a.phone||'-')+'</div><div class="small mono">id '+esc(a.id)+'</div></div>'+
      '<div class="act-status" title="'+esc(statusText)+'"><div>'+esc(a.country||a.country_id||'')+(a.service?(' · '+esc(a.service)):'')+' · '+esc(source)+'</div><div>'+esc(statusText)+'</div></div>'+
      '<div class="code-box">'+esc(a.code||'')+'</div>'+
      '<div class="act-btns">'+
        (a.code&&!ended?'<button class="finish-btn" data-finish="'+i+'" type="button">完成</button>':'')+
        (ended?'':'<button class="cancel-btn" data-cancel="'+i+'" '+(canCancel?'':'disabled')+' type="button">'+esc(cancelLabel)+'</button>')+
      '</div>'+ '</div>';
  }).join('');
}
async function loadActivations(){
  try{const d=await jget('/api/herosms/activations');activations=d.items||[];renderActs()}
  catch(e){document.getElementById('act-empty').style.display='block';document.getElementById('act-empty').textContent='加载号码失败：'+e.message}
}
async function addActivation(a){
  if(a&&a.activation)activations.unshift(a.activation);
  await loadActivations();
}
document.getElementById('clear-done').addEventListener('click',async()=>{
  try{const r=await fetch('/api/herosms/activations/done',{method:'DELETE'});if(!r.ok)throw new Error(await r.text());await loadActivations()}
  catch(e){alert('清除失败：'+e.message)}
});
document.getElementById('act-list').addEventListener('click',async event=>{
  const c=event.target.closest('[data-cancel]'),f=event.target.closest('[data-finish]');
  if(c){const a=activations[+c.dataset.cancel];if(!a)return;c.disabled=true;
    try{const d=await jpost('/api/herosms/cancel',{id:a.id});a.status='cancelled';a.last_raw='取消：'+d.raw;renderActs();await loadBalance();await loadActivations()}
    catch(e){a.statusText='取消失败：'+e.message;renderActs()}
  }
  if(f){const a=activations[+f.dataset.finish];if(!a)return;f.disabled=true;
    try{const d=await jpost('/api/herosms/finish',{id:a.id});a.status='finished';a.last_raw='完成：'+d.raw;renderActs();await loadActivations()}
    catch(e){a.statusText='完成失败：'+e.message;renderActs()}
  }
});
// 每 3s 轮询未收码、未结束的号;每 1s 刷新取消倒计时。
setInterval(async()=>{
  let changed=false;
  for(const a of activations){
    if(a.code||a.status==='cancelled'||a.status==='finished')continue;
    try{const d=await jget('/api/herosms/status?id='+encodeURIComponent(a.id));
      const t=new Date().toLocaleTimeString('zh-CN',{timeZone:'Asia/Shanghai',hour12:false});
      a.last_raw=t+'  '+d.raw;changed=true;
      if(d.code){a.code=d.code;a.status='code';a.last_raw=t+'  ✓ 收到验证码 '+d.code}
    }catch(e){a.statusText='查状态失败：'+e.message;changed=true}
  }
  if(changed)renderActs();
},3000);
setInterval(()=>{if(activations.some(a=>a.status!=='cancelled'&&a.status!=='finished'&&!a.code))renderActs()},1000);
loadActivations();

// ===== 服务列表 =====
let allServices=[];
const svcSelect=document.getElementById('svc-select');
const svcSearch=document.getElementById('svc-search');
function serviceOptions(list,withAll){return (withAll?'<option value="">全部业务</option>':'')+list.slice(0,500).map(s=>'<option value="'+esc(s.code)+'">'+esc(s.name)+' ('+esc(s.code)+')</option>').join('')}
function renderServices(filter){
  filter=(filter||'').trim().toLowerCase();
  const list=filter?allServices.filter(s=>(s.name+' '+s.code).toLowerCase().includes(filter)):allServices;
  const cur=svcSelect.value;
  svcSelect.innerHTML=serviceOptions(list,false);
  if(list.some(s=>s.code===cur))svcSelect.value=cur;
  document.getElementById('svc-count').textContent=list.length+' 个服务'+(list.length>500?'（显示前 500，继续输入缩小范围）':'');
  renderAuxServices('');
}
function renderAuxServices(filter){
  const q=(filter||'').trim().toLowerCase();
  const list=q?allServices.filter(s=>(s.name+' '+s.code).toLowerCase().includes(q)):allServices;
  const log=document.getElementById('log-svc'), bl=document.getElementById('bl-svc-select');
  const logCur=log?.value||'', blCur=bl?.value||'';
  if(log){log.innerHTML=serviceOptions(allServices,true);log.value=allServices.some(s=>s.code===logCur)?logCur:''}
  if(bl){bl.innerHTML=serviceOptions(list,false);if(list.some(s=>s.code===blCur))bl.value=blCur;else if(list.some(s=>s.code==='dr'))bl.value='dr'}
}
async function loadServices(){
  try{
    const d=await jget('/api/herosms/services');
    allServices=(d.services||[]).slice().sort((a,b)=>a.name.localeCompare(b.name));
    renderServices('');
    if(allServices.some(s=>s.code==='dr'))svcSelect.value='dr';
    renderAuxServices('');
    await loadAttemptLogs();
  }catch(e){document.getElementById('svc-count').textContent='服务列表加载失败：'+e.message}
}
svcSearch.addEventListener('input',()=>renderServices(svcSearch.value));
document.getElementById('f-count').addEventListener('input',e=>{e.currentTarget.dataset.touched='1'});

// ===== 查价 =====
let selectedOffer=null;
async function loadOffers(){
  const svc=svcSelect.value;
  if(!svc){alert('先选一个服务');return}
  const btn=document.getElementById('offers-btn');btn.disabled=true;btn.classList.add('busy');
  const price=document.getElementById('f-price').value||'0.2';
  const count=document.getElementById('f-count').value||'2000';
  try{
    const d=await jget('/api/herosms/offers?service='+encodeURIComponent(svc)+'&max_price='+encodeURIComponent(price)+'&min_count='+encodeURIComponent(count));
    const rows=d.rows||[];
    const empty=document.getElementById('offer-empty');
    empty.style.display=rows.length?'none':'block';
    empty.textContent=rows.length?'':'没有符合条件的优惠，放宽单价或降低库存门槛。';
    document.getElementById('offer-rows').innerHTML=rows.map((o,i)=>
      '<tr data-i="'+i+'"><td>'+esc(o.country_name||'-')+'</td><td class="mono">'+esc(o.country_id)+'</td>'+
      '<td class="price">$'+Number(o.price).toFixed(4)+'</td><td class="mono">'+esc(o.count)+'</td>'+
      '<td><button class="pick-btn" data-i="'+i+'" type="button">选择</button> <button class="pick-btn limit-btn '+(gptCountryLimits.some(c=>c.id===String(o.country_id))?'on':'')+'" data-limit-i="'+i+'" data-cid="'+esc(o.country_id)+'" type="button">'+(gptCountryLimits.some(c=>c.id===String(o.country_id))?'取消限定':'限定')+'</button></td></tr>'
    ).join('');
    window._offers=rows;selectedOffer=null;document.getElementById('buybar').classList.add('hidden');
  }catch(e){alert('查询价格失败：'+e.message)}
  finally{btn.disabled=false;btn.classList.remove('busy')}
}
document.getElementById('offers-btn').addEventListener('click',loadOffers);
document.getElementById('offer-rows').addEventListener('click',event=>{
  const limitBtn=event.target.closest('[data-limit-i]');
  if(limitBtn){const o=(window._offers||[])[+limitBtn.dataset.limitI];if(o)toggleCountryLimit(o);loadOffers();return}
  const btn=event.target.closest('.pick-btn');if(!btn||btn.dataset.limitI)return;
  const o=(window._offers||[])[+btn.dataset.i];if(!o)return;
  selectedOffer=o;
  document.querySelectorAll('#offer-rows tr').forEach(tr=>tr.classList.toggle('sel',tr.dataset.i===btn.dataset.i));
  document.getElementById('bb-country').textContent=o.country_name||o.country_id;
  document.getElementById('bb-cid').textContent='id '+o.country_id;
  document.getElementById('bb-price').textContent='$'+Number(o.price).toFixed(4);
  document.getElementById('buybar').classList.remove('hidden');
});

// ===== 购买(按数量批量),买完存本地 + 切到「我的号码」tab =====
document.getElementById('bb-buy').addEventListener('click',async()=>{
  if(!selectedOffer)return;
  const qty=Math.max(1,Math.min(10,parseInt(document.getElementById('bb-qty').value,10)||1));
  const svc=svcSelect.value;
  const svcName=(svcSelect.options[svcSelect.selectedIndex]||{}).textContent||svc;
  const est=(selectedOffer.price*qty).toFixed(4);
  if(!confirm('购买 '+(selectedOffer.country_name||selectedOffer.country_id)+' 的号码 '+qty+' 个，预计约 $'+est+'，真实扣费。'))return;
  const btn=document.getElementById('bb-buy');btn.disabled=true;btn.classList.add('busy');
  const hint=document.getElementById('bb-hint');
  let ok=0,fail=0;
  for(let i=0;i<qty;i++){
    hint.textContent='购买中 '+(i+1)+'/'+qty+'…';
    try{
      const d=await jpost('/api/herosms/number',{service:svc,country:selectedOffer.country_id,max_price:selectedOffer.price});
      await addActivation(Object.assign(d,{service:svcName,country:selectedOffer.country_name||selectedOffer.country_id,price:selectedOffer.price}));ok++;
    }catch(e){fail++;hint.textContent='第 '+(i+1)+' 个失败：'+e.message}
  }
  hint.textContent='完成：成功 '+ok+' 个'+(fail?('，失败 '+fail+' 个'):'');
  btn.disabled=false;btn.classList.remove('busy');
  await loadBalance();
  if(ok>0)switchTab('numbers'); // 买完自动切到「我的号码」
});


// ===== 购买记录 =====
function fmtTime(v){const d=new Date(v);return isNaN(d.getTime())?'-':d.toLocaleString('zh-CN',{timeZone:'Asia/Shanghai',hour12:false})}
function resultText(r){return ({success:'成功',sms_sent_timeout:'发出未收码',whatsapp:'WhatsApp',sms_unsupported:'不支持SMS',number_rejected:'号码失败',too_many:'号码频控',invalid_auth:'授权失效',input_failed:'输入失败',unknown:'未知失败'})[r]||r||'-'}
function serviceText(code){
  code=(code||'').trim();
  if(!code)return '-';
  const zh={dr:'OpenAI（ChatGPT）'};
  if(zh[code])return zh[code];
  const svc=allServices.find(s=>s.code===code);
  return (svc&&svc.name&&svc.name!==code)?(svc.name+'（'+code+'）'):code;
}
function feeText(v){
  const n=Number(v||0);
  return n>0?'$'+n.toFixed(4):'-';
}
async function loadAttemptLogs(){
  const tbody=document.getElementById('log-rows'), empty=document.getElementById('log-empty');
  if(!tbody||!empty)return;
  const svc=document.getElementById('log-svc').value||'';
  try{
    const d=await jget('/api/herosms/attempt-logs?limit=500'+(svc?'&service='+encodeURIComponent(svc):''));
    const rows=d.items||[];empty.style.display=rows.length?'none':'block';
    tbody.innerHTML=rows.map(x=>'<tr><td>'+esc(fmtTime(x.created_at))+'</td><td>'+esc(serviceText(x.service))+'</td><td>'+esc(x.country||x.country_id||'-')+' <span class="mono">'+esc(x.country_id||'')+'</span></td><td class="mono">'+esc(x.phone||'-')+'</td><td class="price">'+esc(feeText(x.fee??x.price))+'</td><td>'+esc(resultText(x.result))+'</td><td title="'+esc(x.raw||'')+'">'+esc(x.reason||'')+'</td><td>'+esc(x.source||'')+'</td></tr>').join('');
  }catch(e){empty.style.display='block';empty.textContent='加载购买记录失败：'+e.message}
}
document.getElementById('log-refresh').addEventListener('click',loadAttemptLogs);
document.getElementById('log-svc').addEventListener('change',loadAttemptLogs);

// ===== 国家拉黑 =====
const blSearch=document.getElementById('bl-svc-search');
blSearch.addEventListener('input',()=>renderAuxServices(blSearch.value));
async function loadBlacklistCountries(){
  const svc=document.getElementById('bl-svc-select').value;
  if(!svc){alert('先选一个业务');return}
  const btn=document.getElementById('bl-load');btn.disabled=true;btn.classList.add('busy');
  try{
    const d=await jget('/api/herosms/countries?service='+encodeURIComponent(svc)+'&max_price=9999&min_count=0');
    const rows=d.rows||[];document.getElementById('bl-count').textContent=rows.length+' 个国家';
    const empty=document.getElementById('bl-empty');empty.style.display=rows.length?'none':'block';empty.textContent=rows.length?'':'该业务暂无可用国家';
    document.getElementById('bl-rows').innerHTML=rows.map((o,i)=>'<tr><td>'+esc(o.country_name||'-')+'</td><td class="mono">'+esc(o.country_id)+'</td><td class="price">$'+Number(o.price||0).toFixed(4)+'</td><td class="mono">'+esc(o.count||0)+'</td><td>'+(o.blacklisted?'<span class="tag cancel">已拉黑</span>':'<span class="tag ok">可用</span>')+'</td><td>'+(o.blacklisted?'<button class="pick-btn" data-unblock="'+i+'" type="button">解除</button>':'<button class="cancel-btn" data-block="'+i+'" type="button">拉黑</button>')+'</td></tr>').join('');
    window._blCountries=rows;
  }catch(e){alert('查询国家失败：'+e.message)}finally{btn.disabled=false;btn.classList.remove('busy')}
}
document.getElementById('bl-load').addEventListener('click',loadBlacklistCountries);
document.getElementById('bl-rows').addEventListener('click',async event=>{
  const b=event.target.closest('[data-block]'), u=event.target.closest('[data-unblock]');
  const svc=document.getElementById('bl-svc-select').value;
  if(b){const o=(window._blCountries||[])[+b.dataset.block];if(!o)return;b.disabled=true;
    try{await jpost('/api/herosms/blacklist',{service:svc,country_id:o.country_id,country:o.country_name||o.country_id,reason:'手动拉黑'});await loadBlacklistCountries();if(svc===svcSelect.value)loadOffers()}catch(e){alert('拉黑失败：'+e.message);b.disabled=false}}
  if(u){const o=(window._blCountries||[])[+u.dataset.unblock];if(!o)return;u.disabled=true;
    try{const r=await fetch('/api/herosms/blacklist?service='+encodeURIComponent(svc)+'&country_id='+encodeURIComponent(o.country_id),{method:'DELETE'});if(!r.ok)throw new Error(await r.text());await loadBlacklistCountries();if(svc===svcSelect.value)loadOffers()}catch(e){alert('解除失败：'+e.message);u.disabled=false}}
});

loadConfig();loadBalance();loadServices();
</script>
</body>
</html>
{{end}}
`
