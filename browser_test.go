package main

import (
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
)

func TestBrowserSessionWaitsPastTemporaryRiskToken(t *testing.T) {
	// 有 Node 的开发和校验环境直接执行页面脚本，避免仅检查脚本文字。
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("当前环境没有 Node，页面脚本回归可独立执行 tests/browser_session.cjs")
	}
	output, err := exec.Command(node, "tests/browser_session.cjs", "browser.go").CombinedOutput()
	if err != nil {
		t.Fatalf("风险会话初始化回归失败：%v\n%s", err, output)
	}
}

// 浏览器中的 IMS 有效期格式会随版本改变，均应恢复为相同的真实时刻。
func TestBrowserExpiryAcceptsIMSFormatsAndJWTFallback(t *testing.T) {
	expected := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	jwtPayload, _ := json.Marshal(map[string]any{"exp": expected.Unix()})
	token := "fixture." + base64.RawURLEncoding.EncodeToString(jwtPayload) + ".fixture"
	tests := []struct {
		name  string
		value any
		token string
	}{
		{"日期字符串", expected.Format(time.RFC3339), ""},
		{"毫秒时间戳", expected.UnixMilli(), ""},
		{"秒时间戳", expected.Unix(), ""},
		{"字符串毫秒", json.Number("1791981000000").String(), ""},
		{"空值读取令牌", nil, token},
		{"未知格式读取令牌", "未知格式", token},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(test.value)
			if result := parseBrowserExpiry(raw, test.token); !result.Equal(expected) {
				t.Fatalf("有效期恢复错误: %v", result)
			}
		})
	}
	if result := parseBrowserExpiry(json.RawMessage(`"NaN"`), ""); !result.IsZero() {
		t.Fatal("非法有效期没有忽略")
	}
	if result := parseBrowserExpiry(json.RawMessage(`null`), "无令牌"); !result.IsZero() {
		t.Fatal("缺失有效期产生了错误时间")
	}
}

func TestBrowserSessionNumericExpiryDoesNotBreakCredentialDecoding(t *testing.T) {
	var session browserSession
	err := json.Unmarshal([]byte(`{"access_token":"测试令牌","arp_session":"测试风险会话","expires_at":1791981000000,"profile":{"userId":"测试用户"}}`), &session)
	if err != nil || session.AccessToken == "" || session.ARPSession == "" || session.Profile.UserID == "" {
		t.Fatalf("数字有效期导致整个登录会话无法读取: %v", err)
	}
}

// 未知身份的 Cookie 不能借用全局登录页，已知身份也必须通过页面用户核对。
func TestBrowserAccountIsolationAndSessionIdentity(t *testing.T) {
	endpoint := "http://127.0.0.1:9222"
	if browserEndpointForAccount(Account{Cookie: "ims_sid=second-account"}, endpoint) != "" {
		t.Fatal("未知身份的 Cookie 账号误用了全局 CDP 登录页")
	}
	if browserEndpointForAccount(Account{}, endpoint) != endpoint {
		t.Fatal("显式浏览器导入没有使用配置的 CDP")
	}
	if browserEndpointForAccount(Account{UserID: "first@AdobeID", Cookie: "ims_sid=first-account"}, endpoint) != endpoint {
		t.Fatal("已知身份的账号无法进行 CDP 会话核对")
	}
	var session browserSession
	if err := json.Unmarshal([]byte(`{"access_token":"page-token","arp_session":"page-arp","profile":{"userId":"first@AdobeID"}}`), &session); err != nil {
		t.Fatal(err)
	}
	if err := validateBrowserSession(Account{UserID: "first@AdobeID"}, session); err != nil {
		t.Fatal("同一 Adobe 用户的浏览器会话被拒绝")
	}
	if err := validateBrowserSession(Account{UserID: "second@AdobeID"}, session); err == nil {
		t.Fatal("池内另一账号借用了当前浏览器的会话")
	}
	session.AccessToken = ""
	if err := validateBrowserSession(Account{}, session); err == nil {
		t.Fatal("缺 token 的浏览器会话被视作同步成功")
	}
}

// IMS 具体域的新值必须胜过其他 Adobe 域的旧值，反转 CDP 返回顺序也不能改变结果。
func TestBrowserCookieHeaderPrefersIMSDomainIndependentOfOrder(t *testing.T) {
	cookies := []*network.Cookie{
		{Name: "ims_sid", Value: "old-adobe-cookie", Domain: ".adobe.com", Path: "/"},
		{Name: "ims_sid", Value: "old-login-cookie", Domain: ".adobelogin.com", Path: "/"},
		{Name: "ims_sid", Value: "new-services-cookie", Domain: ".services.adobe.com", Path: "/"},
		{Name: "aux_sid", Value: "new-aux-cookie", Domain: ".adobe.com", Path: "/"},
		{Name: "wrong_host", Value: "blocked", Domain: ".eviladobe.com", Path: "/"},
		{Name: "wrong_path", Value: "blocked", Domain: ".services.adobe.com", Path: "/generate"},
		{Name: "host_only_other", Value: "blocked", Domain: "services.adobe.com", Path: "/"},
		{Name: "expired", Value: "blocked", Domain: ".services.adobe.com", Path: "/", Expires: float64(time.Now().Add(-time.Hour).Unix())},
		nil,
	}
	expected := "aux_sid=new-aux-cookie; ims_sid=new-services-cookie"
	if result := browserCookieHeader(cookies, adobeIMSURL); result != expected {
		t.Fatal("IMS 目标 Cookie 被其他域或路径的旧值覆盖")
	}
	for i, j := 0, len(cookies)-1; i < j; i, j = i+1, j-1 {
		cookies[i], cookies[j] = cookies[j], cookies[i]
	}
	if result := browserCookieHeader(cookies, adobeIMSURL); result != expected {
		t.Fatal("IMS Cookie 选择依赖浏览器返回顺序")
	}
}

func TestBrowserCookieHeaderPrefersSpecificPathAndRejectsBoundaryMismatch(t *testing.T) {
	cookies := []*network.Cookie{
		{Name: "ims_sid", Value: "domain-root", Domain: ".services.adobe.com", Path: "/"},
		{Name: "ims_sid", Value: "domain-ims", Domain: ".services.adobe.com", Path: "/ims"},
		{Name: "ims_sid", Value: "domain-exact", Domain: ".services.adobe.com", Path: "/ims/check/v6/token"},
		{Name: "bad_prefix", Value: "blocked", Domain: ".services.adobe.com", Path: "/im"},
		{Name: "bad_suffix", Value: "blocked", Domain: ".services.adobe.com", Path: "/ims/check/v6/tokens"},
	}
	if result := browserCookieHeader(cookies, adobeIMSURL); result != "ims_sid=domain-exact" {
		t.Fatal("Cookie 路径没有遵循具体程度或路径边界")
	}
	cookies = append(cookies, &network.Cookie{Name: "ims_sid", Value: "host-specific", Domain: "adobeid-na1.services.adobe.com", Path: "/"})
	if result := browserCookieHeader(cookies, adobeIMSURL); result != "ims_sid=host-specific" {
		t.Fatal("IMS 目标主机的 Cookie 没有优先于父域")
	}
}
