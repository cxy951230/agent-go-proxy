package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// GPT 流程专用的 CDP 传输:完全照 gpt-login-automation skill 的方式——
// `--remote-debugging-port` 起浏览器,再**直连页面的 webSocketDebuggerUrl**(页面级连接)。
//
// 为什么不用 cdp.go 的 pipe + Target.attachToTarget:浏览器级附着会触发 Chrome 的
// AutomationControlled,让 navigator.webdriver=true,被 OpenAI/Cloudflare 判定机器人、
// 返回 HTML 错误页。直连页面 WS 不碰浏览器级目标,不触发该特性,与 skill 行为一致。
//
// WS 客户端零依赖手写(RFC6455 客户端子集):文本帧、客户端掩码、分片重组、ping→pong。

// ---- 最小 WebSocket 客户端 ----

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

// wsDial 拨一个 ws:// 地址并完成握手(CDP 的 webSocketDebuggerUrl 是明文 ws)。
func wsDial(rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return nil, err
	}
	keyRaw := make([]byte, 16)
	_, _ = rand.Read(keyRaw)
	key := base64.StdEncoding.EncodeToString(keyRaw)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(conn, 1<<20)
	// 读握手应答:状态行必须 101,校验 Sec-WebSocket-Accept。
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(statusLine, " 101 ") {
		conn.Close()
		return nil, fmt.Errorf("WebSocket 握手失败: %s", strings.TrimSpace(statusLine))
	}
	accept := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if i := strings.Index(line, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(line[:i]), "Sec-WebSocket-Accept") {
			accept = strings.TrimSpace(line[i+1:])
		}
	}
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if accept != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, errors.New("WebSocket 握手校验失败")
	}
	return &wsConn{conn: conn, br: br}, nil
}

// WriteText 发一个客户端掩码的文本帧(CDP 命令是单条 JSON)。
func (c *wsConn) WriteText(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var header []byte
	header = append(header, 0x81) // FIN=1, opcode=1(text)
	n := len(payload)
	switch {
	case n <= 125:
		header = append(header, byte(0x80|n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		header = append(header, b[:]...)
	default:
		header = append(header, 0x80|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		header = append(header, b[:]...)
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	header = append(header, mask[:]...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

// ReadText 读下一条完整的文本消息(重组分片,应答 ping,跳过 pong)。close 帧返回 EOF。
func (c *wsConn) ReadText() ([]byte, error) {
	var msg []byte
	for {
		h0, err := c.br.ReadByte()
		if err != nil {
			return nil, err
		}
		h1, err := c.br.ReadByte()
		if err != nil {
			return nil, err
		}
		fin := h0&0x80 != 0
		opcode := h0 & 0x0F
		masked := h1&0x80 != 0
		length := int(h1 & 0x7F)
		if length == 126 {
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			length = int(binary.BigEndian.Uint16(b[:]))
		} else if length == 127 {
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			length = int(binary.BigEndian.Uint64(b[:]))
		}
		var maskKey [4]byte
		if masked { // 服务端到客户端本不应掩码,但兼容处理
			if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
		switch opcode {
		case 0x8: // close
			return nil, io.EOF
		case 0x9: // ping → pong
			_ = c.writePong(payload)
			continue
		case 0xA: // pong
			continue
		case 0x0, 0x1, 0x2: // continuation / text / binary
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		default:
			// 未知帧忽略
		}
	}
}

func (c *wsConn) writePong(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	header := []byte{0x8A, byte(0x80 | len(payload))}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

func (c *wsConn) Close() { _ = c.conn.Close() }

// ---- 端口模式的 CDP 会话(直连页面 WS)----

type gptChrome struct {
	proc    *exec.Cmd
	profile string
	ws      *wsConn

	mu      sync.Mutex
	seq     int
	pending map[int]chan cdpMessage
	closed  bool
	done    chan struct{}
}

// launchGPTChrome 起一个带独立 profile 的 Chrome(端口模式),直连页面 WS。任务结束 Close() 删 profile。
func launchGPTChrome(ctx context.Context, startURL string) (*gptChrome, error) {
	binary := chromeExecutable()
	if binary == "" {
		return nil, errors.New("没找到 Chrome,可用环境变量 CHROME_PATH 指定可执行文件路径")
	}
	port, err := freeTCPPort()
	if err != nil {
		return nil, err
	}
	profile, err := os.MkdirTemp("", "agent-go-proxy-gpt-")
	if err != nil {
		return nil, err
	}
	// 与 skill 的启动参数一致(不加 AutomationControlled 之类会弹横幅/被检测的标志)。
	cmd := exec.Command(binary,
		"--user-data-dir="+profile,
		"--no-first-run",
		"--disable-sync",
		"--disable-default-apps",
		"--remote-allow-origins=*",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--new-window",
		startURL,
	)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(profile)
		return nil, fmt.Errorf("启动 Chrome 失败: %w", err)
	}
	c := &gptChrome{proc: cmd, profile: profile, pending: make(map[int]chan cdpMessage), done: make(chan struct{})}

	// 等 CDP HTTP 就绪,取页面目标的 webSocketDebuggerUrl 并直连。
	wsURL, err := waitPageWebSocketURL(ctx, port, 20*time.Second)
	if err != nil {
		c.kill()
		return nil, err
	}
	ws, err := wsDial(wsURL)
	if err != nil {
		c.kill()
		return nil, err
	}
	c.ws = ws
	go c.readLoop()
	_ = c.call(ctx, "Runtime.enable", nil, nil)
	_ = c.call(ctx, "Page.enable", nil, nil)
	return c, nil
}

func (c *gptChrome) readLoop() {
	defer close(c.done)
	for {
		raw, err := c.ws.ReadText()
		if err != nil {
			return
		}
		var m cdpMessage
		if json.Unmarshal(raw, &m) != nil || m.ID == 0 {
			continue // 事件(无 id)/解析不了的忽略
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (c *gptChrome) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("Chrome 会话已关闭")
	}
	c.seq++
	id := c.seq
	ch := make(chan cdpMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	raw, err := json.Marshal(cdpMessage{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if err := c.ws.WriteText(raw); err != nil {
		return fmt.Errorf("写 CDP WS 失败: %w", err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-c.done:
		return errors.New("Chrome 已退出")
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *gptChrome) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Evaluate 在页面执行 JS 取回值(与 cdpPage.Evaluate 同签名)。
func (c *gptChrome) Evaluate(ctx context.Context, expression string, out any) error {
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := c.call(ctx, "Runtime.evaluate", map[string]any{
		"expression": expression, "awaitPromise": true, "returnByValue": true,
		"timeout": 10000, "userGesture": true,
	}, &result); err != nil {
		return err
	}
	if result.ExceptionDetails != nil {
		return fmt.Errorf("页面脚本异常: %s", result.ExceptionDetails.Text)
	}
	if out != nil && len(result.Result.Value) > 0 {
		return json.Unmarshal(result.Result.Value, out)
	}
	return nil
}

func (c *gptChrome) InsertText(ctx context.Context, text string) error {
	return c.call(ctx, "Input.insertText", map[string]any{"text": text}, nil)
}

func (c *gptChrome) Key(ctx context.Context, kind, key, code string, modifiers int) error {
	params := map[string]any{"type": kind, "key": key, "code": code}
	if modifiers != 0 {
		params["modifiers"] = modifiers
	}
	return c.call(ctx, "Input.dispatchKeyEvent", params, nil)
}

func (c *gptChrome) Char(ctx context.Context, text string) error {
	return c.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "char", "text": text}, nil)
}

func (c *gptChrome) MouseClick(ctx context.Context, x, y float64) error {
	params := map[string]any{"x": x, "y": y, "button": "left", "clickCount": 1, "pointerType": "mouse"}
	if err := c.call(ctx, "Input.dispatchMouseEvent", mergeMaps(params, map[string]any{"type": "mouseMoved"}), nil); err != nil {
		return err
	}
	if err := c.call(ctx, "Input.dispatchMouseEvent", mergeMaps(params, map[string]any{"type": "mousePressed"}), nil); err != nil {
		return err
	}
	return c.call(ctx, "Input.dispatchMouseEvent", mergeMaps(params, map[string]any{"type": "mouseReleased"}), nil)
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func (c *gptChrome) Navigate(ctx context.Context, u string) error {
	return c.call(ctx, "Page.navigate", map[string]any{"url": u}, nil)
}

// Close 关掉 Chrome 并删除临时 profile。
func (c *gptChrome) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	if c.ws != nil {
		c.ws.Close()
	}
	c.kill()
}

func (c *gptChrome) kill() {
	if c.proc != nil && c.proc.Process != nil {
		_ = c.proc.Process.Kill()
		_, _ = c.proc.Process.Wait()
	}
	if c.profile != "" {
		_ = os.RemoveAll(c.profile)
	}
}

// freeTCPPort 让内核分配一个空闲端口(与 skill 的 find_free_port 一致)。
func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitPageWebSocketURL 轮询 /json/list,取一个页面目标的 webSocketDebuggerUrl。
func waitPageWebSocketURL(ctx context.Context, port int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/json/list", nil)
		if resp, err := client.Do(req); err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			var targets []struct {
				Type  string `json:"type"`
				URL   string `json:"url"`
				WSURL string `json:"webSocketDebuggerUrl"`
			}
			if json.Unmarshal(body, &targets) == nil {
				// 优先 auth.openai.com / chatgpt.com 的页面,否则第一个 page。
				var first string
				for _, t := range targets {
					if t.Type != "page" || t.WSURL == "" {
						continue
					}
					if strings.Contains(t.URL, "auth.openai.com") || strings.Contains(t.URL, "chatgpt.com") {
						return t.WSURL, nil
					}
					if first == "" {
						first = t.WSURL
					}
				}
				if first != "" {
					return first, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", errors.New("CDP 端口没就绪或没有可用页面目标")
		}
		if !sleepCtx(ctx, 300*time.Millisecond) {
			return "", context.Canceled
		}
	}
}
