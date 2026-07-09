package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

type TraceStartRecord struct {
	Provider     string
	Method       string
	Path         string
	UpstreamURL  string
	RequestBody  string
	RequestHdrs  map[string][]string
	RequestBytes int
}

type TraceFinishRecord struct {
	Provider      string
	Probe         bool
	Status        int
	DurationMS    int64
	ResponseBody  string
	ResponseHdrs  map[string][]string
	ResponseBytes int
	Error         string
}

type traceHandle struct {
	ready chan int64
}

type recorderJob struct {
	kind   string
	handle *traceHandle
	start  TraceStartRecord
	finish TraceFinishRecord
}

type asyncRecorder struct {
	store *Store
	ch    chan recorderJob
	wg    sync.WaitGroup
}

func newAsyncRecorder(store *Store) *asyncRecorder {
	rec := &asyncRecorder{
		store: store,
		ch:    make(chan recorderJob, 2048),
	}
	rec.wg.Add(1)
	go rec.loop()
	return rec
}

func (r *asyncRecorder) Close() {
	close(r.ch)
	r.wg.Wait()
}

func (r *asyncRecorder) Start(in TraceStartRecord) *traceHandle {
	handle := &traceHandle{ready: make(chan int64, 1)}
	if !r.enqueue(recorderJob{kind: "start", handle: handle, start: in}) {
		handle.ready <- 0
		close(handle.ready)
	}
	return handle
}

func (r *asyncRecorder) Finish(handle *traceHandle, in TraceFinishRecord) {
	if handle == nil {
		return
	}
	_ = r.enqueue(recorderJob{kind: "finish", handle: handle, finish: in})
}

func (r *asyncRecorder) enqueue(job recorderJob) bool {
	select {
	case r.ch <- job:
		return true
	default:
		log.Printf("record queue full; dropping %s job", job.kind)
		return false
	}
}

func (r *asyncRecorder) loop() {
	defer r.wg.Done()
	for job := range r.ch {
		switch job.kind {
		case "start":
			r.runStart(job)
		case "finish":
			r.runFinish(job)
		}
	}
}

func (r *asyncRecorder) runStart(job recorderJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	meta := requestMetaFromHeaders(http.Header(job.start.RequestHdrs), []byte(job.start.RequestBody), job.start.Provider)
	traceID, err := r.store.StartTrace(ctx, StartTraceInput{
		SessionID:    meta.SessionID,
		AccountID:    meta.AccountID,
		WindowID:     meta.WindowID,
		TurnID:       meta.TurnID,
		FirstPrompt:  meta.FirstPrompt,
		Model:        meta.Model,
		Agent:        agentLabel(job.start.Provider),
		Method:       job.start.Method,
		Path:         job.start.Path,
		UpstreamURL:  job.start.UpstreamURL,
		RequestBody:  job.start.RequestBody,
		RequestHdrs:  job.start.RequestHdrs,
		RequestBytes: job.start.RequestBytes,
	})
	if err != nil {
		log.Printf("store start trace: %v", err)
		traceID = 0
	}
	job.handle.ready <- traceID
	close(job.handle.ready)
}

func (r *asyncRecorder) runFinish(job recorderJob) {
	traceID := <-job.handle.ready
	if traceID == 0 {
		return
	}
	events := parseSSEEvents(job.finish.ResponseBody)
	usage := extractUsageFromEvents(events, job.finish.Provider)
	// 非流式 Claude 响应没有 SSE 事件,usage 在 JSON 顶层,回退解析。
	if job.finish.Provider == providerClaude && usage.TotalTokens == 0 {
		usage = claudeUsageFromJSONBody(job.finish.ResponseBody)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.store.FinishTrace(ctx, traceID, FinishTraceInput{
		Status:        job.finish.Status,
		DurationMS:    job.finish.DurationMS,
		ResponseBody:  job.finish.ResponseBody,
		ResponseHdrs:  job.finish.ResponseHdrs,
		ResponseBytes: job.finish.ResponseBytes,
		SSEEvents:     events,
		Usage:         usage,
		Error:         job.finish.Error,
		Probe:         job.finish.Probe,
	}); err != nil {
		log.Printf("store finish trace: %v", err)
	}
}
