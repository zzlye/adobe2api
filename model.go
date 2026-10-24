package main

import (
	"context"
	"encoding/json"
	"time"
)

// Settings 保存运行设置，密码摘要不会通过管理接口返回。
type Settings struct {
	PublicURL             string   `json:"public_url"`
	ProxyURL              string   `json:"proxy_url"`
	APIKeys               []string `json:"api_keys"`
	AdminUsername         string   `json:"admin_username"`
	AdminPasswordHash     string   `json:"-"`
	RefreshMinutes        int      `json:"refresh_minutes"`
	RetentionHours        int      `json:"retention_hours"`
	MaxStorageMB          int64    `json:"max_storage_mb"`
	MaxConcurrent         int      `json:"max_concurrent"`
	AccountStrategy       string   `json:"account_strategy"`
	CreditUsage           string   `json:"credit_usage"`
	RequestTimeoutSeconds int      `json:"request_timeout_seconds"`
	LogRetentionDays      int      `json:"log_retention_days"`
}

// Account 的登录凭据仅用于访问 Adobe，列表接口会移除敏感字段。
type Account struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	UserID       string `json:"user_id,omitempty"`
	Enabled      bool   `json:"enabled"`
	Cookie       string `json:"cookie,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	ARPSession   string `json:"arp_session,omitempty"`
	// 风险恢复意图独立于令牌有效期保存，防止旧 Cookie 回填后跳过浏览器重建。
	RiskRefreshRequired bool      `json:"risk_refresh_required,omitempty"`
	Nonce               string    `json:"nonce,omitempty"`
	ExpiresAt           time.Time `json:"expires_at"`
	LastRefresh         time.Time `json:"last_refresh"`
	Status              string    `json:"status"`
	LastError           string    `json:"last_error"`
	Credits             *float64  `json:"credits"`
	CreditsTotal        *float64  `json:"credits_total,omitempty"`
	Plan                string    `json:"plan,omitempty"`
	RequestsPerMinute   int       `json:"requests_per_minute"`
	BrowserProfile      string    `json:"browser_profile,omitempty"`
}

// ImageRecord 只保存文件元信息，图片本体按需从磁盘读取。
type ImageRecord struct {
	ID        string    `json:"id"`
	Path      string    `json:"-"`
	URL       string    `json:"url"`
	MIME      string    `json:"mime_type"`
	Bytes     int64     `json:"bytes"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	Deleted   bool      `json:"deleted"`
}

// RequestLog 保留请求参数、积分和图片索引，不记录认证信息或图片编码。
type RequestLog struct {
	ID          string           `json:"id"`
	CreatedAt   time.Time        `json:"created_at"`
	FinishedAt  time.Time        `json:"finished_at"`
	DurationMS  int64            `json:"duration_ms"`
	Protocol    string           `json:"protocol"`
	Endpoint    string           `json:"endpoint"`
	AccountID   string           `json:"account_id"`
	AccountName string           `json:"account_name"`
	Model       string           `json:"model"`
	Prompt      string           `json:"prompt"`
	Status      string           `json:"status"`
	HTTPStatus  int              `json:"http_status"`
	Error       string           `json:"error"`
	Credits     *float64         `json:"credits"`
	Parameters  json.RawMessage  `json:"parameters"`
	Images      []ImageRecord    `json:"images"`
	References  []ImageRecord    `json:"references"`
	Upstream    []UpstreamRecord `json:"upstream"`
	// 详情预算只用于当前请求的内存控制，不进入持久化或管理接口。
	upstreamDetailBytes       int
	upstreamDetailInitialized bool
}

type UpstreamRecord struct {
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Status          int               `json:"status"`
	DurationMS      int64             `json:"duration_ms"`
	RequestID       string            `json:"request_id,omitempty"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	RequestBody     string            `json:"request_body,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	ResponseBody    string            `json:"response_body,omitempty"`
}

type Model struct {
	ID                          string   `json:"id"`
	Name                        string   `json:"name"`
	AdobeID                     string   `json:"adobe_id"`
	Version                     string   `json:"version"`
	Ratios                      []string `json:"ratios"`
	Resolutions                 []string `json:"resolutions"`
	HighResolutionRequiresRatio bool     `json:"high_resolution_requires_ratio,omitempty"`
	Qualities                   []string `json:"qualities"`
	MaxDetailLevel              int      `json:"max_detail_level,omitempty"`
	SupportsTransparency        bool     `json:"supports_transparency"`
	SupportsCustomSize          bool     `json:"supports_custom_size"`
	SupportsGroundSearch        bool     `json:"supports_ground_search"`
	SupportsSeed                bool     `json:"supports_seed"`
	SupportsMask                bool     `json:"supports_mask"`
	MaxReferences               int      `json:"max_references"`
	MaxReferenceBytes           int64    `json:"max_reference_bytes"`
	ReferenceUsages             []string `json:"reference_usages"`
}

type GenerateInput struct {
	Model                 string          `json:"model"`
	Prompt                string          `json:"prompt"`
	N                     int             `json:"n"`
	Size                  string          `json:"size"`
	AspectRatio           string          `json:"aspect_ratio"`
	Resolution            string          `json:"resolution"`
	Quality               string          `json:"quality"`
	References            []ImageRecord   `json:"references"`
	ReferenceUsages       []string        `json:"reference_usages,omitempty"`
	Background            string          `json:"background,omitempty"`
	TransparentBackground *bool           `json:"transparent_background,omitempty"`
	DetailLevel           *int            `json:"detail_level,omitempty"`
	Seed                  *int64          `json:"seed,omitempty"`
	Seeds                 []int64         `json:"seeds,omitempty"`
	GroundSearch          *bool           `json:"ground_search,omitempty"`
	Mask                  json.RawMessage `json:"mask,omitempty"`
	MaskReference         *ImageRecord    `json:"mask_reference,omitempty"`
	ModelSpecificPayload  map[string]any  `json:"model_specific_payload,omitempty"`
	Output                *AdobeOutput    `json:"output,omitempty"`
}

// AdobeOutput 仅暴露模型目录声明的输出保存选项。
type AdobeOutput struct {
	StoreInputs  bool `json:"storeInputs"`
	TargetFolder *struct {
		CreativeCloudProjectID string `json:"creativeCloudProjectId,omitempty"`
		CreativeCloudPath      string `json:"creativeCloudPath,omitempty"`
	} `json:"targetFolder,omitempty"`
}

type GenerateResult struct {
	Images              []ImageRecord
	Credits             *float64
	AccountCredits      *float64
	AccountCreditsTotal *float64
	AccountPlan         string
}

// Generator 将协议转换和上游生成分开，便于验证协议与真实请求行为。
type Generator interface {
	Generate(context.Context, Account, GenerateInput, *RequestLog) (GenerateResult, error)
	Refresh(context.Context, Account) (Account, error)
}
