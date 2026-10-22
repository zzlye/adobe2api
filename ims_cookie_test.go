package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
)

// Cookie 恢复真实身份不依赖用户手填 ID，也不能把页面令牌当作 IMS 生图令牌。
func TestAdobeIMSCookieOnlyRestoresUserIDWithoutBrowser(t *testing.T) {
	browserCalls := 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == adobeCreditsURL {
			return adobeTestResponse(503, nil, `{}`), nil
		}
		if err := request.ParseForm(); err != nil || len(request.Form) != 2 || request.Form.Get("user_id") != "" || request.Form.Get("guest_allowed") != "" {
			t.Fatal("Cookie-only IMS 请求仍要求用户标识或访客参数")
		}
		return adobeTestResponse(200, nil, `{"access_token":"firefly-ims-token","expires_in":3600000,"userId":"real-user@AdobeID"}`), nil
	})
	generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
		browserCalls++
		return account, errors.New("不应启动浏览器")
	}
	account, err := generator.Refresh(context.Background(), Account{Cookie: "ims_sid=cookie-only-session", ARPSession: "previous-valid-arp"})
	if err != nil || browserCalls != 0 || account.UserID != "real-user@AdobeID" || account.AccessToken != "firefly-ims-token" || account.ARPSession != "previous-valid-arp" || account.Status == "ready" {
		t.Fatal("Cookie-only IMS 未补全真实用户或丢失有效旧风险会话")
	}
}

// 明确的身份冲突必须直接失败，不能悄悄退回其他浏览器账号。
func TestAdobeIMSMismatchedIdentityCannotFallBackToBrowser(t *testing.T) {
	for _, payload := range []string{
		`{"access_token":"other-token","userId":"other-user@AdobeID"}`,
		`{"access_token":"other-token","userId":"expected-user@AdobeID","user_id":"other-user@AdobeID"}`,
	} {
		browserCalls := 0
		generator := newAdobeTestGenerator(t, func(*http.Request) (*http.Response, error) {
			return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=other-session"}}, payload), nil
		})
		generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
			browserCalls++
			return account, nil
		}
		original := Account{UserID: "expected-user@AdobeID", Cookie: "ims_sid=expected-session", AccessToken: "expected-token", ARPSession: "expected-arp"}
		account, err := generator.Refresh(context.Background(), original)
		if !errors.Is(err, errAdobeAccountMismatch) || browserCalls != 0 || account.Cookie != original.Cookie || account.AccessToken != original.AccessToken || account.UserID != original.UserID || account.ARPSession != original.ARPSession {
			t.Fatal("身份冲突未被拒绝，或覆盖了原账号凭据")
		}
	}
}

// 浏览器补充风险会话后必须再走 IMS，不能让页面缓存 token 覆盖已经取得的生图权限。
func TestAdobeBrowserARPRestoreRenewsIMSAgain(t *testing.T) {
	imsCalls, browserCalls := 0, 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		imsCalls++
		token := "initial-ims-token"
		if imsCalls == 2 {
			if request.Header.Get("Cookie") != "ims_sid=browser-renewed" {
				t.Fatal("浏览器续期后没有用更新的 Cookie 重新获取 IMS 令牌")
			}
			token = "final-ims-token"
		}
		return adobeTestResponse(200, nil, `{"access_token":"`+token+`","userId":"real-user@AdobeID"}`), nil
	})
	generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
		browserCalls++
		if account.AccessToken != "initial-ims-token" || account.UserID != "real-user@AdobeID" {
			t.Fatal("浏览器补充会话没有收到 IMS 已解析的身份")
		}
		account.Cookie, account.ARPSession, account.AccessToken = "ims_sid=browser-renewed", "browser-arp", "wrong-scope-page-token"
		return account, nil
	}
	account, err := generator.Refresh(context.Background(), Account{Cookie: "ims_sid=initial"})
	if err != nil || imsCalls != 2 || browserCalls != 1 || account.AccessToken != "final-ims-token" || account.ARPSession != "browser-arp" || account.Cookie != "ims_sid=browser-renewed" {
		t.Fatal("浏览器补风险会话后仍保留页面缓存令牌或未重新获取 IMS")
	}
}

func TestAdobeBrowserFallbackCannotChangeExpectedIdentity(t *testing.T) {
	imsCalls := 0
	generator := newAdobeTestGenerator(t, func(*http.Request) (*http.Response, error) {
		imsCalls++
		return adobeTestResponse(401, nil, `{"error":"invalid_cookie"}`), nil
	})
	generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
		account.UserID, account.Cookie, account.ARPSession = "other@AdobeID", "ims_sid=other", "other-arp"
		return account, nil
	}
	account, err := generator.Refresh(context.Background(), Account{UserID: "expected@AdobeID", Cookie: "ims_sid=expected"})
	if !errors.Is(err, errAdobeAccountMismatch) || imsCalls != 1 || account.UserID != "expected@AdobeID" || account.Cookie != "ims_sid=expected" {
		t.Fatal("浏览器恢复将另一账号的身份写回原账号")
	}
}

func TestAdobeIMSHTTP200AuthenticationErrorPreservesCredentials(t *testing.T) {
	generator := newAdobeTestGenerator(t, func(*http.Request) (*http.Response, error) {
		return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=invalid-new-cookie"}}, `{"error":"invalid_credentials"}`), nil
	})
	original := Account{UserID: "real-user@AdobeID", Cookie: "ims_sid=previous-cookie", AccessToken: "previous-token", ARPSession: "previous-valid-arp"}
	account, err := generator.refreshIMS(context.Background(), original)
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.Code != "invalid_credentials" || upstream.StatusCode != http.StatusUnauthorized || account.Cookie != original.Cookie || account.AccessToken != original.AccessToken || account.ARPSession != original.ARPSession {
		t.Fatal("HTTP 200 中的认证错误未正确识别或覆盖了有效旧会话")
	}
}

func TestAdobeBrowserCookieRecoveryUsesCurrentARP(t *testing.T) {
	imsCalls := 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		imsCalls++
		if imsCalls == 1 {
			return adobeTestResponse(200, nil, `{"error":"invalid_credentials"}`), nil
		}
		return adobeTestResponse(200, nil, `{"access_token":"final-ims-token","userId":"real-user@AdobeID"}`), nil
	})
	generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
		account.Cookie, account.ARPSession, account.AccessToken = "ims_sid=recovered-cookie", "new-unverified-arp", "page-token"
		return account, nil
	}
	account, err := generator.Refresh(context.Background(), Account{UserID: "real-user@AdobeID", Cookie: "ims_sid=expired-cookie", ARPSession: "previous-valid-arp", Status: "ready"})
	if err != nil || imsCalls != 2 || account.AccessToken != "final-ims-token" || account.ARPSession != "new-unverified-arp" || account.Cookie != "ims_sid=recovered-cookie" {
		t.Fatal("浏览器恢复未采用本次风险会话，或沿用了页面缓存令牌")
	}
}

// 浏览器完整 Cookie 中的有效 sherlockToken 可直接作为风险会话，被风控标记（rid=100）的 ark 字段应自动剔除并保留有效 ftr/bfp。
func TestCookieARPSessionExtractsValidSherlockTokenAndSanitizesFlaggedRisk(t *testing.T) {
	validSherlock := base64.StdEncoding.EncodeToString([]byte(`{"sid":"valid-sid","ftr":"valid-ftr","ark":"token|rid=65|ag=101","bfp":"valid-bfp"}`))
	flaggedSherlock := base64.StdEncoding.EncodeToString([]byte(`{"sid":"flagged-sid","ftr":"valid-ftr","ark":"token|rid=100|ag=101","bfp":"valid-bfp"}`))
	arkOnlyFlagged := base64.StdEncoding.EncodeToString([]byte(`{"sid":"flagged-sid","ark":"token|rid=100|ag=101"}`))
	if got := cookieARPSession("ims_sid=s1; sherlockToken=" + validSherlock); got != validSherlock {
		t.Fatalf("未提取有效的 sherlockToken: %s", got)
	}
	sanitized := cookieARPSession("ims_sid=s1; sherlockToken=" + flaggedSherlock)
	decoded, err := base64.StdEncoding.DecodeString(sanitized)
	if err != nil || string(decoded) != `{"bfp":"valid-bfp","ftr":"valid-ftr","sid":"flagged-sid"}` {
		t.Fatalf("未清理触发风控 rid=100 的 ark 字段: %s", string(decoded))
	}
	if got := cookieARPSession("ims_sid=s1; sherlockToken=" + arkOnlyFlagged); got != "" {
		t.Fatalf("仅含 rid=100 ark 的会话不应采用: %s", got)
	}
}

