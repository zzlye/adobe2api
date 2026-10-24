package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 从正式兼容路由调用真实 Adobe 刷新适配器，失败也必须保留完整的 IMS 上游记录。
func TestGenerationIMSRefreshFailureKeepsUpstreamLog(t *testing.T) {
	for _, upstreamStatus := range []int{http.StatusOK, http.StatusUnauthorized} {
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			app, _, _ := newProtocolApp(t)
			accounts, _ := app.store.Accounts()
			account := accounts[0]
			account.AccessToken, account.Cookie, account.Status, account.ARPSession = "", "ims_sid=refresh-secret-fixture", "pending", "previous-valid-arp"
			if err := app.store.SaveAccount(account); err != nil {
				t.Fatal(err)
			}
			requests := 0
			client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
				requests++
				if request.URL.String() != adobeIMSURL || request.Method != http.MethodPost || request.Header.Get("Cookie") != account.Cookie {
					t.Fatal("测试没有经过生成触发的 IMS 真实调用链")
				}
				return adobeTestResponse(upstreamStatus, http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"ims-failure-request"}}, `{"error":"invalid_credentials"}`), nil
			})}
			app.generator = &AdobeGenerator{store: app.store, client: client, downloadClient: client}
			response := protocolRequest(app, http.MethodPost, "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "失败链记录测试"})
			if response.Code != http.StatusServiceUnavailable || requests != 1 {
				t.Fatal("IMS 刷新失败后未停止生成或没有返回正式失败状态")
			}
			logs, total, err := app.store.Logs(1, 20, "failed", "")
			if err != nil || total != 1 || len(logs) != 1 || logs[0].HTTPStatus != http.StatusServiceUnavailable || len(logs[0].Upstream) != 1 {
				t.Fatal("失败生成记录遗漏 IMS 上游请求")
			}
			record := logs[0].Upstream[0]
			if record.URL != adobeIMSURL || record.Method != http.MethodPost || record.Status != upstreamStatus || record.RequestID != "ims-failure-request" || !strings.Contains(record.ResponseBody, "invalid_credentials") {
				t.Fatal("IMS 上游接口、状态、请求编号或响应正文没有保存")
			}
			encoded, _ := json.Marshal(logs)
			if strings.Contains(string(encoded), "refresh-secret-fixture") {
				t.Fatal("IMS 上游记录泄露了请求 Cookie")
			}
			current, _ := app.findAdminAccount(account.ID)
			if current.Status != "expired" || current.ARPSession != account.ARPSession || current.Cookie != account.Cookie {
				t.Fatal("续期失败未标记失效或抹掉了旧登录和风险会话")
			}
		})
	}
}

// 真实生成成功可恢复账号，业务失败仅记错误，认证拒绝才需要重新登录。
func TestGenerationAccountStatusFollowsActualResult(t *testing.T) {
	for _, test := range []struct {
		name     string
		initial  string
		failure  error
		want     string
		httpCode int
	}{
		{"未验证生成成功", "unverified", nil, "ready", http.StatusOK},
		{"错误账号生成成功", "error", nil, "ready", http.StatusOK},
		{"可用账号上游繁忙", "ready", &UpstreamError{StatusCode: http.StatusRequestTimeout, Message: "上游繁忙"}, "error", http.StatusRequestTimeout},
		{"错误账号再次失败", "error", errors.New("上游网络失败"), "error", http.StatusBadGateway},
		{"认证失效", "ready", &UpstreamError{StatusCode: http.StatusUnauthorized, Message: "认证失效"}, "expired", http.StatusUnauthorized},
		{"认证拒绝", "ready", &UpstreamError{StatusCode: http.StatusForbidden, Message: "认证拒绝"}, "expired", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, generator, _ := newProtocolApp(t)
			accounts, _ := app.store.Accounts()
			account := accounts[0]
			account.Status, account.LastError, account.ARPSession = test.initial, "上一次错误", "previous-valid-arp"
			if err := app.store.SaveAccount(account); err != nil {
				t.Fatal(err)
			}
			generator.failure = test.failure
			response := protocolRequest(app, http.MethodPost, "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "状态变化测试"})
			if response.Code != test.httpCode {
				t.Fatal("生成返回状态与上游结果不一致")
			}
			current, _ := app.findAdminAccount(account.ID)
			if current.Status != test.want || current.ARPSession != account.ARPSession || (test.failure == nil) != (current.LastError == "") {
				t.Fatal("账号状态或错误信息没有跟随真实生成结果")
			}
		})
	}
}

func TestExplicitUpstreamLogOverridesContextWithoutDuplicate(t *testing.T) {
	contextLog, explicitLog := &RequestLog{}, &RequestLog{}
	ctx := context.WithValue(context.Background(), requestLogContextKey{}, contextLog)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, adobeCreditsURL, nil)
	client := &http.Client{Transport: adobeTestTransport(func(*http.Request) (*http.Response, error) {
		return adobeTestResponse(http.StatusOK, http.Header{"Content-Type": {"application/json"}}, `{}`), nil
	})}
	generator := &AdobeGenerator{}
	response, err := generator.do(client, request, explicitLog)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if len(contextLog.Upstream) != 0 || len(explicitLog.Upstream) != 1 {
		t.Fatal("显式日志和上下文日志重复保存同一个上游请求")
	}
}

func TestARPSessionExpiryAndOpaqueVendorMarkers(t *testing.T) {
	now := time.UnixMilli(1792778000000)
	makeARP := func(ftr string, withAll bool) string {
		payload := map[string]string{"sid": "sid-1", "ftr": ftr}
		if withAll {
			payload["ark"] = "ark-token"
			payload["bfp"] = "bfp-token"
		}
		raw, _ := json.Marshal(payload)
		return base64.StdEncoding.EncodeToString(raw)
	}
	freshARP := makeARP("cd966c780c114ed18f037acd8525af53_"+strconv.FormatInt(now.Add(-2*time.Hour).UnixMilli(), 10)+"__UDFT5_31ck_tt", true)
	expiredARP := makeARP("cd966c780c114ed18f037acd8525af53_"+strconv.FormatInt(now.Add(-8*24*time.Hour).UnixMilli(), 10)+"__UDFT5_31ck_tt", true)
	incompleteARP := makeARP("cd966c780c114ed18f037acd8525af53_"+strconv.FormatInt(now.UnixMilli(), 10)+"__UDFT5_31ck_tt", false)
	udf9ARP := makeARP("cd966c780c114ed18f037acd8525af53_"+strconv.FormatInt(now.UnixMilli(), 10)+"__UDF9_13ck_tt", true)
	if arpSessionExpired(freshARP, now) {
		t.Fatal("未满 4 小时的完整风险会话被误判为过期")
	}
	if !arpSessionExpired(expiredARP, now) || !arpSessionExpired(incompleteARP, now) {
		t.Fatal("已超过维护窗口或缺字段的风险会话未被识别为需要刷新")
	}
	if arpSessionExpired(udf9ARP, now) {
		t.Fatal("不透明的供应商标记不能被本地擅自判定为失效")
	}
}
