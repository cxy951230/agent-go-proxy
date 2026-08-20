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
	// 以下字段描述这次转发实际命中的链路,用于写入 token_usages 明细表(尽量用 id 关联)。
	Provider    string
	SourceType  string // direct / route / chain / account
	RouteID     int64  // route / chain 命中的具体路由
	ChainID     int64  // chain 命中的链式代理
	APIKeyID    int64  // account 路径命中的 API Key
	AccountDBID int64  // account 路径命中的 openai_accounts.id
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

// OpenAIAccount 是通过 Codex Bridge 登录的 ChatGPT 账号。AuthJSON 只在写入数据库时
// 使用，列表 API 不返回该字段，避免浏览器页面接触 access/refresh token。
type OpenAIAccount struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	AccountID   string `json:"account_id"`
	PlanType    string `json:"plan_type"`
	Tags        string `json:"tags"`
	AuthJSON    string `json:"-"`
	CodexCommit string `json:"codex_commit"`
	// TokenExpiresAt 是 access_token(JWT)的 exp,登录与每次刷新后写入,
	// 转发前据此判断是否需要提前刷新。零值表示未知。
	TokenExpiresAt time.Time `json:"token_expires_at,omitempty"`
	// RefreshError 记录最近一次刷新失败原因;permanent 失败(refresh token 过期/
	// 被复用/被吊销)意味着必须重新登录。
	RefreshError string `json:"refresh_error,omitempty"`
	// Models 是 Bridge 拉回的可选模型目录(ModelPreset 列表),按账号套餐过滤后的结果。
	// 每个模型自带 supported_reasoning_efforts 与 service_tiers。
	Models   any       `json:"models,omitempty"`
	ModelsAt time.Time `json:"models_at,omitempty"`
	// 以下三项是用户在页面上选的配置。
	SelectedModel           string    `json:"selected_model"`
	SelectedReasoningEffort string    `json:"selected_reasoning_effort"`
	SelectedServiceTier     string    `json:"selected_service_tier"`
	Status                  any       `json:"status,omitempty"`
	StatusError             string    `json:"status_error,omitempty"`
	StatusAt                time.Time `json:"status_at,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
	// Enabled 是页面上的「启用」开关。关掉的账号不参与转发时的账号选择。
	Enabled bool `json:"enabled"`
	// QuotaExhausted 表示最近一次拉取的额度里已有窗口用满(used_percent>=100)。
	// 这类账号打过去必然 429,选择时直接剔除(全满时兜底保留,见 accountPlansForRequest)。
	QuotaExhausted bool `json:"quota_exhausted"`
}

// quotaWindow / quotaStatus 只取判断「额度是否用满」需要的字段,
// status_json 的其余内容由页面自己渲染,这里不关心。
type quotaWindow struct {
	UsedPercent *float64 `json:"used_percent"`
}

type quotaStatus struct {
	Usage struct {
		RateLimit struct {
			Primary   *quotaWindow `json:"primary_window"`
			Secondary *quotaWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	} `json:"usage"`
}

// quotaMaxUsedPercent 返回额度快照里各窗口 used_percent 的最大值。free 账号只有月窗口,
// plus/pro 还有 5 小时 / 周窗口,任一窗口满了请求都会被 429,所以取「最大」。
// ok=false 表示没有额度数据(未拉取 / 解析失败 / 无窗口)——「未知」不等于「用满」。
func quotaMaxUsedPercent(statusRaw string) (float64, bool) {
	statusRaw = strings.TrimSpace(statusRaw)
	if statusRaw == "" || statusRaw == "null" {
		return 0, false
	}
	var parsed quotaStatus
	if json.Unmarshal([]byte(statusRaw), &parsed) != nil {
		return 0, false
	}
	max, found := 0.0, false
	for _, window := range []*quotaWindow{parsed.Usage.RateLimit.Primary, parsed.Usage.RateLimit.Secondary} {
		if window != nil && window.UsedPercent != nil {
			found = true
			if *window.UsedPercent > max {
				max = *window.UsedPercent
			}
		}
	}
	return max, found
}

// quotaExhausted 判断这份额度快照里是否已有窗口用满(used_percent>=100)。
func quotaExhausted(statusRaw string) bool {
	pct, ok := quotaMaxUsedPercent(statusRaw)
	return ok && pct >= 100
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
	// Multimodal 标记这条路由的目标模型是否支持图片。仅在 Responses→Chat 转换时生效:
	// true 才把 input_image 转成 image_url 转发;false(默认)则只留文字,避免非多模态上游 400。
	Multimodal bool      `json:"multimodal"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// APIKey 是「API Key」页管理的密钥配置,只保存名称与 Key 两个字段。
type APIKey struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	APIKey    string    `json:"api_key"`
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

// HeroSMSActivation 是 HeroSMS 买号记录。页面手动买号和 GPT 登录自动化买号共用同一张表，
// 避免以前页面只存在 localStorage、自动化只存在进程内存导致互相看不到。
type HeroSMSActivation struct {
	ID           string    `json:"id"`
	Phone        string    `json:"phone"`
	Service      string    `json:"service"`
	CountryID    string    `json:"country_id"`
	CountryName  string    `json:"country"`
	Price        float64   `json:"price"`
	Source       string    `json:"source"`
	Status       string    `json:"status"`
	Code         string    `json:"code"`
	LastRaw      string    `json:"last_raw"`
	BoughtAt     time.Time `json:"bought_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	CancelledAt  time.Time `json:"cancelled_at,omitempty"`
	CancelAfterS int64     `json:"cancel_after_s"`
}

type HeroSMSAttemptLog struct {
	ID           int64     `json:"id"`
	ActivationID string    `json:"activation_id"`
	Phone        string    `json:"phone"`
	Service      string    `json:"service"`
	CountryID    string    `json:"country_id"`
	CountryName  string    `json:"country"`
	Fee          float64   `json:"fee"`
	Source       string    `json:"source"`
	Result       string    `json:"result"`
	Reason       string    `json:"reason"`
	Raw          string    `json:"raw"`
	CreatedAt    time.Time `json:"created_at"`
}

type HeroSMSCountryBlacklist struct {
	ID          int64     `json:"id"`
	Service     string    `json:"service"`
	CountryID   string    `json:"country_id"`
	CountryName string    `json:"country"`
	Reason      string    `json:"reason"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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
	Models         []string
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
		`CREATE TABLE IF NOT EXISTS token_usages (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			trace_id BIGINT NOT NULL,
			conversation_id BIGINT NOT NULL,
			session_id VARCHAR(128) NOT NULL DEFAULT '',
			provider VARCHAR(24) NOT NULL DEFAULT '',
			model VARCHAR(128) NOT NULL DEFAULT '',
			source_type VARCHAR(24) NOT NULL DEFAULT 'direct',
			route_id BIGINT NOT NULL DEFAULT 0,
			chain_id BIGINT NOT NULL DEFAULT 0,
			api_key_id BIGINT NOT NULL DEFAULT 0,
			account_db_id BIGINT NOT NULL DEFAULT 0,
			account_id VARCHAR(128) NOT NULL DEFAULT '',
			input_tokens INT NOT NULL DEFAULT 0,
			output_tokens INT NOT NULL DEFAULT 0,
			total_tokens INT NOT NULL DEFAULT 0,
			cached_tokens INT NOT NULL DEFAULT 0,
			reasoning_tokens INT NOT NULL DEFAULT 0,
			created_at DATETIME(6) NOT NULL,
			year SMALLINT NOT NULL DEFAULT 0,
			month VARCHAR(7) NOT NULL DEFAULT '',
			date VARCHAR(10) NOT NULL DEFAULT '',
			INDEX idx_tu_created (created_at),
			INDEX idx_tu_route (route_id),
			INDEX idx_tu_chain (chain_id),
			INDEX idx_tu_apikey (api_key_id),
			INDEX idx_tu_account (account_db_id),
			INDEX idx_tu_conv (conversation_id),
			INDEX idx_tu_model (model),
			INDEX idx_tu_source (source_type),
			INDEX idx_tu_date (date),
			INDEX idx_tu_month (month)
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
			multimodal TINYINT(1) NOT NULL DEFAULT 0,
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
		`CREATE TABLE IF NOT EXISTS api_keys (
				id BIGINT AUTO_INCREMENT PRIMARY KEY,
				name VARCHAR(128) NOT NULL DEFAULT '',
				api_key VARCHAR(512) NOT NULL DEFAULT '',
				created_at DATETIME(6) NOT NULL,
				updated_at DATETIME(6) NOT NULL,
				INDEX idx_api_keys_updated_at (updated_at)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		// app_settings 是通用键值设置表(目前存「定时刷新额度是否开启」)。
		`CREATE TABLE IF NOT EXISTS app_settings (
				k VARCHAR(128) NOT NULL PRIMARY KEY,
				v TEXT NOT NULL,
				updated_at DATETIME(6) NOT NULL
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS herosms_activations (
			activation_id VARCHAR(64) NOT NULL PRIMARY KEY,
			phone VARCHAR(64) NOT NULL DEFAULT '',
			service VARCHAR(32) NOT NULL DEFAULT '',
			country_id VARCHAR(32) NOT NULL DEFAULT '',
			country_name VARCHAR(128) NOT NULL DEFAULT '',
			price DECIMAL(10,4) NOT NULL DEFAULT 0,
			source VARCHAR(32) NOT NULL DEFAULT '',
			status VARCHAR(32) NOT NULL DEFAULT 'waiting',
			code VARCHAR(32) NOT NULL DEFAULT '',
			last_raw TEXT NULL,
			bought_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			finished_at DATETIME(6) NULL,
			cancelled_at DATETIME(6) NULL,
			INDEX idx_herosms_bought (bought_at),
			INDEX idx_herosms_status (status),
			INDEX idx_herosms_source (source)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS herosms_attempt_logs (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			activation_id VARCHAR(64) NOT NULL DEFAULT '',
			phone VARCHAR(64) NOT NULL DEFAULT '',
			service VARCHAR(32) NOT NULL DEFAULT '',
			country_id VARCHAR(32) NOT NULL DEFAULT '',
			country_name VARCHAR(128) NOT NULL DEFAULT '',
			source VARCHAR(32) NOT NULL DEFAULT '',
			result VARCHAR(32) NOT NULL DEFAULT '',
			reason VARCHAR(128) NOT NULL DEFAULT '',
			raw TEXT NULL,
			created_at DATETIME(6) NOT NULL,
			INDEX idx_hsal_created (created_at),
			INDEX idx_hsal_service_country (service, country_id),
			INDEX idx_hsal_result (result)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS herosms_country_blacklist (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			service VARCHAR(32) NOT NULL DEFAULT '',
			country_id VARCHAR(32) NOT NULL DEFAULT '',
			country_name VARCHAR(128) NOT NULL DEFAULT '',
			reason VARCHAR(255) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			UNIQUE KEY uk_hscb_service_country (service, country_id),
			INDEX idx_hscb_service (service)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS openai_accounts (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(128) NOT NULL DEFAULT '',
			email VARCHAR(320) NOT NULL DEFAULT '',
			account_id VARCHAR(128) NOT NULL,
			plan_type VARCHAR(64) NOT NULL DEFAULT '',
			tags VARCHAR(255) NOT NULL DEFAULT '',
			auth_json LONGTEXT NOT NULL,
			codex_commit VARCHAR(64) NOT NULL DEFAULT '',
			status_json LONGTEXT NOT NULL,
			status_error TEXT NOT NULL,
			status_at DATETIME(6) NULL,
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			UNIQUE KEY uk_openai_accounts_account_id (account_id),
		INDEX idx_openai_accounts_updated_at (updated_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		// outlook_login_tokens 的权威 schema 归 outlook-login-automation skill 所有;
		// 这里用同样的 IF NOT EXISTS DDL 兜底,保证代理单独启动(skill 从未跑过)时
		// OUTLOOK 页面查询不因表缺失而报错。skill 已建表时这里是空操作。
		`CREATE TABLE IF NOT EXISTS outlook_login_tokens (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			email VARCHAR(320) NOT NULL,
			password VARCHAR(512) NOT NULL DEFAULT '',
			display_name VARCHAR(255) DEFAULT NULL,
			tags VARCHAR(255) NOT NULL DEFAULT '',
			client_id VARCHAR(80) NOT NULL,
			tenant_id VARCHAR(80) DEFAULT NULL,
			account_oid VARCHAR(80) DEFAULT NULL,
			home_account_id VARCHAR(180) DEFAULT NULL,
			scope TEXT DEFAULT NULL,
			token_type VARCHAR(40) DEFAULT NULL,
			access_token LONGTEXT DEFAULT NULL,
			refresh_token LONGTEXT DEFAULT NULL,
			id_token LONGTEXT DEFAULT NULL,
			client_info LONGTEXT DEFAULT NULL,
			expires_in INT DEFAULT NULL,
			ext_expires_in INT DEFAULT NULL,
			refresh_token_expires_in INT DEFAULT NULL,
			token_issued_at DATETIME DEFAULT NULL,
			access_token_expires_at DATETIME DEFAULT NULL,
			refresh_token_expires_at DATETIME DEFAULT NULL,
			cookies_json LONGTEXT DEFAULT NULL,
			cookie_count INT DEFAULT NULL,
			user_agent TEXT DEFAULT NULL,
			session_id VARCHAR(80) DEFAULT NULL,
			session_file TEXT DEFAULT NULL,
			profile_dir TEXT DEFAULT NULL,
			run_dir TEXT DEFAULT NULL,
			final_url TEXT DEFAULT NULL,
			last_refresh_status VARCHAR(80) DEFAULT NULL,
			last_refresh_error TEXT DEFAULT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY uniq_email_client_scope (email, client_id, scope(255))
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
	// 历史数据回填只需要跑一次。traces 里保存了完整请求/响应体，表可能很大；
	// 如果每次启动都 UPDATE/扫描，会明显拖慢服务启动。
	if done, err := s.GetSetting(ctx, "migration_account_id_backfill_v1", "0"); err != nil {
		return err
	} else if done != "1" {
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
		if err := s.SetSetting(ctx, "migration_account_id_backfill_v1", "1"); err != nil {
			return err
		}
	}
	if done, err := s.GetSetting(ctx, "migration_repair_injected_prompts_v1", "0"); err != nil {
		return err
	} else if done != "1" {
		if err := s.repairInjectedPrompts(ctx); err != nil {
			return err
		}
		if err := s.SetSetting(ctx, "migration_repair_injected_prompts_v1", "1"); err != nil {
			return err
		}
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
	if err := s.ensureColumn(ctx, "api_routes", "multimodal", "TINYINT(1) NOT NULL DEFAULT 0 AFTER enabled"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "openai_accounts", "status_json", "LONGTEXT NOT NULL AFTER codex_commit"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "openai_accounts", "status_error", "TEXT NOT NULL AFTER status_json"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "openai_accounts", "status_at", "DATETIME(6) NULL AFTER status_error"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "openai_accounts", "token_expires_at", "DATETIME(6) NULL AFTER auth_json"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "openai_accounts", "refresh_error", "TEXT NULL AFTER token_expires_at"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "openai_accounts", "tags", "VARCHAR(255) NOT NULL DEFAULT '' AFTER plan_type"); err != nil {
		return err
	}
	for _, col := range [][2]string{
		{"models_json", "LONGTEXT NULL"},
		{"models_at", "DATETIME(6) NULL"},
		{"selected_model", "VARCHAR(128) NOT NULL DEFAULT ''"},
		{"selected_reasoning_effort", "VARCHAR(32) NOT NULL DEFAULT ''"},
		{"selected_service_tier", "VARCHAR(64) NOT NULL DEFAULT ''"},
		// enabled:页面上的「启用」开关。默认 1,老数据自动全部启用。
		{"enabled", "TINYINT(1) NOT NULL DEFAULT 1"},
	} {
		if err := s.ensureColumn(ctx, "openai_accounts", col[0], col[1]); err != nil {
			return err
		}
	}
	// outlook_login_tokens 存明文登录密码(手动新增/编辑账号用,供后续自动登录复用)。
	// skill 的 upsert 列表不含 password,登录时不会覆盖它。
	if err := s.ensureColumn(ctx, "outlook_login_tokens", "password", "VARCHAR(512) NOT NULL DEFAULT '' AFTER email"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "outlook_login_tokens", "tags", "VARCHAR(255) NOT NULL DEFAULT '' AFTER display_name"); err != nil {
		return err
	}
	// has_gpt_account 历史字段:缓存「该邮箱是否已登录 Codex/OPENAI 菜单存在账号」。
	// registered_gpt_account 是单独的「是否已完成 ChatGPT 注册」标记,不在页面展示,只控制注册按钮。
	if err := s.ensureColumn(ctx, "outlook_login_tokens", "has_gpt_account", "TINYINT(1) NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "outlook_login_tokens", "registered_gpt_account", "TINYINT(1) NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

func (s *Store) SaveHeroSMSActivation(ctx context.Context, a HeroSMSActivation) error {
	if strings.TrimSpace(a.ID) == "" {
		return errors.New("empty HeroSMS activation id")
	}
	now := time.Now()
	if a.BoughtAt.IsZero() {
		a.BoughtAt = now
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = now
	}
	if a.Status == "" {
		a.Status = "waiting"
	}
	var finished any
	var cancelled any
	if a.Status == "finished" {
		finished = a.UpdatedAt
	}
	if a.Status == "cancelled" {
		cancelled = a.UpdatedAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO herosms_activations
		(activation_id, phone, service, country_id, country_name, price, source, status, code, last_raw, bought_at, updated_at, finished_at, cancelled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE phone=VALUES(phone), service=VALUES(service), country_id=VALUES(country_id),
		country_name=VALUES(country_name), price=VALUES(price), source=VALUES(source), status=VALUES(status),
		code=IF(VALUES(code)<>'', VALUES(code), code), last_raw=VALUES(last_raw), updated_at=VALUES(updated_at),
		finished_at=IF(VALUES(finished_at) IS NULL, finished_at, VALUES(finished_at)),
		cancelled_at=IF(VALUES(cancelled_at) IS NULL, cancelled_at, VALUES(cancelled_at))`,
		a.ID, a.Phone, a.Service, a.CountryID, a.CountryName, a.Price, a.Source, a.Status, a.Code, a.LastRaw, a.BoughtAt, a.UpdatedAt, finished, cancelled)
	return err
}

func (s *Store) ListHeroSMSActivations(ctx context.Context, includeDone bool) ([]HeroSMSActivation, error) {
	where := ""
	if !includeDone {
		where = "WHERE status NOT IN ('cancelled','finished','cancel_failed')"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT activation_id, phone, service, country_id, country_name, price, source, status, code, COALESCE(last_raw,''), bought_at, updated_at,
		finished_at, cancelled_at FROM herosms_activations `+where+` ORDER BY bought_at DESC LIMIT 300`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []HeroSMSActivation{}
	for rows.Next() {
		var a HeroSMSActivation
		var finished, cancelled sql.NullTime
		if err := rows.Scan(&a.ID, &a.Phone, &a.Service, &a.CountryID, &a.CountryName, &a.Price, &a.Source, &a.Status, &a.Code, &a.LastRaw, &a.BoughtAt, &a.UpdatedAt, &finished, &cancelled); err != nil {
			return nil, err
		}
		if finished.Valid {
			a.FinishedAt = finished.Time
		}
		if cancelled.Valid {
			a.CancelledAt = cancelled.Time
		}
		left := int64(herosmsCancelAfter.Seconds()) - int64(time.Since(a.BoughtAt).Seconds())
		if left < 0 {
			left = 0
		}
		a.CancelAfterS = left
		items = append(items, a)
	}
	return items, rows.Err()
}

func (s *Store) UpdateHeroSMSActivationStatus(ctx context.Context, id, status, code, raw string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = "waiting"
	}
	now := time.Now()
	var finished any
	var cancelled any
	if status == "finished" {
		finished = now
	}
	if status == "cancelled" || status == "cancel_failed" {
		cancelled = now
	}
	_, err := s.db.ExecContext(ctx, `UPDATE herosms_activations SET status=?, code=IF(?<>'', ?, code), last_raw=?, updated_at=?,
		finished_at=COALESCE(finished_at, ?), cancelled_at=COALESCE(cancelled_at, ?) WHERE activation_id=?`,
		status, code, code, raw, now, finished, cancelled, id)
	return err
}

func (s *Store) DueHeroSMSActivations(ctx context.Context, olderThan time.Duration) ([]HeroSMSActivation, error) {
	cutoff := time.Now().Add(-olderThan)
	rows, err := s.db.QueryContext(ctx, `SELECT activation_id, phone, service, country_id, country_name, price, source, status, code, COALESCE(last_raw,''), bought_at, updated_at,
		finished_at, cancelled_at FROM herosms_activations
		WHERE status NOT IN ('cancelled','finished','cancel_failed') AND bought_at<=? ORDER BY bought_at ASC LIMIT 100`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []HeroSMSActivation{}
	for rows.Next() {
		var a HeroSMSActivation
		var finished, cancelled sql.NullTime
		if err := rows.Scan(&a.ID, &a.Phone, &a.Service, &a.CountryID, &a.CountryName, &a.Price, &a.Source, &a.Status, &a.Code, &a.LastRaw, &a.BoughtAt, &a.UpdatedAt, &finished, &cancelled); err != nil {
			return nil, err
		}
		if finished.Valid {
			a.FinishedAt = finished.Time
		}
		if cancelled.Valid {
			a.CancelledAt = cancelled.Time
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func (s *Store) ClearDoneHeroSMSActivations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM herosms_activations WHERE status IN ('cancelled','finished','cancel_failed')`)
	return err
}

func (s *Store) InsertHeroSMSAttemptLog(ctx context.Context, in HeroSMSAttemptLog) error {
	if strings.TrimSpace(in.ActivationID) == "" && strings.TrimSpace(in.Phone) == "" {
		return nil
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO herosms_attempt_logs
		(activation_id, phone, service, country_id, country_name, source, result, reason, raw, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ActivationID, in.Phone, in.Service, in.CountryID, in.CountryName, in.Source, in.Result, in.Reason, in.Raw, in.CreatedAt)
	return err
}

func (s *Store) UpdateLatestHeroSMSAttemptLog(ctx context.Context, activationID, result, reason, raw string) (bool, error) {
	if strings.TrimSpace(activationID) == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE herosms_attempt_logs SET result=?, reason=?, raw=?
		WHERE id=(SELECT id FROM (SELECT id FROM herosms_attempt_logs WHERE activation_id=? ORDER BY id DESC LIMIT 1) AS t)`,
		result, reason, raw, activationID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) ListHeroSMSAttemptLogs(ctx context.Context, service string, limit int) ([]HeroSMSAttemptLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	where := ""
	args := []any{}
	if service = strings.TrimSpace(service); service != "" {
		where = "WHERE l.service=?"
		args = append(args, service)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.activation_id, l.phone, l.service, l.country_id, l.country_name, COALESCE(a.price,0), l.source, l.result, l.reason, COALESCE(l.raw,''), l.created_at
		FROM herosms_attempt_logs l LEFT JOIN herosms_activations a ON a.activation_id=l.activation_id `+where+` ORDER BY l.created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []HeroSMSAttemptLog{}
	for rows.Next() {
		var x HeroSMSAttemptLog
		if err := rows.Scan(&x.ID, &x.ActivationID, &x.Phone, &x.Service, &x.CountryID, &x.CountryName, &x.Fee, &x.Source, &x.Result, &x.Reason, &x.Raw, &x.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, x)
	}
	return items, rows.Err()
}

func (s *Store) HeroSMSBlacklistedCountryIDs(ctx context.Context, service string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT country_id FROM herosms_country_blacklist WHERE service=?`, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) ListHeroSMSCountryBlacklist(ctx context.Context, service string) ([]HeroSMSCountryBlacklist, error) {
	where := ""
	args := []any{}
	if service = strings.TrimSpace(service); service != "" {
		// 这张表的查询没有 join、也没起别名，早期从 ListHeroSMSAttemptLogs 抄过来的 l. 前缀
		// 会直接报 Unknown column 'l.service'，导致「国家拉黑」页始终 500。
		where = "WHERE service=?"
		args = append(args, service)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, service, country_id, country_name, reason, created_at, updated_at FROM herosms_country_blacklist `+where+` ORDER BY service, country_name, country_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []HeroSMSCountryBlacklist{}
	for rows.Next() {
		var x HeroSMSCountryBlacklist
		if err := rows.Scan(&x.ID, &x.Service, &x.CountryID, &x.CountryName, &x.Reason, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, x)
	}
	return items, rows.Err()
}

func (s *Store) UpsertHeroSMSCountryBlacklist(ctx context.Context, in HeroSMSCountryBlacklist) error {
	if strings.TrimSpace(in.Service) == "" || strings.TrimSpace(in.CountryID) == "" {
		return errors.New("缺少 service/country_id")
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO herosms_country_blacklist (service, country_id, country_name, reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE country_name=VALUES(country_name), reason=VALUES(reason), updated_at=VALUES(updated_at)`,
		in.Service, in.CountryID, in.CountryName, in.Reason, now, now)
	return err
}

func (s *Store) DeleteHeroSMSCountryBlacklist(ctx context.Context, service, countryID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM herosms_country_blacklist WHERE service=? AND country_id=?`, service, countryID)
	return err
}

// UpdateOpenAIAccountModels 缓存 Bridge 拉回的模型目录。目录随账号套餐变化,
// 由页面手动刷新触发,不在转发路径里拉取。
func (s *Store) UpdateOpenAIAccountModels(ctx context.Context, id int64, modelsJSON string) error {
	if !json.Valid([]byte(modelsJSON)) {
		return errors.New("Bridge 返回的模型 JSON 无效")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE openai_accounts SET models_json=?, models_at=? WHERE id=?`,
		modelsJSON, time.Now(), id)
	return err
}

// ToggleOpenAIAccount 切换账号的启用状态,返回切换后的值。
// 只改 enabled 不动 updated_at:这是一次纯开关操作,不该算作账号内容更新。
func (s *Store) ToggleOpenAIAccount(ctx context.Context, id int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var enabled bool
	if err := tx.QueryRowContext(ctx, `SELECT enabled FROM openai_accounts WHERE id=? FOR UPDATE`, id).Scan(&enabled); err != nil {
		return false, err
	}
	next := !enabled
	if _, err := tx.ExecContext(ctx, `UPDATE openai_accounts SET enabled=? WHERE id=?`, next, id); err != nil {
		return false, err
	}
	return next, tx.Commit()
}

// SetAllOpenAIAccountsEnabled 批量把所有账号设成启用/停用,返回真正改动的行数。
// 带 `enabled<>?` 是为了只动状态不一致的行,避免无谓写入。
func (s *Store) SetAllOpenAIAccountsEnabled(ctx context.Context, enabled bool) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE openai_accounts SET enabled=? WHERE enabled<>?`, enabled, enabled)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdateOpenAIAccountSettings 保存页面上选择的模型 / 推理强度 / 速度档位。
func (s *Store) UpdateOpenAIAccountSettings(ctx context.Context, id int64, model, effort, serviceTier string) error {
	model = strings.TrimSpace(model)
	effort = strings.TrimSpace(effort)
	serviceTier = strings.TrimSpace(serviceTier)
	if len(model) > 128 || len(effort) > 32 || len(serviceTier) > 64 {
		return errors.New("模型配置字段过长")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE openai_accounts
		SET selected_model=?, selected_reasoning_effort=?, selected_service_tier=?, updated_at=? WHERE id=?`,
		model, effort, serviceTier, time.Now(), id)
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

// UpdateOpenAIAccountAuth 刷新成功后回写完整凭证与新的过期时间。
// 必须整份覆盖 auth_json:上游刷新会轮换 refresh_token,旧值再用会被判定为
// refresh_token_reused 而导致账号必须重新登录。
func (s *Store) UpdateOpenAIAccountAuth(ctx context.Context, id int64, authJSON string, expiresAt time.Time) error {
	if !json.Valid([]byte(authJSON)) {
		return errors.New("刷新返回的鉴权 JSON 无效")
	}
	var expires any
	if !expiresAt.IsZero() {
		expires = expiresAt
	}
	_, err := s.db.ExecContext(ctx, `UPDATE openai_accounts
		SET auth_json=?, token_expires_at=?, refresh_error='', updated_at=? WHERE id=?`,
		authJSON, expires, time.Now(), id)
	return err
}

// UpdateOpenAIAccountTokenExpiry 只回填过期时间。用于历史记录补齐:过期时间能直接从
// 已存的 access_token(JWT)本地算出,不需要发起刷新。
func (s *Store) UpdateOpenAIAccountTokenExpiry(ctx context.Context, id int64, expiresAt time.Time) error {
	var expires any
	if !expiresAt.IsZero() {
		expires = expiresAt
	}
	_, err := s.db.ExecContext(ctx, `UPDATE openai_accounts SET token_expires_at=? WHERE id=?`, expires, id)
	return err
}

// SetOpenAIAccountRefreshError 记录刷新失败原因,供页面提示「需重新登录」。
func (s *Store) SetOpenAIAccountRefreshError(ctx context.Context, id int64, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE openai_accounts SET refresh_error=? WHERE id=?`, message, id)
	return err
}

func (s *Store) GetOpenAIAccount(ctx context.Context, id int64) (OpenAIAccount, error) {
	var account OpenAIAccount
	var statusRaw string
	var statusAt, expiresAt sql.NullTime
	var refreshError sql.NullString
	var modelsRaw sql.NullString
	var modelsAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, name, email, account_id, plan_type, tags, auth_json, codex_commit, status_json, status_error, status_at, token_expires_at, refresh_error,
		models_json, models_at, selected_model, selected_reasoning_effort, selected_service_tier, enabled, created_at, updated_at
		FROM openai_accounts WHERE id=?`, id).Scan(&account.ID, &account.Name, &account.Email, &account.AccountID, &account.PlanType, &account.Tags, &account.AuthJSON, &account.CodexCommit, &statusRaw, &account.StatusError, &statusAt, &expiresAt, &refreshError,
		&modelsRaw, &modelsAt, &account.SelectedModel, &account.SelectedReasoningEffort, &account.SelectedServiceTier, &account.Enabled, &account.CreatedAt, &account.UpdatedAt)
	if err != nil {
		return account, err
	}
	account.QuotaExhausted = quotaExhausted(statusRaw)
	account.Models = decodeModelsJSON(modelsRaw)
	if modelsAt.Valid {
		account.ModelsAt = modelsAt.Time
	}
	if expiresAt.Valid {
		account.TokenExpiresAt = expiresAt.Time
	}
	if refreshError.Valid {
		account.RefreshError = refreshError.String
	}
	if json.Valid([]byte(statusRaw)) && statusRaw != "null" && statusRaw != "" {
		_ = json.Unmarshal([]byte(statusRaw), &account.Status)
	}
	if statusAt.Valid {
		account.StatusAt = statusAt.Time
	}
	return account, nil
}

func (s *Store) UpdateOpenAIAccountStatus(ctx context.Context, id int64, statusJSON, statusError string) error {
	if statusJSON == "" {
		statusJSON = "null"
	}
	if !json.Valid([]byte(statusJSON)) {
		return errors.New("Bridge 返回的额度 JSON 无效")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE openai_accounts SET status_json=?, status_error=?, status_at=? WHERE id=?`, statusJSON, statusError, time.Now(), id)
	return err
}

func (s *Store) ListOpenAIAccounts(ctx context.Context) ([]OpenAIAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, email, account_id, plan_type, tags, codex_commit, status_json, status_error, status_at, token_expires_at, refresh_error,
		models_json, models_at, selected_model, selected_reasoning_effort, selected_service_tier, enabled, created_at, updated_at
		FROM openai_accounts ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OpenAIAccount, 0)
	for rows.Next() {
		var account OpenAIAccount
		var statusRaw string
		var statusAt, expiresAt, modelsAt sql.NullTime
		var refreshError, modelsRaw sql.NullString
		if err := rows.Scan(&account.ID, &account.Name, &account.Email, &account.AccountID, &account.PlanType, &account.Tags, &account.CodexCommit, &statusRaw, &account.StatusError, &statusAt, &expiresAt, &refreshError,
			&modelsRaw, &modelsAt, &account.SelectedModel, &account.SelectedReasoningEffort, &account.SelectedServiceTier, &account.Enabled, &account.CreatedAt, &account.UpdatedAt); err != nil {
			return nil, err
		}
		account.QuotaExhausted = quotaExhausted(statusRaw)
		if expiresAt.Valid {
			account.TokenExpiresAt = expiresAt.Time
		}
		if refreshError.Valid {
			account.RefreshError = refreshError.String
		}
		account.Models = decodeModelsJSON(modelsRaw)
		if modelsAt.Valid {
			account.ModelsAt = modelsAt.Time
		}
		if json.Valid([]byte(statusRaw)) && statusRaw != "null" && statusRaw != "" {
			_ = json.Unmarshal([]byte(statusRaw), &account.Status)
		}
		if statusAt.Valid {
			account.StatusAt = statusAt.Time
		}
		out = append(out, account)
	}
	return out, rows.Err()
}

func (s *Store) UpsertOpenAIAccount(ctx context.Context, account OpenAIAccount) (int64, error) {
	account.Name = strings.TrimSpace(account.Name)
	account.Email = strings.TrimSpace(account.Email)
	account.AccountID = strings.TrimSpace(account.AccountID)
	account.PlanType = strings.TrimSpace(account.PlanType)
	if account.AccountID == "" {
		return 0, errors.New("ChatGPT account id 为空")
	}
	if !json.Valid([]byte(account.AuthJSON)) {
		return 0, errors.New("Bridge 返回的鉴权 JSON 无效")
	}
	if len(account.Name) > 128 || len(account.AccountID) > 128 || len(account.PlanType) > 64 || len(account.CodexCommit) > 64 {
		return 0, errors.New("OpenAI 账号字段过长")
	}
	if len(account.Email) > 320 {
		return 0, errors.New("OpenAI 账号邮箱过长")
	}
	now := time.Now()
	var expires any
	if !account.TokenExpiresAt.IsZero() {
		expires = account.TokenExpiresAt
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO openai_accounts
		(name, email, account_id, plan_type, auth_json, token_expires_at, refresh_error, codex_commit, status_json, status_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '', ?, 'null', '', ?, ?)
		ON DUPLICATE KEY UPDATE
			name=IF(VALUES(name)='', name, VALUES(name)), email=VALUES(email),
			plan_type=VALUES(plan_type), auth_json=VALUES(auth_json),
			token_expires_at=VALUES(token_expires_at), refresh_error='',
			codex_commit=VALUES(codex_commit), updated_at=VALUES(updated_at)`,
		account.Name, account.Email, account.AccountID, account.PlanType, account.AuthJSON, expires, account.CodexCommit, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if id == 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM openai_accounts WHERE account_id=?`, account.AccountID).Scan(&id); err != nil {
			return 0, err
		}
	}
	if account.Name != "" {
		_, err = s.db.ExecContext(ctx, `INSERT INTO account_aliases (account_id, display_name, created_at, updated_at)
			VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE display_name=VALUES(display_name), updated_at=VALUES(updated_at)`,
			account.AccountID, account.Name, now, now)
	}
	return id, err
}

// DeleteOpenAIAccount 删除 GPT 账号,并把 OUTLOOK 里同邮箱的邮箱记录(含 token 与 cookie)
// 一并删掉——这两边本来就是同一个号的两半,留着孤儿邮箱记录只会让列表越攒越脏。
// 返回顺带删掉的 outlook 行数,供页面提示。
//
// 邮箱为空时**不做级联**:空串匹配不出「同一个人」,一旦按空串删会把所有没邮箱的行全清掉。
func (s *Store) DeleteOpenAIAccount(ctx context.Context, id int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var email string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(email,'') FROM openai_accounts WHERE id=? FOR UPDATE`, id).Scan(&email); err != nil {
		return 0, err // 不存在时是 sql.ErrNoRows,由上层转 404
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM openai_accounts WHERE id=?`, id); err != nil {
		return 0, err
	}
	var outlookDeleted int64
	if email = strings.TrimSpace(email); email != "" {
		res, err := tx.ExecContext(ctx, `DELETE FROM outlook_login_tokens WHERE email=?`, email)
		if err != nil {
			return 0, err
		}
		outlookDeleted, _ = res.RowsAffected()
	}
	return outlookDeleted, tx.Commit()
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
	// token 消耗明细:只在真实产生 token 时落一行,冗余年/月/日方便按时间维度查询与画图。
	// conversation_id / session_id / model / account_id 直接取自 traces 行,来源字段由转发层传入。
	if in.Usage.TotalTokens > 0 {
		sourceType := in.SourceType
		if sourceType == "" {
			sourceType = "direct"
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO token_usages
			(trace_id, conversation_id, session_id, provider, model, source_type, route_id, chain_id, api_key_id, account_db_id, account_id,
			 input_tokens, output_tokens, total_tokens, cached_tokens, reasoning_tokens, created_at, year, month, date)
			SELECT t.id, t.conversation_id, t.session_id, ?, t.model, ?, ?, ?, ?, ?, t.account_id,
			 ?, ?, ?, ?, ?, ?, ?, ?, ?
			FROM traces t WHERE t.id=?`,
			in.Provider, sourceType, in.RouteID, in.ChainID, in.APIKeyID, in.AccountDBID,
			in.Usage.InputTokens, in.Usage.OutputTokens, in.Usage.TotalTokens, in.Usage.CachedInputTokens, in.Usage.ReasoningTokens,
			now, now.Year(), now.Format("2006-01"), now.Format("2006-01-02"), traceID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConversationFilter 归集看板顶部所有过滤条件。model 按会话记录的模型过滤;
// source 按 token_usages 记录的转发来源(direct/route/chain/account)过滤——
// 来源是 trace 级别的,所以用 EXISTS 判断会话是否有匹配来源的 token 消耗。
type ConversationFilter struct {
	Query     string
	Status    string
	Month     string
	Date      string
	Agent     string
	AccountID string
	Model     string
	Source    string
}

func conversationWhere(f ConversationFilter) (string, []any) {
	where := "WHERE 1=1"
	args := []any{}
	if f.Status != "" && f.Status != "all" {
		where += " AND c.status=?"
		args = append(args, f.Status)
	}
	if f.Month != "" && f.Month != "all" {
		where += " AND DATE_FORMAT(c.updated_at, '%Y-%m')=?"
		args = append(args, f.Month)
	}
	if f.Date != "" && f.Date != "all" {
		where += " AND DATE(c.updated_at)=?"
		args = append(args, f.Date)
	}
	if f.Agent != "" && f.Agent != "all" {
		where += " AND c.agent=?"
		args = append(args, f.Agent)
	}
	if f.AccountID != "" && f.AccountID != "all" {
		where += " AND c.account_id=?"
		args = append(args, f.AccountID)
	}
	if f.Model != "" && f.Model != "all" {
		where += " AND c.model=?"
		args = append(args, f.Model)
	}
	if f.Source != "" && f.Source != "all" {
		where += " AND EXISTS(SELECT 1 FROM token_usages tu WHERE tu.conversation_id=c.id AND tu.source_type=?)"
		args = append(args, f.Source)
	}
	if f.Query != "" {
		where += " AND (c.session_id LIKE ? OR c.first_prompt LIKE ? OR c.model LIKE ? OR c.account_id LIKE ? OR a.display_name LIKE ?)"
		like := "%" + f.Query + "%"
		args = append(args, like, like, like, like, like)
	}
	return where, args
}

func (s *Store) ListConversations(ctx context.Context, f ConversationFilter) ([]ConversationSummary, error) {
	return s.ListConversationsPage(ctx, f, 200, 0)
}

func (s *Store) ListConversationsPage(ctx context.Context, f ConversationFilter, limit, offset int) ([]ConversationSummary, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	where, args := conversationWhere(f)
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

	modelRows, err := s.db.QueryContext(ctx, `SELECT DISTINCT model FROM conversations WHERE model<>'' ORDER BY model ASC`)
	if err != nil {
		return opts, err
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var value string
		if err := modelRows.Scan(&value); err != nil {
			return opts, err
		}
		opts.Models = append(opts.Models, value)
	}
	if err := modelRows.Err(); err != nil {
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

// TokenPoint 是 token 消耗时间序列上的一个时间桶(按天/月/年聚合)。
type TokenPoint struct {
	Period       string `json:"period"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
}

// TokenModelSeriesPoint 是「时间桶 × 模型」二维聚合的一格:某个时间点上某个模型的消耗。
// 前端据此画分组柱状图(同一时间点内各模型的柱子并排)。
type TokenModelSeriesPoint struct {
	Period       string `json:"period"`
	Model        string `json:"model"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
}

// tokenDimColumn 把维度名安全映射到 token_usages 的过滤列(白名单,防注入)。
var tokenDimColumn = map[string]string{
	"route":   "route_id",
	"chain":   "chain_id",
	"api_key": "api_key_id",
	"account": "account_db_id",
}

// tokenGranularityExpr 把粒度名映射到分组用的期间列(白名单)。
var tokenGranularityExpr = map[string]string{
	"day":   "date",
	"month": "month",
	"year":  "CAST(year AS CHAR)",
}

// TokenSeries 按维度(路由/链式/API Key/账号)与粒度(天/月/年)聚合 token 消耗时间序列。
func (s *Store) TokenSeries(ctx context.Context, dim string, id int64, granularity string) ([]TokenPoint, error) {
	column, ok := tokenDimColumn[dim]
	if !ok {
		return nil, errors.New("不支持的统计维度")
	}
	periodExpr, ok := tokenGranularityExpr[granularity]
	if !ok {
		periodExpr = tokenGranularityExpr["day"]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+periodExpr+` AS period,
		COUNT(*) requests, COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cached_tokens),0), COALESCE(SUM(total_tokens),0)
		FROM token_usages WHERE `+column+`=?
		GROUP BY period ORDER BY period ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TokenPoint, 0)
	for rows.Next() {
		var p TokenPoint
		if err := rows.Scan(&p.Period, &p.Requests, &p.InputTokens, &p.OutputTokens, &p.CachedTokens, &p.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TokenSeriesByModel 按维度与粒度做「时间桶 × 模型」二维聚合:每个时间点上每个模型各一行。
// 用于请求数/ token 消耗的分组柱状图(同一时间点内各模型并排)。
func (s *Store) TokenSeriesByModel(ctx context.Context, dim string, id int64, granularity string) ([]TokenModelSeriesPoint, error) {
	column, ok := tokenDimColumn[dim]
	if !ok {
		return nil, errors.New("不支持的统计维度")
	}
	periodExpr, ok := tokenGranularityExpr[granularity]
	if !ok {
		periodExpr = tokenGranularityExpr["day"]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+periodExpr+` AS period,
		COALESCE(NULLIF(model,''),'unknown') model,
		COUNT(*) requests, COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cached_tokens),0), COALESCE(SUM(total_tokens),0)
		FROM token_usages WHERE `+column+`=?
		GROUP BY period, model ORDER BY period ASC, SUM(total_tokens) DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TokenModelSeriesPoint, 0)
	for rows.Next() {
		var p TokenModelSeriesPoint
		if err := rows.Scan(&p.Period, &p.Model, &p.Requests, &p.InputTokens, &p.OutputTokens, &p.CachedTokens, &p.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
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

func (s *Store) SetOpenAIAccountTags(ctx context.Context, id int64, tags string) error {
	tags = strings.TrimSpace(tags)
	if len(tags) > 255 {
		tags = tags[:255]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE openai_accounts SET tags=?, updated_at=? WHERE id=?`, tags, time.Now(), id)
	return err
}

func (s *Store) SetOutlookAccountTags(ctx context.Context, id int64, tags string) error {
	tags = strings.TrimSpace(tags)
	if len(tags) > 255 {
		tags = tags[:255]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE outlook_login_tokens SET tags=?, updated_at=? WHERE id=?`, tags, time.Now(), id)
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

func (s *Store) Stats(ctx context.Context, f ConversationFilter) (conversationCount, traceCount int, inputTokens, outputTokens, cachedTokens int64, err error) {
	where, args := conversationWhere(f)
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, base_url, model, api_style, protocol, api_key, enabled, multimodal, created_at, updated_at
		FROM api_routes ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIRoute, 0)
	for rows.Next() {
		var r APIRoute
		if err := rows.Scan(&r.ID, &r.Name, &r.BaseURL, &r.Model, &r.APIStyle, &r.Protocol, &r.APIKey, &r.Enabled, &r.Multimodal, &r.CreatedAt, &r.UpdatedAt); err != nil {
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
	err := s.db.QueryRowContext(ctx, `SELECT id, name, base_url, model, api_style, protocol, api_key, enabled, multimodal, created_at, updated_at
		FROM api_routes WHERE enabled=1 AND api_style=? ORDER BY id LIMIT 1`, style).
		Scan(&r.ID, &r.Name, &r.BaseURL, &r.Model, &r.APIStyle, &r.Protocol, &r.APIKey, &r.Enabled, &r.Multimodal, &r.CreatedAt, &r.UpdatedAt)
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_routes (name, base_url, model, api_style, protocol, api_key, multimodal, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.Name, in.BaseURL, in.Model, in.APIStyle, in.Protocol, in.APIKey, in.Multimodal, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateAPIRoute(ctx context.Context, id int64, in APIRoute) error {
	if err := validateAPIRoute(&in); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE api_routes SET name=?, base_url=?, model=?, api_style=?, protocol=?, api_key=?, multimodal=?, updated_at=? WHERE id=?`,
		in.Name, in.BaseURL, in.Model, in.APIStyle, in.Protocol, in.APIKey, in.Multimodal, time.Now(), id)
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

// GetSetting 读通用设置;不存在返回 def(不报错)。
func (s *Store) GetSetting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM app_settings WHERE k=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, err
	}
	return v, nil
}

// SetSetting 写通用设置(upsert)。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_settings (k, v, updated_at) VALUES (?,?,?)
		 ON DUPLICATE KEY UPDATE v=VALUES(v), updated_at=VALUES(updated_at)`,
		key, value, time.Now())
	return err
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, api_key, created_at, updated_at
		FROM api_keys ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]APIKey, 0)
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.APIKey, &k.CreatedAt, &k.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// MatchAPIKeyID 判断客户端带来的 key 是否命中「API Key」页配置,命中返回其 id(用于 token 明细关联)。
func (s *Store) MatchAPIKeyID(ctx context.Context, key string) (int64, bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, false, nil
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE api_key=? ORDER BY id LIMIT 1`, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// DefaultOpenAIAccount 取用于直连反代的 GPT 账号。当前按 id 取第一个(只配了一个),
// 后续要做多账号调度时改这里即可。返回值包含 AuthJSON。
func (s *Store) DefaultOpenAIAccount(ctx context.Context) (OpenAIAccount, bool, error) {
	return s.queryOpenAIAccount(ctx, "", "")
}

// OpenAIAccountForModel 按请求里的模型挑账号:选中该模型的账号里取 id 最小的一个。
// 匹配不到返回 ok=false,由调用方决定如何报错(不静默回落,避免用错模型)。
func (s *Store) OpenAIAccountForModel(ctx context.Context, model string) (OpenAIAccount, bool, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return OpenAIAccount{}, false, nil
	}
	return s.queryOpenAIAccount(ctx, "WHERE LOWER(selected_model)=LOWER(?)", model)
}

// OpenAIAccountsForModel 返回所有把 selected_model 设为该模型、**且已启用**的账号
// (含 AuthJSON),供负载均衡在候选集里挑选。关掉「启用」的账号直接不进候选,
// 额度用满的账号仍会返回但带 QuotaExhausted 标记,由 accountPool 沉到最底。
// 空模型或无候选返回空切片。按 id 升序,保证候选顺序稳定。
func (s *Store) OpenAIAccountsForModel(ctx context.Context, model string) ([]OpenAIAccount, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, email, account_id, plan_type, auth_json, token_expires_at, refresh_error,
		selected_model, selected_reasoning_effort, selected_service_tier, status_json
		FROM openai_accounts WHERE LOWER(selected_model)=LOWER(?) AND enabled=1 ORDER BY id`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenAIAccount
	for rows.Next() {
		var account OpenAIAccount
		var expiresAt sql.NullTime
		var refreshError sql.NullString
		var statusRaw string
		if err := rows.Scan(&account.ID, &account.Name, &account.Email, &account.AccountID, &account.PlanType, &account.AuthJSON, &expiresAt, &refreshError,
			&account.SelectedModel, &account.SelectedReasoningEffort, &account.SelectedServiceTier, &statusRaw); err != nil {
			return nil, err
		}
		account.Enabled = true
		account.QuotaExhausted = quotaExhausted(statusRaw)
		if expiresAt.Valid {
			account.TokenExpiresAt = expiresAt.Time
		}
		if refreshError.Valid {
			account.RefreshError = refreshError.String
		}
		out = append(out, account)
	}
	return out, rows.Err()
}

func (s *Store) queryOpenAIAccount(ctx context.Context, where string, args ...any) (OpenAIAccount, bool, error) {
	var account OpenAIAccount
	var expiresAt sql.NullTime
	var refreshError sql.NullString
	query := `SELECT id, name, email, account_id, plan_type, auth_json, token_expires_at, refresh_error,
		selected_model, selected_reasoning_effort, selected_service_tier
		FROM openai_accounts ` + where + ` ORDER BY id LIMIT 1`
	if where == "" {
		args = nil
	}
	err := s.db.QueryRowContext(ctx, query, args...).
		Scan(&account.ID, &account.Name, &account.Email, &account.AccountID, &account.PlanType, &account.AuthJSON, &expiresAt, &refreshError,
			&account.SelectedModel, &account.SelectedReasoningEffort, &account.SelectedServiceTier)
	if errors.Is(err, sql.ErrNoRows) {
		return account, false, nil
	}
	if expiresAt.Valid {
		account.TokenExpiresAt = expiresAt.Time
	}
	if refreshError.Valid {
		account.RefreshError = refreshError.String
	}
	if err != nil {
		return account, false, err
	}
	return account, true, nil
}

func (s *Store) CreateAPIKey(ctx context.Context, in APIKey) (int64, error) {
	if err := validateAPIKey(&in); err != nil {
		return 0, err
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_keys (name, api_key, created_at, updated_at)
		VALUES (?, ?, ?, ?)`, in.Name, in.APIKey, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateAPIKey(ctx context.Context, id int64, in APIKey) error {
	if err := validateAPIKey(&in); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET name=?, api_key=?, updated_at=? WHERE id=?`,
		in.Name, in.APIKey, time.Now(), id)
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

func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id=?`, id)
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

// validateAPIKey 归一化并校验:API Key 必填,名称可选,两者都有长度上限。
func validateAPIKey(in *APIKey) error {
	in.Name = strings.TrimSpace(in.Name)
	in.APIKey = strings.TrimSpace(in.APIKey)
	if in.APIKey == "" {
		return errors.New("API Key 不能为空")
	}
	if len(in.Name) > 128 {
		return errors.New("名称过长")
	}
	if len(in.APIKey) > 512 {
		return errors.New("API Key 过长")
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

// decodeModelsJSON 把缓存的模型目录还原成结构化值,供 API 直接返回给页面。
func decodeModelsJSON(raw sql.NullString) any {
	if !raw.Valid || raw.String == "" || raw.String == "null" || !json.Valid([]byte(raw.String)) {
		return nil
	}
	var value any
	if json.Unmarshal([]byte(raw.String), &value) != nil {
		return nil
	}
	return value
}

func nullJSON(raw []byte) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

// OutlookAccount 是 OUTLOOK 页面展示用的一行 outlook_login_tokens 记录。
// 出于安全考虑,列表接口不返回 access_token / refresh_token / id_token / cookies 等敏感明文,
// 只返回过期时间、状态等元数据(与 OPENAI 页面 API 不返回 token 的约定一致)。
type OutlookAccount struct {
	ID                    int64     `json:"id"`
	Email                 string    `json:"email"`
	DisplayName           string    `json:"display_name"`
	Tags                  string    `json:"tags"`
	TenantID              string    `json:"tenant_id"`
	Scope                 string    `json:"scope"`
	TokenType             string    `json:"token_type"`
	AccessTokenLen        int       `json:"access_token_len"`
	RefreshTokenLen       int       `json:"refresh_token_len"`
	ExpiresIn             int64     `json:"expires_in"`
	RefreshTokenExpiresIn int64     `json:"refresh_token_expires_in"`
	CookieCount           int64     `json:"cookie_count"`
	TokenIssuedAt         time.Time `json:"token_issued_at"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
	LastRefreshStatus     string    `json:"last_refresh_status"`
	LastRefreshError      string    `json:"last_refresh_error"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	// HasCodexAccount 表示这个 Outlook 邮箱是否已经在 OPENAI/Codex 里登录过账号。
	// 底层沿用历史列 outlook_login_tokens.has_gpt_account。
	HasCodexAccount bool `json:"has_codex_account"`
	// HasGPTAccount 保留给旧前端/旧调用方兼容,含义同 HasCodexAccount。
	HasGPTAccount bool `json:"has_gpt_account"`
	// RegisteredGPTAccount 是「是否已完成 ChatGPT 注册」标记,不在页面展示,只控制注册按钮。
	RegisteredGPTAccount bool `json:"registered_gpt_account"`
	// HasPassword 表示这行存了明文登录密码。只返回「有没有」,不返回密码本身,
	// 供页面决定行内「登录」按钮能不能点(自动登录需要密码)。
	HasPassword bool `json:"has_password"`
}

// ListOutlookAccounts 列出所有已登录的 Outlook 账号(不含任何 token/cookie 明文)。
// Codex 登录标记以历史列 has_gpt_account 为准;该列为 0 的行才按邮箱关联 openai_accounts
// 复算一次。注册标记 registered_gpt_account 独立维护;Codex 已登录时会顺手视为已注册并回写。
func (s *Store) ListOutlookAccounts(ctx context.Context) ([]OutlookAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.email, COALESCE(t.display_name,''), COALESCE(t.tags,''), COALESCE(t.tenant_id,''), COALESCE(t.scope,''),
		COALESCE(t.token_type,''), COALESCE(CHAR_LENGTH(t.access_token),0), COALESCE(CHAR_LENGTH(t.refresh_token),0),
		COALESCE(t.expires_in,0), COALESCE(t.refresh_token_expires_in,0), COALESCE(t.cookie_count,0),
		t.token_issued_at, t.access_token_expires_at, t.refresh_token_expires_at,
		COALESCE(t.last_refresh_status,''), COALESCE(t.last_refresh_error,''), t.created_at, t.updated_at,
		t.has_gpt_account, t.registered_gpt_account,
		CASE WHEN t.has_gpt_account=1 THEN 1
			ELSE EXISTS(SELECT 1 FROM openai_accounts oa WHERE oa.email = t.email AND oa.email <> '') END,
		CHAR_LENGTH(COALESCE(t.password,'')) > 0
		FROM outlook_login_tokens t ORDER BY t.created_at DESC, t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]OutlookAccount, 0)
	var codexBackfill []int64      // has_gpt_account 列里是 0、但刚关联到 Codex 账号的行
	var registeredBackfill []int64 // 注册列是 0、但已确认注册/Codex 已登录的行
	for rows.Next() {
		var a OutlookAccount
		var issuedAt, accessExp, refreshExp sql.NullTime
		var storedCodex, storedRegistered, hasCodex, hasPassword int64
		if err := rows.Scan(&a.ID, &a.Email, &a.DisplayName, &a.Tags, &a.TenantID, &a.Scope,
			&a.TokenType, &a.AccessTokenLen, &a.RefreshTokenLen,
			&a.ExpiresIn, &a.RefreshTokenExpiresIn, &a.CookieCount,
			&issuedAt, &accessExp, &refreshExp,
			&a.LastRefreshStatus, &a.LastRefreshError, &a.CreatedAt, &a.UpdatedAt, &storedCodex, &storedRegistered, &hasCodex, &hasPassword); err != nil {
			return nil, err
		}
		a.HasCodexAccount = hasCodex != 0
		a.HasGPTAccount = a.HasCodexAccount
		a.RegisteredGPTAccount = storedRegistered != 0 || a.HasCodexAccount
		a.HasPassword = hasPassword != 0
		if storedCodex == 0 && a.HasCodexAccount {
			codexBackfill = append(codexBackfill, a.ID)
		}
		if storedRegistered == 0 && a.RegisteredGPTAccount {
			registeredBackfill = append(registeredBackfill, a.ID)
		}
		if issuedAt.Valid {
			a.TokenIssuedAt = issuedAt.Time
		}
		if accessExp.Valid {
			a.AccessTokenExpiresAt = accessExp.Time
		}
		if refreshExp.Valid {
			a.RefreshTokenExpiresAt = refreshExp.Time
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := s.markOutlookHasCodex(ctx, codexBackfill); err != nil {
		return nil, err
	}
	if err := s.MarkOutlookRegisteredGPT(ctx, registeredBackfill); err != nil {
		return nil, err
	}
	return out, nil
}

// markOutlookHasCodex 把这些行的 has_gpt_account(历史字段)刷成 1。显式写 updated_at=updated_at,
// 避免 ON UPDATE CURRENT_TIMESTAMP 把「更新时间」刷成这次纯内部回写的时刻。
func (s *Store) markOutlookHasCodex(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE outlook_login_tokens SET has_gpt_account=1, updated_at=updated_at
		WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	return err
}

// MarkOutlookRegisteredGPT 把这些 Outlook 行标记为“已注册 ChatGPT”。
func (s *Store) MarkOutlookRegisteredGPT(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE outlook_login_tokens SET registered_gpt_account=1, updated_at=updated_at
		WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	return err
}

// MarkOutlookRegisteredGPTByEmail 按邮箱标记为“已注册 ChatGPT”;注册自动化成功后调用。
func (s *Store) MarkOutlookRegisteredGPTByEmail(ctx context.Context, email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE outlook_login_tokens SET registered_gpt_account=1, updated_at=updated_at WHERE email=?`, email)
	return err
}

// ListOutlookRefreshableIDs 列出「有 access token」的账号主键,供一键刷新全部 Token 用。
// 顺序与列表页一致(最近创建在前),便于前端进度和页面顺序对得上。
func (s *Store) ListOutlookRefreshableIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM outlook_login_tokens
		WHERE COALESCE(access_token,'') <> '' ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetOutlookAccountEmail 按主键取邮箱,供刷新时定位 skill 要刷的账号。
func (s *Store) GetOutlookAccountEmail(ctx context.Context, id int64) (string, error) {
	var email string
	err := s.db.QueryRowContext(ctx, `SELECT email FROM outlook_login_tokens WHERE id=?`, id).Scan(&email)
	return email, err
}

// GetOutlookAccountByID 取单行元数据(刷新完成后回读返回给前端)。
func (s *Store) GetOutlookAccountByID(ctx context.Context, id int64) (OutlookAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, email, COALESCE(display_name,''), COALESCE(tags,''), COALESCE(tenant_id,''), COALESCE(scope,''),
		COALESCE(token_type,''), COALESCE(CHAR_LENGTH(access_token),0), COALESCE(CHAR_LENGTH(refresh_token),0),
		COALESCE(expires_in,0), COALESCE(refresh_token_expires_in,0), COALESCE(cookie_count,0),
		token_issued_at, access_token_expires_at, refresh_token_expires_at,
		COALESCE(last_refresh_status,''), COALESCE(last_refresh_error,''), created_at, updated_at, has_gpt_account, registered_gpt_account,
		CHAR_LENGTH(COALESCE(password,'')) > 0
		FROM outlook_login_tokens WHERE id=?`, id)
	if err != nil {
		return OutlookAccount{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return OutlookAccount{}, err
		}
		return OutlookAccount{}, sql.ErrNoRows
	}
	var a OutlookAccount
	var issuedAt, accessExp, refreshExp sql.NullTime
	var hasCodex, registeredGPT, hasPassword int64
	if err := rows.Scan(&a.ID, &a.Email, &a.DisplayName, &a.Tags, &a.TenantID, &a.Scope,
		&a.TokenType, &a.AccessTokenLen, &a.RefreshTokenLen,
		&a.ExpiresIn, &a.RefreshTokenExpiresIn, &a.CookieCount,
		&issuedAt, &accessExp, &refreshExp,
		&a.LastRefreshStatus, &a.LastRefreshError, &a.CreatedAt, &a.UpdatedAt, &hasCodex, &registeredGPT, &hasPassword); err != nil {
		return OutlookAccount{}, err
	}
	a.HasCodexAccount = hasCodex != 0
	a.HasGPTAccount = a.HasCodexAccount
	a.RegisteredGPTAccount = registeredGPT != 0 || a.HasCodexAccount
	a.HasPassword = hasPassword != 0
	if issuedAt.Valid {
		a.TokenIssuedAt = issuedAt.Time
	}
	if accessExp.Valid {
		a.AccessTokenExpiresAt = accessExp.Time
	}
	if refreshExp.Valid {
		a.RefreshTokenExpiresAt = refreshExp.Time
	}
	return a, nil
}

// DeleteOutlookAccount 删除一条 Outlook 登录记录(含其 token/cookie)。
func (s *Store) DeleteOutlookAccount(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM outlook_login_tokens WHERE id=?`, id)
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

// ClearOutlookTokens 登出:清掉这行的 token 与会话相关字段,让账号回到「需重新登录」状态。
// 保留 email/密码/标签等静态信息,这样清完后 access_token_len=0,页面上「登录」按钮会重新出现。
// cookies 一并清掉,避免登出后还能用旧会话静默刷回来。
func (s *Store) ClearOutlookTokens(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE outlook_login_tokens SET
			access_token=NULL, refresh_token=NULL, id_token=NULL,
			expires_in=NULL, ext_expires_in=NULL, refresh_token_expires_in=NULL,
			access_token_expires_at=NULL, refresh_token_expires_at=NULL,
			cookies_json=NULL, cookie_count=0,
			last_refresh_status='logged_out', last_refresh_error=NULL
		WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetOutlookRefreshInput 取免登录刷新所需的输入:邮箱、已保存的 cookie(JSON)、User-Agent、scope。
func (s *Store) GetOutlookRefreshInput(ctx context.Context, id int64) (email, cookiesJSON, userAgent, scope string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT email, COALESCE(cookies_json,''), COALESCE(user_agent,''), COALESCE(scope,'')
		FROM outlook_login_tokens WHERE id=?`, id).Scan(&email, &cookiesJSON, &userAgent, &scope)
	return
}

// outlookTokenUpdate 是刷新成功后要写回 outlook_login_tokens 的一组字段。
// 指针字段为 nil 时写 NULL;display_name/tenant_id/account_oid/home_account_id 为空时保留原值。
type outlookTokenUpdate struct {
	TokenType             string
	Scope                 string
	AccessToken           string
	RefreshToken          string
	IDToken               string
	ClientInfo            string
	UserAgent             string
	DisplayName           string
	TenantID              string
	AccountOID            string
	HomeAccountID         string
	ExpiresIn             *int64
	ExtExpiresIn          *int64
	RefreshTokenExpiresIn *int64
	TokenIssuedAt         time.Time
	AccessTokenExpiresAt  time.Time
	RefreshTokenExpiresAt *time.Time
	CookiesJSON           string
	CookieCount           int
}

// sqlExecer 让同一段 SQL 既能走 *sql.DB 也能走事务里的 *sql.Tx。
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// UpdateOutlookTokens 把刷新拿到的新 token 与续期后的 cookie 写回指定行。
func (s *Store) UpdateOutlookTokens(ctx context.Context, id int64, u outlookTokenUpdate) error {
	return updateOutlookTokenRow(ctx, s.db, id, u, "refreshed")
}

// updateOutlookTokenRow 把一组 token 字段写到指定行。status 落 last_refresh_status:
// 静默刷新是 refreshed,手动登录抓取是 login_captured(与 skill 写入的取值保持一致)。
func updateOutlookTokenRow(ctx context.Context, ex sqlExecer, id int64, u outlookTokenUpdate, status string) error {
	res, err := ex.ExecContext(ctx, `UPDATE outlook_login_tokens SET
			token_type=?, scope=?, access_token=?, refresh_token=?, id_token=?, client_info=?,
			expires_in=?, ext_expires_in=?,
			refresh_token_expires_in=COALESCE(?, refresh_token_expires_in),
			token_issued_at=?, access_token_expires_at=?,
			refresh_token_expires_at=COALESCE(?, refresh_token_expires_at),
			cookies_json=?, cookie_count=?, user_agent=?,
			display_name=IF(?='', display_name, ?),
			tenant_id=IF(?='', tenant_id, ?),
			account_oid=IF(?='', account_oid, ?),
			home_account_id=IF(?='', home_account_id, ?),
			last_refresh_status=?, last_refresh_error=NULL
		WHERE id=?`,
		u.TokenType, u.Scope, u.AccessToken, u.RefreshToken, u.IDToken, u.ClientInfo,
		nullInt(u.ExpiresIn), nullInt(u.ExtExpiresIn), nullInt(u.RefreshTokenExpiresIn),
		u.TokenIssuedAt, u.AccessTokenExpiresAt, nullTime(u.RefreshTokenExpiresAt),
		u.CookiesJSON, u.CookieCount, u.UserAgent,
		u.DisplayName, u.DisplayName,
		u.TenantID, u.TenantID,
		u.AccountOID, u.AccountOID,
		u.HomeAccountID, u.HomeAccountID,
		status, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SaveOutlookLogin 保存一次「页面手动登录」抓到的凭证,并决定写到哪一行:
//
//  1. 同邮箱 + 同 client_id 的行 —— 同一账号重新登录,原地更新;
//  2. 同邮箱、但还没有 access_token 的行 —— 认领「新增账号」建的空行。这类行的
//     client_id/scope 可能是空/NULL,和唯一键对不上,直接 INSERT 会让同一邮箱出现两行,
//     所以这里改成更新它并把 client_id/scope 补成规范值;
//  3. 都没有就插一行新的。
//
// password 只在非空时覆盖:手动登录时弹窗里的密码是选填的,没填不该清掉已存的。
func (s *Store) SaveOutlookLogin(ctx context.Context, email, password string, u outlookTokenUpdate) (int64, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return 0, errors.New("没有解析出登录邮箱，请在弹窗里手动填写邮箱后重试")
	}
	if len(email) > 320 {
		return 0, errors.New("邮箱过长")
	}
	if len(password) > 512 {
		return 0, errors.New("密码过长")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM outlook_login_tokens
		WHERE email=? AND client_id=? ORDER BY id LIMIT 1`, email, outlookClientID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM outlook_login_tokens
			WHERE email=? AND COALESCE(access_token,'')='' ORDER BY id LIMIT 1`, email).Scan(&id)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		now := time.Now()
		res, insertErr := tx.ExecContext(ctx, `INSERT INTO outlook_login_tokens
			(email, password, client_id, scope, cookie_count, created_at, updated_at)
			VALUES (?, ?, ?, ?, 0, ?, ?)`,
			email, password, outlookClientID, outlookScopeStored, now, now)
		if insertErr != nil {
			return 0, insertErr
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE outlook_login_tokens
			SET client_id=?, password=IF(?='', password, ?) WHERE id=?`,
			outlookClientID, password, password, id); err != nil {
			return 0, err
		}
	}
	if err := updateOutlookTokenRow(ctx, tx, id, u, "login_captured"); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// MarkOutlookRefreshError 记录一次刷新失败的原因(供页面显示「失败」并提示重新登录)。
func (s *Store) MarkOutlookRefreshError(ctx context.Context, id int64, msg string) error {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE outlook_login_tokens SET last_refresh_status='error', last_refresh_error=? WHERE id=?`, msg, id)
	return err
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return *v
}

// CreateOutlookAccount 手动新增一个待登录的 Outlook 账号:只存邮箱 + 明文密码。
// client_id/scope 与 skill 登录后写入的保持一致(uniq_email_client_scope),
// 这样之后 skill 用同一邮箱登录时命中同一行、补上 token/cookie,而不会新建重复行、也不会覆盖密码。
func (s *Store) CreateOutlookAccount(ctx context.Context, email, password string) (int64, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return 0, errors.New("邮箱不能为空")
	}
	if len(email) > 320 {
		return 0, errors.New("邮箱过长")
	}
	if len(password) > 512 {
		return 0, errors.New("密码过长")
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO outlook_login_tokens
			(email, password, client_id, scope, cookie_count, created_at, updated_at)
			VALUES (?, ?, ?, ?, 0, ?, ?)`,
		email, password, outlookClientID, outlookScopeStored, now, now)
	if err != nil {
		if isDuplicateKeyErr(err) {
			return 0, errors.New("该邮箱已存在")
		}
		return 0, err
	}
	return res.LastInsertId()
}

// GetOutlookCredentials 取某行的邮箱 + 明文密码(仅供编辑弹窗回填,不进列表接口)。
func (s *Store) GetOutlookCredentials(ctx context.Context, id int64) (email, password string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT email, COALESCE(password,'') FROM outlook_login_tokens WHERE id=?`, id).Scan(&email, &password)
	return
}

// UpdateOutlookCredentials 修改某行的邮箱与明文密码。
func (s *Store) UpdateOutlookCredentials(ctx context.Context, id int64, email, password string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("邮箱不能为空")
	}
	if len(email) > 320 {
		return errors.New("邮箱过长")
	}
	if len(password) > 512 {
		return errors.New("密码过长")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE outlook_login_tokens SET email=?, password=?, updated_at=? WHERE id=?`,
		email, password, time.Now(), id)
	if err != nil {
		if isDuplicateKeyErr(err) {
			return errors.New("该邮箱已存在")
		}
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// outlookScopeStored 是 skill 登录后实际写入 scope 列的值(token 响应返回的 scope),
// 手动新增时对齐它,保证 uniq_email_client_scope 命中同一行。
const outlookScopeStored = "service::outlook.office.com::MBI_SSL"

func isDuplicateKeyErr(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// GetOutlookAccessToken 取某行的 access_token 与邮箱,用于构造 Outlook Web 邮件 API 的
// MSAuth1.0 鉴权头(usertoken=access_token)与 x-anchormailbox。
func (s *Store) GetOutlookAccessToken(ctx context.Context, id int64) (accessToken, email string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(access_token,''), email FROM outlook_login_tokens WHERE id=?`, id).Scan(&accessToken, &email)
	return
}

// GetOutlookIDByEmail 按邮箱定位账号主键(取最近更新的一条),供「按邮箱取验证码」接口用。
func (s *Store) GetOutlookIDByEmail(ctx context.Context, email string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM outlook_login_tokens WHERE email=? ORDER BY updated_at DESC, id DESC LIMIT 1`, email).Scan(&id)
	return id, err
}
