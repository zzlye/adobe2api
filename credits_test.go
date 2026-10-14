package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func creditSnapshot(t *testing.T, total, used, available float64, period string) *adobeCreditBalance {
	t.Helper()
	payload := fmt.Sprintf(`{"total":{"quota":{"total":%g,"used":%g,"available":%g},"planCap":"HARD","availableUntil":%q}}`, total, used, available, period)
	var balance adobeCreditBalance
	if err := json.Unmarshal([]byte(payload), &balance); err != nil {
		t.Fatal(err)
	}
	return &balance
}

func TestCreditConsumptionRequiresConsistentPeriodAndQuota(t *testing.T) {
	period := "2027-10-22T09:09:24.208Z"
	before := creditSnapshot(t, 100, 10, 90, period)
	tests := []struct {
		name     string
		after    *adobeCreditBalance
		expected *float64
	}{
		{"正常扣费", creditSnapshot(t, 100, 13, 87, period), floatPointer(3)},
		{"明确零消耗", creditSnapshot(t, 100, 10, 90, period), floatPointer(0)},
		{"周期重置", creditSnapshot(t, 100, 0, 100, "2027-11-22T09:09:24.208Z"), nil},
		{"额度改变", creditSnapshot(t, 120, 13, 107, period), nil},
		{"使用量回退", creditSnapshot(t, 100, 5, 95, period), nil},
		{"快照矛盾", creditSnapshot(t, 100, 13, 88, period), nil},
		{"未知余额", nil, nil},
		{"无有效周期", creditSnapshot(t, 100, 13, 87, ""), nil},
		{"周期无效", creditSnapshot(t, 100, 13, 87, "invalid"), nil},
		{"周期已过期", creditSnapshot(t, 100, 13, 87, "2020-10-22T09:09:24.208Z"), nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := creditConsumption(before, test.after)
			if test.expected == nil {
				if got != nil {
					t.Fatalf("应保留未知，得到 %v", *got)
				}
			} else if got == nil || *got != *test.expected {
				t.Fatalf("实际消耗错误：%v", got)
			}
		})
	}
	// 缺失字段不能被默认为零，套餐策略改变也不能跨快照计算。
	after := creditSnapshot(t, 100, 13, 87, period)
	after.Total.Quota.Used = nil
	if creditConsumption(before, after) != nil {
		t.Fatal("缺失使用量应保持未知")
	}
	after = creditSnapshot(t, 100, 13, 87, period)
	after.Total.PlanCap = "SOFT"
	if creditConsumption(before, after) != nil {
		t.Fatal("套餐策略改变应保持未知")
	}
	if creditConsumption(creditSnapshot(t, 100, 10, 90, "2020-10-22T09:09:24.208Z"), creditSnapshot(t, 100, 13, 87, "2020-10-22T09:09:24.208Z")) != nil {
		t.Fatal("已过期周期的缓存快照应保持未知")
	}
}

func floatPointer(value float64) *float64 { return &value }

func TestAdobeBalanceQueryUsesObservedHeadersAndSanitizedLog(t *testing.T) {
	account := Account{AccessToken: "secret-token", UserID: "user@AdobeID", Cookie: "secret-cookie", ARPSession: "secret-arp", APIKey: "generation-key"}
	generator := newAdobeTestGenerator(t, nil)
	client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != adobeCreditsURL || request.Header.Get("Authorization") != "Bearer secret-token" || request.Header.Get("X-Account-Id") != account.UserID || request.Header.Get("X-Api-Key") != "SunbreakWebUI1" {
			t.Fatal("余额查询必须使用页面已验证的认证和客户端标识")
		}
		if request.Header.Get("Cookie") != "" || request.Header.Get("X-Arp-Session-Id") != "" {
			t.Fatal("余额查询不应发送多余凭据")
		}
		return adobeTestResponse(200, http.Header{"X-Request-Id": {"balance-request"}}, `{"total":{"quota":{"total":100,"used":10,"available":90},"planCap":"HARD","availableUntil":"2027-10-22T09:09:24.208Z"}}`), nil
	})}
	entry := RequestLog{}
	balance, err := generator.fetchCreditBalance(context.Background(), client, account, &entry)
	if err != nil || balance == nil || *balance.Total.Quota.Available != 90 {
		t.Fatalf("余额读取失败：%v", err)
	}
	encoded, _ := json.Marshal(entry.Upstream)
	if len(entry.Upstream) != 1 || entry.Upstream[0].RequestID != "balance-request" || strings.Contains(string(encoded), "secret-") || strings.Contains(string(encoded), account.UserID) {
		t.Fatalf("请求记录错误或泄露认证信息：%s", encoded)
	}
}

func TestAdobeBalanceMissingInvalidAndFailedResponsesStayUnknown(t *testing.T) {
	account := Account{AccessToken: "test-token", UserID: "user@AdobeID"}
	generator := newAdobeTestGenerator(t, nil)
	for _, payload := range []string{`{}`, `{"total":{"quota":{"total":100,"available":100}}}`, `{"total":{"quota":{"total":100,"used":10,"available":99}}}`, strings.Repeat(" ", 129<<10) + `{}`} {
		client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
			return adobeTestResponse(200, nil, payload), nil
		})}
		if generator.readCreditBalance(context.Background(), client, account, nil) != nil {
			t.Fatal("无效余额应保持未知")
		}
	}
	for _, status := range []int{401, 429, 503} {
		client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
			return adobeTestResponse(status, nil, `{}`), nil
		})}
		if generator.readCreditBalance(context.Background(), client, account, nil) != nil {
			t.Fatal("读取失败不能猜测余额")
		}
	}
	client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
		return nil, errors.New("连接失败")
	})}
	if generator.readCreditBalance(context.Background(), client, account, nil) != nil {
		t.Fatal("网络失败应保持未知")
	}
}

func TestAdobeGenerationRecordsActualBalanceDifference(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jfGQAAAAASUVORK5CYII=")
	for _, test := range []struct {
		name       string
		count      int
		failureAt  int
		wantImages int
	}{
		{"生成成功", 1, 0, 1},
		{"生成失败仍核对已发生消耗", 1, 1, 0},
		{"多图部分失败保留图片与消耗", 2, 2, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			generator := newAdobeTestGenerator(t, nil)
			queries, posts := 0, 0
			client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/v1/credits/balance":
					queries++
					used, available := 10, 90
					if queries == 2 {
						used, available = 12, 88
					}
					return adobeTestResponse(200, nil, fmt.Sprintf(`{"total":{"quota":{"total":100,"used":%d,"available":%d},"planCap":"HARD","availableUntil":"2027-10-22T09:09:24.208Z"}}`, used, available)), nil
				case "/v2/3p-images/generate-async":
					posts++
					if test.failureAt == posts {
						return adobeTestResponse(422, nil, `{"error":{"message":"生成失败"}}`), nil
					}
					return adobeTestResponse(200, http.Header{"X-Task-Status": {"COMPLETED"}}, `{"contentType":"image/png","outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/image"}}]}`), nil
				case "/image":
					return adobeTestResponse(200, http.Header{"Content-Type": {"image/png"}}, string(png)), nil
				default:
					return nil, errors.New("意外请求")
				}
			})}
			generator.client, generator.downloadClient = client, client
			entry := RequestLog{}
			result, err := generator.Generate(context.Background(), Account{AccessToken: "test-token", UserID: "user@AdobeID"}, GenerateInput{Model: "nano-banana-2", Prompt: "猫", N: test.count}, &entry)
			if (test.failureAt > 0) != (err != nil) || result.Credits == nil || *result.Credits != 2 || queries != 2 || posts != test.count || len(result.Images) != test.wantImages {
				t.Fatalf("生成与余额核对错误：credits=%v queries=%d posts=%d err=%v", result.Credits, queries, posts, err)
			}
		})
	}
}

func TestAdobeRefreshReadsBalanceWithoutRepeatingBrowser(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			generator := newAdobeTestGenerator(t, nil)
			browserCalls, balanceCalls := 0, 0
			generator.browserRefresh = func(ctx context.Context, account Account) (Account, error) {
				browserCalls++
				account.AccessToken = "refreshed-token"
				account.ARPSession = "refreshed-arp"
				account.Cookie = "ims_sid=browser-recovered"
				return account, nil
			}
			client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() == adobeIMSURL {
					return adobeTestResponse(200, nil, `{"access_token":"refreshed-token","userId":"user@AdobeID"}`), nil
				}
				balanceCalls++
				if request.URL.String() != adobeCreditsURL || request.Header.Get("Authorization") != "Bearer refreshed-token" {
					t.Fatal("刷新余额应使用新登录状态")
				}
				return adobeTestResponse(status, nil, `{"total":{"quota":{"total":100,"used":10,"available":90}}}`), nil
			})}
			generator.client, generator.downloadClient = client, client
			account, err := generator.Refresh(context.Background(), Account{UserID: "user@AdobeID", Credits: floatPointer(10)})
			if err != nil || browserCalls != 1 || balanceCalls != 1 || account.AccessToken != "refreshed-token" {
				t.Fatalf("刷新流程错误：browser=%d balance=%d err=%v", browserCalls, balanceCalls, err)
			}
			if status == 200 {
				if account.Credits == nil || *account.Credits != 90 {
					t.Fatal("账号池应显示查询到的可用余额")
				}
			} else if account.Credits != nil {
				t.Fatal("余额查询失败不能沿用旧余额伪装为实时结果")
			}
		})
	}
}
