package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
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
	"sync"
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
	chains   *chainRouteState
}

const chainRouteCooldown = 2 * time.Minute

type chainSessionState struct {
	LastSuccessRouteID int64
	FailedUntil        map[int64]time.Time
}

type chainRouteState struct {
	mu       sync.Mutex
	sessions map[string]*chainSessionState
}

type forwardPlan struct {
	route          *APIRoute
	chainID        int64
	upstreamURL    string
	upstreamHost   string
	forwardBody    []byte
	effectiveModel string
	adaptTarget    string
	adaptState     *adapterState
	mismatch       string
}

func newChainRouteState() *chainRouteState {
	return &chainRouteState{sessions: make(map[string]*chainSessionState)}
}

func (s *chainRouteState) OrderedRoutes(chainID int64, sessionID string, routes []APIRoute) []APIRoute {
	if len(routes) == 0 {
		return nil
	}
	key := chainSessionKey(chainID, sessionID)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[key]
	if state == nil {
		state = &chainSessionState{FailedUntil: make(map[int64]time.Time)}
		s.sessions[key] = state
	}
	for routeID, until := range state.FailedUntil {
		if !until.After(now) {
			delete(state.FailedUntil, routeID)
		}
	}

	lastSuccessIndex := -1
	for i, route := range routes {
		if route.ID == state.LastSuccessRouteID {
			lastSuccessIndex = i
			break
		}
	}

	out := make([]APIRoute, 0, len(routes))
	addIfReady := func(route APIRoute) {
		if until, cooling := state.FailedUntil[route.ID]; cooling && until.After(now) {
			return
		}
		out = append(out, route)
	}
	if lastSuccessIndex > 0 {
		for _, route := range routes[:lastSuccessIndex] {
			addIfReady(route)
		}
	}
	if lastSuccessIndex >= 0 {
		addIfReady(routes[lastSuccessIndex])
		for _, route := range routes[lastSuccessIndex+1:] {
			addIfReady(route)
		}
		return out
	}
	for _, route := range routes {
		addIfReady(route)
	}
	return out
}

func (s *chainRouteState) MarkFailure(chainID int64, sessionID string, routeID int64) {
	if routeID == 0 {
		return
	}
	key := chainSessionKey(chainID, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[key]
	if state == nil {
		state = &chainSessionState{FailedUntil: make(map[int64]time.Time)}
		s.sessions[key] = state
	}
	state.FailedUntil[routeID] = time.Now().Add(chainRouteCooldown)
}

func (s *chainRouteState) MarkSuccess(chainID int64, sessionID string, routeID int64) {
	if routeID == 0 {
		return
	}
	key := chainSessionKey(chainID, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[key]
	if state == nil {
		state = &chainSessionState{FailedUntil: make(map[int64]time.Time)}
		s.sessions[key] = state
	}
	state.LastSuccessRouteID = routeID
	delete(state.FailedUntil, routeID)
}

func (s *chainRouteState) ClearFailures(chainID int64, sessionID string) {
	key := chainSessionKey(chainID, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[key]
	if state == nil {
		return
	}
	state.FailedUntil = make(map[int64]time.Time)
}

func chainSessionKey(chainID int64, sessionID string) string {
	return fmt.Sprintf("%d:%s", chainID, fallback(sessionID, "unknown"))
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
		chains:   newChainRouteState(),
	}

	router := chi.NewRouter()
	router.Get("/", srv.handleIndex)
	router.Get("/routes", srv.handleRoutes)
	router.Get("/chains", srv.handleChains)
	router.Get("/conversations/{id}", srv.handleConversationDetail)
	router.Get("/favicon.ico", srv.handleFavicon)
	router.Get("/assets/favicon.jpg", srv.handleFavicon)
	router.Get("/api/dashboard", srv.handleAPIDashboard)
	router.Get("/api/routes", srv.handleAPIRoutesList)
	router.Post("/api/routes", srv.handleAPIRouteCreate)
	router.Put("/api/routes/{id}", srv.handleAPIRouteUpdate)
	router.Post("/api/routes/{id}/toggle", srv.handleAPIRouteToggle)
	router.Delete("/api/routes/{id}", srv.handleAPIRouteDelete)
	router.Get("/api/chains", srv.handleAPIChainsList)
	router.Post("/api/chains", srv.handleAPIChainCreate)
	router.Put("/api/chains/{id}", srv.handleAPIChainUpdate)
	router.Post("/api/chains/{id}/toggle", srv.handleAPIChainToggle)
	router.Delete("/api/chains/{id}", srv.handleAPIChainDelete)
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
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		p.logs.Write(logEntry{
			Timestamp:  time.Now().Format(time.RFC3339Nano),
			DurationMS: time.Since(start).Milliseconds(),
			Method:     r.Method,
			Path:       r.URL.RequestURI(),
			Status:     http.StatusBadRequest,
			Error:      err.Error(),
		})
		fmt.Printf("<- %s %s status=%d error=%s\n", r.Method, r.URL.RequestURI(), http.StatusBadRequest, err.Error())
		return
	}
	defer r.Body.Close()
	recordedReqBody := truncateBase64Images(string(reqBody))
	logModel := modelFromBody(reqBody)
	effectiveModel := logModel
	clientStream := clientWantsStream(reqBody)
	meta := requestMetaFromHeaders(r.Header, reqBody, provider)
	// Claude Code 的 quota/warmup 探测请求(max_tokens<=1,内容为 "quota"):
	// 只转发给上游让 CC 读限流状态,不落库、不写日志,避免污染看板。
	isProbe := provider == providerClaude && isClaudeQuotaProbe(reqBody)

	upstreamTarget := p.cfg.target
	if provider == providerClaude {
		upstreamTarget = p.cfg.claudeTarget
	}
	routeStyle := "openai"
	if provider == providerClaude {
		routeStyle = "anthropic"
	}

	reqProtocol := detectProtocol(r.URL.Path)
	plans, chainMode, planErr := p.forwardPlans(r.Context(), routeStyle, upstreamTarget, r, reqProtocol, reqBody, logModel, meta.SessionID)
	if planErr != nil {
		p.finishLocalError(w, r, start, nil, provider, http.StatusServiceUnavailable, planErr.Error(), recordedReqBody, effectiveModel, !isProbe)
		return
	}
	if len(plans) == 0 {
		msg := "链式代理所有路由都在冷却中"
		p.finishLocalError(w, r, start, nil, provider, http.StatusServiceUnavailable, msg, recordedReqBody, effectiveModel, !isProbe)
		return
	}

	var plan forwardPlan
	var resp *http.Response
	var preparedNative string
	var preparedResponseBytes int
	var preparedReadErr error
	var adapterInfo map[string]any
	var lastErr error
	var lastMismatch string
	for i, candidate := range plans {
		plan = candidate
		preparedNative = ""
		preparedResponseBytes = 0
		preparedReadErr = nil
		adapterInfo = nil
		effectiveModel = plan.effectiveModel
		if plan.mismatch != "" {
			lastMismatch = plan.mismatch
			if chainMode && plan.route != nil {
				p.chains.MarkFailure(plan.chainID, meta.SessionID, plan.route.ID)
			}
			if !chainMode || i == len(plans)-1 {
				if chainMode {
					p.chains.ClearFailures(plan.chainID, meta.SessionID)
				}
				p.finishLocalError(w, r, start, &plan, provider, http.StatusMisdirectedRequest, plan.mismatch, recordedReqBody, effectiveModel, !isProbe)
				return
			}
			fmt.Printf("!! chain route failed current=%s next=%s reason=%s\n", chainPlanLabel(plans, i), chainNextLabel(plans, i), plan.mismatch)
			continue
		}
		fmt.Printf("-> [%s] %s %s upstream=%s adapt=%s\n", provider, r.Method, r.URL.RequestURI(), plan.upstreamURL, fallback(plan.adaptTarget, "none"))
		resp, err = p.doUpstream(r, plan)
		if err != nil {
			lastErr = err
			if chainMode && plan.route != nil {
				p.chains.MarkFailure(plan.chainID, meta.SessionID, plan.route.ID)
			}
			if !chainMode || i == len(plans)-1 {
				if chainMode {
					p.chains.ClearFailures(plan.chainID, meta.SessionID)
				}
				p.finishLocalError(w, r, start, &plan, provider, http.StatusBadGateway, err.Error(), recordedReqBody, effectiveModel, !isProbe)
				return
			}
			fmt.Printf("!! chain route failed current=%s next=%s error=%v\n", chainPlanLabel(plans, i), chainNextLabel(plans, i), err)
			continue
		}
		if chainMode && resp.StatusCode >= 400 {
			if plan.route != nil {
				p.chains.MarkFailure(plan.chainID, meta.SessionID, plan.route.ID)
			}
			if i < len(plans)-1 {
				rawFail, _ := io.ReadAll(resp.Body)
				decodedFail := decodeResponseBody(rawFail, resp.Header.Get("Content-Encoding"))
				_ = resp.Body.Close()
				p.writeChainAttemptLog(r, start, plan, resp, recordedReqBody, decodedFail, rawFail, map[string]any{
					"chain_attempt_result": "status_failure",
					"chain_current":        chainPlanLabel(plans, i),
					"chain_next":           chainNextLabel(plans, i),
				}, !isProbe)
				fmt.Printf("!! chain route returned current=%s status=%d next=%s\n", chainPlanLabel(plans, i), resp.StatusCode, chainNextLabel(plans, i))
				continue
			}
			p.chains.ClearFailures(plan.chainID, meta.SessionID)
		}
		if chainMode && plan.adaptTarget != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rawUp, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			chatDecoded := decodeResponseBody(rawUp, resp.Header.Get("Content-Encoding"))
			chat := parseChatResponse(chatDecoded)
			adapterInfo = chainAdapterInfo(plan, plans, i, rawUp, chatDecoded, chat, resp.Header.Get("Content-Encoding"))
			if emptyChatCompletion(chat) {
				if plan.route != nil {
					p.chains.MarkFailure(plan.chainID, meta.SessionID, plan.route.ID)
				}
				lastErr = errors.New("chain route returned empty response")
				if i < len(plans)-1 {
					adapterInfo["chain_attempt_result"] = "empty_response"
					p.writeChainAttemptLog(r, start, plan, resp, recordedReqBody, "", rawUp, adapterInfo, !isProbe)
					fmt.Printf("!! chain route empty current=%s next=%s\n", chainPlanLabel(plans, i), chainNextLabel(plans, i))
					continue
				}
				p.chains.ClearFailures(plan.chainID, meta.SessionID)
				preparedNative = adaptChatResponseToNative(plan.adaptTarget, chatDecoded, clientStream, effectiveModel, plan.adaptState)
				preparedResponseBytes = len(preparedNative)
				preparedReadErr = readErr
				adapterInfo["chain_attempt_result"] = "final_empty_response"
				break
			}
			preparedNative = adaptChatResponseToNative(plan.adaptTarget, chatDecoded, clientStream, effectiveModel, plan.adaptState)
			preparedResponseBytes = len(preparedNative)
			preparedReadErr = readErr
			adapterInfo["chain_attempt_result"] = "selected"
		}
		if chainMode && resp.StatusCode < 400 && plan.route != nil {
			p.chains.MarkSuccess(plan.chainID, meta.SessionID, plan.route.ID)
		}
		break
	}
	if resp == nil {
		msg := fallback(lastMismatch, "链式代理没有可用路由")
		if lastErr != nil {
			msg = lastErr.Error()
		}
		p.finishLocalError(w, r, start, &plan, provider, http.StatusBadGateway, msg, recordedReqBody, effectiveModel, !isProbe)
		return
	}
	defer resp.Body.Close()

	var traceHandle *traceHandle
	if !isProbe {
		p.logs.Write(logEntry{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			Phase:       "start",
			DurationMS:  time.Since(start).Milliseconds(),
			Method:      r.Method,
			Path:        r.URL.RequestURI(),
			UpstreamURL: plan.upstreamURL,
			Model:       effectiveModel,
			RequestBody: recordedReqBody,
			RequestHdrs: sanitizeHeader(cloneHeader(r.Header)),
		})

		traceHandle = p.recorder.Start(TraceStartRecord{
			Provider:     provider,
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			UpstreamURL:  plan.upstreamURL,
			Model:        effectiveModel,
			RequestBody:  recordedReqBody,
			RequestHdrs:  sanitizeHeader(cloneHeader(r.Header)),
			RequestBytes: len(reqBody),
		})
	}

	var decodedRespBody string
	var copyErr error
	var responseBytes int
	if plan.adaptTarget != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 适配路径:读完上游 chat 响应,转换回原生协议(SSE/JSON)后一次性回写。
		native := preparedNative
		readErr := preparedReadErr
		if native == "" {
			rawUp, err := io.ReadAll(resp.Body)
			readErr = err
			chatDecoded := decodeResponseBody(rawUp, resp.Header.Get("Content-Encoding"))
			chat := parseChatResponse(chatDecoded)
			adapterInfo = chainAdapterInfo(plan, nil, -1, rawUp, chatDecoded, chat, resp.Header.Get("Content-Encoding"))
			adapterInfo["chain_attempt_result"] = "selected"
			native = adaptChatResponseToNative(plan.adaptTarget, chatDecoded, clientStream, effectiveModel, plan.adaptState)
		}
		if clientStream {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		w.WriteHeader(resp.StatusCode)
		_, writeErr := io.WriteString(w, native)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if readErr != nil {
			copyErr = readErr
		} else {
			copyErr = writeErr
		}
		decodedRespBody = native
		responseBytes = fallbackInt(preparedResponseBytes, len(native))
	} else {
		// 原生透传:边读边写回(可能 gzip),记录/解析前再按 Content-Encoding 解压副本。
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		var respBody bytes.Buffer
		copyErr = copyAndFlush(w, resp.Body, &respBody)
		decodedRespBody = decodeResponseBody(respBody.Bytes(), resp.Header.Get("Content-Encoding"))
		responseBytes = respBody.Len()
	}
	recordedRespBody := truncateBase64Images(decodedRespBody)

	entry := logEntry{
		Timestamp:    time.Now().Format(time.RFC3339Nano),
		DurationMS:   time.Since(start).Milliseconds(),
		Method:       r.Method,
		Path:         r.URL.RequestURI(),
		UpstreamURL:  plan.upstreamURL,
		Model:        effectiveModel,
		Status:       resp.StatusCode,
		RequestBody:  recordedReqBody,
		ResponseBody: recordedRespBody,
		RequestHdrs:  sanitizeHeader(cloneHeader(r.Header)),
		ResponseHdrs: sanitizeHeader(cloneHeader(resp.Header)),
		AdapterInfo:  adapterInfo,
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
		ResponseBytes: responseBytes,
	}
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		finishRecord.Error = copyErr.Error()
	}
	p.recorder.Finish(traceHandle, finishRecord)
	if copyErr != nil {
		fmt.Printf("<- %s %s status=%d duration=%dms copy_error=%s\n", r.Method, r.URL.RequestURI(), resp.StatusCode, entry.DurationMS, copyErr.Error())
	} else {
		fmt.Printf("<- %s %s status=%d duration=%dms bytes=%d\n", r.Method, r.URL.RequestURI(), resp.StatusCode, entry.DurationMS, responseBytes)
	}
}

func (p *proxyServer) forwardPlans(ctx context.Context, routeStyle string, upstreamTarget *url.URL, r *http.Request, reqProtocol string, reqBody []byte, logModel, sessionID string) ([]forwardPlan, bool, error) {
	chain, chainOK, err := p.store.EnabledChainProxyForStyle(ctx, routeStyle)
	if err != nil {
		log.Printf("load enabled chain proxy: %v", err)
		chainOK = false
	}
	if chainOK {
		routes := p.chains.OrderedRoutes(chain.ID, sessionID, chain.Routes)
		if len(routes) == 0 && len(chain.Routes) > 0 {
			p.chains.ClearFailures(chain.ID, sessionID)
			routes = p.chains.OrderedRoutes(chain.ID, sessionID, chain.Routes)
		}
		plans := make([]forwardPlan, 0, len(routes))
		for _, route := range routes {
			plans = append(plans, buildRoutePlan(route, chain.ID, reqProtocol, r.URL, reqBody, logModel))
		}
		return plans, true, nil
	}

	route, routeOK, err := p.store.EnabledAPIRouteForStyle(ctx, routeStyle)
	if err != nil {
		log.Printf("load enabled route: %v", err)
		routeOK = false
	}
	if routeOK {
		return []forwardPlan{buildRoutePlan(route, 0, reqProtocol, r.URL, reqBody, logModel)}, false, nil
	}
	return []forwardPlan{{
		upstreamURL:    buildUpstreamURL(upstreamTarget, r.URL),
		upstreamHost:   upstreamTarget.Host,
		forwardBody:    reqBody,
		effectiveModel: logModel,
	}}, false, nil
}

func buildRoutePlan(route APIRoute, chainID int64, reqProtocol string, requestURL *url.URL, reqBody []byte, logModel string) forwardPlan {
	plan := forwardPlan{
		route:          &route,
		chainID:        chainID,
		forwardBody:    reqBody,
		effectiveModel: logModel,
	}
	base, err := url.Parse(route.BaseURL)
	switch {
	case err != nil || base.Scheme == "" || base.Host == "":
		plan.mismatch = "启用路由的 Base URL 无效"
	case reqProtocol == route.Protocol:
		plan.upstreamURL = buildRouteUpstreamURL(route.BaseURL, reqProtocol, requestURL.RawQuery)
		plan.upstreamHost = base.Host
		plan.forwardBody = rewriteModel(reqBody, route.Model)
		plan.effectiveModel = effectiveRequestModel(plan.forwardBody, route.Model, logModel)
	case route.Protocol == "chat_completions" && (reqProtocol == "messages" || reqProtocol == "responses"):
		chatBody, state, err := adaptRequestToChat(reqProtocol, reqBody)
		if err != nil {
			plan.mismatch = "请求转换为 Chat 失败: " + err.Error()
		} else {
			plan.upstreamURL = buildRouteUpstreamURL(route.BaseURL, "chat_completions", requestURL.RawQuery)
			plan.upstreamHost = base.Host
			plan.forwardBody = rewriteModel(chatBody, route.Model)
			plan.effectiveModel = effectiveRequestModel(plan.forwardBody, route.Model, logModel)
			plan.adaptTarget = reqProtocol
			plan.adaptState = state
		}
	default:
		plan.mismatch = fmt.Sprintf("请求接口协议(%s)与启用路由(%s)不匹配", fallback(reqProtocol, "unknown"), route.Protocol)
	}
	return plan
}

func (p *proxyServer) doUpstream(r *http.Request, plan forwardPlan) (*http.Response, error) {
	const maxAttempts = 3
	var resp *http.Response
	var err error
	for attempt := 1; ; attempt++ {
		var upstreamReq *http.Request
		upstreamReq, err = http.NewRequestWithContext(r.Context(), r.Method, plan.upstreamURL, bytes.NewReader(plan.forwardBody))
		if err != nil {
			return nil, err
		}
		copyHeaders(upstreamReq.Header, r.Header)
		upstreamReq.Host = plan.upstreamHost
		if plan.route != nil {
			applyRouteAuth(upstreamReq.Header, *plan.route)
		}
		resp, err = p.client.Do(upstreamReq)
		if err == nil {
			return resp, nil
		}
		if r.Context().Err() != nil || attempt >= maxAttempts {
			return nil, err
		}
		fmt.Printf("!! upstream attempt %d/%d failed, retrying: %v\n", attempt, maxAttempts, err)
		time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
	}
}

func (p *proxyServer) finishLocalError(w http.ResponseWriter, r *http.Request, start time.Time, plan *forwardPlan, provider string, status int, message, recordedReqBody, model string, record bool) {
	upstreamURL := ""
	if plan != nil {
		upstreamURL = plan.upstreamURL
	}
	http.Error(w, message, status)
	if record {
		p.logs.Write(logEntry{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			DurationMS:  time.Since(start).Milliseconds(),
			Method:      r.Method,
			Path:        r.URL.RequestURI(),
			UpstreamURL: upstreamURL,
			Model:       model,
			Status:      status,
			RequestBody: recordedReqBody,
			RequestHdrs: sanitizeHeader(cloneHeader(r.Header)),
			Error:       message,
		})
		traceHandle := p.recorder.Start(TraceStartRecord{
			Provider:     provider,
			Method:       r.Method,
			Path:         r.URL.RequestURI(),
			UpstreamURL:  upstreamURL,
			Model:        model,
			RequestBody:  recordedReqBody,
			RequestHdrs:  sanitizeHeader(cloneHeader(r.Header)),
			RequestBytes: len(recordedReqBody),
		})
		p.recorder.Finish(traceHandle, TraceFinishRecord{
			Provider:   provider,
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
			Error:      message,
		})
	}
	fmt.Printf("<- %s %s status=%d error=%s\n", r.Method, r.URL.RequestURI(), status, message)
}

func (p *proxyServer) writeChainAttemptLog(r *http.Request, start time.Time, plan forwardPlan, resp *http.Response, recordedReqBody, decodedBody string, rawBody []byte, info map[string]any, record bool) {
	if !record {
		return
	}
	if info == nil {
		info = make(map[string]any)
	}
	info["upstream_raw_body"] = truncateBase64Images(string(rawBody))
	if decodedBody != "" {
		info["upstream_decoded_body"] = truncateBase64Images(decodedBody)
	}
	p.logs.Write(logEntry{
		Timestamp:    time.Now().Format(time.RFC3339Nano),
		Phase:        "chain_attempt",
		DurationMS:   time.Since(start).Milliseconds(),
		Method:       r.Method,
		Path:         r.URL.RequestURI(),
		UpstreamURL:  plan.upstreamURL,
		Model:        plan.effectiveModel,
		Status:       resp.StatusCode,
		RequestBody:  recordedReqBody,
		ResponseBody: truncateBase64Images(decodedBody),
		RequestHdrs:  sanitizeHeader(cloneHeader(r.Header)),
		ResponseHdrs: sanitizeHeader(cloneHeader(resp.Header)),
		AdapterInfo:  info,
	})
}

func chainPlanLabel(plans []forwardPlan, idx int) string {
	if idx < 0 || idx >= len(plans) {
		return "none"
	}
	plan := plans[idx]
	if plan.route == nil {
		return fmt.Sprintf("%d/%d official", idx+1, len(plans))
	}
	parts := []string{fmt.Sprintf("%d/%d", idx+1, len(plans)), fmt.Sprintf("id=%d", plan.route.ID)}
	if name := strings.TrimSpace(plan.route.Name); name != "" {
		parts = append(parts, "name="+name)
	}
	if model := strings.TrimSpace(plan.route.Model); model != "" {
		parts = append(parts, "model="+model)
	}
	return strings.Join(parts, " ")
}

func chainNextLabel(plans []forwardPlan, idx int) string {
	if idx+1 >= len(plans) {
		return "none"
	}
	return chainPlanLabel(plans, idx+1)
}

func emptyChatCompletion(c chatCompletion) bool {
	return strings.TrimSpace(c.Text) == "" && len(c.ToolCalls) == 0
}

func chainAdapterInfo(plan forwardPlan, plans []forwardPlan, idx int, rawBody []byte, decoded string, chat chatCompletion, encoding string) map[string]any {
	info := map[string]any{
		"adapt_target":              plan.adaptTarget,
		"upstream_content_encoding": encoding,
		"upstream_raw_bytes":        len(rawBody),
		"upstream_decoded_bytes":    len(decoded),
		"upstream_raw_body":         truncateBase64Images(string(rawBody)),
		"upstream_decoded_body":     truncateBase64Images(decoded),
		"chat_text_chars":           len(chat.Text),
		"chat_text_trimmed_chars":   len(strings.TrimSpace(chat.Text)),
		"chat_tool_calls":           len(chat.ToolCalls),
		"chat_finish_reason":        chat.FinishReason,
		"chat_usage_input":          chat.Usage.Input,
		"chat_usage_output":         chat.Usage.Output,
		"chat_usage_total":          chat.Usage.total(),
		"chat_usage_cached":         chat.Usage.Cached,
		"chat_usage_reasoning":      chat.Usage.Reasoning,
		"chat_empty":                emptyChatCompletion(chat),
	}
	if plan.route != nil {
		info["route_id"] = plan.route.ID
		info["route_name"] = plan.route.Name
		info["route_model"] = plan.route.Model
		info["route_protocol"] = plan.route.Protocol
	}
	if len(plans) > 0 && idx >= 0 {
		info["chain_current"] = chainPlanLabel(plans, idx)
		info["chain_next"] = chainNextLabel(plans, idx)
	}
	if len(chat.ToolCalls) > 0 {
		tools := make([]map[string]string, 0, len(chat.ToolCalls))
		for _, tc := range chat.ToolCalls {
			tools = append(tools, map[string]string{
				"id":        tc.ID,
				"name":      tc.Name,
				"arguments": limitString(tc.Arguments, 1000),
			})
		}
		info["chat_tool_call_details"] = tools
	}
	if strings.TrimSpace(chat.Text) != "" {
		info["chat_text_preview"] = limitString(chat.Text, 2000)
	}
	return info
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

// protocolEndpoint 是每种接口协议对应的标准 endpoint 后缀,用于拼第三方路由的上游地址。
var protocolEndpoint = map[string]string{
	"chat_completions": "/chat/completions",
	"responses":        "/responses",
	"messages":         "/messages",
}

// detectProtocol 从请求路径判断接口协议,用于和启用路由配置的协议做匹配。
// 命中不了(如 legacy /v1/complete)返回空,交由上层判定为不匹配。
func detectProtocol(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/chat/completions"):
		return "chat_completions"
	case strings.Contains(p, "/responses"):
		return "responses"
	case strings.Contains(p, "/messages"):
		return "messages"
	}
	return ""
}

// buildRouteUpstreamURL 用路由配置的 Base URL 拼上游地址:Base URL 已含版本前缀
// (如 .../v1),按协议追加标准 endpoint,并保留原查询串。
func buildRouteUpstreamURL(baseURL, protocol, rawQuery string) string {
	u := strings.TrimRight(baseURL, "/") + protocolEndpoint[protocol]
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

// rewriteModel 把请求体里的 model 改成路由配置的 model(仅当配置了 model 时)。
// 用 RawMessage map 只替换 model 键,其余字段原值保留,避免 re-marshal 改动。
func rewriteModel(body []byte, model string) []byte {
	if strings.TrimSpace(model) == "" {
		return body
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	mj, err := json.Marshal(model)
	if err != nil {
		return body
	}
	obj["model"] = mj
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func effectiveRequestModel(body []byte, routeModel, fallbackModel string) string {
	if model := strings.TrimSpace(routeModel); model != "" {
		return model
	}
	if model := modelFromBody(body); model != "" {
		return model
	}
	return fallbackModel
}

// applyRouteAuth 用路由配置的凭证替换客户端自带的认证头,按 API 风格设置。
func applyRouteAuth(h http.Header, route APIRoute) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("Chatgpt-Account-Id")
	h.Del("Openai-Organization")
	if route.APIKey == "" {
		return
	}
	if route.APIStyle == "anthropic" {
		h.Set("X-Api-Key", route.APIKey)
		if h.Get("Anthropic-Version") == "" {
			h.Set("Anthropic-Version", "2023-06-01")
		}
	} else {
		h.Set("Authorization", "Bearer "+route.APIKey)
	}
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
