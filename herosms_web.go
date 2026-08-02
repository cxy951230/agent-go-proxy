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
    .act-row{display:grid;grid-template-columns:130px 1fr 120px 150px;gap:12px;align-items:center;padding:12px 18px;border-bottom:1px solid var(--line)}
    .act-row:last-child{border-bottom:0}
    .code-box{font-size:18px;font-weight:700;letter-spacing:2px;color:var(--green)}
    .act-status{font-size:12px;color:var(--muted);font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
    .cancel-btn{height:32px;color:var(--red);border-color:#f0b3b3;background:#fff6f6}.cancel-btn:hover:not(:disabled){background:#fff1f1}
    .tag{display:inline-block;font-size:11px;font-weight:600;padding:2px 8px;border-radius:999px;background:#eef1f6;color:#4b5563}
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
    <div class="notice">流程：① 选服务（openai / google …）→ ② 查价选国家 → ③ 选数量购买 → ④ 轮询验证码 → 满 2 分钟可取消。<b>买号真实扣费</b>，收到码后系统这里不自动完成，未用上的记得取消。</div>

    <section class="panel">
      <div class="panel-head"><h2>API Key 配置</h2><span class="small" id="cfg-hint"></span></div>
      <div class="cfg-row">
        <input id="cfg-key" type="text" placeholder="HeroSMS API Key" autocomplete="off">
        <button class="btn-primary" id="cfg-save" type="button">保存</button>
        <button id="cfg-reset" type="button" title="清空为使用 skill 内置默认 Key">恢复默认</button>
      </div>
    </section>

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

    <section class="panel" id="act-panel" style="display:none">
      <div class="panel-head"><h2><span class="step">4</span>号码 / 验证码</h2><span class="small">每 3 秒自动轮询；满 120s 才能取消。取消只调 HeroSMS 取消接口。</span></div>
      <div id="act-list"></div>
    </section>
  </main>
</div>
<script>
const appEl=document.querySelector('.app');
if(localStorage.getItem('sidebarCollapsed')==='1')appEl.classList.add('sidebar-collapsed');
document.getElementById('sidebar-toggle').addEventListener('click',()=>{appEl.classList.toggle('sidebar-collapsed');localStorage.setItem('sidebarCollapsed',appEl.classList.contains('sidebar-collapsed')?'1':'0')});
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
async function jpost(url,body){const r=await fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body||{})});if(!r.ok)throw new Error(await r.text());return r.json()}
async function jget(url){const r=await fetch(url,{cache:'no-store'});if(!r.ok)throw new Error(await r.text());return r.json()}

// ===== 配置 =====
async function loadConfig(){
  try{const c=await jget('/api/herosms/config');
    document.getElementById('cfg-key').value=c.api_key||'';
    document.getElementById('cfg-hint').textContent=c.is_default?'当前用的是 skill 内置默认 Key':'已使用自定义 Key';
  }catch(e){}
}
document.getElementById('cfg-save').addEventListener('click',async()=>{
  const btn=document.getElementById('cfg-save');btn.disabled=true;
  try{await jpost('/api/herosms/config',{api_key:document.getElementById('cfg-key').value.trim()});await loadConfig();await loadBalance();loadServices()}
  catch(e){alert('保存失败：'+e.message)}finally{btn.disabled=false}
});
document.getElementById('cfg-reset').addEventListener('click',async()=>{
  if(!confirm('清空自定义 Key，恢复使用 skill 内置默认 Key？'))return;
  try{await jpost('/api/herosms/config',{api_key:''});await loadConfig();await loadBalance();loadServices()}catch(e){alert('失败：'+e.message)}
});

// ===== 余额 =====
async function loadBalance(){
  const el=document.getElementById('balance');
  try{const b=await jget('/api/herosms/balance');el.textContent='余额 $'+Number(b.balance||0).toFixed(4)}
  catch(e){el.textContent='余额 获取失败'}
}
document.getElementById('balance-btn').addEventListener('click',loadBalance);

// ===== 服务列表 =====
let allServices=[];
const svcSelect=document.getElementById('svc-select');
const svcSearch=document.getElementById('svc-search');
function renderServices(filter){
  filter=(filter||'').trim().toLowerCase();
  const list=filter?allServices.filter(s=>(s.name+' '+s.code).toLowerCase().includes(filter)):allServices;
  const cur=svcSelect.value;
  svcSelect.innerHTML=list.slice(0,500).map(s=>'<option value="'+esc(s.code)+'">'+esc(s.name)+' ('+esc(s.code)+')</option>').join('');
  if(list.some(s=>s.code===cur))svcSelect.value=cur;
  document.getElementById('svc-count').textContent=list.length+' 个服务'+(list.length>500?'（显示前 500，继续输入缩小范围）':'');
}
async function loadServices(){
  try{
    const d=await jget('/api/herosms/services');
    allServices=(d.services||[]).slice().sort((a,b)=>a.name.localeCompare(b.name));
    renderServices('');
    // 默认选 OpenAI(dr)
    if(allServices.some(s=>s.code==='dr'))svcSelect.value='dr';
  }catch(e){document.getElementById('svc-count').textContent='服务列表加载失败：'+e.message}
}
svcSearch.addEventListener('input',()=>renderServices(svcSearch.value));

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
      '<td><button class="pick-btn" data-i="'+i+'" type="button">选择</button></td></tr>'
    ).join('');
    window._offers=rows;selectedOffer=null;document.getElementById('buybar').classList.add('hidden');
  }catch(e){alert('查询价格失败：'+e.message)}
  finally{btn.disabled=false;btn.classList.remove('busy')}
}
document.getElementById('offers-btn').addEventListener('click',loadOffers);
document.getElementById('offer-rows').addEventListener('click',event=>{
  const btn=event.target.closest('.pick-btn');if(!btn)return;
  const o=(window._offers||[])[+btn.dataset.i];if(!o)return;
  selectedOffer=o;
  document.querySelectorAll('#offer-rows tr').forEach(tr=>tr.classList.toggle('sel',tr.dataset.i===btn.dataset.i));
  document.getElementById('bb-country').textContent=o.country_name||o.country_id;
  document.getElementById('bb-cid').textContent='id '+o.country_id;
  document.getElementById('bb-price').textContent='$'+Number(o.price).toFixed(4);
  document.getElementById('buybar').classList.remove('hidden');
});

// ===== 购买(按数量批量) =====
document.getElementById('bb-buy').addEventListener('click',async()=>{
  if(!selectedOffer)return;
  const qty=Math.max(1,Math.min(10,parseInt(document.getElementById('bb-qty').value,10)||1));
  const svc=svcSelect.value;
  const est=(selectedOffer.price*qty).toFixed(4);
  if(!confirm('购买 '+(selectedOffer.country_name||selectedOffer.country_id)+' 的号码 '+qty+' 个，预计约 $'+est+'，真实扣费。'))return;
  const btn=document.getElementById('bb-buy');btn.disabled=true;btn.classList.add('busy');
  const hint=document.getElementById('bb-hint');
  let ok=0,fail=0;
  for(let i=0;i<qty;i++){
    hint.textContent='购买中 '+(i+1)+'/'+qty+'…';
    try{
      const d=await jpost('/api/herosms/number',{service:svc,country:selectedOffer.country_id,max_price:selectedOffer.price});
      addActivation(d);ok++;
    }catch(e){fail++;hint.textContent='第 '+(i+1)+' 个失败：'+e.message}
  }
  hint.textContent='完成：成功 '+ok+' 个'+(fail?('，失败 '+fail+' 个'):'');
  btn.disabled=false;btn.classList.remove('busy');
  await loadBalance();
});

// ===== 激活列表 + 轮询 + 取消 =====
const CANCEL_MIN_MS=120000; // HeroSMS 最小激活期 120s
let activations=[];
function addActivation(d){
  activations.push({id:d.activation_id,phone:d.phone,code:'',status:'已购买，等待短信…',bought:Date.now(),done:false});
  document.getElementById('act-panel').style.display='block';
  renderActivations();
}
function renderActivations(){
  document.getElementById('act-list').innerHTML=activations.map((a,i)=>{
    const left=Math.max(0,Math.ceil((CANCEL_MIN_MS-(Date.now()-a.bought))/1000));
    const canCancel=left<=0&&!a.done;
    const cancelLabel=a.done?'已结束':(left>0?('取消 ('+left+'s)'):'取消');
    return '<div class="act-row">'+
      '<div><span class="tag">'+esc(a.status.startsWith('取消')?'已取消':(a.code?'已收码':'等待中'))+'</span><div class="mono" style="margin-top:4px">'+esc(a.phone||'-')+'</div><div class="small mono">id '+esc(a.id)+'</div></div>'+
      '<div class="act-status" title="'+esc(a.status)+'">'+esc(a.status)+'</div>'+
      '<div class="code-box">'+esc(a.code||'')+'</div>'+
      '<div><button class="cancel-btn" data-i="'+i+'" '+(canCancel?'':'disabled')+' type="button">'+esc(cancelLabel)+'</button></div>'+
    '</div>';
  }).join('');
}
document.getElementById('act-list').addEventListener('click',async event=>{
  const btn=event.target.closest('.cancel-btn');if(!btn)return;
  const a=activations[+btn.dataset.i];if(!a||a.done)return;
  btn.disabled=true;
  try{const d=await jpost('/api/herosms/cancel',{id:a.id});a.status='取消：'+d.raw;a.done=true;await loadBalance()}
  catch(e){a.status='取消失败：'+e.message}
  renderActivations();
});
// 每 3 秒轮询所有未收码、未结束的激活；顺带刷新倒计时。
setInterval(async()=>{
  for(const a of activations){
    if(a.done||a.code)continue;
    try{
      const d=await jget('/api/herosms/status?id='+encodeURIComponent(a.id));
      const t=new Date().toLocaleTimeString('zh-CN',{hour12:false});
      a.status=t+'  '+d.raw;
      if(d.code){a.code=d.code;a.status=t+'  ✓ 收到验证码 '+d.code}
    }catch(e){a.status='查状态失败：'+e.message}
  }
  if(activations.length)renderActivations();
},3000);
setInterval(()=>{if(activations.some(a=>!a.done&&!a.code))renderActivations()},1000); // 倒计时刷新

loadConfig();loadBalance();loadServices();
</script>
</body>
</html>
{{end}}
`
