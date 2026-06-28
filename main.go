package main

import (
	"bytes"
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
	listenAddr string
	target     *url.URL
	logDir     string
	dsn        string
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
	targetValue := flag.String("target", envOrDefault("UPSTREAM_BASE_URL", "https://chatgpt.com/backend-api/codex"), "upstream base URL")
	logDir := flag.String("log-dir", "log", "directory for date-based JSONL logs")
	dsn := flag.String("mysql-dsn", envOrDefault("MYSQL_DSN", "root:123456@tcp(127.0.0.1:3306)/agent_go_proxy?parseTime=true&charset=utf8mb4&loc=Local"), "MySQL DSN")
	flag.Parse()

	target, err := url.Parse(*targetValue)
	if err != nil || target.Scheme == "" || target.Host == "" {
		log.Fatalf("invalid -target %q", *targetValue)
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
			listenAddr: *listenAddr,
			target:     target,
			logDir:     *logDir,
			dsn:        *dsn,
		},
		client: &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
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
		fmt.Printf("upstream target: %s\n", target.String())
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
	upstreamURL := buildUpstreamURL(p.cfg.target, r.URL)
	fmt.Printf("-> %s %s upstream=%s\n", r.Method, r.URL.RequestURI(), upstreamURL)

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

	p.logs.Write(logEntry{
		Timestamp:   time.Now().Format(time.RFC3339Nano),
		Phase:       "start",
		DurationMS:  time.Since(start).Milliseconds(),
		Method:      r.Method,
		Path:        r.URL.RequestURI(),
		UpstreamURL: upstreamURL,
		RequestBody: string(reqBody),
		RequestHdrs: sanitizeHeader(cloneHeader(r.Header)),
	})

	traceHandle := p.recorder.Start(TraceStartRecord{
		Method:       r.Method,
		Path:         r.URL.RequestURI(),
		UpstreamURL:  upstreamURL,
		RequestBody:  string(reqBody),
		RequestHdrs:  sanitizeHeader(cloneHeader(r.Header)),
		RequestBytes: len(reqBody),
	})

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusBadGateway)
		p.logs.Write(logEntry{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			DurationMS:  time.Since(start).Milliseconds(),
			Method:      r.Method,
			Path:        r.URL.RequestURI(),
			UpstreamURL: upstreamURL,
			Status:      http.StatusBadGateway,
			RequestBody: string(reqBody),
			RequestHdrs: sanitizeHeader(cloneHeader(r.Header)),
			Error:       err.Error(),
		})
		fmt.Printf("<- %s %s status=%d error=%s\n", r.Method, r.URL.RequestURI(), http.StatusBadGateway, err.Error())
		p.recorder.Finish(traceHandle, TraceFinishRecord{
			Status:     http.StatusBadGateway,
			DurationMS: time.Since(start).Milliseconds(),
			Error:      err.Error(),
		})
		return
	}
	copyHeaders(upstreamReq.Header, r.Header)
	upstreamReq.Host = p.cfg.target.Host

	resp, err := p.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		p.logs.Write(logEntry{
			Timestamp:   time.Now().Format(time.RFC3339Nano),
			DurationMS:  time.Since(start).Milliseconds(),
			Method:      r.Method,
			Path:        r.URL.RequestURI(),
			UpstreamURL: upstreamURL,
			Status:      http.StatusBadGateway,
			RequestBody: string(reqBody),
			RequestHdrs: cloneHeader(r.Header),
			Error:       err.Error(),
		})
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

	entry := logEntry{
		Timestamp:    time.Now().Format(time.RFC3339Nano),
		DurationMS:   time.Since(start).Milliseconds(),
		Method:       r.Method,
		Path:         r.URL.RequestURI(),
		UpstreamURL:  upstreamURL,
		Status:       resp.StatusCode,
		RequestBody:  string(reqBody),
		ResponseBody: respBody.String(),
		RequestHdrs:  sanitizeHeader(cloneHeader(r.Header)),
		ResponseHdrs: sanitizeHeader(cloneHeader(resp.Header)),
	}
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		entry.Error = copyErr.Error()
	}
	p.logs.Write(entry)

	finishRecord := TraceFinishRecord{
		Status:        resp.StatusCode,
		DurationMS:    time.Since(start).Milliseconds(),
		ResponseBody:  respBody.String(),
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

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
