package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

func (p *proxyServer) handleAPIKeysPage(w http.ResponseWriter, r *http.Request) {
	if err := baseTemplate.ExecuteTemplate(w, "apikeys", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *proxyServer) handleAPIKeysList(w http.ResponseWriter, r *http.Request) {
	keys, err := p.store.ListAPIKeys(r.Context())
	writeJSON(w, keys, err)
}

func (p *proxyServer) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	var key APIKey
	if err := json.NewDecoder(r.Body).Decode(&key); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id, err := p.store.CreateAPIKey(r.Context(), key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id}, nil)
}

func (p *proxyServer) handleAPIKeyUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad api key id", http.StatusBadRequest)
		return
	}
	var key APIKey
	if err := json.NewDecoder(r.Body).Decode(&key); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := p.store.UpdateAPIKey(r.Context(), id, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "api key not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

func (p *proxyServer) handleAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad api key id", http.StatusBadRequest)
		return
	}
	if err := p.store.DeleteAPIKey(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "api key not found", http.StatusNotFound)
			return
		}
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true}, nil)
}

const apiKeysHTML = `
{{define "apikeys"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>API Key · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--orange:#d46f1e;--red:#c94040;--purple:#6750d8}
    *{box-sizing:border-box} body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif}
    .app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;align-self:flex-start;height:100vh;transition:width .16s ease,padding .16s ease}
    .sidebar .brand-row{display:flex;align-items:center;gap:8px;padding:6px 2px 18px 10px}.sidebar .brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;flex:1;white-space:nowrap;overflow:hidden}
    .sidebar-toggle{width:30px;height:30px;padding:0;border-radius:7px;border:1px solid var(--line);background:#fff;color:#667284;font-weight:800}
    .app.sidebar-collapsed .sidebar{width:58px;padding-left:10px;padding-right:10px}.app.sidebar-collapsed .brand{display:none}.app.sidebar-collapsed .nav-item{padding:10px 0;text-align:center;font-size:0}.app.sidebar-collapsed .nav-item::first-letter{font-size:15px}.app.sidebar-collapsed .sidebar-toggle{transform:rotate(180deg)}
    .nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px;text-decoration:none}
    .nav-item:hover{background:#eef2fa}
    .nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:22px 28px 56px}
    .top{display:flex;align-items:center;gap:16px}.top h1{font-size:18px;font-weight:600;margin:0;flex:1}
    button{cursor:pointer;height:42px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 14px;font:inherit;color:var(--text)}
    .btn-primary{background:var(--blue);border-color:var(--blue);color:#fff;font-weight:600}
    .btn-primary:hover{background:#265fd0}
    .table{margin-top:18px;background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:0 1px 3px rgba(20,30,50,.05);overflow-x:auto}
    table{width:100%;border-collapse:collapse}
    th,td{padding:16px 18px;border-bottom:1px solid var(--line);text-align:left;vertical-align:middle}
    tbody tr:last-child td{border-bottom:0}
    tbody tr.row-link{cursor:pointer}tbody tr.row-link:hover{background:#fafcff}
    th{font-size:12px;color:#606a7a;font-weight:600;letter-spacing:.04em;background:#fbfcfe}
    .mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px}
    .muted{color:var(--muted)}
    .action{display:inline-flex;align-items:center;justify-content:center;min-width:44px;height:30px;border:1px solid var(--line);padding:0 12px;border-radius:7px;background:#fff;font-weight:500}
    .action + .action{margin-left:6px}
    .delete-btn{color:var(--red);border-color:#f0b3b3;background:#fff6f6}.delete-btn:hover{background:#fff1f1;border-color:#e07d7d}
    .overlay{display:none;position:fixed;inset:0;background:rgba(22,28,40,.4);align-items:center;justify-content:center;z-index:20}
    .overlay.show{display:flex}
    .modal{width:460px;max-width:calc(100vw - 32px);background:#fff;border-radius:12px;box-shadow:0 12px 40px rgba(20,30,50,.24);padding:24px}
    .modal h2{margin:0 0 18px;font-size:16px}
    .field{margin-bottom:14px}
    .field label{display:block;font-size:12px;font-weight:600;color:var(--muted);margin-bottom:6px}
    .field input{width:100%;height:40px;border:1px solid var(--line);border-radius:7px;padding:0 12px;font:inherit;color:var(--text);background:#fff}
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
      <a class="nav-item" href="/routes">路由</a>
      <a class="nav-item" href="/chains">链式代理</a>
      <a class="nav-item" href="/openai">OPENAI</a>
      <a class="nav-item active" href="/api-keys">API Key</a>
    </nav>
  </aside>
  <main class="page">
    <div class="top"><h1>API Key · 密钥配置</h1><button class="btn-primary" id="add-btn">+ 添加配置</button></div>
    <section class="table">
      <table>
        <thead><tr><th>名称</th><th>API Key</th><th style="width:150px">操作</th></tr></thead>
        <tbody id="key-rows"></tbody>
      </table>
      <div class="empty" id="empty-tip" style="display:none">还没有配置。点击右上角「添加配置」新建一个。</div>
    </section>
  </main>
</div>

<div class="overlay" id="modal-overlay">
  <div class="modal">
    <h2 id="modal-title">添加配置</h2>
    <div class="field"><label>名称</label><input id="f-name" placeholder="便于识别的名称，可留空"></div>
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
let keys = [];
let editingId = null;
const overlay = document.getElementById('modal-overlay');
const errBox = document.getElementById('modal-err');

// 新增时默认生成一个随机 sk- Key,用户可直接改。
function genApiKey(){
  const bytes = new Uint8Array(40);
  (window.crypto || window.msCrypto).getRandomValues(bytes);
  const chars = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  let s = '';
  for (const b of bytes) s += chars[b % chars.length];
  return 'sk-' + s;
}

function renderRows(){
  const tbody = document.getElementById('key-rows');
  const empty = document.getElementById('empty-tip');
  if (!keys.length){
    tbody.innerHTML = '';
    empty.style.display = 'block';
    return;
  }
  empty.style.display = 'none';
  tbody.innerHTML = keys.map(k =>
    '<tr class="row-link" data-href="/stats/tokens?dim=api_key&id=' + k.id + '&name=' + encodeURIComponent(k.name || '') + '">' +
      '<td>' + (k.name ? esc(k.name) : '<span class="muted">未命名</span>') + '</td>' +
      '<td class="mono">' + maskKey(k.api_key) + '</td>' +
      '<td>' +
        '<button class="action" data-edit="' + k.id + '">编辑</button>' +
        '<button class="action delete-btn" data-delete="' + k.id + '">删除</button>' +
      '</td>' +
    '</tr>'
  ).join('');
}

async function loadKeys(){
  const rsp = await fetch('/api/api-keys', {cache:'no-store'});
  if (!rsp.ok) return;
  keys = await rsp.json() || [];
  renderRows();
}

function openModal(key){
  editingId = key ? key.id : null;
  document.getElementById('modal-title').textContent = key ? '编辑配置' : '添加配置';
  document.getElementById('f-name').value = key ? (key.name || '') : '';
  document.getElementById('f-api-key').value = key ? (key.api_key || '') : genApiKey();
  errBox.textContent = '';
  overlay.classList.add('show');
  document.getElementById('f-api-key').focus();
}
function closeModal(){ overlay.classList.remove('show'); editingId = null; }

async function saveKey(){
  const payload = {
    name: document.getElementById('f-name').value.trim(),
    api_key: document.getElementById('f-api-key').value.trim()
  };
  if (!payload.api_key){ errBox.textContent = 'API Key 不能为空'; return; }
  const url = editingId ? '/api/api-keys/' + editingId : '/api/api-keys';
  const method = editingId ? 'PUT' : 'POST';
  const rsp = await fetch(url, {method, headers:{'Content-Type':'application/json'}, body: JSON.stringify(payload)});
  if (!rsp.ok){ errBox.textContent = '保存失败：' + await rsp.text(); return; }
  closeModal();
  loadKeys().catch(() => {});
}

async function deleteKey(id){
  if (!confirm('确认删除这个配置吗？')) return;
  const rsp = await fetch('/api/api-keys/' + id, {method:'DELETE'});
  if (!rsp.ok){ alert('删除失败：' + await rsp.text()); return; }
  loadKeys().catch(() => {});
}

document.getElementById('add-btn').addEventListener('click', () => openModal(null));
document.getElementById('cancel-btn').addEventListener('click', closeModal);
document.getElementById('save-btn').addEventListener('click', () => saveKey().catch(err => errBox.textContent = String(err)));
overlay.addEventListener('click', e => { if (e.target === overlay) closeModal(); });
document.getElementById('key-rows').addEventListener('click', e => {
  const editBtn = e.target.closest('[data-edit]');
  if (editBtn){ const k = keys.find(x => String(x.id) === editBtn.dataset.edit); if (k) openModal(k); return; }
  const delBtn = e.target.closest('[data-delete]');
  if (delBtn){ deleteKey(delBtn.dataset.delete).catch(() => {}); return; }
  if (e.target.closest('a,button,input,select')) return;
  const row = e.target.closest('tr[data-href]');
  if (row) location.href = row.dataset.href;
});
loadKeys().catch(() => {});
</script>
</body>
</html>
{{end}}
`
