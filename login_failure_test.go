package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 复现 IMS 明确拒绝登录后，浏览器未登录超时掩盖原始认证错误的真实刷新链。
func TestLoginFailurePreservesIMSRejection(t *testing.T) {
	g := newAdobeTestGenerator(t, func(*http.Request) (*http.Response, error) {
		return adobeTestResponse(200, nil, `{"error":"invalid_credentials"}`), nil
	})
	g.browserRefresh = func(_ context.Context, a Account) (Account, error) {
		return a, errors.New("Adobe 登录会话尚未就绪")
	}
	_, err := g.Refresh(context.Background(), Account{Cookie: "ims_sid=revoked", UserID: "test-user"})
	var rejection *UpstreamError
	if !errors.As(err, &rejection) || rejection.Code != "invalid_credentials" || !strings.Contains(err.Error(), "尚未就绪") {
		t.Fatalf("浏览器失败丢失了 IMS 明确认证拒绝: %v", err)
	}
}

// 从正式路由连续请求，已确认失效的账号必须退出自动分配，健康账号继续接单。
func TestLoginFailureStopsRepeatedAutomaticRefresh(t *testing.T) {
	app, bad := sessionIntegrationApp(t)
	bad.Status, bad.AccessToken = "expired", "revoked-token"
	if err := app.store.SaveAccount(bad); err != nil {
		t.Fatal(err)
	}
	hits := 0
	app.generator = &sessionIntegrationGenerator{
		refresh: func(_ context.Context, a Account) (Account, error) {
			hits++
			return a, &UpstreamError{StatusCode: http.StatusUnauthorized, Code: "invalid_credentials", Message: "Adobe 登录状态已失效"}
		},
		generate: func(_ context.Context, _ Account, _ GenerateInput, _ *RequestLog) (GenerateResult, error) {
			return GenerateResult{}, nil
		},
	}
	first := protocolRequest(app, http.MethodPost, "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "失效账号"})
	if first.Code != http.StatusServiceUnavailable || hits != 1 {
		t.Fatal("未触发认证失败现场")
	}
	// 模拟认证失败前已预约的另一条请求，等待会话门后不能再触发续签。
	if _, err := app.refreshGenerationAccount(context.Background(), bad, &UpstreamError{StatusCode: http.StatusUnauthorized}); err == nil || hits != 1 {
		t.Fatal("已排队请求重复续签失效凭据")
	}
	good := bad
	good.ID, good.Name, good.Status, good.AccessToken = "healthy-account", "可用账号", "ready", "healthy-token"
	good.LastRefresh, good.ExpiresAt = time.Now(), time.Now().Add(time.Hour)
	if err := app.store.SaveAccount(good); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		r := protocolRequest(app, http.MethodPost, "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "健康账号继续接单"})
		if r.Code != http.StatusOK || hits != 1 {
			t.Fatalf("已失效账号仍被反复续签: status=%d refresh=%d", r.Code, hits)
		}
	}
	// 后台维护也不能重复使用已被撤销的 Cookie。
	current, _ := app.findAdminAccount(bad.ID)
	current.LastRefresh = time.Now().Add(-24 * time.Hour)
	if err := app.store.SaveAccount(current); err != nil {
		t.Fatal(err)
	}
	app.refreshAccounts(context.Background())
	if hits != 1 {
		t.Fatal("后台维护重新使用已失效凭据")
	}
}

// 管理员更新 Cookie 或显式刷新成功后，应恢复原账号，保留名称、开关及统计身份。
func TestLoginFailureNewSessionCanRecover(t *testing.T) {
	app, handler, cookie, g := cookieAdminTestApp(t)
	a := Account{ID: "expired-account", Name: "原账号", Enabled: true, Cookie: "ims_sid=revoked", UserID: "adobe-user-fixture", Status: "expired", LoginRequired: true}
	if err := app.store.SaveAccount(a); err != nil {
		t.Fatal(err)
	}
	r := adminTestRequest(handler, http.MethodPut, "/api/admin/accounts/"+a.ID, `{"cookie":"ims_sid=new-login"}`, cookie, "")
	current, _ := app.findAdminAccount(a.ID)
	if r.Code != http.StatusOK || current.LoginRequired || current.Status != "pending" {
		t.Fatal("新 Cookie 未解除自动分配暂停")
	}
	current.LoginRequired = true
	if err := app.store.SaveAccount(current); err != nil {
		t.Fatal(err)
	}
	r = adminTestRequest(handler, http.MethodPost, "/api/admin/accounts/"+a.ID+"/refresh", `{}`, cookie, "")
	current, _ = app.findAdminAccount(a.ID)
	if r.Code != http.StatusOK || current.LoginRequired || g.refreshHits != 1 || current.Name != a.Name || !current.Enabled {
		t.Fatal("手动成功续签未恢复原账号")
	}
}

// 旧任务收尾不能撤销已确认的登录失效；临时错误则不能被当作需要重新登录。
func TestLoginFailureKeepsNewStateAndAllowsTransientRecovery(t *testing.T) {
	for _, failure := range []error{
		&UpstreamError{StatusCode: http.StatusUnauthorized, Code: "invalid_credentials", Message: "凭据失效"},
		&UpstreamError{StatusCode: http.StatusUnauthorized, Code: "invalid_access_token", Message: "令牌需续签"},
		&UpstreamError{StatusCode: http.StatusRequestTimeout, Code: "timeout_error", Message: "上游繁忙"},
		context.DeadlineExceeded,
	} {
		app, old := sessionIntegrationApp(t)
		app.generator = &sessionIntegrationGenerator{refresh: func(_ context.Context, a Account) (Account, error) { return a, failure }}
		_, err := app.refreshGenerationAccount(context.Background(), old, &UpstreamError{StatusCode: http.StatusUnauthorized})
		if err == nil {
			t.Fatal("没有复现续签失败")
		}
		old.Status = "ready"
		if err := app.saveGenerationAccount(old); err != nil {
			t.Fatal(err)
		}
		current, _ := app.findAdminAccount(old.ID)
		if current.LoginRequired != adobeLoginRequired(failure) {
			t.Fatalf("认证拒绝与临时故障分类错误: %v", failure)
		}
	}
}
