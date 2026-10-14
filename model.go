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
	RequestTimeoutSeconds int      `json:"request_timeout_seconds"`
	LogRetentionDays      int      `json:"log_retention_days"`
}

// Account 的登录凭据仅用于访问 Adobe，列表接口会移除敏感字段。
type Account struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Email          string    `json:"email"`
	UserID         string    `json:"user_id,omitempty"`
	Enabled        bool      `json:"enabled"`
	Cookie         string    `json:"cookie,omitempty"`
	AccessToken    string    `json:"access_token,omitempty"`
	RefreshToken   string    `json:"refresh_token,omitempty"`
	ClientID       string    `json:"client_id,omitempty"`
	APIKey         string    `json:"api_key,omitempty"`
	ARPSession     string    `json:"arp_session,omitempty"`
	Nonce          string    `json:"nonce,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	LastRefresh    time.Time `json:"last_refresh"`
	Status         string    `json:"status"`
	LastError      string    `json:"last_error"`
	Credits        *float64  `json:"credits"`
	BrowserProfile string    `json:"browser_profile,omitempty"`
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
}

type UpstreamRecord struct {
	Method     string `json:"method"`
	URL        string `json:"url"`
	Status     int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	RequestID  string `json:"request_id,omitempty"`
}

type Model struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	AdobeID     string   `json:"adobe_id"`
	Version     string   `json:"version"`
	Ratios      []string `json:"ratios"`
	Resolutions []string `json:"resolutions"`
}

type GenerateInput struct {
	Model       string        `json:"model"`
	Prompt      string        `json:"prompt"`
	N           int           `json:"n"`
	Size        string        `json:"size"`
	AspectRatio string        `json:"aspect_ratio"`
	Resolution  string        `json:"resolution"`
	Quality     string        `json:"quality"`
	References  []ImageRecord `json:"references"`
}

type GenerateResult struct {
	Images  []ImageRecord
	Credits *float64
}

// Generator 将协议转换和上游生成分开，便于验证协议与真实请求行为。
type Generator interface {
	Generate(context.Context, Account, GenerateInput, *RequestLog) (GenerateResult, error)
	Refresh(context.Context, Account) (Account, error)
}
