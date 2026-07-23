package main

// tokenStatsHTML 是 token 消耗详情图表页。由路由 / OPENAI 账号 / API Key 列表点击某行跳转而来,
// 通过查询串带 dim(维度)/id/name,前端拉 /api/stats/tokens 数据后用内联 SVG 画折线图与柱状图,
// 不依赖任何外部图表库。
const tokenStatsHTML = `
{{define "token-stats"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Token 消耗详情 · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--orange:#d46f1e;--red:#c94040;--purple:#6750d8;--cyan:#0e9aa7}
    *{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
    .app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;height:100vh}
    .brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;padding:12px}
    .nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px;text-decoration:none}.nav-item:hover{background:#eef2fa}.nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:22px 28px 56px}
    .top{display:flex;align-items:center;gap:14px;flex-wrap:wrap}.top h1{font-size:18px;margin:0;flex:1}.back{color:var(--blue);text-decoration:none}
    .seg{display:inline-flex;border:1px solid var(--line);border-radius:8px;overflow:hidden;background:#fff}
    .seg button{height:36px;padding:0 16px;border:0;background:#fff;color:var(--muted);font:inherit;cursor:pointer}
    .seg button.on{background:var(--blue);color:#fff;font-weight:600}
    .cards{display:flex;gap:16px;flex-wrap:wrap;margin-top:18px}
    .card{flex:1;min-width:150px;background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:16px 18px;box-shadow:0 1px 3px rgba(20,30,50,.05)}
    .card .k{font-size:12px;color:var(--muted);font-weight:600}.card .v{font-size:26px;font-weight:700;margin-top:6px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
    .panel{margin-top:18px;background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:18px 20px;box-shadow:0 1px 3px rgba(20,30,50,.05)}
    .panel h2{font-size:15px;margin:0 0 4px}.panel .sub{color:var(--muted);font-size:12px;margin-bottom:12px}
    .legend{display:flex;gap:16px;flex-wrap:wrap;margin:6px 0 2px;font-size:12px;color:#4b5563}
    .legend span{display:inline-flex;align-items:center;gap:6px}.dot{width:10px;height:10px;border-radius:3px;display:inline-block}
    .chart-wrap{width:100%;overflow-x:auto}
    svg{display:block;max-width:100%}
    .empty{color:var(--muted);padding:26px;text-align:center}
    text{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
  </style>
</head>
<body>
<div class="app">
  <aside class="sidebar">
    <div class="brand">AGENT-GO-PROXY</div>
    <nav>
      <a class="nav-item" href="/">Dashboard</a>
      <a class="nav-item" href="/routes">路由</a>
      <a class="nav-item" href="/chains">链式代理</a>
      <a class="nav-item" href="/openai">OPENAI</a>
      <a class="nav-item" href="/api-keys">API Key</a>
    </nav>
  </aside>
  <main class="page">
    <div class="top">
      <a class="back" id="back" href="/">← 返回</a>
      <h1 id="title">Token 消耗详情</h1>
      <div class="seg" id="granularity">
        <button data-g="day" class="on">按天</button>
        <button data-g="month">按月</button>
        <button data-g="year">按年</button>
      </div>
    </div>
    <div class="cards" id="cards"></div>
    <section class="panel">
      <h2>Token 消耗趋势</h2>
      <div class="sub" id="trend-sub">按时间粒度聚合的输入 / 输出 / 缓存 / 合计 token</div>
      <div class="legend">
        <span><i class="dot" style="background:#2563eb"></i>输入</span>
        <span><i class="dot" style="background:#16a34a"></i>输出</span>
        <span><i class="dot" style="background:#f59e0b"></i>缓存</span>
        <span><i class="dot" style="background:#111827"></i>合计</span>
      </div>
      <div class="chart-wrap" id="line-wrap"></div>
    </section>
    <section class="panel">
      <h2>请求数趋势</h2>
      <div class="sub">每个时间桶的请求次数</div>
      <div class="chart-wrap" id="req-wrap"></div>
    </section>
    <section class="panel">
      <h2>按模型拆分</h2>
      <div class="sub">该维度下各模型的合计 token 消耗</div>
      <div class="chart-wrap" id="model-wrap"></div>
    </section>
  </main>
</div>
<script>
const DIM = {{.Dim}};
const ID = {{.ID}};
const NAME = {{.Name}};
const dimLabel = {route:'路由',chain:'链式路由','api_key':'API Key',account:'OPENAI 账号'}[DIM] || DIM;
const backHref = {route:'/routes',chain:'/chains','api_key':'/api-keys',account:'/openai'}[DIM] || '/';
document.getElementById('back').href = backHref;
document.getElementById('title').textContent = 'Token 消耗详情 · ' + dimLabel + (NAME ? ' · ' + NAME : ' #' + ID);
let granularity = 'day';
const fmtInt = n => Number(n||0).toLocaleString('en-US');
const esc = v => String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

function niceMax(v){ if(v<=0) return 10; const p=Math.pow(10,Math.floor(Math.log10(v))); const n=v/p; const steps=[1,1.2,1.5,2,2.5,3,4,5,6,8,10]; const s=steps.find(x=>x>=n)||10; return s*p; }

// 折线图:多序列。series=[{color,key,label,emphasis?}], data=[{period,...}]
function lineChart(data, series){
  if(!data.length) return '<div class="empty">暂无数据</div>';
  const single=data.length===1;
  const W=Math.max(680, data.length*70+120), H=340, padL=72, padR=28, padT=24, padB=56;
  const iw=W-padL-padR, ih=H-padT-padB;
  let max=0; data.forEach(d=>series.forEach(s=>{max=Math.max(max,Number(d[s.key]||0))}));
  max=niceMax(max);
  const x=i=>padL+(single?iw/2:iw*i/(data.length-1));
  const y=v=>padT+ih-ih*(Number(v||0)/max);
  let svg='<svg viewBox="0 0 '+W+' '+H+'" width="'+W+'" height="'+H+'">';
  // 网格 + Y 轴刻度
  for(let g=0;g<=4;g++){const gv=max*g/4, gy=padT+ih-ih*g/4;
    svg+='<line x1="'+padL+'" y1="'+gy.toFixed(1)+'" x2="'+(W-padR)+'" y2="'+gy.toFixed(1)+'" stroke="#eef1f6"/>';
    svg+='<text x="'+(padL-10)+'" y="'+(gy+4).toFixed(1)+'" text-anchor="end" font-size="11" fill="#8a93a3">'+fmtInt(gv)+'</text>';}
  // X 轴刻度 + 单点时的竖向参考线
  data.forEach((d,i)=>{ if(data.length>14 && i%Math.ceil(data.length/14)!==0 && i!==data.length-1) return;
    if(single) svg+='<line x1="'+x(i).toFixed(1)+'" y1="'+padT+'" x2="'+x(i).toFixed(1)+'" y2="'+(padT+ih)+'" stroke="#eef1f6" stroke-dasharray="4 4"/>';
    svg+='<text x="'+x(i).toFixed(1)+'" y="'+(H-padB+20)+'" text-anchor="middle" font-size="11" fill="#8a93a3"'+(single?'':' transform="rotate(28 '+x(i).toFixed(1)+' '+(H-padB+20)+')"')+'>'+esc(d.period)+'</text>';});
  // 先画非强调序列,强调序列(合计)最后画压在最上层
  const ordered=series.slice().sort((a,b)=>(a.emphasis?1:0)-(b.emphasis?1:0));
  ordered.forEach(s=>{
    const w=s.emphasis?3:2.2;
    if(!single){
      let path=''; data.forEach((d,i)=>{path+=(i?'L':'M')+x(i).toFixed(1)+' '+y(d[s.key]).toFixed(1)+' ';});
      svg+='<path d="'+path+'" fill="none" stroke="'+s.color+'" stroke-width="'+w+'" stroke-linejoin="round" stroke-linecap="round"/>';
    }
    data.forEach((d,i)=>{const cx=x(i).toFixed(1), cy=y(d[s.key]).toFixed(1), r=s.emphasis?5:4;
      // 白色描边光晕,让相近颜色的点也彼此分明
      svg+='<circle cx="'+cx+'" cy="'+cy+'" r="'+r+'" fill="'+s.color+'" stroke="#fff" stroke-width="2"><title>'+esc(d.period)+' · '+esc(s.label)+': '+fmtInt(d[s.key])+'</title></circle>';});
    // 强调序列在每个点上方标注数值(单点时也一目了然)
    if(s.emphasis) data.forEach((d,i)=>{svg+='<text x="'+x(i).toFixed(1)+'" y="'+(y(d[s.key])-12).toFixed(1)+'" text-anchor="middle" font-size="12" font-weight="700" fill="'+s.color+'">'+fmtInt(d[s.key])+'</text>';});
  });
  return svg+'</svg>';
}

// 柱状图:单序列。items=[{label,value}]
function barChart(items, color){
  if(!items.length) return '<div class="empty">暂无数据</div>';
  const W=Math.max(680, items.length*70+90), H=300, padL=64, padR=20, padT=18, padB=64;
  const iw=W-padL-padR, ih=H-padT-padB;
  let max=0; items.forEach(it=>max=Math.max(max,Number(it.value||0))); max=niceMax(max);
  const bw=Math.min(52, iw/items.length*0.6);
  const step=iw/items.length;
  let svg='<svg viewBox="0 0 '+W+' '+H+'" width="'+W+'" height="'+H+'">';
  for(let g=0;g<=4;g++){const gv=max*g/4, gy=padT+ih-ih*g/4;
    svg+='<line x1="'+padL+'" y1="'+gy+'" x2="'+(W-padR)+'" y2="'+gy+'" stroke="#eef1f6"/>';
    svg+='<text x="'+(padL-8)+'" y="'+(gy+4)+'" text-anchor="end" font-size="10" fill="#8a93a3">'+fmtInt(gv)+'</text>';}
  items.forEach((it,i)=>{const cx=padL+step*i+step/2, h=ih*(Number(it.value||0)/max), by=padT+ih-h;
    svg+='<rect x="'+(cx-bw/2).toFixed(1)+'" y="'+by.toFixed(1)+'" width="'+bw.toFixed(1)+'" height="'+Math.max(0,h).toFixed(1)+'" rx="4" fill="'+color+'"><title>'+esc(it.label)+': '+fmtInt(it.value)+'</title></rect>';
    svg+='<text x="'+cx.toFixed(1)+'" y="'+(by-6).toFixed(1)+'" text-anchor="middle" font-size="10" fill="#4b5563">'+fmtInt(it.value)+'</text>';
    svg+='<text x="'+cx.toFixed(1)+'" y="'+(H-padB+18)+'" text-anchor="middle" font-size="10" fill="#8a93a3" transform="rotate(20 '+cx.toFixed(1)+' '+(H-padB+18)+')">'+esc(it.label)+'</text>';});
  return svg+'</svg>';
}

function renderCards(series){
  const sum=k=>series.reduce((a,d)=>a+Number(d[k]||0),0);
  const cards=[['请求数',sum('requests')],['输入 Token',sum('input_tokens')],['输出 Token',sum('output_tokens')],['缓存 Token',sum('cached_tokens')],['合计 Token',sum('total_tokens')]];
  document.getElementById('cards').innerHTML=cards.map(c=>'<div class="card"><div class="k">'+c[0]+'</div><div class="v">'+fmtInt(c[1])+'</div></div>').join('');
}

async function load(){
  const rsp=await fetch('/api/stats/tokens?dim='+encodeURIComponent(DIM)+'&id='+encodeURIComponent(ID)+'&granularity='+granularity,{cache:'no-store'});
  if(!rsp.ok){document.getElementById('cards').innerHTML='<div class="empty">加载失败：'+esc(await rsp.text())+'</div>';return;}
  const data=await rsp.json();
  const series=data.series||[];
  renderCards(series);
  document.getElementById('line-wrap').innerHTML=lineChart(series,[
    {key:'input_tokens',color:'#2563eb',label:'输入'},
    {key:'output_tokens',color:'#16a34a',label:'输出'},
    {key:'cached_tokens',color:'#f59e0b',label:'缓存'},
    {key:'total_tokens',color:'#111827',label:'合计',emphasis:true},
  ]);
  document.getElementById('req-wrap').innerHTML=barChart(series.map(d=>({label:d.period,value:d.requests})),'#d46f1e');
  document.getElementById('model-wrap').innerHTML=barChart((data.by_model||[]).map(m=>({label:m.model,value:m.total_tokens})),'#6750d8');
}
document.getElementById('granularity').addEventListener('click',e=>{
  const b=e.target.closest('button');if(!b)return;
  granularity=b.dataset.g;
  [...document.querySelectorAll('#granularity button')].forEach(x=>x.classList.toggle('on',x===b));
  load().catch(()=>{});
});
load().catch(()=>{});
</script>
</body>
</html>
{{end}}
`
