package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// 权限拒绝应给出准确中文原因，同时保留原始上游错误用于诊断。
func TestAdobeEntitlementErrorKeepsOriginalResponseLog(t *testing.T) {
	const originalMessage = "Forbidden: selected model is unavailable for the account"
	const responseJSON = `{"error_code":"forbidden","message":"` + originalMessage + `"}`
	client := &http.Client{Transport: adobeTestTransport(func(*http.Request) (*http.Response, error) {
		return adobeTestResponse(http.StatusForbidden, http.Header{"Content-Type": {"application/json"}, "X-Access-Error": {" user_not_entitled "}}, responseJSON), nil
	})}
	request, _ := http.NewRequest(http.MethodPost, "https://firefly.adobe.io/generate", strings.NewReader(`{"prompt":"猫"}`))
	request.Header.Set("Content-Type", "application/json")
	log := &RequestLog{}
	response, err := (&AdobeGenerator{}).do(client, request, log)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := decodeAdobeResponse(response)
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.StatusCode != http.StatusForbidden || upstream.AccessError != "user_not_entitled" || upstream.Message != "当前Adobe账号没有所选模型的使用权限" {
		t.Fatalf("模型无权限没有映射准确错误：%v", err)
	}
	if payload.Message != originalMessage || payload.ErrorCode != "forbidden" || len(log.Upstream) != 1 {
		t.Fatal("中文错误映射改变了原始业务响应")
	}
	var logged struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(log.Upstream[0].ResponseBody), &logged); err != nil || logged.Message != originalMessage {
		t.Fatal("接口日志未保留原始错误正文")
	}
}

// 权限原因也可能回显认证信息，必须与错误正文使用相同的脱敏规则。
func TestAdobeAccessErrorRedactsCredentialsAndBoundsText(t *testing.T) {
	request, _ := http.NewRequest(http.MethodPost, "https://firefly.adobe.io/generate", nil)
	request.Header.Set("Authorization", "Bearer access-secret-fixture")
	response := adobeTestResponse(http.StatusForbidden, http.Header{"X-Access-Error": {"denied access-secret-fixture " + strings.Repeat("猫", 300)}}, `{}`)
	response.Request = request
	upstream := adobeUpstreamError(response, response.StatusCode, "forbidden", "Forbidden")
	if strings.Contains(upstream.AccessError, "access-secret-fixture") || !strings.Contains(upstream.AccessError, upstreamLogHidden) || len(upstream.AccessError) > 256 || upstream.Message != "Forbidden" {
		t.Fatal("权限原因没有脱敏或限制长度")
	}
}

// 无权限账号保留登录状态，只有认证拒绝才标记过期。
func TestGenerationEntitlementStatusDoesNotExpireAccount(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		accessError string
		want        string
	}{
		{"模型无权限", http.StatusForbidden, "user_not_entitled", "error"},
		{"普通认证拒绝", http.StatusForbidden, "", "expired"},
		{"其他权限拒绝", http.StatusForbidden, "invalid_session", "expired"},
		{"令牌失效", http.StatusUnauthorized, "user_not_entitled", "expired"},
		{"上游繁忙", http.StatusRequestTimeout, "", "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, generator, _ := newProtocolApp(t)
			accounts, _ := app.store.Accounts()
			account := accounts[0]
			account.Status, account.ARPSession = "ready", "previous-valid-arp"
			if err := app.store.SaveAccount(account); err != nil {
				t.Fatal(err)
			}
			generator.failure = &UpstreamError{StatusCode: test.status, AccessError: test.accessError, Message: "生成失败"}
			response := protocolRequest(app, http.MethodPost, "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "权限状态测试"})
			current, _ := app.findAdminAccount(account.ID)
			if response.Code != test.status || current.Status != test.want || current.ARPSession != account.ARPSession || current.LastError == "" {
				t.Fatal("账号权限错误没有保存正确状态或登录信息")
			}
		})
	}
}
