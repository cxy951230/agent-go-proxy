package main

import (
	"context"
	"sync"
	"time"
)

// batchRefreshJob 是「一键刷新全部」类后台任务的状态(进程内存,不落库)。
// OUTLOOK 页的批量刷 Token、OPENAI 页的批量刷额度各持有一个实例;
// 同一实例同一时刻只允许跑一个任务,前端发起后立即返回,再轮询进度。
type batchRefreshJob struct {
	mu      sync.Mutex
	running bool
	total   int
	done    int
	ok      int
	failed  int
	errs    []string // 最多保留前 batchRefreshMaxErrs 条失败原因,给前端提示用
	startAt time.Time
	endAt   time.Time
}

const batchRefreshMaxErrs = 10

// batchRefreshStatus 是进度快照,直接序列化给前端轮询用。
type batchRefreshStatus struct {
	Running  bool     `json:"running"`
	Total    int      `json:"total"`
	Done     int      `json:"done"`
	OK       int      `json:"ok"`
	Failed   int      `json:"failed"`
	Errors   []string `json:"errors"`
	StartAt  string   `json:"start_at,omitempty"`
	EndAt    string   `json:"end_at,omitempty"`
	Finished bool     `json:"finished"` // 跑过且已结束(前端据此决定是否收尾刷新列表)
}

func (j *batchRefreshJob) snapshot() batchRefreshStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := batchRefreshStatus{
		Running:  j.running,
		Total:    j.total,
		Done:     j.done,
		OK:       j.ok,
		Failed:   j.failed,
		Errors:   append([]string(nil), j.errs...),
		Finished: !j.running && !j.endAt.IsZero(),
	}
	if !j.startAt.IsZero() {
		st.StartAt = j.startAt.Format(time.RFC3339)
	}
	if !j.endAt.IsZero() {
		st.EndAt = j.endAt.Format(time.RFC3339)
	}
	return st
}

// begin 抢占任务槽位。已有任务在跑时返回 false,调用方据此返回 409。
func (j *batchRefreshJob) begin(total int) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return false
	}
	j.running, j.total, j.done, j.ok, j.failed = true, total, 0, 0, 0
	j.errs = nil
	j.startAt, j.endAt = time.Now(), time.Time{}
	return true
}

func (j *batchRefreshJob) finishOne(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.done++
	if err != nil {
		j.failed++
		if len(j.errs) < batchRefreshMaxErrs {
			j.errs = append(j.errs, truncate(err.Error(), 200))
		}
		return
	}
	j.ok++
}

func (j *batchRefreshJob) end() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	j.endAt = time.Now()
}

// startBatchRefresh 抢下任务槽位后,在后台按 concurrency 并发对每个 id 调 refresh。
// 返回本次的任务总数;已有任务在跑时返回 (0,false),调用方回 409。
// 任务用独立的 context.Background()(HTTP handler 早就返回了),每个 id 另有 timeout 上限;
// refresh 返回的 error 直接进失败列表,所以调用方应在 error 里带上账号邮箱/名称。
func startBatchRefresh(job *batchRefreshJob, ids []int64, concurrency int, timeout time.Duration, refresh func(context.Context, int64) error) (int, bool) {
	if !job.begin(len(ids)) {
		return 0, false
	}
	if len(ids) == 0 {
		job.end()
		return 0, true
	}
	go func() {
		defer job.end()
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for _, id := range ids {
			id := id
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				job.finishOne(refresh(ctx, id))
			}()
		}
		wg.Wait()
	}()
	return len(ids), true
}
