package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

type Store struct {
	db *sql.DB
}

type StartTraceInput struct {
	SessionID    string
	AccountID    string
	WindowID     string
	TurnID       string
	FirstPrompt  string
	Model        string
	Agent        string
	Method       string
	Path         string
	UpstreamURL  string
	RequestBody  string
	RequestHdrs  map[string][]string
	RequestBytes int
}

type FinishTraceInput struct {
	Status        int
	DurationMS    int64
	ResponseBody  string
	ResponseHdrs  map[string][]string
	ResponseBytes int
	SSEEvents     []sseEvent
	Usage         usageStats
	Error         string
	Probe         bool
}

type ConversationSummary struct {
	ID            int64
	SessionID     string
	AccountID     string
	AccountName   string
	Tags          string
	StartedAt     time.Time
	UpdatedAt     time.Time
	FirstPrompt   string
	TraceCount    int
	ErrorCount    int
	TotalTokens   int64
	InputTokens   int64
	OutputTokens  int64
	CachedTokens  int64
	DurationMin   float64
	Model         string
	Agent         string
	Status        string
	LastStatus    int
	LastDuration  int64
	LastRequestID string
}

type TraceRecord struct {
	ID              int64
	ConversationID  int64
	AccountID       string
	AccountName     string
	SequenceNo      int
	StartedAt       time.Time
	CompletedAt     sql.NullTime
	DurationMS      int64
	Method          string
	Path            string
	UpstreamURL     string
	Status          int
	Model           string
	RequestHeaders  string
	ResponseHeaders string
	RequestBody     string
	ResponseBody    string
	SSEEvents       string
	Error           string
	InputTokens     int
	OutputTokens    int
	TotalTokens     int
	CachedTokens    int
	ReasoningTokens int
	RequestBytes    int
	ResponseBytes   int
}

type AccountAliasOption struct {
	AccountID   string
	DisplayName string
}

// APIRoute 是「路由」页配置的第三方 API 供应商,保存 Base URL / Model / API Key。
// APIStyle(openai/anthropic)与 Protocol(接口协议)联动,Enabled 全表互斥只能一条为真。
type APIRoute struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url"`
	Model     string    `json:"model"`
	APIStyle  string    `json:"api_style"`
	Protocol  string    `json:"protocol"`
	APIKey    string    `json:"api_key"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ChainProxy 是链式代理配置:同一种 API 风格下按 route_ids 顺序尝试多个路由。
// 这里只负责配置保存与启用状态,实际转发链路另行接入。
type ChainProxy struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	APIStyle  string     `json:"api_style"`
	RouteIDs  []int64    `json:"route_ids"`
	Enabled   bool       `json:"enabled"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Routes    []APIRoute `json:"routes,omitempty"`
}

// routeProtocols 定义每种 API 风格支持的接口协议(值→展示名),前后端各用一份保持一致。
var routeProtocols = map[string]map[string]string{
	"openai": {
		"chat_completions": "Chat Completions API",
		"responses":        "Responses API",
	},
	"anthropic": {
		"messages":         "Messages API",
		"chat_completions": "Chat Completions API",
	},
}

type FilterOptions struct {
	Months         []string
	Dates          []string
	Agents         []string
	AccountAliases []AccountAliasOption
}

var subagentIDPattern = regexp.MustCompile(`\\*"agent_id\\*"\s*:\s*\\*"([^"\\]+)\\*"`)

func NewStore(dsn string) (*Store, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func ensureDatabase(dsn string) error {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return err
	}
	dbName := cfg.DBName
	if dbName == "" {
		return nil
	}
	cfg.DBName = ""
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("CREATE DATABASE IF NOT EXISTS " + quoteIdentifier(dbName) + " DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		return fmt.Errorf("create database %s: %w", dbName, err)
	}
	return nil
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS conversations (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			session_id VARCHAR(128) NOT NULL UNIQUE,
			account_id VARCHAR(128) NOT NULL DEFAULT '',
			window_id VARCHAR(160) NOT NULL DEFAULT '',
			started_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			first_prompt TEXT,
			tags VARCHAR(255) NOT NULL DEFAULT '',
			model VARCHAR(128) NOT NULL DEFAULT '',
			agent VARCHAR(64) NOT NULL DEFAULT 'Codex',
			status VARCHAR(24) NOT NULL DEFAULT 'LIVE',
			trace_count INT NOT NULL DEFAULT 0,
			error_count INT NOT NULL DEFAULT 0,
			total_tokens BIGINT NOT NULL DEFAULT 0,
			last_status INT NOT NULL DEFAULT 0,
			last_duration_ms BIGINT NOT NULL DEFAULT 0,
			last_request_id VARCHAR(128) NOT NULL DEFAULT '',
			INDEX idx_conversations_updated_at (updated_at),
			INDEX idx_conversations_status (status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS traces (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			conversation_id BIGINT NOT NULL,
			session_id VARCHAR(128) NOT NULL,
			account_id VARCHAR(128) NOT NULL DEFAULT '',
			turn_id VARCHAR(128) NOT NULL DEFAULT '',
			sequence_no INT NOT NULL,
			started_at DATETIME(6) NOT NULL,
			completed_at DATETIME(6) NULL,
			duration_ms BIGINT NOT NULL DEFAULT 0,
			method VARCHAR(16) NOT NULL,
			path VARCHAR(512) NOT NULL,
			upstream_url TEXT NOT NULL,
			status INT NOT NULL DEFAULT 0,
			model VARCHAR(128) NOT NULL DEFAULT '',
			request_headers JSON NULL,
			response_headers JSON NULL,
			request_body LONGTEXT,
			response_body LONGTEXT,
			sse_events JSON NULL,
			error TEXT,
			input_tokens INT NOT NULL DEFAULT 0,
			output_tokens INT NOT NULL DEFAULT 0,
			total_tokens INT NOT NULL DEFAULT 0,
			cached_tokens INT NOT NULL DEFAULT 0,
			reasoning_tokens INT NOT NULL DEFAULT 0,
			request_bytes INT NOT NULL DEFAULT 0,
			response_bytes INT NOT NULL DEFAULT 0,
			INDEX idx_traces_conversation (conversation_id, sequence_no),
			INDEX idx_traces_account (account_id),
			INDEX idx_traces_session (session_id),
			CONSTRAINT fk_traces_conversation FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS account_aliases (
			account_id VARCHAR(128) PRIMARY KEY,
			display_name VARCHAR(128) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS api_routes (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(128) NOT NULL DEFAULT '',
			base_url VARCHAR(512) NOT NULL DEFAULT '',
			model VARCHAR(128) NOT NULL DEFAULT '',
			api_style VARCHAR(32) NOT NULL DEFAULT 'openai',
			protocol VARCHAR(64) NOT NULL DEFAULT '',
			api_key VARCHAR(512) NOT NULL DEFAULT '',
			enabled TINYINT(1) NOT NULL DEFAULT 0,
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS chain_proxies (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(128) NOT NULL DEFAULT '',
			api_style VARCHAR(32) NOT NULL DEFAULT 'openai',
			route_ids JSON NOT NULL,
			enabled TINYINT(1) NOT NULL DEFAULT 0,
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			INDEX idx_chain_proxies_api_style (api_style),
			INDEX idx_chain_proxies_updated_at (updated_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := s.ensureColumn(ctx, "conversations", "account_id", "VARCHAR(128) NOT NULL DEFAULT '' AFTER session_id"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "conversations", "tags", "VARCHAR(255) NOT NULL DEFAULT '' AFTER first_prompt"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "traces", "account_id", "VARCHAR(128) NOT NULL DEFAULT '' AFTER session_id"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE traces
		SET account_id=COALESCE(JSON_UNQUOTE(JSON_EXTRACT(request_headers, '$."Chatgpt-Account-Id"[0]')), '')
		WHERE account_id='' AND request_headers IS NOT NULL`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE conversations c
		JOIN (
			SELECT conversation_id, MAX(account_id) account_id FROM traces WHERE account_id<>'' GROUP BY conversation_id
		) t ON t.conversation_id=c.id
		SET c.account_id=t.account_id
		WHERE c.account_id=''`); err != nil {
		return err
	}
	if err := s.repairInjectedPrompts(ctx); err != nil {
		return err
	}
	// api_routes 新增字段(旧库补迁移)
	if err := s.ensureColumn(ctx, "api_routes", "api_style", "VARCHAR(32) NOT NULL DEFAULT 'openai' AFTER model"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "api_routes", "protocol", "VARCHAR(64) NOT NULL DEFAULT '' AFTER api_style"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "api_routes", "enabled", "TINYINT(1) NOT NULL DEFAULT 0 AFTER api_key"); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, tableName, columnName, definition string) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, tableName, columnName).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, "ALTER TABLE "+quoteIdentifier(tableName)+" ADD COLUMN "+quoteIdentifier(columnName)+" "+definition)
	return err
}

func (s *Store) repairInjectedPrompts(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, t.request_body
		FROM conversations c
		JOIN traces t ON t.conversation_id=c.id AND t.sequence_no=1
		WHERE c.first_prompt LIKE '<environment_context>%'
			OR c.first_prompt LIKE '<permissions instructions>%'
			OR c.first_prompt LIKE '# AGENTS.md instructions%'
			OR c.first_prompt LIKE '<skill>%'`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type repair struct {
		id     int64
		prompt string
	}
	var repairs []repair
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			return err
		}
		meta := requestMetaFromHeaders(nil, []byte(body), providerCodex)
		if meta.FirstPrompt != "" && meta.FirstPrompt != "未捕获到用户 prompt。" && !isInjectedContext(meta.FirstPrompt) {
			repairs = append(repairs, repair{id: id, prompt: meta.FirstPrompt})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range repairs {
		if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET first_prompt=? WHERE id=?`, item.prompt, item.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) StartTrace(ctx context.Context, in StartTraceInput) (int64, error) {
	if in.SessionID == "" {
		return 0, errors.New("empty session id")
	}
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	agent := in.Agent
	if agent == "" {
		agent = "Codex"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO conversations
		(session_id, account_id, window_id, started_at, updated_at, first_prompt, model, agent, status, trace_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'LIVE', 0)
		ON DUPLICATE KEY UPDATE updated_at=VALUES(updated_at), window_id=IF(window_id='', VALUES(window_id), window_id),
			account_id=IF(account_id='', VALUES(account_id), account_id),
			first_prompt=IF(first_prompt IS NULL OR first_prompt='' OR first_prompt='未捕获到用户 prompt。' OR first_prompt LIKE '<environment_context>%' OR first_prompt LIKE '<permissions instructions>%' OR first_prompt LIKE '# AGENTS.md instructions%' OR first_prompt LIKE '<skill>%' OR first_prompt LIKE '<system-reminder>%', VALUES(first_prompt), first_prompt),
			model=IF(VALUES(model)<>'', VALUES(model), model), agent=VALUES(agent), status='LIVE'`,
		in.SessionID, in.AccountID, in.WindowID, now, now, in.FirstPrompt, in.Model, agent)
	if err != nil {
		return 0, err
	}

	var conversationID int64
	var sequenceNo int
	if err := tx.QueryRowContext(ctx, `SELECT id, trace_count + 1 FROM conversations WHERE session_id=? FOR UPDATE`, in.SessionID).Scan(&conversationID, &sequenceNo); err != nil {
		return 0, err
	}

	reqHeaders, _ := json.Marshal(in.RequestHdrs)
	res, err := tx.ExecContext(ctx, `INSERT INTO traces
		(conversation_id, session_id, account_id, turn_id, sequence_no, started_at, method, path, upstream_url, model, request_headers, request_body, request_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conversationID, in.SessionID, in.AccountID, in.TurnID, sequenceNo, now, in.Method, in.Path, in.UpstreamURL, in.Model, nullJSON(reqHeaders), in.RequestBody, in.RequestBytes)
	if err != nil {
		return 0, err
	}
	traceID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET trace_count=trace_count+1, updated_at=? WHERE id=?`, now, conversationID); err != nil {
		return 0, err
	}
	return traceID, tx.Commit()
}

func (s *Store) FinishTrace(ctx context.Context, traceID int64, in FinishTraceInput) error {
	now := time.Now()
	responseHeaders, _ := json.Marshal(in.ResponseHdrs)
	eventsJSON, _ := json.Marshal(in.SSEEvents)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var conversationID int64
	if err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM traces WHERE id=? FOR UPDATE`, traceID).Scan(&conversationID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE traces SET
		completed_at=?, duration_ms=?, status=?, response_headers=?, response_body=?, response_bytes=?,
		sse_events=?, error=?, input_tokens=?, output_tokens=?, total_tokens=?, cached_tokens=?, reasoning_tokens=?
		WHERE id=?`,
		now, in.DurationMS, in.Status, nullJSON(responseHeaders), in.ResponseBody, in.ResponseBytes,
		nullJSON(eventsJSON), in.Error, in.Usage.InputTokens, in.Usage.OutputTokens, in.Usage.TotalTokens,
		in.Usage.CachedInputTokens, in.Usage.ReasoningTokens, traceID)
	if err != nil {
		return err
	}
	errorInc := 0
	// error_count 仅统计上游真实返回的错误(排除传输层瞬断与 quota 探测),留作参考信息。
	if in.Status >= 400 && in.Error == "" && !in.Probe {
		errorInc = 1
	}
	// 会话状态取「最后一轮(最大 sequence_no)已完成 trace」的结果:
	// 无论代理层还是客户端层的重试,最终一轮成功即 OK,失败即 ERROR。
	// 这样瞬时 502/探测失败被后续成功覆盖,而彻底失败(全程无成功)如实标 ERROR。
	_, err = tx.ExecContext(ctx, `UPDATE conversations SET
		status=IF(EXISTS(SELECT 1 FROM traces WHERE conversation_id=? AND completed_at IS NULL), 'LIVE',
			IF(COALESCE((SELECT t2.status FROM traces t2 WHERE t2.conversation_id=? AND t2.completed_at IS NOT NULL ORDER BY t2.sequence_no DESC LIMIT 1), 0) >= 400, 'ERROR', 'OK')),
		error_count=error_count+?, total_tokens=total_tokens+?,
		last_status=?, last_duration_ms=?, last_request_id=COALESCE((
			SELECT COALESCE(
				JSON_UNQUOTE(JSON_EXTRACT(response_headers, '$."X-Oai-Request-Id"[0]')),
				JSON_UNQUOTE(JSON_EXTRACT(response_headers, '$."Request-Id"[0]'))
			) FROM traces WHERE id=?
		), last_request_id)
		WHERE id=?`,
		conversationID, conversationID, errorInc, in.Usage.TotalTokens, in.Status, in.DurationMS, traceID, conversationID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func conversationWhere(query, status, month, date, agent, accountID string) (string, []any) {
	where := "WHERE 1=1"
	args := []any{}
	if status != "" && status != "all" {
		where += " AND c.status=?"
		args = append(args, status)
	}
	if month != "" && month != "all" {
		where += " AND DATE_FORMAT(c.updated_at, '%Y-%m')=?"
		args = append(args, month)
	}
	if date != "" && date != "all" {
		where += " AND DATE(c.updated_at)=?"
		args = append(args, date)
	}
	if agent != "" && agent != "all" {
		where += " AND c.agent=?"
		args = append(args, agent)
	}
	if accountID != "" && accountID != "all" {
		where += " AND c.account_id=?"
		args = append(args, accountID)
	}
	if query != "" {
		where += " AND (c.session_id LIKE ? OR c.first_prompt LIKE ? OR c.model LIKE ? OR c.account_id LIKE ? OR a.display_name LIKE ?)"
		like := "%" + query + "%"
		args = append(args, like, like, like, like, like)
	}
	return where, args
}

func (s *Store) ListConversations(ctx context.Context, query, status, month, date, agent, accountID string) ([]ConversationSummary, error) {
	return s.ListConversationsPage(ctx, query, status, month, date, agent, accountID, 200, 0)
}

func (s *Store) ListConversationsPage(ctx context.Context, query, status, month, date, agent, accountID string, limit, offset int) ([]ConversationSummary, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	where, args := conversationWhere(query, status, month, date, agent, accountID)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `SELECT pc.id, pc.session_id, pc.account_id, pc.account_name, pc.tags, pc.started_at, pc.updated_at, pc.first_prompt, pc.trace_count,
		pc.error_count, pc.total_tokens, COALESCE(SUM(t.input_tokens),0), COALESCE(SUM(t.output_tokens),0), COALESCE(SUM(t.cached_tokens),0),
		TIMESTAMPDIFF(MICROSECOND, pc.started_at, COALESCE(MAX(t.completed_at), pc.updated_at)) / 60000000,
		pc.model, pc.agent, pc.status, pc.last_status, pc.last_duration_ms, pc.last_request_id
		FROM (
			SELECT c.id, c.session_id, c.account_id, COALESCE(a.display_name,'') account_name, COALESCE(c.tags,'') tags, c.started_at, c.updated_at,
				COALESCE(c.first_prompt,'') first_prompt, c.trace_count, c.error_count, c.total_tokens, c.model, c.agent, c.status, c.last_status,
				c.last_duration_ms, c.last_request_id
			FROM conversations c
			LEFT JOIN account_aliases a ON a.account_id=c.account_id
			`+where+`
			ORDER BY c.updated_at DESC LIMIT ? OFFSET ?
		) pc
		LEFT JOIN traces t ON t.conversation_id=pc.id
		GROUP BY pc.id, pc.session_id, pc.account_id, pc.account_name, pc.tags, pc.started_at, pc.updated_at, pc.first_prompt, pc.trace_count,
			pc.error_count, pc.total_tokens, pc.model, pc.agent, pc.status, pc.last_status, pc.last_duration_ms, pc.last_request_id
		ORDER BY pc.updated_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConversationSummary
	for rows.Next() {
		var item ConversationSummary
		if err := rows.Scan(&item.ID, &item.SessionID, &item.AccountID, &item.AccountName, &item.Tags, &item.StartedAt, &item.UpdatedAt, &item.FirstPrompt, &item.TraceCount,
			&item.ErrorCount, &item.TotalTokens, &item.InputTokens, &item.OutputTokens, &item.CachedTokens, &item.DurationMin,
			&item.Model, &item.Agent, &item.Status, &item.LastStatus, &item.LastDuration, &item.LastRequestID); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) SubagentLinks(ctx context.Context, conversations []ConversationSummary) (map[string][]string, error) {
	links := make(map[string][]string)
	if len(conversations) == 0 {
		return links, nil
	}
	parentSessionByID := make(map[int64]string, len(conversations))
	placeholders := make([]string, 0, len(conversations))
	args := make([]any, 0, len(conversations))
	for _, c := range conversations {
		parentSessionByID[c.ID] = c.SessionID
		placeholders = append(placeholders, "?")
		args = append(args, c.ID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT conversation_id, COALESCE(request_body,''), COALESCE(response_body,''), COALESCE(sse_events, JSON_ARRAY())
		FROM traces WHERE conversation_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]struct{})
	for rows.Next() {
		var conversationID int64
		var requestBody, responseBody, sseEvents string
		if err := rows.Scan(&conversationID, &requestBody, &responseBody, &sseEvents); err != nil {
			return nil, err
		}
		parentSession := parentSessionByID[conversationID]
		if parentSession == "" {
			continue
		}
		for _, childSession := range extractSubagentSessionIDs(requestBody, responseBody, sseEvents) {
			if childSession == parentSession {
				continue
			}
			key := parentSession + "\x00" + childSession
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			links[parentSession] = append(links[parentSession], childSession)
		}
	}
	return links, rows.Err()
}

func extractSubagentSessionIDs(values ...string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, value := range values {
		for _, match := range subagentIDPattern.FindAllStringSubmatch(value, -1) {
			if len(match) < 2 {
				continue
			}
			id := strings.TrimSpace(match[1])
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

func (s *Store) FilterOptions(ctx context.Context) (FilterOptions, error) {
	var opts FilterOptions
	monthRows, err := s.db.QueryContext(ctx, `SELECT DISTINCT DATE_FORMAT(updated_at, '%Y-%m') FROM conversations ORDER BY DATE_FORMAT(updated_at, '%Y-%m') DESC`)
	if err != nil {
		return opts, err
	}
	defer monthRows.Close()
	for monthRows.Next() {
		var value string
		if err := monthRows.Scan(&value); err != nil {
			return opts, err
		}
		opts.Months = append(opts.Months, value)
	}
	if err := monthRows.Err(); err != nil {
		return opts, err
	}

	dateRows, err := s.db.QueryContext(ctx, `SELECT DISTINCT DATE_FORMAT(updated_at, '%Y-%m-%d') FROM conversations ORDER BY DATE(updated_at) DESC`)
	if err != nil {
		return opts, err
	}
	defer dateRows.Close()
	for dateRows.Next() {
		var value string
		if err := dateRows.Scan(&value); err != nil {
			return opts, err
		}
		opts.Dates = append(opts.Dates, value)
	}
	if err := dateRows.Err(); err != nil {
		return opts, err
	}

	agentRows, err := s.db.QueryContext(ctx, `SELECT DISTINCT agent FROM conversations WHERE agent<>'' ORDER BY agent ASC`)
	if err != nil {
		return opts, err
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var value string
		if err := agentRows.Scan(&value); err != nil {
			return opts, err
		}
		opts.Agents = append(opts.Agents, value)
	}
	if err := agentRows.Err(); err != nil {
		return opts, err
	}

	aliasRows, err := s.db.QueryContext(ctx, `SELECT account_id, display_name FROM account_aliases WHERE display_name<>'' ORDER BY display_name ASC`)
	if err != nil {
		return opts, err
	}
	defer aliasRows.Close()
	for aliasRows.Next() {
		var item AccountAliasOption
		if err := aliasRows.Scan(&item.AccountID, &item.DisplayName); err != nil {
			return opts, err
		}
		opts.AccountAliases = append(opts.AccountAliases, item)
	}
	return opts, aliasRows.Err()
}

func (s *Store) GetConversation(ctx context.Context, id int64) (ConversationSummary, []TraceRecord, error) {
	var c ConversationSummary
	err := s.db.QueryRowContext(ctx, `SELECT c.id, c.session_id, c.account_id, COALESCE(a.display_name,''), COALESCE(c.tags,''), c.started_at, c.updated_at, COALESCE(c.first_prompt,''), c.trace_count,
		c.error_count, c.total_tokens, COALESCE(tok.input_tokens,0), COALESCE(tok.output_tokens,0), COALESCE(tok.cached_tokens,0),
		TIMESTAMPDIFF(MICROSECOND, c.started_at, COALESCE(tok.completed_at, c.updated_at)) / 60000000,
		c.model, c.agent, c.status, c.last_status, c.last_duration_ms, c.last_request_id
		FROM conversations c
		LEFT JOIN account_aliases a ON a.account_id=c.account_id
		LEFT JOIN (
			SELECT conversation_id, SUM(input_tokens) input_tokens, SUM(output_tokens) output_tokens, SUM(cached_tokens) cached_tokens, MAX(completed_at) completed_at
			FROM traces GROUP BY conversation_id
		) tok ON tok.conversation_id=c.id
		WHERE c.id=?`, id).Scan(&c.ID, &c.SessionID, &c.AccountID, &c.AccountName, &c.Tags, &c.StartedAt, &c.UpdatedAt, &c.FirstPrompt, &c.TraceCount,
		&c.ErrorCount, &c.TotalTokens, &c.InputTokens, &c.OutputTokens, &c.CachedTokens, &c.DurationMin,
		&c.Model, &c.Agent, &c.Status, &c.LastStatus, &c.LastDuration, &c.LastRequestID)
	if err != nil {
		return c, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.conversation_id, t.account_id, COALESCE(a.display_name,''), t.sequence_no, t.started_at, t.completed_at, t.duration_ms,
		t.method, t.path, t.upstream_url, t.status, t.model, COALESCE(t.request_headers, JSON_OBJECT()), COALESCE(t.response_headers, JSON_OBJECT()),
		COALESCE(t.request_body,''), COALESCE(t.response_body,''), COALESCE(t.sse_events, JSON_ARRAY()), COALESCE(t.error,''),
		t.input_tokens, t.output_tokens, t.total_tokens, t.cached_tokens, t.reasoning_tokens, t.request_bytes, t.response_bytes
		FROM traces t LEFT JOIN account_aliases a ON a.account_id=t.account_id WHERE t.conversation_id=? ORDER BY t.sequence_no ASC`, id)
	if err != nil {
		return c, nil, err
	}
	defer rows.Close()
	var traces []TraceRecord
	for rows.Next() {
		var t TraceRecord
		if err := rows.Scan(&t.ID, &t.ConversationID, &t.AccountID, &t.AccountName, &t.SequenceNo, &t.StartedAt, &t.CompletedAt, &t.DurationMS,
			&t.Method, &t.Path, &t.UpstreamURL, &t.Status, &t.Model, &t.RequestHeaders, &t.ResponseHeaders,
			&t.RequestBody, &t.ResponseBody, &t.SSEEvents, &t.Error, &t.InputTokens, &t.OutputTokens,
			&t.TotalTokens, &t.CachedTokens, &t.ReasoningTokens, &t.RequestBytes, &t.ResponseBytes); err != nil {
			return c, nil, err
		}
		traces = append(traces, t)
	}
	return c, traces, rows.Err()
}

func (s *Store) SetAccountAlias(ctx context.Context, accountID, displayName string) error {
	accountID = strings.TrimSpace(accountID)
	displayName = strings.TrimSpace(displayName)
	if accountID == "" {
		return errors.New("empty account id")
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_aliases (account_id, display_name, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE display_name=VALUES(display_name), updated_at=VALUES(updated_at)`,
		accountID, displayName, now, now)
	return err
}

func (s *Store) SetConversationTags(ctx context.Context, id int64, tags string) error {
	tags = strings.TrimSpace(tags)
	if len(tags) > 255 {
		tags = tags[:255]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET tags=? WHERE id=?`, tags, id)
	return err
}

func (s *Store) DeleteConversation(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var sessionID string
	if err := tx.QueryRowContext(ctx, `SELECT session_id FROM conversations WHERE id=? FOR UPDATE`, id).Scan(&sessionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return err
	}

	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(request_body,''), COALESCE(response_body,''), COALESCE(sse_events, JSON_ARRAY())
		FROM traces WHERE conversation_id=?`, id)
	if err != nil {
		return err
	}
	childSet := make(map[string]struct{})
	for rows.Next() {
		var requestBody, responseBody, sseEvents string
		if err := rows.Scan(&requestBody, &responseBody, &sseEvents); err != nil {
			rows.Close()
			return err
		}
		for _, childSession := range extractSubagentSessionIDs(requestBody, responseBody, sseEvents) {
			if childSession != "" && childSession != sessionID {
				childSet[childSession] = struct{}{}
			}
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	sessionIDs := []string{sessionID}
	for childSession := range childSet {
		sessionIDs = append(sessionIDs, childSession)
	}
	placeholders := make([]string, 0, len(sessionIDs))
	args := make([]any, 0, len(sessionIDs))
	for _, value := range sessionIDs {
		placeholders = append(placeholders, "?")
		args = append(args, value)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE session_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) Stats(ctx context.Context, query, status, month, date, agent, accountID string) (conversationCount, traceCount int, inputTokens, outputTokens, cachedTokens int64, err error) {
	where, args := conversationWhere(query, status, month, date, agent, accountID)
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(c.trace_count),0)
		FROM conversations c
		LEFT JOIN account_aliases a ON a.account_id=c.account_id `+where, args...).
		Scan(&conversationCount, &traceCount)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}

	tokenArgs := append([]any{}, args...)
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(t.input_tokens),0), COALESCE(SUM(t.output_tokens),0), COALESCE(SUM(t.cached_tokens),0)
		FROM traces t
		INNER JOIN conversations c ON c.id=t.conversation_id
		LEFT JOIN account_aliases a ON a.account_id=c.account_id `+where, tokenArgs...).
		Scan(&inputTokens, &outputTokens, &cachedTokens)
	if err != nil {
		return 0, 0, 0, 0, 0, err
	}
	return conversationCount, traceCount, inputTokens, outputTokens, cachedTokens, nil
}

func (s *Store) ListAPIRoutes(ctx context.Context) ([]APIRoute, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, base_url, model, api_style, protocol, api_key, enabled, created_at, updated_at
		FROM api_routes ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIRoute, 0)
	for rows.Next() {
		var r APIRoute
		if err := rows.Scan(&r.ID, &r.Name, &r.BaseURL, &r.Model, &r.APIStyle, &r.Protocol, &r.APIKey, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnabledAPIRouteForStyle 返回指定 API 风格下当前启用的路由(每种风格至多一条)。
// 无启用项时 ok=false。转发时按请求所属风格(codex→openai / claude→anthropic)选取。
func (s *Store) EnabledAPIRouteForStyle(ctx context.Context, style string) (APIRoute, bool, error) {
	var r APIRoute
	err := s.db.QueryRowContext(ctx, `SELECT id, name, base_url, model, api_style, protocol, api_key, enabled, created_at, updated_at
		FROM api_routes WHERE enabled=1 AND api_style=? ORDER BY id LIMIT 1`, style).
		Scan(&r.ID, &r.Name, &r.BaseURL, &r.Model, &r.APIStyle, &r.Protocol, &r.APIKey, &r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, true, nil
}

func (s *Store) CreateAPIRoute(ctx context.Context, in APIRoute) (int64, error) {
	if err := validateAPIRoute(&in); err != nil {
		return 0, err
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_routes (name, base_url, model, api_style, protocol, api_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, in.Name, in.BaseURL, in.Model, in.APIStyle, in.Protocol, in.APIKey, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateAPIRoute(ctx context.Context, id int64, in APIRoute) error {
	if err := validateAPIRoute(&in); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE api_routes SET name=?, base_url=?, model=?, api_style=?, protocol=?, api_key=?, updated_at=? WHERE id=?`,
		in.Name, in.BaseURL, in.Model, in.APIStyle, in.Protocol, in.APIKey, time.Now(), id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ToggleAPIRoute 切换某条配置的启用状态,并保证全表互斥:开启一条会关闭其余所有。
// 返回切换后的启用状态。
func (s *Store) ToggleAPIRoute(ctx context.Context, id int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var enabled bool
	var style string
	if err := tx.QueryRowContext(ctx, `SELECT enabled, api_style FROM api_routes WHERE id=? FOR UPDATE`, id).Scan(&enabled, &style); err != nil {
		return false, err
	}
	// 只切 enabled 状态,不动 updated_at,避免列表按 updated_at 排序时启用项跳到最前。
	if enabled {
		// 当前已开 → 关掉它
		if _, err := tx.ExecContext(ctx, `UPDATE api_routes SET enabled=0 WHERE id=?`, id); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	// 当前关闭 → 先关掉同一 API 风格下的其它启用项(按风格互斥,每种风格至多一条),再开这条
	if _, err := tx.ExecContext(ctx, `UPDATE api_routes SET enabled=0 WHERE enabled=1 AND api_style=?`, style); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_routes SET enabled=1 WHERE id=?`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) DeleteAPIRoute(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_routes WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListChainProxies(ctx context.Context) ([]ChainProxy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, api_style, route_ids, enabled, created_at, updated_at
		FROM chain_proxies ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ChainProxy, 0)
	for rows.Next() {
		var item ChainProxy
		var routeIDsRaw string
		if err := rows.Scan(&item.ID, &item.Name, &item.APIStyle, &routeIDsRaw, &item.Enabled, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.RouteIDs = decodeInt64List(routeIDsRaw)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachChainProxyRoutes(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) EnabledChainProxyForStyle(ctx context.Context, style string) (ChainProxy, bool, error) {
	var chain ChainProxy
	var routeIDsRaw string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, api_style, route_ids, enabled, created_at, updated_at
		FROM chain_proxies WHERE enabled=1 AND api_style=? ORDER BY id LIMIT 1`, style).
		Scan(&chain.ID, &chain.Name, &chain.APIStyle, &routeIDsRaw, &chain.Enabled, &chain.CreatedAt, &chain.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return chain, false, nil
	}
	if err != nil {
		return chain, false, err
	}
	chain.RouteIDs = decodeInt64List(routeIDsRaw)
	chains := []ChainProxy{chain}
	if err := s.attachChainProxyRoutes(ctx, chains); err != nil {
		return chain, false, err
	}
	return chains[0], true, nil
}

func (s *Store) attachChainProxyRoutes(ctx context.Context, chains []ChainProxy) error {
	if len(chains) == 0 {
		return nil
	}
	seen := make(map[int64]struct{})
	var ids []int64
	for _, chain := range chains {
		for _, id := range chain.RouteIDs {
			if id <= 0 {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, base_url, model, api_style, protocol, api_key, enabled, created_at, updated_at
		FROM api_routes WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[int64]APIRoute)
	for rows.Next() {
		var r APIRoute
		if err := rows.Scan(&r.ID, &r.Name, &r.BaseURL, &r.Model, &r.APIStyle, &r.Protocol, &r.APIKey, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return err
		}
		byID[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range chains {
		for _, id := range chains[i].RouteIDs {
			if route, ok := byID[id]; ok {
				chains[i].Routes = append(chains[i].Routes, route)
			}
		}
	}
	return nil
}

func (s *Store) CreateChainProxy(ctx context.Context, in ChainProxy) (int64, error) {
	if err := validateChainProxy(&in); err != nil {
		return 0, err
	}
	if err := s.validateChainProxyRoutes(ctx, in.APIStyle, in.RouteIDs); err != nil {
		return 0, err
	}
	now := time.Now()
	routeIDs, _ := json.Marshal(in.RouteIDs)
	res, err := s.db.ExecContext(ctx, `INSERT INTO chain_proxies (name, api_style, route_ids, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, in.Name, in.APIStyle, string(routeIDs), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateChainProxy(ctx context.Context, id int64, in ChainProxy) error {
	if err := validateChainProxy(&in); err != nil {
		return err
	}
	if err := s.validateChainProxyRoutes(ctx, in.APIStyle, in.RouteIDs); err != nil {
		return err
	}
	routeIDs, _ := json.Marshal(in.RouteIDs)
	res, err := s.db.ExecContext(ctx, `UPDATE chain_proxies SET name=?, api_style=?, route_ids=?, updated_at=? WHERE id=?`,
		in.Name, in.APIStyle, string(routeIDs), time.Now(), id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ToggleChainProxy(ctx context.Context, id int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var enabled bool
	var style string
	if err := tx.QueryRowContext(ctx, `SELECT enabled, api_style FROM chain_proxies WHERE id=? FOR UPDATE`, id).Scan(&enabled, &style); err != nil {
		return false, err
	}
	if enabled {
		if _, err := tx.ExecContext(ctx, `UPDATE chain_proxies SET enabled=0 WHERE id=?`, id); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chain_proxies SET enabled=0 WHERE enabled=1 AND api_style=?`, style); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chain_proxies SET enabled=1 WHERE id=?`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) DeleteChainProxy(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM chain_proxies WHERE id=?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) validateChainProxyRoutes(ctx context.Context, style string, routeIDs []int64) error {
	if len(routeIDs) == 0 {
		return errors.New("请至少选择一个路由")
	}
	placeholders := make([]string, 0, len(routeIDs))
	args := make([]any, 0, len(routeIDs)+1)
	args = append(args, style)
	for _, id := range routeIDs {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_routes WHERE api_style=? AND id IN (`+strings.Join(placeholders, ",")+`)`, args...).Scan(&count)
	if err != nil {
		return err
	}
	if count != len(routeIDs) {
		return errors.New("选择的路由与 API 类型不匹配或不存在")
	}
	return nil
}

// validateAPIRoute 归一化字段并校验必填项与长度,避免超出列宽被数据库截断。
func validateAPIRoute(in *APIRoute) error {
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.Model = strings.TrimSpace(in.Model)
	in.APIKey = strings.TrimSpace(in.APIKey)
	in.APIStyle = strings.TrimSpace(strings.ToLower(in.APIStyle))
	in.Protocol = strings.TrimSpace(in.Protocol)
	if in.BaseURL == "" {
		return errors.New("Base URL 不能为空")
	}
	if len(in.Name) > 128 || len(in.Model) > 128 {
		return errors.New("名称 / Model 过长")
	}
	if len(in.BaseURL) > 512 || len(in.APIKey) > 512 {
		return errors.New("Base URL / API Key 过长")
	}
	if in.APIStyle == "" {
		in.APIStyle = "openai"
	}
	protocols, ok := routeProtocols[in.APIStyle]
	if !ok {
		return errors.New("API 风格不合法")
	}
	if in.Protocol != "" {
		if _, ok := protocols[in.Protocol]; !ok {
			return errors.New("接口协议与 API 风格不匹配")
		}
	}
	return nil
}

func validateChainProxy(in *ChainProxy) error {
	in.Name = strings.TrimSpace(in.Name)
	in.APIStyle = strings.TrimSpace(strings.ToLower(in.APIStyle))
	if in.APIStyle == "" {
		in.APIStyle = "openai"
	}
	if _, ok := routeProtocols[in.APIStyle]; !ok {
		return errors.New("API 风格不合法")
	}
	if len(in.Name) > 128 {
		return errors.New("名称过长")
	}
	seen := make(map[int64]struct{})
	out := make([]int64, 0, len(in.RouteIDs))
	for _, id := range in.RouteIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return errors.New("请至少选择一个路由")
	}
	in.RouteIDs = out
	return nil
}

func decodeInt64List(raw string) []int64 {
	var values []int64
	_ = json.Unmarshal([]byte(raw), &values)
	return values
}

func nullJSON(raw []byte) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}
