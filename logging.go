package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type logEntry struct {
	Timestamp    string              `json:"timestamp"`
	Phase        string              `json:"phase,omitempty"`
	DurationMS   int64               `json:"duration_ms"`
	Method       string              `json:"method"`
	Path         string              `json:"path"`
	UpstreamURL  string              `json:"upstream_url"`
	Status       int                 `json:"status"`
	RequestBody  string              `json:"request_body"`
	ResponseBody string              `json:"response_body"`
	RequestHdrs  map[string][]string `json:"request_headers,omitempty"`
	ResponseHdrs map[string][]string `json:"response_headers,omitempty"`
	Error        string              `json:"error,omitempty"`
}

type logWriter struct {
	dir string
	ch  chan logEntry
	wg  sync.WaitGroup
}

func newLogWriter(dir string) *logWriter {
	lw := &logWriter{
		dir: dir,
		ch:  make(chan logEntry, 1024),
	}
	lw.wg.Add(1)
	go lw.loop()
	return lw
}

func (lw *logWriter) Write(entry logEntry) {
	select {
	case lw.ch <- entry:
	default:
		log.Printf("log queue full; dropping entry for %s %s", entry.Method, entry.Path)
	}
}

func (lw *logWriter) Close() {
	close(lw.ch)
	lw.wg.Wait()
}

func (lw *logWriter) loop() {
	defer lw.wg.Done()
	for entry := range lw.ch {
		if err := lw.append(entry); err != nil {
			log.Printf("write log: %v", err)
		}
	}
}

func (lw *logWriter) append(entry logEntry) error {
	if err := os.MkdirAll(lw.dir, 0755); err != nil {
		return err
	}
	name := time.Now().Format("2006-01-02") + ".log"
	path := filepath.Join(lw.dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return nil
}

func sanitizeHeader(src http.Header) map[string][]string {
	out := make(map[string][]string, len(src))
	for key, values := range src {
		copied := make([]string, len(values))
		copy(copied, values)
		out[key] = copied
	}
	return out
}
