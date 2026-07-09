package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
)

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"trailers":            {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

type config struct {
	listenAddr   string
	target       *url.URL
	claudeTarget *url.URL
	logDir       string
	dsn          string
}

type proxyServer struct {
	cfg      config
	client   *http.Client
	logs     *logWriter
	store    *Store
	recorder *asyncRecorder
}

func main() {
	listenAddr := flag.String("listen", "127.0.0.1:8080", "local listen address")
	targetValue := flag.String("target", envOrDefault("UPSTREAM_BASE_URL", "https://chatgpt.com/backend-api/codex"), "upstream base URL for Codex requests")
	claudeTargetValue := flag.String("claude-target", envOrDefault("CLAUDE_BASE_URL", "https://api.anthropic.com"), "upstream base URL for Claude (Anthropic) requests")
	logDir := flag.String("log-dir", "log", "directory for date-based JSONL logs")
	dsn := flag.String("mysql-dsn", envOrDefault("MYSQL_DSN", "root:123456@tcp(127.0.0.1:3306)/agent_go_proxy?parseTime=true&charset=utf8mb4&loc=Local"), "MySQL DSN")
	flag.Parse()

	target, err := url.Parse(*targetValue)
	if err != nil || target.Scheme == "" || target.Host == "" {
		log.Fatalf("invalid -target %q", *targetValue)
	}

	claudeTarget, err := url.Parse(*claudeTargetValue)
	if err != nil || claudeTarget.Scheme == "" || claudeTarget.Host == "" {
		log.Fatalf("invalid -claude-target %q", *claudeTargetValue)
	}

	if err := os.MkdirAll(*logDir, 0755); err != nil {
		log.Fatalf("create log dir: %v", err)
	}

	lw := newLogWriter(*logDir)
	defer lw.Close()

	store, err := NewStore(*dsn)
	if err != nil {
		log.Fatalf("init mysql store: %v", err)
	}
	defer store.Close()
	recorder := newAsyncRecorder(store)
	defer recorder.Close()

	srv := &proxyServer{
		cfg: config{
			listenAddr:   *listenAddr,
			target:       target,
			claudeTarget: claudeTarget,
			logDir:       *logDir,
			dsn:          *dsn,
		},
		client: &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2: true,
				MaxIdleConns:      100,
				// 经本地代理(clash 等)时空闲隧道常被上游提前关闭,复用会得到 EOF。
				// 缩短空闲超时,减少复用失效连接的概率(配合上层重试基本消除瞬时 502)。
				IdleConnTimeout:       15 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
			Timeout: 0,
		},
		logs:     lw,
		store:    store,
		recorder: recorder,
	}

	router := chi.NewRouter()
	router.Get("/", srv.handleIndex)
	router.Get("/conversations/{id}", srv.handleConversationDetail)
	router.Get("/favicon.ico", srv.handleFavicon)
	router.Get("/assets/favicon.jpg", srv.handleFavicon)
	router.Get("/api/dashboard", srv.handleAPIDashboard)
	router.Get("/api/conversations", srv.handleAPIConversations)
	router.Get("/api/conversations/{id}", srv.handleAPIConversationDetail)
	router.Delete("/api/conversations/{id}", srv.handleAPIConversationDelete)
	router.Post("/api/conversations/{id}/tags", srv.handleAPIConversationTags)
	router.Post("/api/accounts/{id}/alias", srv.handleAPIAccountAlias)
	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	router.NotFound(srv.handleProxyFallback)
	router.MethodNotAllowed(srv.handleProxyFallback)

	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           router,
		ReadHeaderTimeout: 30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("agent-go-proxy listening on http://%s\n", *listenAddr)
		fmt.Printf("codex upstream target: %s\n", target.String())
		fmt.Printf("claude upstream target: %s\n", claudeTarget.String())
		fmt.Printf("log dir: %s\n", *logDir)
		fmt.Printf("dashboard: http://%s/\n", *listenAddr)
		fmt.Printf("for Codex CLI: CODEX_HOME=/path/to/codex_home NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost codex\n")
		errCh <- httpServer.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		fmt.Printf("\nreceived %s, shutting down\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}
}

func (p *proxyServer) handleProxyFallback(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	p.ServeHTTP(w, r)
}

func (p *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	provider := detectProvider(r)
	upstreamTarget := p.cfg.target
	if provider == providerClaude {
		upstreamTarget = p.cfg.claudeTarget
	}
	upstreamURL := buildUpstreamURL(upstreamTarget, r.URL)
	fmt.Printf("-> [%s] %s %s upstream=%s\n", provider, r.Method, r.URL.RequestURI(), upstreamURL)

	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		p.logs.Write(logEntry{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			DurationMS:  time.Since(start).Milliseconds(),
			Method:      r.Method,
			Path:        r.URL.RequestURI(),
			UpstreamURL: upstreamURL,
			Status:      http.StatusBadRequest,
			Error:       err.Error(),
		})
		fmt.Printf("<- %s %s status=%d error=%s\n", r.Method, r.URL.RequestURI(), http.StatusBadRequest, err.Error())
		return
	}
	defer r.Body.Close()
	recordedReqBody := truncateBase64Images(string(reqBody))
	// Claude Code 的 quota/warmup 探测请求(max_tokens<=1,内容为 "quota"):
	// 只转发给上游让 CC 读限流状态,不落库、不写日志,避免污染看板。
	isProbe := provider == providerClaude && isClaudeQuotaProbe(reqBody)

	var traceHandle *traceHandle
	if !isProbe {
		p.logs.Write(logEntry{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			Phase:       "start",
			DurationMS:  time.Since(start).Milliseconds(),
			Method:      r.Method,
			Path:        r.URL.RequestURI(),
			UpstreamURL: upstreamURL,
			RequestBody: recordedReqBody,
			RequestHdrs: sanitizeHeader(cloneHeader(r.Header)),
		})

		traceHandle = p.recorder.Start(TraceStartRecord{
			Provider:     provider,
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			UpstreamURL:  upstreamURL,
			RequestBody:  recordedReqBody,
			RequestHdrs:  sanitizeHeader(cloneHeader(r.Header)),
			RequestBytes: len(reqBody),
		})
	}

	// 请求体已缓存,响应写回前若上游传输层瞬时失败(常见于经本地代理复用了
	// 失效的长连接导致 EOF),用全新连接重试若干次,避免把可恢复的抖动透传成 502。
	const maxAttempts = 3
	var resp *http.Response
	for attempt := 1; ; attempt++ {
		var upstreamReq *http.Request
		upstreamReq, err = http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(reqBody))
		if err != nil {
			break // 构造请求失败,不可重试
		}
		copyHeaders(upstreamReq.Header, r.Header)
		upstreamReq.Host = upstreamTarget.Host

		resp, err = p.client.Do(upstreamReq)
		if err == nil {
			break
		}
		// 客户端已断开、已达上限则不再重试
		if r.Context().Err() != nil || attempt >= maxAttempts {
			break
		}
		fmt.Printf("!! upstream attempt %d/%d failed, retrying: %v\n", attempt, maxAttempts, err)
		time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		if !isProbe {
			p.logs.Write(logEntry{
				Timestamp:   time.Now().Format(time.RFC3339Nano),
				DurationMS:  time.Since(start).Milliseconds(),
				Method:      r.Method,
				Path:        r.URL.RequestURI(),
				UpstreamURL: upstreamURL,
				Status:      http.StatusBadGateway,
				RequestBody: recordedReqBody,
				RequestHdrs: sanitizeHeader(cloneHeader(r.Header)),
				Error:       err.Error(),
			})
		}
		fmt.Printf("<- %s %s status=%d error=%s\n", r.Method, r.URL.RequestURI(), http.StatusBadGateway, err.Error())
		p.recorder.Finish(traceHandle, TraceFinishRecord{
			Status:     http.StatusBadGateway,
			DurationMS: time.Since(start).Milliseconds(),
			Error:      err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var respBody bytes.Buffer
	copyErr := copyAndFlush(w, resp.Body, &respBody)
	// 转发给客户端的是原始字节(可能 gzip),记录/解析前要按 Content-Encoding 解压,
	// 否则 SSE 解析拿到的是压缩流,token 等全部解不出来。
	decodedRespBody := decodeResponseBody(respBody.Bytes(), resp.Header.Get("Content-Encoding"))
	recordedRespBody := truncateBase64Images(decodedRespBody)

	entry := logEntry{
		Timestamp:    time.Now().Format(time.RFC3339Nano),
		DurationMS:   time.Since(start).Milliseconds(),
		Method:       r.Method,
		Path:         r.URL.RequestURI(),
		UpstreamURL:  upstreamURL,
		Status:       resp.StatusCode,
		RequestBody:  recordedReqBody,
		ResponseBody: recordedRespBody,
		RequestHdrs:  sanitizeHeader(cloneHeader(r.Header)),
		ResponseHdrs: sanitizeHeader(cloneHeader(resp.Header)),
	}
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		entry.Error = copyErr.Error()
	}
	if !isProbe {
		p.logs.Write(entry)
	}

	finishRecord := TraceFinishRecord{
		Provider:      provider,
		Probe:         isProbe,
		Status:        resp.StatusCode,
		DurationMS:    time.Since(start).Milliseconds(),
		ResponseBody:  recordedRespBody,
		ResponseHdrs:  sanitizeHeader(cloneHeader(resp.Header)),
		ResponseBytes: respBody.Len(),
	}
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		finishRecord.Error = copyErr.Error()
	}
	p.recorder.Finish(traceHandle, finishRecord)
	if copyErr != nil {
		fmt.Printf("<- %s %s status=%d duration=%dms copy_error=%s\n", r.Method, r.URL.RequestURI(), resp.StatusCode, entry.DurationMS, copyErr.Error())
	} else {
		fmt.Printf("<- %s %s status=%d duration=%dms bytes=%d\n", r.Method, r.URL.RequestURI(), resp.StatusCode, entry.DurationMS, respBody.Len())
	}
}

func buildUpstreamURL(target *url.URL, requestURL *url.URL) string {
	out := *target
	targetPath := strings.TrimRight(target.Path, "/")
	requestPath := requestURL.EscapedPath()
	if requestPath == "" {
		requestPath = "/"
	}

	forwardPath := requestPath
	if strings.Contains(targetPath, "/backend-api/codex") && strings.HasPrefix(forwardPath, "/v1/") {
		forwardPath = strings.TrimPrefix(forwardPath, "/v1")
	}

	if targetPath == "" {
		out.Path = forwardPath
	} else if targetPath == "/v1" && strings.HasPrefix(forwardPath, "/v1/") {
		out.Path = forwardPath
	} else {
		out.Path = targetPath + "/" + strings.TrimLeft(forwardPath, "/")
	}
	out.RawQuery = requestURL.RawQuery
	return out.String()
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := hopByHopHeaders[strings.ToLower(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func cloneHeader(src http.Header) map[string][]string {
	dst := make(map[string][]string, len(src))
	for key, values := range src {
		copied := make([]string, len(values))
		copy(copied, values)
		dst[key] = copied
	}
	return dst
}

func copyAndFlush(dst http.ResponseWriter, src io.Reader, logBuf *bytes.Buffer) error {
	flusher, _ := dst.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			logBuf.Write(chunk)
			if _, writeErr := dst.Write(chunk); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// decodeResponseBody 按响应的 Content-Encoding 解压,仅用于本地记录与解析。
// 转发给客户端的仍是原始压缩字节,这里只解压副本。无法识别或解压失败时回退原文。
func decodeResponseBody(raw []byte, encoding string) string {
	if len(raw) == 0 {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return string(raw)
		}
		defer zr.Close()
		if out, err := io.ReadAll(zr); err == nil {
			return string(out)
		}
	case "deflate":
		// deflate 可能带 zlib 头,也可能是裸 flate,两种都试一下。
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			defer zr.Close()
			if out, err := io.ReadAll(zr); err == nil {
				return string(out)
			}
		}
		fr := flate.NewReader(bytes.NewReader(raw))
		defer fr.Close()
		if out, err := io.ReadAll(fr); err == nil {
			return string(out)
		}
	}
	// 空 / identity / 不支持的(br、zstd)直接回退原文
	return string(raw)
}

// detectProvider 区分进来的是 Claude(Anthropic)还是 Codex(OpenAI)请求。
// 以路径为主、请求头为辅，默认回落到 Codex 以兼容历史行为。
func detectProvider(r *http.Request) string {
	path := strings.ToLower(r.URL.Path)
	switch {
	case strings.Contains(path, "/messages"), strings.Contains(path, "/v1/complete"):
		return providerClaude
	case strings.Contains(path, "/responses"), strings.Contains(path, "/chat/completions"):
		return providerCodex
	}
	if r.Header.Get("Anthropic-Version") != "" || r.Header.Get("X-Api-Key") != "" {
		return providerClaude
	}
	if strings.Contains(strings.ToLower(r.Header.Get("User-Agent")), "claude") {
		return providerClaude
	}
	return providerCodex
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
