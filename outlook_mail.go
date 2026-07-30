package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Outlook Web 邮件 REST(consumer)。鉴权头复刻真实浏览器:
// Authorization: MSAuth1.0 usertoken="<access_token>", type="MSACT" + x-anchormailbox。
const outlookMailAPIBase = "https://outlook.live.com/api/beta"

// 允许的文件夹白名单(拼进 URL path,避免注入)。key=前端值,value=well-known folder。
var outlookMailFolders = map[string]bool{
	"inbox": true, "sentitems": true, "drafts": true,
	"deleteditems": true, "junkemail": true, "archive": true,
}

func outlookMailAuthHeader(accessToken string) string {
	return `MSAuth1.0 usertoken="` + accessToken + `", type="MSACT"`
}

func (p *proxyServer) outlookMailClient() *http.Client {
	return &http.Client{Transport: p.client.Transport, Timeout: 30 * time.Second}
}

// outlookMailGet 发一个带鉴权头的 GET,返回解析后的 JSON、HTTP 状态码。
func outlookMailGet(ctx context.Context, client *http.Client, rawURL, accessToken, anchor, prefer string) (map[string]any, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", outlookMailAuthHeader(accessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://outlook.live.com")
	req.Header.Set("Referer", "https://outlook.live.com/mail/0/")
	if anchor != "" {
		req.Header.Set("x-anchormailbox", anchor)
	}
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		return out, resp.StatusCode, fmt.Errorf("Outlook 邮件接口返回 %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return out, resp.StatusCode, nil
}

// outlookMailFetch 取该账号的 access_token 调邮件接口;遇 401(token 过期)自动刷新一次再重试。
func (p *proxyServer) outlookMailFetch(ctx context.Context, id int64, buildURL func() string, prefer string) (map[string]any, error) {
	token, email, err := p.store.GetOutlookAccessToken(ctx, id)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("该账号还没有 access token(尚未登录),无法查询邮件")
	}
	client := p.outlookMailClient()
	data, status, err := outlookMailGet(ctx, client, buildURL(), token, email, prefer)
	if status == http.StatusUnauthorized {
		// token 过期:用 cookie 静默刷新一次,拿到新 token 再试。
		if _, rerr := p.refreshOutlookToken(ctx, id); rerr == nil {
			if token, email, err = p.store.GetOutlookAccessToken(ctx, id); err == nil && token != "" {
				data, _, err = outlookMailGet(ctx, client, buildURL(), token, email, prefer)
			}
		} else {
			return nil, fmt.Errorf("access token 已失效且自动刷新失败(%v),请手动点「刷新 Token」或重新登录", rerr)
		}
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ---- 处理函数 ----

func (p *proxyServer) handleOutlookAccountMail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	account, err := p.store.GetOutlookAccountByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := baseTemplate.ExecuteTemplate(w, "outlook-mail", account); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleAPIOutlookMailList 邮件分页列表。folder + top,或直接用上一页返回的 next(OData @odata.nextLink)。
func (p *proxyServer) handleAPIOutlookMailList(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	next := strings.TrimSpace(q.Get("next"))
	folder := strings.ToLower(strings.TrimSpace(q.Get("folder")))
	if !outlookMailFolders[folder] {
		folder = "inbox"
	}
	top := 25
	if v, e := strconv.Atoi(q.Get("top")); e == nil && v > 0 && v <= 100 {
		top = v
	}
	buildURL := func() string {
		if next != "" {
			return next
		}
		return fmt.Sprintf("%s/me/mailfolders/%s/messages?$top=%d&$select=Id,Subject,From,ReceivedDateTime,BodyPreview,IsRead&$orderby=ReceivedDateTime%%20desc",
			outlookMailAPIBase, folder, top)
	}
	if next != "" {
		// next 是上游返回的完整 URL,校验只允许 outlook.live.com,防 SSRF。
		u, perr := url.Parse(next)
		if perr != nil || !strings.EqualFold(u.Host, "outlook.live.com") {
			http.Error(w, "invalid next link", http.StatusBadRequest)
			return
		}
	}
	data, err := p.outlookMailFetch(r.Context(), id, buildURL, outlookMailListPrefer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"folder":    folder,
		"messages":  summarizeOutlookMail(data),
		"next_link": asString(data["@odata.nextLink"]),
	}, nil)
}

const outlookMailListPrefer = `outlook.body-content-type="text"`

// handleAPIOutlookMailMessage 取单封邮件正文。mid 走查询参数(消息 Id 含 =/+ 等字符,不适合放 path)。
func (p *proxyServer) handleAPIOutlookMailMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad account id", http.StatusBadRequest)
		return
	}
	mid := strings.TrimSpace(r.URL.Query().Get("mid"))
	if mid == "" {
		http.Error(w, "missing mid", http.StatusBadRequest)
		return
	}
	bodyType := "html"
	if r.URL.Query().Get("bodyType") == "text" {
		bodyType = "text"
	}
	buildURL := func() string {
		return fmt.Sprintf("%s/me/messages/%s?$select=Id,Subject,From,ToRecipients,ReceivedDateTime,Body,IsRead,ConversationId",
			outlookMailAPIBase, url.QueryEscape(mid))
	}
	prefer := fmt.Sprintf(`outlook.body-content-type="%s"`, bodyType)
	data, err := p.outlookMailFetch(r.Context(), id, buildURL, prefer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, reshapeOutlookMessage(data), nil)
}

// outlookCodePatterns 按优先级从上到下尝试;命中即返回,不再试后面的。
// 覆盖微软安全码(Security code: 190871 / 安全代码)与 ChatGPT 临时验证码(验证码...801745),
// 最后用「独立 6 位数字」兜底。捕获组 1 就是验证码。
var outlookCodePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)security\s*code\s*[:：]?\s*([0-9]{4,8})`),
	regexp.MustCompile(`安全代码\s*[:：]?\s*([0-9]{4,8})`),
	regexp.MustCompile(`验证码[^0-9]{0,16}([0-9]{4,8})`),
	regexp.MustCompile(`(?i)(?:verification|temporary|one[- ]?time|login|access)\s*code[^0-9]{0,16}([0-9]{4,8})`),
	regexp.MustCompile(`(?i)\bcode\s*[:：]?\s*([0-9]{4,8})`),
	regexp.MustCompile(`(?:^|[^0-9])([0-9]{6})(?:[^0-9]|$)`),
}

// extractVerificationCode 按 outlookCodePatterns 顺序解析,命中第一个即返回。
func extractVerificationCode(text string) string {
	for _, re := range outlookCodePatterns {
		if m := re.FindStringSubmatch(text); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// handleAPIOutlookMailCode 入参邮箱,查该账号最新一封邮件,按顺序解析验证码后返回。
// GET /api/outlook/mail/code?email=user@outlook.com
func (p *proxyServer) handleAPIOutlookMailCode(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" {
		http.Error(w, "missing email", http.StatusBadRequest)
		return
	}
	id, err := p.store.GetOutlookIDByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "该邮箱不存在于 outlook_login_tokens", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 1) 取收件箱最新一封
	listURL := fmt.Sprintf("%s/me/mailfolders/inbox/messages?$top=1&$select=Id,Subject,From,ReceivedDateTime,BodyPreview&$orderby=ReceivedDateTime%%20desc", outlookMailAPIBase)
	listData, err := p.outlookMailFetch(r.Context(), id, func() string { return listURL }, outlookMailListPrefer)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	msgs := summarizeOutlookMail(listData)
	if len(msgs) == 0 {
		writeJSON(w, map[string]any{"ok": false, "email": email, "code": "", "message": "收件箱为空"}, nil)
		return
	}
	latest := msgs[0]
	mid := asString(latest["id"])
	// 2) 取该邮件纯文本正文
	getURL := fmt.Sprintf("%s/me/messages/%s?$select=Id,Subject,From,ReceivedDateTime,Body", outlookMailAPIBase, url.QueryEscape(mid))
	msgData, err := p.outlookMailFetch(r.Context(), id, func() string { return getURL }, `outlook.body-content-type="text"`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	msg := reshapeOutlookMessage(msgData)
	// 正文 + 预览 + 主题一起喂给解析器,提高命中率。
	text := strings.Join([]string{asString(msg["body"]), asString(latest["preview"]), asString(msg["subject"])}, "\n")
	code := extractVerificationCode(text)
	writeJSON(w, map[string]any{
		"ok":       code != "",
		"email":    email,
		"code":     code,
		"subject":  msg["subject"],
		"from":     msg["from"],
		"received": msg["received"],
	}, nil)
}

// ---- OData 解析 ----

func mailChild(m map[string]any, key string) map[string]any {
	if c, ok := m[key].(map[string]any); ok {
		return c
	}
	return nil
}

func mailAddress(emailAddrHolder map[string]any) (addr, name string) {
	ea := mailChild(emailAddrHolder, "EmailAddress")
	if ea == nil {
		return "", ""
	}
	return asString(ea["Address"]), asString(ea["Name"])
}

func summarizeOutlookMail(data map[string]any) []map[string]any {
	out := []map[string]any{}
	values, _ := data["value"].([]any)
	for _, v := range values {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		addr, name := mailAddress(mailChild(m, "From"))
		out = append(out, map[string]any{
			"id":        asString(m["Id"]),
			"subject":   asString(m["Subject"]),
			"from":      addr,
			"from_name": name,
			"received":  asString(m["ReceivedDateTime"]),
			"is_read":   m["IsRead"] == true,
			"preview":   asString(m["BodyPreview"]),
		})
	}
	return out
}

func reshapeOutlookMessage(m map[string]any) map[string]any {
	addr, name := mailAddress(mailChild(m, "From"))
	body := mailChild(m, "Body")
	to := []map[string]any{}
	if tos, ok := m["ToRecipients"].([]any); ok {
		for _, t := range tos {
			if tm, ok := t.(map[string]any); ok {
				a, n := mailAddress(tm)
				to = append(to, map[string]any{"address": a, "name": n})
			}
		}
	}
	return map[string]any{
		"id":              asString(m["Id"]),
		"subject":         asString(m["Subject"]),
		"from":            addr,
		"from_name":       name,
		"to":              to,
		"received":        asString(m["ReceivedDateTime"]),
		"is_read":         m["IsRead"] == true,
		"conversation_id": asString(m["ConversationId"]),
		"body_type":       asString(mailChild(m, "Body")["ContentType"]),
		"body":            asString(body["Content"]),
	}
}

const outlookMailHTML = `
{{define "outlook-mail"}}
<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Outlook 邮件 · AGENT-GO-PROXY</title>
  <link rel="icon" type="image/jpeg" href="/assets/favicon.jpg">
  <style>
    :root{--bg:#f6f7fb;--panel:#fff;--line:#dfe5ee;--text:#20242c;--muted:#6f7787;--blue:#2f6fed;--green:#139a55;--red:#c94040}
    *{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif}.app{display:flex;min-height:100vh}
    .sidebar{width:208px;flex-shrink:0;background:#fff;border-right:1px solid var(--line);padding:20px 14px;position:sticky;top:0;height:100vh}.brand{font-size:13px;font-weight:700;color:var(--muted);letter-spacing:.06em;padding:12px}.nav-item{display:block;padding:10px 12px;border-radius:8px;color:var(--text);font-weight:500;margin-bottom:4px;text-decoration:none}.nav-item:hover{background:#eef2fa}.nav-item.active{background:#eaf1ff;color:var(--blue);font-weight:600}
    .page{flex:1;min-width:0;padding:20px 24px 40px;display:flex;flex-direction:column;height:100vh}.top{display:flex;align-items:center;gap:14px;flex-wrap:wrap}.top h1{font-size:17px;margin:0}.back{color:var(--blue);text-decoration:none}.spacer{flex:1}
    select,button{height:34px;border:1px solid var(--line);border-radius:7px;background:#fff;padding:0 12px;font:inherit;color:var(--text);cursor:pointer}button:hover{border-color:#b8c7dc}
    .mailbox{margin-top:16px;display:grid;grid-template-columns:minmax(320px,400px) 1fr;gap:16px;flex:1;min-height:0}
    .list-pane,.read-pane{background:var(--panel);border:1px solid var(--line);border-radius:9px;display:flex;flex-direction:column;min-height:0}
    .list-head{padding:10px 14px;border-bottom:1px solid var(--line);display:flex;align-items:center;gap:10px;font-size:12px;color:var(--muted)}
    .list-body{overflow-y:auto;flex:1}
    .mrow{padding:12px 14px;border-bottom:1px solid #eef1f6;cursor:pointer}.mrow:hover{background:#f7faff}.mrow.active{background:#eaf1ff}.mrow.unread .msubj{font-weight:700}
    .msubj{font-size:13px;margin-bottom:3px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.mfrom{font-size:12px;color:#4b5563;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.mmeta{display:flex;justify-content:space-between;gap:8px;margin-top:4px}.mtime{font-size:11px;color:var(--muted)}.dot{width:7px;height:7px;border-radius:50%;background:var(--blue);flex-shrink:0}
    .pager{padding:10px 14px;border-top:1px solid var(--line);display:flex;align-items:center;gap:10px;font-size:12px;color:var(--muted)}
    .read-head{padding:14px 18px;border-bottom:1px solid var(--line)}.read-subj{font-size:16px;font-weight:600;margin:0 0 6px}.read-meta{font-size:12px;color:var(--muted);line-height:1.7}
    .read-body{flex:1;min-height:0;overflow:auto;padding:0}iframe.body-frame{width:100%;min-height:200px;border:0;background:#fff;display:block}pre.body-text{margin:0;padding:16px 18px;white-space:pre-wrap;word-break:break-word;font:13px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC",sans-serif}
    .hint{padding:40px;color:var(--muted);text-align:center}.err{color:var(--red)}
  </style>
</head>
<body><div class="app">
  <aside class="sidebar"><div class="brand">AGENT-GO-PROXY</div><nav>
    <a class="nav-item" href="/">Dashboard</a><a class="nav-item" href="/routes">路由</a><a class="nav-item" href="/chains">链式代理</a>
    <a class="nav-item" href="/openai">OPENAI</a><a class="nav-item active" href="/outlook">OUTLOOK</a><a class="nav-item" href="/api-keys">API Key</a>
  </nav></aside>
  <main class="page">
    <div class="top">
      <a class="back" href="/outlook">← 返回账号列表</a>
      <h1 id="title">邮件</h1>
      <div class="spacer"></div>
      <select id="folder">
        <option value="inbox">收件箱</option>
        <option value="sentitems">已发送</option>
        <option value="drafts">草稿</option>
        <option value="junkemail">垃圾邮件</option>
        <option value="deleteditems">已删除</option>
        <option value="archive">存档</option>
      </select>
      <select id="bodytype"><option value="html">正文 HTML</option><option value="text">正文 纯文本</option></select>
      <button id="reload" type="button">刷新</button>
    </div>
    <div class="mailbox">
      <section class="list-pane">
        <div class="list-head"><span id="list-info">加载中…</span></div>
        <div class="list-body" id="list-body"></div>
        <div class="pager"><button id="prev" type="button" disabled>上一页</button><button id="next" type="button" disabled>下一页</button><span id="page-info"></span></div>
      </section>
      <section class="read-pane">
        <div id="read"><div class="hint">从左侧选择一封邮件查看内容</div></div>
      </section>
    </div>
  </main>
</div>
<script>
const account={{toJSON .}};
const esc=v=>String(v??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const fmtTime=v=>v?new Date(v).toLocaleString('zh-CN',{hour12:false}):'';
document.getElementById('title').textContent='邮件 · '+(account.email||'');
let history=[''];   // 每页的 next token(第一页为空串);栈式前进/后退
let cur=0;
let curFolder='inbox';
function api(path){return '/api/outlook/accounts/'+account.id+path}
async function loadList(nextToken){
  const info=document.getElementById('list-info');info.textContent='加载中…';
  const body=document.getElementById('list-body');
  try{
    const qs=nextToken?('?next='+encodeURIComponent(nextToken)):('?folder='+encodeURIComponent(curFolder)+'&top=25');
    const rsp=await fetch(api('/messages'+qs),{cache:'no-store'});
    if(!rsp.ok)throw new Error(await rsp.text());
    const data=await rsp.json();
    const msgs=data.messages||[];
    body.innerHTML=msgs.map(m=>
      '<div class="mrow'+(m.is_read?'':' unread')+'" data-id="'+esc(m.id)+'">'+
      '<div class="msubj">'+esc(m.subject||'(无主题)')+'</div>'+
      '<div class="mmeta"><span class="mfrom">'+esc(m.from_name||m.from||'-')+'</span><span class="mtime">'+esc(fmtTime(m.received))+'</span></div>'+
      '</div>').join('')||'<div class="hint">该文件夹没有邮件</div>';
    info.textContent=curFolder+' · 本页 '+msgs.length+' 封';
    // 分页 token 记账
    window.__nextLink=data.next_link||'';
    document.getElementById('next').disabled=!window.__nextLink;
    document.getElementById('prev').disabled=cur<=0;
    document.getElementById('page-info').textContent='第 '+(cur+1)+' 页';
  }catch(err){info.innerHTML='<span class="err">加载失败：'+esc(err.message)+'</span>';body.innerHTML=''}
}
document.getElementById('list-body').addEventListener('click',async event=>{
  const row=event.target.closest('.mrow');if(!row)return;
  document.querySelectorAll('.mrow.active').forEach(e=>e.classList.remove('active'));
  row.classList.add('active');row.classList.remove('unread');
  await openMessage(row.dataset.id);
});
async function openMessage(mid){
  const read=document.getElementById('read');read.innerHTML='<div class="hint">加载邮件内容…</div>';
  try{
    const bt=document.getElementById('bodytype').value;
    const rsp=await fetch(api('/message?mid='+encodeURIComponent(mid)+'&bodyType='+bt),{cache:'no-store'});
    if(!rsp.ok)throw new Error(await rsp.text());
    const m=await rsp.json();
    const to=(m.to||[]).map(t=>t.name||t.address).filter(Boolean).join(', ');
    const head='<div class="read-head"><h2 class="read-subj">'+esc(m.subject||'(无主题)')+'</h2>'+
      '<div class="read-meta">发件人：'+esc(m.from_name||'')+' &lt;'+esc(m.from||'')+'&gt;<br>收件人：'+esc(to||'-')+'<br>时间：'+esc(fmtTime(m.received))+'</div></div>';
    let bodyHTML;
    if((m.body_type||'').toLowerCase()==='html'){
      // 用 sandbox iframe 隔离渲染邮件 HTML(禁脚本),避免 XSS。
      bodyHTML='<div class="read-body"><iframe class="body-frame" sandbox="allow-same-origin" srcdoc="'+esc(m.body||'')+'"></iframe></div>';
    }else{
      bodyHTML='<div class="read-body"><pre class="body-text">'+esc(m.body||'')+'</pre></div>';
    }
    read.innerHTML=head+bodyHTML;
    // HTML 正文:iframe 高度自适应内容,让外层阅读区滚动,避免正文被固定高度截断。
    const frame=read.querySelector('iframe.body-frame');
    if(frame){
      const fit=()=>{try{const d=frame.contentDocument;if(d&&d.body){frame.style.height=Math.max(d.documentElement.scrollHeight,d.body.scrollHeight)+'px'}}catch(e){}};
      frame.addEventListener('load',fit);setTimeout(fit,120);setTimeout(fit,500);
    }
  }catch(err){read.innerHTML='<div class="hint err">加载失败：'+esc(err.message)+'</div>'}
}
document.getElementById('next').addEventListener('click',()=>{
  if(!window.__nextLink)return;
  history=history.slice(0,cur+1);history.push(window.__nextLink);cur++;loadList(window.__nextLink);
});
document.getElementById('prev').addEventListener('click',()=>{
  if(cur<=0)return;cur--;loadList(history[cur]);
});
function reset(){history=[''];cur=0;document.getElementById('read').innerHTML='<div class="hint">从左侧选择一封邮件查看内容</div>';loadList('')}
document.getElementById('folder').addEventListener('change',e=>{curFolder=e.target.value;reset()});
document.getElementById('reload').addEventListener('click',()=>loadList(history[cur]));
reset();
</script>
</body></html>
{{end}}
`
