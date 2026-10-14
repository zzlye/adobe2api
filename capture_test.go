package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 单行、续行和不同终端的引号应还原相同请求，正文内容不能覆盖认证头。
func TestCaptureCurlReadsSingleLineAndBrowserCopyFormats(t *testing.T) {
	tests := []struct{ name, capture string }{
		{"单行", `curl 'https://firefly-3p.ff.adobe.io/generate-async' -H 'Authorization: Bearer fixture-token' -H 'X-Arp-Session-Id: fixture-arp' -b 'ims_sid=fixture-cookie' -H 'X-Api-Key: fixture-api' --data-raw '{"prompt":"只读提示词 -H Bearer fake-token"}'`},
		{"Bash续行", "curl 'https://firefly-3p.ff.adobe.io/generate-async' \\\n  -H 'authorization: bearer fixture-token' \\\n  -H 'x-arp-session-id: fixture-arp' \\\n  --cookie 'ims_sid=fixture-cookie' \\\n  --header 'x-api-key: fixture-api'"},
		{"Windows续行", "curl \"https://firefly-3p.ff.adobe.io/generate-async\" ^\r\n  -H \"Authorization: Bearer fixture-token\" ^\r\n  -H \"X-Arp-Session-Id: fixture-arp\" ^\r\n  -b \"ims_sid=fixture-cookie\" ^\r\n  -H \"X-Api-Key: fixture-api\""},
		{"Windows包装引号", "curl ^\"https://firefly-3p.ff.adobe.io/generate-async^\" ^\r\n  -H ^\"Authorization: Bearer fixture-token^\" ^\r\n  -H ^\"X-Arp-Session-Id: fixture-arp^\" ^\r\n  -b ^\"ims_sid=fixture-cookie^\" ^\r\n  -H ^\"X-Api-Key: fixture-api^\""},
		{"长选项与ANSI引号", `curl --url='https://firefly-3p.ff.adobe.io/generate-async' --header=$'Authorization: Bearer fixture-token' --header='X-Arp-Session-Id: fixture-arp' --cookie='ims_sid=fixture-cookie' --header='X-Api-Key: fixture-api'`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account, err := parseCapture(test.capture)
			if err != nil {
				t.Fatal(err)
			}
			if account.AccessToken != "fixture-token" || account.ARPSession != "fixture-arp" || account.Cookie != "ims_sid=fixture-cookie" || account.APIKey != "fixture-api" {
				t.Fatal("没有从同一请求中正确读取认证字段")
			}
		})
	}
}

func TestCaptureCurlRejectsThirdPartyAndCommandChaining(t *testing.T) {
	for _, capture := range []string{
		`curl 'https://adobe.com.external.test/generate' -H 'Authorization: Bearer fixture-token'`,
		`curl 'https://fakeadobe.com/generate' -H 'Authorization: Bearer fixture-token'`,
		`curl 'https://firefly.adobe.com/generate/image' 'https://external.test/' -H 'Authorization: Bearer fixture-token'`,
		`curl 'https://firefly.adobe.com/generate/image' -H 'Authorization: Bearer fixture-token'; curl 'https://external.test/'`,
		`curl 'https://firefly.adobe.com/generate/image' -H 'Authorization: Bearer fixture-token`,
	} {
		if _, err := parseCapture(capture); err == nil {
			t.Fatal("不可信请求或无效命令被接受")
		}
	}
}

func captureTestHAR(t *testing.T, entries []map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"log": map[string]any{"entries": entries}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func captureTestEntry(url string, headers map[string]string) map[string]any {
	items := make([]map[string]string, 0, len(headers))
	for name, value := range headers {
		items = append(items, map[string]string{"name": name, "value": value})
	}
	return map[string]any{"request": map[string]any{"url": url, "headers": items}}
}

func TestCaptureHARPrefersCompleteSameRequestAndIgnoresThirdParty(t *testing.T) {
	account, err := parseCapture(captureTestHAR(t, []map[string]any{
		captureTestEntry("https://firefly-3p.ff.adobe.io/generate-async", map[string]string{"Authorization": "Bearer generation-token", "X-Arp-Session-Id": "generation-arp", "Cookie": "ims_sid=generation-cookie", "X-Api-Key": "generation-api"}),
		captureTestEntry("https://adobeid-na1.services.adobe.com/ims/check/v6/token", map[string]string{"Authorization": "Bearer later-auth-token", "Cookie": "ims_sid=later-auth-cookie"}),
		captureTestEntry("https://third-party.test/analytics", map[string]string{"Authorization": "Bearer alien-token", "X-Arp-Session-Id": "alien-arp", "Cookie": "ims_sid=alien-cookie", "X-Api-Key": "alien-api"}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != "generation-token" || account.ARPSession != "generation-arp" || account.Cookie != "ims_sid=generation-cookie" || account.APIKey != "generation-api" {
		t.Fatal("同请求认证字段被其他请求覆盖")
	}
	for _, url := range []string{"https://adobe.com.third-party.test/", "https://notadobe.io/", "http://firefly.adobe.com/", "https://third-party.test/?next=https://adobe.com/"} {
		if _, err := parseCapture(captureTestHAR(t, []map[string]any{captureTestEntry(url, map[string]string{"Authorization": "Bearer alien-token"})})); err == nil {
			t.Fatal("不可信 HAR 来源被接受")
		}
	}
}

func TestCaptureHARDoesNotCombineTokenAndARPFromDifferentRequests(t *testing.T) {
	account, err := parseCapture(captureTestHAR(t, []map[string]any{
		captureTestEntry("https://firefly-3p.ff.adobe.io/generate-async", map[string]string{"Authorization": "Bearer token-one"}),
		captureTestEntry("https://firefly-3p.ff.adobe.io/generate-async", map[string]string{"X-Arp-Session-Id": "arp-two"}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != "token-one" || account.ARPSession != "" {
		t.Fatal("来自不同请求的认证字段被混用")
	}
}

func TestCaptureCookieExportUsesExactAdobeDomainBoundaries(t *testing.T) {
	account, err := parseCapture(`[
 {"name":"ims_sid","value":"official","domain":".services.adobe.com"},
 {"name":"sso_sid","value":"login","domain":".adobelogin.com"},
 {"name":"alien_one","value":"bad","domain":"adobe.com.external.test"},
 {"name":"alien_two","value":"bad","domain":"notadobe.io"},
 {"name":"unknown_domain","value":"bad","domain":""}
]`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(account.Cookie, "ims_sid=official") || !strings.Contains(account.Cookie, "sso_sid=login") || strings.Contains(account.Cookie, "bad") {
		t.Fatal("Cookie 域名过滤错误")
	}
	if _, err := parseCapture(`[{"name":"ims_sid","value":"bad","domain":"adobe.example.test"}]`); err == nil {
		t.Fatal("没有可信 Cookie 的导出仍被接受")
	}
}

func TestCaptureClearsPromptBoundNonceAndKeepsRawCookieImport(t *testing.T) {
	account, err := parseCapture(`{"access_token":"Bearer fixture-token","nonce":"旧提示词签名"}`)
	if err != nil || account.AccessToken != "fixture-token" || account.Nonce != "" {
		t.Fatal("账号对象没有正确归一化")
	}
	account, err = parseCapture("fixture@example.test----ims_sid=fixture-cookie; sso_sid=fixture-sso")
	if err != nil || account.Email != "fixture@example.test" || account.Cookie == "" {
		t.Fatal("原始邮箱与 Cookie 导入不兼容")
	}
}
