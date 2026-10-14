package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type cookieAdminGenerator struct {
	store       *Store
	session     Account
	refreshErr  error
	refreshHits int
	generateHit int
	image       []byte
}

func (g *cookieAdminGenerator) Refresh(_ context.Context, account Account) (Account, error) {
	g.refreshHits++
	if g.refreshErr != nil {
		return account, g.refreshErr
	}
	updated := g.session
	updated.ID, updated.Enabled, updated.Name = account.ID, account.Enabled, account.Name
	if account.Cookie != "" {
		updated.Cookie = account.Cookie + "; refreshed=fixture"
		updated.AccessToken = "ims-access-session-fixture"
		if account.ARPSession != "" {
			updated.ARPSession = account.ARPSession
		}
	}
	return updated, nil
}

func (g *cookieAdminGenerator) Generate(_ context.Context, account Account, _ GenerateInput, _ *RequestLog) (GenerateResult, error) {
	g.generateHit++
	if account.AccessToken == "" || account.ARPSession == "" || account.UserID == "" || !strings.Contains(account.Cookie, "refreshed=fixture") {
		return GenerateResult{}, errors.New("生成没有使用刷新后的完整凭据")
	}
	image, err := g.store.SaveImage(bytes.NewReader(g.image), "image/png", "generated")
	return GenerateResult{Images: []ImageRecord{image}}, err
}

// 使用正常管理登录和真实路由验证导入、续期与协议生成，测试凭据只留在临时库中。
func cookieAdminTestApp(t *testing.T) (*App, http.Handler, *http.Cookie, *cookieAdminGenerator) {
	t.Helper()
	store := newStorageTestStore(t)
	generator := &cookieAdminGenerator{store: store, session: Account{Cookie: "ims_sid=session-fixture", AccessToken: "access-session-fixture", ARPSession: "arp-session-fixture", UserID: "adobe-user-fixture", Email: "user@example.test", Status: "unverified", ExpiresAt: time.Now().Add(time.Hour)}}
	app := &App{store: store, generator: generator}
	handler := app.Handler()
	response := adminTestRequest(handler, "POST", "/api/auth/setup", `{"username":"admin","password":"测试密码abcdefghij"}`, nil, "")
	if response.Code != http.StatusOK {
		t.Fatal("测试管理员创建失败")
	}
	return app, handler, response.Result().Cookies()[0], generator
}

func TestCookieOnlyImportRefreshesBeforeProtocolGeneration(t *testing.T) {
	app, handler, cookie, generator := cookieAdminTestApp(t)
	generator.image = storageTestPNG(t)
	response := adminTestRequest(handler, "POST", "/api/admin/accounts/import", `{"name":"Cookie账号","cookie":"Cookie: ims_sid=import-session-fixture; sso_sid=import-sso-fixture"}`, cookie, "")
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), "import-session-fixture") || strings.Contains(response.Body.String(), "access-session-fixture") {
		t.Fatal("只提供 Cookie 无法导入，或响应泄露登录信息")
	}
	accounts, err := app.store.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].Status != "pending" || accounts[0].AccessToken != "" {
		t.Fatal("未验证的 Cookie 账号被误标为可用")
	}
	request := adminTestRequest(handler, "POST", "/v1/images/generations", `{"model":"gemini-2.5-flash-image","prompt":"生成测试图片"}`, nil, "")
	if request.Code != http.StatusUnauthorized {
		t.Fatal("协议接口允许未授权生成")
	}
	generation := httptest.NewRequest(http.MethodPost, "http://localhost/v1/images/generations", strings.NewReader(`{"model":"nano-banana-2","prompt":"生成测试图片","response_format":"url"}`))
	generation.Header.Set("Content-Type", "application/json")
	generation.Header.Set("Authorization", "Bearer "+app.store.Settings().APIKeys[0])
	generated := httptest.NewRecorder()
	handler.ServeHTTP(generated, generation)
	if generated.Code != http.StatusOK || !strings.Contains(generated.Body.String(), "/images/") || generator.refreshHits != 1 || generator.generateHit != 1 {
		t.Fatal("Cookie-only 导入后无法通过兼容接口完成刷新和生成")
	}
	logs, _, err := app.store.Logs(1, 20, "success", "")
	if err != nil || len(logs) != 1 || len(logs[0].Images) != 1 || logs[0].Endpoint != "/v1/images/generations" {
		t.Fatal("Cookie-only 生成没有保留接口和图片记录")
	}
	accounts, err = app.store.Accounts()
	if err != nil || len(accounts) != 1 || !strings.Contains(accounts[0].Cookie, "refreshed=fixture") || accounts[0].AccessToken == "" {
		t.Fatal("自动刷新后的 Cookie 或会话未保存")
	}
}

func TestCookieReplacementClearsOldIdentityAndBlankPreservesIt(t *testing.T) {
	app, handler, cookie, _ := cookieAdminTestApp(t)
	account := Account{ID: "fixture-account", Name: "原账号", Enabled: true, Cookie: "ims_sid=old-session", AccessToken: "old-access", ARPSession: "old-arp", UserID: "old-user", Email: "old@example.test", Status: "ready", LastRefresh: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := app.store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	response := adminTestRequest(handler, "PUT", "/api/admin/accounts/"+account.ID, `{"name":"新名称","cookie":""}`, cookie, "")
	if response.Code != http.StatusOK {
		t.Fatal("留空 Cookie 的编辑失败")
	}
	current, _ := app.findAdminAccount(account.ID)
	if current.Cookie != account.Cookie || current.AccessToken != account.AccessToken || current.UserID != account.UserID || current.Status != "ready" {
		t.Fatal("留空 Cookie 意外清除了已有会话")
	}
	response = adminTestRequest(handler, "PUT", "/api/admin/accounts/"+account.ID, `{"cookie":"ims_sid=new-session"}`, cookie, "")
	if response.Code != http.StatusOK {
		t.Fatal("替换 Cookie 的编辑失败")
	}
	current, _ = app.findAdminAccount(account.ID)
	if current.Cookie != "ims_sid=new-session" || current.AccessToken != "" || current.ARPSession != "" || current.UserID != "" || current.Email != "" || current.Status != "pending" || !current.ExpiresAt.IsZero() {
		t.Fatal("替换 Cookie 仍沿用了旧账号身份或可用状态")
	}
}

func TestBrowserSyncRedactsDeduplicatesAndRejectsBusyAccount(t *testing.T) {
	t.Setenv("ADOBE2API_CDP_URL", "http://127.0.0.1:9222")
	app, handler, cookie, generator := cookieAdminTestApp(t)
	response := adminTestRequest(handler, "POST", "/api/admin/accounts/browser", `{}`, cookie, "")
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"created":true`) {
		t.Fatal("浏览器同步未创建账号")
	}
	for _, secret := range []string{"session-fixture", "adobe-user-fixture"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("浏览器同步响应泄露登录凭据")
		}
	}
	accounts, _ := app.store.Accounts()
	if len(accounts) != 1 || accounts[0].Status != "unverified" {
		t.Fatal("浏览器读取成功被误当成生图已验证")
	}
	account := accounts[0]
	account.Name, account.Enabled, account.Status, account.ARPSession = "保留名称", false, "ready", "previous-valid-arp"
	if err := app.store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	generator.session.AccessToken, generator.session.ARPSession = "browser-wrong-scope-token", "browser-unverified-arp"
	response = adminTestRequest(handler, "POST", "/api/admin/accounts/browser", `{}`, cookie, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"created":false`) {
		t.Fatal("重复同步没有按同一 Adobe 用户更新")
	}
	accounts, _ = app.store.Accounts()
	if len(accounts) != 1 || accounts[0].ID != account.ID || accounts[0].Name != account.Name || accounts[0].Enabled {
		t.Fatal("重复同步改变了原账号名称、启用状态或创建重复账号")
	}
	if accounts[0].AccessToken != "ims-access-session-fixture" || accounts[0].ARPSession != "browser-unverified-arp" {
		t.Fatal("同步未保留可生图 IMS 令牌和当前页面风险会话")
	}
	app.busy = map[string]bool{account.ID: true}
	response = adminTestRequest(handler, "POST", "/api/admin/accounts/browser", `{}`, cookie, "")
	if response.Code != http.StatusConflict {
		t.Fatal("同步覆盖了正在生成中的账号")
	}
	app.busy = nil
	generator.session.AccessToken = ""
	response = adminTestRequest(handler, "POST", "/api/admin/accounts/browser", `{}`, cookie, "")
	if response.Code != http.StatusBadGateway {
		t.Fatal("浏览器没有 token 时同步被当作成功")
	}
	t.Setenv("ADOBE2API_CDP_URL", "")
	response = adminTestRequest(handler, "POST", "/api/admin/accounts/browser", `{}`, cookie, "")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatal("未配置已登录浏览器时同步没有明确失败")
	}
}

// 旧风险会话无论是否还留有错误，都不能覆盖管理员本次明确同步的页面会话。
func TestBrowserSyncReplacesStaleARPForFailedAndUnverifiedAccounts(t *testing.T) {
	t.Setenv("ADOBE2API_CDP_URL", "http://127.0.0.1:9222")
	for _, state := range []struct{ status, lastError string }{
		{"error", "生成失败"},
		{"unverified", ""},
		{"expired", ""},
	} {
		t.Run(state.status, func(t *testing.T) {
			app, handler, adminCookie, generator := cookieAdminTestApp(t)
			previous := Account{ID: "stale-browser-account", Name: "保留名称", Enabled: false, Cookie: "ims_sid=old-session", AccessToken: "old-token", UserID: generator.session.UserID, ARPSession: "stale-arp", Status: state.status, LastError: state.lastError}
			if err := app.store.SaveAccount(previous); err != nil {
				t.Fatal(err)
			}
			generator.session.ARPSession = "current-browser-arp"
			response := adminTestRequest(handler, http.MethodPost, "/api/admin/accounts/browser", `{}`, adminCookie, "")
			accounts, err := app.store.Accounts()
			if response.Code != http.StatusOK || err != nil || len(accounts) != 1 {
				t.Fatal("旧账号同步没有更新唯一账号记录")
			}
			current := accounts[0]
			if current.ID != previous.ID || current.Name != previous.Name || current.Enabled || current.ARPSession != "current-browser-arp" || current.Status != "unverified" || current.LastError != "" {
				t.Fatal("同步没有采用本次页面 ARP，或覆盖了账号基本信息")
			}
			if current.AccessToken != "ims-access-session-fixture" || generator.refreshHits != 2 {
				t.Fatal("同步当前 ARP 后没有完成最后一次 IMS 规范化")
			}
		})
	}
}

func TestFailedCookieRefreshKeepsImportedCookie(t *testing.T) {
	app, handler, cookie, generator := cookieAdminTestApp(t)
	response := adminTestRequest(handler, "POST", "/api/admin/accounts/import", `{"cookie":"ims_sid=keep-session"}`, cookie, "")
	var account adminAccount
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &account) != nil {
		t.Fatal("Cookie 导入失败")
	}
	generator.refreshErr = errors.New("登录会话无法续期")
	response = adminTestRequest(handler, "POST", "/api/admin/accounts/"+account.ID+"/refresh", "", cookie, "")
	if response.Code != http.StatusBadGateway {
		t.Fatal("刷新失败被返回为成功")
	}
	current, _ := app.findAdminAccount(account.ID)
	if current.Cookie != "ims_sid=keep-session" || current.Status == "ready" || current.LastError == "" {
		t.Fatal("刷新失败丢失 Cookie 或误标账号可用")
	}
}

// 导入同一账号的新 Cookie 只触发 IMS 续期，不新建重复账号或抹掉成功风险会话。
func TestCookieReimportKeepsExistingVerifiedARP(t *testing.T) {
	app, handler, cookie, _ := cookieAdminTestApp(t)
	previous := Account{ID: "existing-account", Name: "原账号", Enabled: true, Cookie: "ims_sid=stable-session; rotated=old", UserID: "existing-user@AdobeID", AccessToken: "previous-token", ARPSession: "previous-valid-arp", Status: "ready"}
	if err := app.store.SaveAccount(previous); err != nil {
		t.Fatal(err)
	}
	response := adminTestRequest(handler, "POST", "/api/admin/accounts/import", `{"cookie":"ims_sid=stable-session; rotated=new"}`, cookie, "")
	if response.Code != http.StatusOK {
		t.Fatal("同账号 Cookie 未更新原记录")
	}
	accounts, _ := app.store.Accounts()
	if len(accounts) != 1 || accounts[0].ID != previous.ID || accounts[0].UserID != previous.UserID || accounts[0].ARPSession != previous.ARPSession || accounts[0].Name != previous.Name || accounts[0].Status != "pending" || !strings.Contains(accounts[0].Cookie, "rotated=new") {
		t.Fatal("Cookie 再导入创建了重复账号或丢失有效风险会话")
	}
	response = adminTestRequest(handler, "PUT", "/api/admin/accounts/"+previous.ID, `{"cookie":"ims_sid=stable-session; rotated=newest"}`, cookie, "")
	if response.Code != http.StatusOK {
		t.Fatal("同账号 Cookie 编辑失败")
	}
	current, _ := app.findAdminAccount(previous.ID)
	if current.UserID != previous.UserID || current.ARPSession != previous.ARPSession || current.Status != "pending" {
		t.Fatal("编辑同会话 Cookie 清除了旧用户或风险会话")
	}
	app.busy = map[string]bool{previous.ID: true}
	response = adminTestRequest(handler, "POST", "/api/admin/accounts/import", `{"cookie":"ims_sid=stable-session; rotated=concurrent"}`, cookie, "")
	if response.Code != http.StatusConflict {
		t.Fatal("同账号 Cookie 再导入覆盖了正在生成中的会话")
	}
}

func TestAdminCookieInputRejectsInvalidAndAcceptsEmailHeader(t *testing.T) {
	cookie, email, err := parseAdminCookie("user@example.test----Cookie: sso_sid=fixture-sso; ims_sid=fixture-ims")
	if err != nil || email != "user@example.test" || cookie != "ims_sid=fixture-ims; sso_sid=fixture-sso" {
		t.Fatal("邮箱或 Cookie 请求头没有正确规范化")
	}
	for _, raw := range []string{"", "没有Cookie", "Cookie: ", "ims_sid=a\r\nAuthorization: leaked", "bad-email----ims_sid=a"} {
		if _, _, err := parseAdminCookie(raw); err == nil {
			t.Fatal("无效 Cookie 文本被保存")
		}
	}
	if sameAdobeCookieSession("analytics=same", "analytics=same") || sameAdobeCookieSession("ims_sid=first", "ims_sid=second") || !sameAdobeCookieSession("ims_sid=first; rotated=old", "ims_sid=first; rotated=new") {
		t.Fatal("Cookie 去重没有仅使用同一 Adobe 登录会话")
	}
}
