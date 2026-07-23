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
	"sort"
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
	cfg          config
	client       *http.Client
	logs         *logWriter
	store        *Store
	recorder     *asyncRecorder
	chains       *chainRouteState
	accounts     *accountPool
	openaiLogins *openAILoginManager
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
	// accountAuth 非空表示走「API Key 直连」:用 OPENAI 菜单里配置的 GPT 账号
	// 凭证直接打官方 ChatGPT 后端,不经过路由/链式代理。
	accountAuth *openaiAccountAuth
	// apiKeyID 是命中的「API Key」页配置 id(account 直连路径),用于 token 明细关联。
	apiKeyID int64
	// accountCandidate 是 account 直连路径待尝试的候选账号(负载均衡/容错时逐个尝试)。
	// 鉴权(刷新+解析 token)延后到真正尝试前再做,避免为用不到的候选浪费刷新。
	accountCandidate *OpenAIAccount
	// directJSON:直连时客户端要非流式响应。上游一律流式,拿到 SSE 后聚合成单个 JSON 返回。
	directJSON bool
}

// openaiAccountAuth 是直连官方上游时要注入的账号凭证。
type openaiAccountAuth struct {
	AccessToken string
	AccountID   string
	Label       string
	// AccountDBID 是 openai_accounts.id,用于负载均衡状态跟踪与 token 明细关联。
	AccountDBID int64
}

// planSource 依据最终命中的转发计划算出 token 明细要记录的来源信息(尽量用 id 关联)。
func planSource(plan forwardPlan) (sourceType string, routeID, chainID, apiKeyID, accountDBID int64) {
	switch {
	case plan.accountAuth != nil:
		sourceType = "account"
		apiKeyID = plan.apiKeyID
		accountDBID = plan.accountAuth.AccountDBID
	case plan.chainID != 0:
		sourceType = "chain"
		chainID = plan.chainID
		if plan.route != nil {
			routeID = plan.route.ID
		}
	case plan.route != nil:
		sourceType = "route"
		routeID = plan.route.ID
	default:
		sourceType = "direct"
	}
	return sourceType, routeID, chainID, apiKeyID, accountDBID
}

// localError 是代理自己产生的错误(不是上游返回的),带上要回给客户端的状态码。
type localError struct {
	status  int
	message string
}

func (e *localError) Error() string { return e.message }

func newLocalError(status int, format string, args ...any) *localError {
	return &localError{status: status, message: fmt.Sprintf(format, args...)}
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
	bridgeLibrary := flag.String("codex-bridge-lib", defaultBridgeLoginBin(), "path to libcodex_bridge dynamic library")
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
	openaiLogins := newOpenAILoginManager(*bridgeLibrary, store)
	defer openaiLogins.Close()

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
		logs:         lw,
		store:        store,
		recorder:     recorder,
		chains:       newChainRouteState(),
		accounts:     newAccountPool(),
		openaiLogins: openaiLogins,
	}

	router := chi.NewRouter()
	router.Get("/", srv.handleIndex)
	router.Get("/routes", srv.handleRoutes)
	router.Get("/chains", srv.handleChains)
	router.Get("/api-keys", srv.handleAPIKeysPage)
	router.Get("/openai", srv.handleOpenAI)
	router.Get("/openai/accounts/{id}", srv.handleOpenAIAccount)
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
	router.Get("/api/api-keys", srv.handleAPIKeysList)
	router.Post("/api/api-keys", srv.handleAPIKeyCreate)
	router.Put("/api/api-keys/{id}", srv.handleAPIKeyUpdate)
	router.Delete("/api/api-keys/{id}", srv.handleAPIKeyDelete)
	router.Get("/api/openai/accounts", srv.handleAPIOpenAIAccounts)
	router.Delete("/api/openai/accounts/{id}", srv.handleAPIOpenAIAccountDelete)
	router.Post("/api/openai/logins", srv.handleAPIOpenAILoginStart)
	router.Get("/api/openai/logins/{id}", srv.handleAPIOpenAILoginStatus)
	router.Post("/api/openai/logins/{id}/cancel", srv.handleAPIOpenAILoginCancel)
	router.Post("/api/openai/accounts/{id}/refresh", srv.handleAPIOpenAIAccountRefresh)
	router.Post("/api/openai/accounts/{id}/models", srv.handleAPIOpenAIAccountModels)
	router.Post("/api/openai/accounts/{id}/settings", srv.handleAPIOpenAIAccountSettings)
	router.Get("/api/conversations", srv.handleAPIConversations)
	router.Get("/api/conversations/{id}", srv.handleAPIConversationDetail)
	router.Delete("/api/conversations/{id}", srv.handleAPIConversationDelete)
	router.Post("/api/conversations/batch-delete", srv.handleAPIConversationsBatchDelete)
	router.Get("/stats/tokens", srv.handleTokenStatsPage)
	router.Get("/api/stats/tokens", srv.handleAPITokenStats)
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
		fmt.Printf("codex bridge library: %s\n", *bridgeLibrary)
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
	plans, chainMode, accountMode, planErr := p.forwardPlans(r.Context(), routeStyle, upstreamTarget, r, reqProtocol, reqBody, logModel, meta.SessionID)
	failoverMode := chainMode || accountMode
	if planErr != nil {
		// 代理自己产生的错误可以指定状态码(如模型匹配不上用 400,避免 CLI 反复重试)。
		status := http.StatusServiceUnavailable
		var local *localError
		if errors.As(planErr, &local) {
			status = local.status
		}
		p.finishLocalError(w, r, start, nil, provider, status, planErr.Error(), recordedReqBody, effectiveModel, !isProbe)
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
	// acquiredAccountID 记录当前持有在途计数的账号,响应彻底结束后释放(least-connections 需要)。
	var acquiredAccountID int64
	defer func() { p.accounts.Release(acquiredAccountID) }()
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
			if !failoverMode || i == len(plans)-1 {
				if chainMode {
					p.chains.ClearFailures(plan.chainID, meta.SessionID)
				}
				p.finishLocalError(w, r, start, &plan, provider, http.StatusMisdirectedRequest, plan.mismatch, recordedReqBody, effectiveModel, !isProbe)
				return
			}
			fmt.Printf("!! chain route failed current=%s next=%s reason=%s\n", chainPlanLabel(plans, i), chainNextLabel(plans, i), plan.mismatch)
			continue
		}
		// account 直连:尝试前解析当前候选账号的鉴权,并接管在途计数(换账号时释放上一个)。
		if accountMode && plan.accountCandidate != nil {
			if acquiredAccountID != 0 && acquiredAccountID != plan.accountCandidate.ID {
				p.accounts.Release(acquiredAccountID)
				acquiredAccountID = 0
			}
			resolved, resolveErr := p.resolveAccountAuth(r.Context(), plan.accountCandidate, plan.apiKeyID)
			if resolveErr != nil {
				lastErr = resolveErr
				p.accounts.MarkFailure(meta.SessionID, plan.accountCandidate.ID)
				if i == len(plans)-1 {
					status := http.StatusServiceUnavailable
					var local *localError
					if errors.As(resolveErr, &local) {
						status = local.status
					}
					p.finishLocalError(w, r, start, &plan, provider, status, resolveErr.Error(), recordedReqBody, effectiveModel, !isProbe)
					return
				}
				fmt.Printf("!! account auth failed id=%d next id=%d err=%v\n", plan.accountCandidate.ID, accountNextID(plans, i), resolveErr)
				continue
			}
			plan.accountAuth = resolved
			if acquiredAccountID == 0 {
				p.accounts.Acquire(plan.accountCandidate.ID)
				acquiredAccountID = plan.accountCandidate.ID
			}
		}
		accountNote := ""
		if plan.accountAuth != nil {
			accountNote = " account=" + plan.accountAuth.Label
		}
		fmt.Printf("-> [%s] %s %s upstream=%s adapt=%s%s\n", provider, r.Method, r.URL.RequestURI(), plan.upstreamURL, fallback(plan.adaptTarget, "none"), accountNote)
		resp, err = p.doUpstream(r, plan)
		if err != nil {
			lastErr = err
			p.markAttemptFailure(chainMode, accountMode, plan, meta.SessionID)
			if !failoverMode || i == len(plans)-1 {
				if chainMode {
					p.chains.ClearFailures(plan.chainID, meta.SessionID)
				}
				p.finishLocalError(w, r, start, &plan, provider, http.StatusBadGateway, err.Error(), recordedReqBody, effectiveModel, !isProbe)
				return
			}
			fmt.Printf("!! attempt failed current=%s next=%s error=%v\n", chainPlanLabel(plans, i), chainNextLabel(plans, i), err)
			continue
		}
		// 直连路径的 401 兜底:凭证被上游拒绝时强制刷新并重试一次。
		if plan.accountAuth != nil && resp.StatusCode == http.StatusUnauthorized {
			if retried, ok := p.retryDirectWithRefresh(r, &plan); ok {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				resp = retried
				plans[i] = plan
				fmt.Printf("== upstream 401, refreshed credentials and retried, status=%d\n", resp.StatusCode)
			}
		}
		if failoverMode && resp.StatusCode >= 400 {
			p.markAttemptFailure(chainMode, accountMode, plan, meta.SessionID)
			if i < len(plans)-1 {
				rawFail, _ := io.ReadAll(resp.Body)
				decodedFail := decodeResponseBody(rawFail, resp.Header.Get("Content-Encoding"))
				_ = resp.Body.Close()
				if chainMode {
					p.writeChainAttemptLog(r, start, plan, resp, recordedReqBody, decodedFail, rawFail, map[string]any{
						"chain_attempt_result": "status_failure",
						"chain_current":        chainPlanLabel(plans, i),
						"chain_next":           chainNextLabel(plans, i),
					}, !isProbe)
				}
				fmt.Printf("!! attempt returned current=%s status=%d next=%s\n", chainPlanLabel(plans, i), resp.StatusCode, chainNextLabel(plans, i))
				continue
			}
			if chainMode {
				p.chains.ClearFailures(plan.chainID, meta.SessionID)
			}
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
		if resp.StatusCode < 400 {
			if chainMode && plan.route != nil {
				p.chains.MarkSuccess(plan.chainID, meta.SessionID, plan.route.ID)
			}
			if accountMode && plan.accountAuth != nil {
				p.accounts.MarkSuccess(meta.SessionID, plan.accountAuth.AccountDBID)
			}
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
	} else if plan.directJSON && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 直连但客户端要非流式:上游是强制的 SSE,读完聚合成单个 Responses JSON 返回。
		rawUp, err := io.ReadAll(resp.Body)
		sse := decodeResponseBody(rawUp, resp.Header.Get("Content-Encoding"))
		jsonBody, ok := aggregateResponsesSSEToJSON(sse)
		if !ok {
			// 聚合不出来(异常流)就把解码后的原文回给客户端,别吞掉。
			jsonBody = sse
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		_, writeErr := io.WriteString(w, jsonBody)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if err != nil {
			copyErr = err
		} else {
			copyErr = writeErr
		}
		// 记录/统计仍用原始 SSE:usage 与事件解析都依赖它,客户端拿到的 JSON 只是聚合结果。
		decodedRespBody = sse
		responseBytes = len(jsonBody)
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

	sourceType, srcRouteID, srcChainID, srcAPIKeyID, srcAccountDBID := planSource(plan)
	finishRecord := TraceFinishRecord{
		Provider:      provider,
		Probe:         isProbe,
		Status:        resp.StatusCode,
		DurationMS:    time.Since(start).Milliseconds(),
		ResponseBody:  recordedRespBody,
		ResponseHdrs:  sanitizeHeader(cloneHeader(resp.Header)),
		ResponseBytes: responseBytes,
		SourceType:    sourceType,
		RouteID:       srcRouteID,
		ChainID:       srcChainID,
		APIKeyID:      srcAPIKeyID,
		AccountDBID:   srcAccountDBID,
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

// forwardPlans 决定这次请求要依次尝试哪些转发计划,并返回是否处于「可容错重试」的模式:
// chainMode(链式代理逐路由)/ accountMode(API Key 直连多账号负载均衡)。
func (p *proxyServer) forwardPlans(ctx context.Context, routeStyle string, upstreamTarget *url.URL, r *http.Request, reqProtocol string, reqBody []byte, logModel, sessionID string) (plans []forwardPlan, chainMode bool, accountMode bool, err error) {
	// 最高优先级:请求头里的 key 命中「API Key」页配置 → 换成 GPT 账号凭证直连官方上游,
	// 按负载均衡在候选账号里逐个尝试,不走链式代理/路由。没命中则继续下面的老逻辑。
	accountPlans, ok, authErr := p.accountPlansForRequest(ctx, r, routeStyle, logModel, sessionID, upstreamTarget, reqBody)
	if authErr != nil {
		return nil, false, false, authErr
	}
	if ok {
		return accountPlans, false, true, nil
	}

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
		return plans, true, false, nil
	}

	route, routeOK, err := p.store.EnabledAPIRouteForStyle(ctx, routeStyle)
	if err != nil {
		log.Printf("load enabled route: %v", err)
		routeOK = false
	}
	if routeOK {
		return []forwardPlan{buildRoutePlan(route, 0, reqProtocol, r.URL, reqBody, logModel)}, false, false, nil
	}
	return []forwardPlan{{
		upstreamURL:    buildUpstreamURL(upstreamTarget, r.URL),
		upstreamHost:   upstreamTarget.Host,
		forwardBody:    reqBody,
		effectiveModel: logModel,
	}}, false, false, nil
}

// accountPlansForRequest 构造「API Key 直连」路径的候选计划集:命中 api_keys 且模型有匹配账号时,
// 按 accountPool 的会话粘性 + 最少连接排序返回全部候选账号(逐个作为一个 plan),供上层做负载均衡与容错。
// 每个 plan 只带候选账号,鉴权(刷新+解析 token)延后到真正尝试前再做。
func (p *proxyServer) accountPlansForRequest(ctx context.Context, r *http.Request, routeStyle, model, sessionID string, upstreamTarget *url.URL, reqBody []byte) ([]forwardPlan, bool, error) {
	if routeStyle != "openai" {
		return nil, false, nil
	}
	key := clientAPIKey(r)
	if key == "" {
		return nil, false, nil
	}
	apiKeyID, matched, err := p.store.MatchAPIKeyID(ctx, key)
	if err != nil {
		log.Printf("match api key: %v", err)
		return nil, false, nil
	}
	if !matched {
		return nil, false, nil
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, false, newLocalError(http.StatusBadRequest, "请求里没有 model 字段，无法匹配 GPT 账号")
	}
	candidates, err := p.store.OpenAIAccountsForModel(ctx, model)
	if err != nil {
		log.Printf("load openai accounts for model %q: %v", model, err)
		return nil, false, newLocalError(http.StatusServiceUnavailable, "查询 GPT 账号失败: %v", err)
	}
	if len(candidates) == 0 {
		return nil, false, newLocalError(http.StatusBadRequest,
			"没有账号配置了模型 %s，请在 OPENAI 页面为某个账号选择该模型", model)
	}
	ordered := p.accounts.OrderedAccounts(sessionID, candidates)
	forwardBody := p.prepareCodexBackendBody(reqBody)
	directJSON := !clientWantsSSE(reqBody)
	upstreamURL := buildUpstreamURL(upstreamTarget, r.URL)
	plans := make([]forwardPlan, 0, len(ordered))
	for i := range ordered {
		account := ordered[i]
		plans = append(plans, forwardPlan{
			upstreamURL:      upstreamURL,
			upstreamHost:     upstreamTarget.Host,
			forwardBody:      forwardBody,
			effectiveModel:   model,
			apiKeyID:         apiKeyID,
			accountCandidate: &account,
			directJSON:       directJSON,
		})
	}
	return plans, true, nil
}

// resolveAccountAuth 在尝试某个候选账号前解析其鉴权:先按需刷新 access_token,再构造直连凭证。
func (p *proxyServer) resolveAccountAuth(ctx context.Context, account *OpenAIAccount, apiKeyID int64) (*openaiAccountAuth, error) {
	acc := *account
	if refreshed, err := p.openaiLogins.EnsureFreshAuth(ctx, acc); err != nil {
		log.Printf("refresh openai account %d: %v", acc.ID, err)
	} else {
		acc = refreshed
	}
	auth, err := newAccountAuth(acc)
	if err != nil {
		log.Printf("parse openai account auth (id=%d): %v", acc.ID, err)
		return nil, newLocalError(http.StatusServiceUnavailable,
			"账号 %s 的鉴权信息不可用，请重新登录", fallback(acc.Name, acc.AccountID))
	}
	return auth, nil
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
		if plan.accountAuth != nil {
			applyAccountAuth(upstreamReq.Header, *plan.accountAuth)
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

// retryDirectWithRefresh 是直连路径的 401 兜底:强制刷新凭证后用新 token 重试一次。
// 只有拿到新响应才算成功,失败时不动原响应,由调用方把上游的 401 原样返回。
func (p *proxyServer) retryDirectWithRefresh(r *http.Request, plan *forwardPlan) (*http.Response, bool) {
	var auth *openaiAccountAuth
	var err error
	if plan.accountAuth != nil && plan.accountAuth.AccountDBID != 0 {
		// 多账号直连:只刷新当前命中的这个账号,不要动别的账号。
		auth, err = p.openaiLogins.ForceRefreshAccount(r.Context(), plan.accountAuth.AccountDBID)
	} else {
		auth, err = p.openaiLogins.ForceRefreshAuth(r.Context(), plan.accountAuth.AccessToken)
	}
	if err != nil {
		log.Printf("upstream returned 401 but refreshing credentials failed: %v", err)
		return nil, false
	}
	retryPlan := *plan
	retryPlan.accountAuth = auth
	retried, err := p.doUpstream(r, retryPlan)
	if err != nil {
		log.Printf("retry after credential refresh failed: %v", err)
		return nil, false
	}
	plan.accountAuth = auth
	return retried, true
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

// markAttemptFailure 按当前模式给失败的这一跳打上失败标记:链式代理按路由冷却,账号模式按账号熔断。
func (p *proxyServer) markAttemptFailure(chainMode, accountMode bool, plan forwardPlan, sessionID string) {
	if chainMode && plan.route != nil {
		p.chains.MarkFailure(plan.chainID, sessionID, plan.route.ID)
	}
	if accountMode && plan.accountCandidate != nil {
		p.accounts.MarkFailure(sessionID, plan.accountCandidate.ID)
	}
}

// accountNextID 返回下一个待尝试候选账号的 id,仅用于日志。
func accountNextID(plans []forwardPlan, idx int) int64 {
	if idx+1 >= len(plans) || plans[idx+1].accountCandidate == nil {
		return 0
	}
	return plans[idx+1].accountCandidate.ID
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

// codexBackendUnsupportedParams 是 ChatGPT Codex 后端不认、但原生 SDK 会带的参数,
// 直连转发前剥掉,否则报 400 {"detail":"Unsupported parameter: ..."}。Codex CLI 本身不带这些。
var codexBackendUnsupportedParams = []string{"max_output_tokens"}

// prepareCodexBackendBody 把请求体补成 ChatGPT Codex 后端(/backend-api/codex)能接受的形式。
// Codex CLI 自带 store/include/stream 等字段,原生 OpenAI SDK 客户端不带,直连时代理据 Bridge 的
// JSON Schema 默认值补齐缺失字段(客户端已设的保留),并处理后端的两条硬性要求:
//
//   - store 必须 false —— 缺了 400 {"detail":"Store must be set to false"}
//   - stream 必须 true —— 缺了 400 {"detail":"Stream must be set to true"}
//
// 无论客户端是否要流式,上游都强制 stream=true(后端不支持非流式);客户端若要非流式,
// 由转发层把 SSE 聚合回 JSON,见 aggregateResponsesSSEToJSON。
func (p *proxyServer) prepareCodexBackendBody(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	for key, def := range p.openaiLogins.ResponsesDefaults() {
		if _, exists := obj[key]; !exists {
			obj[key] = def
		}
	}
	// 后端硬性要求,强制覆盖(Schema 默认值也是这两个,这里再兜一层保证 Bridge 不可用时也对)。
	obj["store"] = json.RawMessage("false")
	obj["stream"] = json.RawMessage("true")
	for _, key := range codexBackendUnsupportedParams {
		delete(obj, key)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// clientWantsSSE 判断客户端是否期望 SSE 流式响应。
// 与 clientWantsStream 不同:这里「缺省」视为非流式——原生 OpenAI SDK 的非流式调用会
// 省略 stream 字段(期待一次性 JSON),只有显式 stream:true 才当作要流。
func clientWantsSSE(body []byte) bool {
	var s struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(body, &s) == nil && s.Stream != nil {
		return *s.Stream
	}
	return false
}

// aggregateResponsesSSEToJSON 把 Responses 协议的 SSE 流聚合成非流式响应体。
//
// 注意 ChatGPT Codex 后端的一个特性:response.completed 事件里的 response.output 是**空**的,
// 真正的输出项分散在流式过程的 response.output_item.done 事件里。所以只取 completed 会得到
// 一个没有正文的壳(SDK 的 output_text 为空)。这里以 completed 的 response 为基,若它的
// output 为空,就用各 output_item.done 的 item 按 output_index 顺序重建 output 数组。
func aggregateResponsesSSEToJSON(sseBody string) (string, bool) {
	events := parseSSEEvents(sseBody)
	var completed map[string]any
	type indexedItem struct {
		idx  int
		item json.RawMessage
	}
	var items []indexedItem
	for _, ev := range events {
		if len(ev.Data) == 0 {
			continue
		}
		var d struct {
			Type        string          `json:"type"`
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
			Response    json.RawMessage `json:"response"`
		}
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		switch d.Type {
		case "response.completed":
			if len(d.Response) > 0 {
				var m map[string]any
				if json.Unmarshal(d.Response, &m) == nil {
					completed = m
				}
			}
		case "response.output_item.done":
			if len(d.Item) > 0 {
				items = append(items, indexedItem{idx: d.OutputIndex, item: d.Item})
			}
		}
	}
	if completed == nil {
		return "", false
	}
	if out, ok := completed["output"].([]any); !ok || len(out) == 0 {
		sort.SliceStable(items, func(i, j int) bool { return items[i].idx < items[j].idx })
		rebuilt := make([]any, 0, len(items))
		for _, it := range items {
			var v any
			if json.Unmarshal(it.item, &v) == nil {
				rebuilt = append(rebuilt, v)
			}
		}
		completed["output"] = rebuilt
	}
	out, err := json.Marshal(completed)
	if err != nil {
		return "", false
	}
	return string(out), true
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

// clientAPIKey 从请求头取客户端携带的 key:OpenAI 风格在 Authorization: Bearer,
// Anthropic 风格在 X-Api-Key。两者都取不到返回空串。
func clientAPIKey(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("Authorization")); value != "" {
		if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
			return strings.TrimSpace(value[7:])
		}
		return value
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// accountAuthFromJSON 从存库的 Codex auth.json 里取出 access token 与 account id。
// account_id 缺失时回落到登录时解析出的列值。
func accountAuthFromJSON(raw, fallbackAccountID string) (*openaiAccountAuth, error) {
	var parsed struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	if strings.TrimSpace(parsed.Tokens.AccessToken) == "" {
		return nil, errors.New("账号鉴权信息里没有 access_token")
	}
	return &openaiAccountAuth{
		AccessToken: parsed.Tokens.AccessToken,
		AccountID:   fallback(parsed.Tokens.AccountID, fallbackAccountID),
	}, nil
}

// applyAccountAuth 用 GPT 账号凭证替换客户端带来的代理 key,直连官方上游时使用。
func applyAccountAuth(h http.Header, auth openaiAccountAuth) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
	h.Del("Chatgpt-Account-Id")
	h.Set("Authorization", "Bearer "+auth.AccessToken)
	if auth.AccountID != "" {
		h.Set("Chatgpt-Account-Id", auth.AccountID)
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
