package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 真实 IMS 先轮换 Cookie 后浏览器失败，生成和管理员错误分支都应保存轮换结果供下次续期。
func TestPartialIMSRefreshKeepsRotatedCookieAfterBrowserFailure(t *testing.T) {
	for _, source := range []string{"生成触发", "管理员触发"} {
		t.Run(source, func(t *testing.T) {
			app, handler, adminCookie, _ := cookieAdminTestApp(t)
			original := Account{ID: "partial-refresh-fixture", Name: "待续期账号", Enabled: true, Cookie: "ims_sid=original-session", Status: "pending"}
			if err := app.store.SaveAccount(original); err != nil {
				t.Fatal(err)
			}
			var cookieHeaders []string
			browserCalls := 0
			client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() == adobeCreditsURL {
					return adobeTestResponse(503, http.Header{"Content-Type": {"application/json"}}, `{}`), nil
				}
				if request.URL.String() != adobeIMSURL {
					t.Fatal("续期失败后仍调用了实际生成接口")
				}
				cookieHeaders = append(cookieHeaders, request.Header.Get("Cookie"))
				return adobeTestResponse(200, http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"ims_sid=rotated-session; Secure; HttpOnly"}}, `{"access_token":"ims-token-`+strconv.Itoa(len(cookieHeaders))+`","userId":"confirmed-user@AdobeID","expires_in":3600000}`), nil
			})}
			generator := &AdobeGenerator{store: app.store, client: client, downloadClient: client}
			generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
				browserCalls++
				if browserCalls == 1 {
					return Account{}, errors.New("风险会话读取失败")
				}
				account.ARPSession, account.AccessToken = "recovered-arp", "page-cached-token"
				return account, nil
			}
			app.generator = generator
			if source == "生成触发" {
				request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"nano-banana-2","prompt":"续期恢复测试"}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+app.store.Settings().APIKeys[0])
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusServiceUnavailable {
					t.Fatal("生成续期后半程失败没有明确返回失败")
				}
			} else {
				response := adminTestRequest(handler, http.MethodPost, "/api/admin/accounts/"+original.ID+"/refresh", "", adminCookie, "")
				if response.Code != http.StatusBadGateway {
					t.Fatal("管理员续期后半程失败没有明确返回失败")
				}
			}
			partial, _ := app.findAdminAccount(original.ID)
			if partial.Cookie != "ims_sid=rotated-session" || partial.AccessToken != "ims-token-1" || partial.UserID != "confirmed-user@AdobeID" || partial.Status != "expired" || partial.LastError == "" || partial.Name != original.Name || !partial.Enabled {
				t.Fatal("失败分支写回旧账号，丢失已确认身份和轮换凭据")
			}
			retry := adminTestRequest(handler, http.MethodPost, "/api/admin/accounts/"+original.ID+"/refresh", "", adminCookie, "")
			if retry.Code != http.StatusOK || len(cookieHeaders) != 3 || cookieHeaders[0] != "ims_sid=original-session" || cookieHeaders[1] != "ims_sid=rotated-session" || cookieHeaders[2] != "ims_sid=rotated-session" {
				t.Fatal("下次续期没有使用前次已轮换的新 Cookie")
			}
			updated, _ := app.findAdminAccount(original.ID)
			if updated.ARPSession != "recovered-arp" || updated.AccessToken != "ims-token-3" || updated.Status != "unverified" || updated.LastError != "" {
				t.Fatal("续期重试成功后没有保留规范化的完整登录信息")
			}
		})
	}
}

func TestMergePartialRefreshRejectsEmptyAndOtherIdentity(t *testing.T) {
	original := Account{ID: "original-account", Name: "原账号", Enabled: true, UserID: "original@AdobeID", Cookie: "ims_sid=old", AccessToken: "old-token", ARPSession: "verified-arp", Status: "ready"}
	otherClaims := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"other@AdobeID"}`))
	for _, partial := range []Account{
		{},
		{UserID: original.UserID, Cookie: "ims_sid=new"},
		{ID: "other-account", UserID: original.UserID, Cookie: "ims_sid=new"},
		{ID: original.ID, Cookie: "ims_sid=new"},
		{ID: original.ID, UserID: "other@AdobeID", Cookie: "ims_sid=new"},
		{ID: original.ID, UserID: original.UserID, AccessToken: "fixture." + otherClaims + ".fixture"},
	} {
		if actual := mergeConfirmedRefresh(original, partial); !reflect.DeepEqual(actual, original) {
			t.Fatal("空账号或其他身份的失败返回值覆盖了原账号")
		}
	}
	partial := Account{ID: original.ID, UserID: original.UserID, Name: "不应覆盖名称", Enabled: false, Cookie: "ims_sid=new", AccessToken: "new-token", ARPSession: "unverified-arp", LastRefresh: time.Now(), Status: "ready"}
	merged := mergeConfirmedRefresh(original, partial)
	if merged.Cookie != partial.Cookie || merged.AccessToken != partial.AccessToken || merged.UserID != original.UserID || merged.Name != original.Name || !merged.Enabled || merged.ARPSession != original.ARPSession || merged.LastRefresh.IsZero() {
		t.Fatal("同账号部分更新未保留新凭据、原基本字段和旧有效 ARP")
	}
}

// 首次同步已经确认并轮换的凭据，在第二阶段认证失败或身份冲突时仍可供后续恢复。
func TestBrowserSyncPreservesFirstConfirmedSessionAfterSecondFailure(t *testing.T) {
	t.Setenv("ADOBE2API_CDP_URL", "http://127.0.0.1:9222")
	for _, existing := range []bool{false, true} {
		for _, mismatch := range []bool{false, true} {
			t.Run(strconv.FormatBool(existing)+"/"+strconv.FormatBool(mismatch), func(t *testing.T) {
				app, handler, adminCookie, _ := cookieAdminTestApp(t)
				original := Account{ID: "existing-browser-account", Name: "保留名称", Enabled: false, UserID: "confirmed-user@AdobeID", Cookie: "ims_sid=previous-session", AccessToken: "previous-token", ARPSession: "previous-valid-arp", Status: "ready"}
				if existing {
					if err := app.store.SaveAccount(original); err != nil {
						t.Fatal(err)
					}
				}
				imsCalls, browserCalls := 0, 0
				client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
					if request.URL.String() == adobeCreditsURL {
						return adobeTestResponse(503, nil, `{}`), nil
					}
					imsCalls++
					if imsCalls == 1 {
						return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=first-confirmed-session; Secure; HttpOnly"}}, `{"access_token":"first-confirmed-token","userId":"confirmed-user@AdobeID","expires_in":3600000}`), nil
					}
					if request.Header.Get("Cookie") != "ims_sid=first-confirmed-session" {
						t.Fatal("第二阶段未使用第一次 IMS 已轮换的 Cookie")
					}
					if mismatch {
						return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=other-session"}}, `{"access_token":"other-token","userId":"other-user@AdobeID"}`), nil
					}
					return adobeTestResponse(401, nil, `{"error":"invalid_credentials"}`), nil
				})}
				generator := &AdobeGenerator{store: app.store, client: client, downloadClient: client}
				generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
					browserCalls++
					if browserCalls > 1 {
						return Account{}, errors.New("浏览器会话恢复失败")
					}
					account.Cookie, account.AccessToken, account.ARPSession, account.UserID = "ims_sid=browser-session", "page-token", "browser-arp", "confirmed-user@AdobeID"
					return account, nil
				}
				app.generator = generator
				response := adminTestRequest(handler, http.MethodPost, "/api/admin/accounts/browser", `{}`, adminCookie, "")
				accounts, err := app.store.Accounts()
				if response.Code != http.StatusBadGateway || err != nil || len(accounts) != 1 || imsCalls != 2 {
					t.Fatal("同步第二阶段失败没有保存唯一的已确认账号记录")
				}
				saved := accounts[0]
				if saved.Cookie != "ims_sid=first-confirmed-session" || saved.AccessToken != "first-confirmed-token" || saved.UserID != "confirmed-user@AdobeID" || saved.Status != "expired" || saved.LastError == "" {
					t.Fatal("同步后半程失败丢失首次已确认凭据或写入其他账号")
				}
				if existing && (saved.ID != original.ID || saved.Name != original.Name || saved.Enabled || saved.ARPSession != "browser-arp") {
					t.Fatal("同步失败覆盖了旧账号基本信息，或没有保存本次页面风险会话")
				}
				if !existing && (saved.ID == "" || !saved.Enabled || saved.Name == "" || saved.ARPSession != "browser-arp") {
					t.Fatal("新账号同步失败没有保留可恢复的账号基本信息")
				}
				app.poolMu.Lock()
				busy := app.busy[saved.ID]
				app.poolMu.Unlock()
				if busy {
					t.Fatal("失败后未释放浏览器同步预留的账号")
				}
			})
		}
	}
}

func TestPartialIMSRefreshKeepsConfirmedCredentialsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generator := newAdobeTestGenerator(t, func(*http.Request) (*http.Response, error) {
		cancel()
		return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=confirmed-rotation"}}, `{"access_token":"confirmed-token","userId":"confirmed-user@AdobeID"}`), nil
	})
	account, err := generator.Refresh(ctx, Account{ID: "cancelled-account", Cookie: "ims_sid=original-session"})
	if !errors.Is(err, context.Canceled) || account.Cookie != "ims_sid=confirmed-rotation" || account.AccessToken != "confirmed-token" || account.UserID != "confirmed-user@AdobeID" {
		t.Fatal("取消分支丢失取消前已确认并轮换的 IMS 凭据")
	}
}

// 浏览器中的缓存令牌和 Cookie 未经最后一次 IMS 验证，失败时不得盖掉之前已确认的会话。
func TestPartialIMSRefreshRejectsUnconfirmedBrowserCredentialsOnFinalFailure(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(strconv.FormatBool(mismatch), func(t *testing.T) {
			imsCalls := 0
			generator := newAdobeTestGenerator(t, func(*http.Request) (*http.Response, error) {
				imsCalls++
				if imsCalls == 1 {
					return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=confirmed-rotation"}}, `{"access_token":"confirmed-token","userId":"confirmed-user@AdobeID"}`), nil
				}
				if mismatch {
					return adobeTestResponse(200, nil, `{"access_token":"other-token","userId":"other-user@AdobeID"}`), nil
				}
				return adobeTestResponse(401, nil, `{"error":"invalid_credentials"}`), nil
			})
			generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
				account.Cookie, account.AccessToken, account.ARPSession = "ims_sid=unconfirmed-browser-session", "page-cached-token", "new-browser-arp"
				return account, nil
			}
			account, err := generator.Refresh(context.Background(), Account{ID: "final-failure-account", Cookie: "ims_sid=original-session"})
			if err == nil || imsCalls != 2 || mismatch && !errors.Is(err, errAdobeAccountMismatch) {
				t.Fatal("最后一次 IMS 失败或身份冲突未明确返回")
			}
			if account.Cookie != "ims_sid=confirmed-rotation" || account.AccessToken != "confirmed-token" || account.UserID != "confirmed-user@AdobeID" {
				t.Fatal("最后一次 IMS 失败将未确认的浏览器缓存凭据当作已确认会话")
			}
		})
	}
}
