package main

import (
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"testing"
	"time"
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
