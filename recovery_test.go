package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 使用与线上相同的完整风险会话结构，避免占位字符串掩盖 Cookie 回填问题。
func recoveryTestARP(sid string, age time.Duration) string {
	raw, _ := json.Marshal(map[string]string{"sid": sid, "ftr": "fixture_" + strconv.FormatInt(time.Now().Add(-age).UnixMilli(), 10) + "__UDFT5_31ck_tt", "ark": "fixture-ark", "bfp": "fixture-bfp"})
	return base64.StdEncoding.EncodeToString(raw)
}

func recoveryTestApp(t *testing.T, transport adobeTestTransport) (*App, *AdobeGenerator, Account) {
	t.Helper()
	app, _, _ := newProtocolApp(t)
	accounts, _ := app.store.Accounts()
	account := accounts[0]
	account.ARPSession = recoveryTestARP("previous", 0)
	account.Cookie = "ims_sid=previous; sherlockToken=" + account.ARPSession
	account.Status = "ready"
	account.LastRefresh = time.Now().Add(-time.Hour)
	account.ExpiresAt = time.Now().Add(12 * time.Hour)
	if err := app.store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: adobeTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == adobeCreditsURL {
			return adobeTestResponse(404, nil, `{}`), nil
		}
		return transport(r)
	})}
	g := &AdobeGenerator{store: app.store, client: client, downloadClient: client}
	app.generator = g
	return app, g, account
}

// 从正式路由触发真实适配器，408 后必须换掉 Cookie 中仍然很新的旧风险会话。
func TestRecovery408RebuildsRejectedCookieSession(t *testing.T) {
	for _, browserFails := range []bool{false, true} {
		t.Run(strconv.FormatBool(browserFails), func(t *testing.T) {
			posts, ims, browsers := 0, 0, 0
			fresh := recoveryTestARP("rebuilt", 0)
			app, g, original := recoveryTestApp(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == adobeIMSURL {
					ims++
					return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=rotated"}}, `{"access_token":"rotated-token","userId":"test-user","expires_in":86400000}`), nil
				}
				if r.URL.Path == "/v2/3p-images/generate-async" {
					posts++
					if posts == 1 {
						return adobeTestResponse(408, nil, `{"error_code":"timeout_error","message":"system under load"}`), nil
					}
					if r.Header.Get("X-Arp-Session-Id") != fresh {
						t.Error("408 重试仍然复用了 Cookie 内已被拒绝的旧风险会话")
					}
					return adobeTestResponse(200, nil, `{"outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/image"}}]}`), nil
				}
				return adobeTestResponse(200, http.Header{"Content-Type": {"image/png"}}, string(storageTestPNG(t))), nil
			})
			g.browserRefresh = func(_ context.Context, acc Account) (Account, error) {
				browsers++
				if browserFails {
					return acc, errors.New("浏览器初始化失败")
				}
				acc.ARPSession = fresh
				return acc, nil
			}
			response := protocolRequest(app, "POST", "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "风险恢复回归"})
			saved, _ := app.findAdminAccount(original.ID)
			if browsers != 1 {
				t.Errorf("风险拒绝后浏览器调用次数=%d，期望 1", browsers)
			}
			if saved.AccessToken != "rotated-token" || saved.Cookie == original.Cookie {
				t.Error("恢复失败丢失已确认的新令牌或轮换 Cookie")
			}
			if browserFails {
				if posts != 1 || response.Code == 200 {
					t.Error("浏览器失败仍重放了生成请求")
				}
			} else if posts != 2 || ims != 2 || response.Code != 200 || saved.ARPSession != fresh {
				t.Errorf("恢复链路不完整：posts=%d ims=%d status=%d", posts, ims, response.Code)
			}
		})
	}
}

// 已被接受的任务发生轮询、下载或后续图片失败时，绝不能从第一张开始重新提交。
func TestRecoveryNeverReplaysAcceptedGeneration(t *testing.T) {
	for _, stage := range []string{"轮询", "下载", "第二张"} {
		t.Run(stage, func(t *testing.T) {
			posts, ims := 0, 0
			app, _, original := recoveryTestApp(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == adobeIMSURL {
					ims++
					return adobeTestResponse(200, nil, `{"access_token":"new-token","userId":"test-user","expires_in":86400000}`), nil
				}
				if r.URL.Path == "/v2/3p-images/generate-async" {
					posts++
					if stage == "第二张" && posts > 1 {
						return adobeTestResponse(408, nil, `{"error_code":"timeout_error"}`), nil
					}
					if stage == "轮询" {
						return adobeTestResponse(200, http.Header{"Retry-After": {"0"}}, `{"links":{"result":{"href":"https://firefly-epo855232.adobe.io/jobs/result/fixture"}}}`), nil
					}
					return adobeTestResponse(200, nil, `{"outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/image"}}]}`), nil
				}
				if stage == "轮询" {
					return adobeTestResponse(408, nil, `{"error_code":"timeout_error"}`), nil
				}
				if stage == "下载" {
					return adobeTestResponse(401, nil, `{}`), nil
				}
				return adobeTestResponse(200, http.Header{"Content-Type": {"image/png"}}, string(storageTestPNG(t))), nil
			})
			n := 1
			if stage == "第二张" {
				n = 2
			}
			response := protocolRequest(app, "POST", "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "禁止重复提交", "n": n})
			if response.Code == 200 || posts != n || ims != 0 {
				t.Errorf("已接受任务被重放：posts=%d want=%d ims=%d status=%d", posts, n, ims, response.Code)
			}
			if stage == "下载" {
				saved, _ := app.findAdminAccount(original.ID)
				if saved.Status == "expired" {
					t.Error("签名图片地址失效不应将账号登录标记为过期")
				}
			}
		})
	}
}

// 旧令牌尚有余量不能成为丢弃本次 IMS 轮换结果的理由。
func TestRecoveryMaintenanceKeepsPartialRotation(t *testing.T) {
	app, g, account := recoveryTestApp(t, func(r *http.Request) (*http.Response, error) {
		return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=maintenance-rotated"}}, `{"access_token":"maintenance-token","userId":"test-user","expires_in":86400000}`), nil
	})
	account.ARPSession = recoveryTestARP("expired", 5*time.Hour)
	if err := app.store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	g.browserRefresh = func(_ context.Context, acc Account) (Account, error) {
		return acc, errors.New("页面临时加载失败")
	}
	app.refreshAccounts(context.Background())
	saved, _ := app.findAdminAccount(account.ID)
	if saved.AccessToken != "maintenance-token" || saved.Cookie == account.Cookie || !saved.LastRefresh.After(account.LastRefresh) {
		t.Fatal("后台风险续期失败，丢失已轮换凭据")
	}
}

// 两个真实生成链路先同时失败，再共享一次恢复；不能串行化正常提交或并发轮换 Cookie。
func TestRecoveryConcurrentRequestsShareOneRefresh(t *testing.T) {
	for _, status := range []int{401, 408} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var firstPosts, ims, browsers atomic.Int32
			both := make(chan struct{})
			fresh := recoveryTestARP("concurrent-rebuilt", 0)
			app, g, account := recoveryTestApp(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == adobeIMSURL {
					ims.Add(1)
					return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=concurrent-rotated"}}, `{"access_token":"concurrent-token","userId":"test-user","expires_in":86400000}`), nil
				}
				if r.URL.Path == "/v2/3p-images/generate-async" {
					if r.Header.Get("Authorization") == "Bearer test-token" {
						if firstPosts.Add(1) == 2 {
							close(both)
						}
						select {
						case <-both:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
						return adobeTestResponse(status, nil, `{"error_code":"timeout_error"}`), nil
					}
					if status == 408 && r.Header.Get("X-Arp-Session-Id") != fresh {
						t.Error("并发恢复未使用新的风险会话")
					}
					return adobeTestResponse(200, nil, `{"outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/image"}}]}`), nil
				}
				return adobeTestResponse(200, http.Header{"Content-Type": {"image/png"}}, string(storageTestPNG(t))), nil
			})
			account.RequestsPerMinute = 5
			if err := app.store.SaveAccount(account); err != nil {
				t.Fatal(err)
			}
			g.browserRefresh = func(_ context.Context, acc Account) (Account, error) {
				browsers.Add(1)
				acc.ARPSession = fresh
				return acc, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _, err := app.runGenerate(ctx, GenerateInput{Model: "nano-banana-2", Prompt: "并发恢复测试"}, "test", "/test")
					if err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			wantIMS, wantBrowsers := int32(1), int32(0)
			if status == 408 {
				wantIMS, wantBrowsers = 2, 1
			}
			if firstPosts.Load() != 2 || ims.Load() != wantIMS || browsers.Load() != wantBrowsers {
				t.Errorf("并发去重失败 posts=%d ims=%d browsers=%d", firstPosts.Load(), ims.Load(), browsers.Load())
			}
		})
	}
}

func TestRecoveryCancellationKeepsConfirmedRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app, _, account := recoveryTestApp(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == adobeIMSURL {
			cancel()
			return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=cancelled-rotation"}}, `{"access_token":"cancelled-token","userId":"test-user","expires_in":86400000}`), nil
		}
		return adobeTestResponse(408, nil, `{"error_code":"timeout_error"}`), nil
	})
	_, entry, err := app.runGenerate(ctx, GenerateInput{Model: "nano-banana-2", Prompt: "取消恢复测试"}, "test", "/test")
	if !errors.Is(err, context.Canceled) || entry.Status != "cancelled" {
		t.Fatalf("取消丢失语义：%v %s", err, entry.Status)
	}
	saved, _ := app.findAdminAccount(account.ID)
	if saved.AccessToken != "cancelled-token" || !saved.RiskRefreshRequired {
		t.Fatal("取消后丢失已轮换令牌或恢复意图")
	}
	gate := app.sessionGate(account.ID)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.readers != 0 || gate.writing || len(gate.writers) != 0 {
		t.Fatal("取消泄漏会话锁")
	}
}

// 失败状态与任务链接同时存在时无法证明未受理，宁可返回错误也不能再次扣费。
func TestRecoveryDoesNotReplayAmbiguousRejection(t *testing.T) {
	for _, header := range []http.Header{{"X-Task-Status": {"ACCEPTED"}}, {"X-Override-Status-Link": {"https://firefly-epo855232.adobe.io/jobs/result/fixture"}}} {
		posts, ims := 0, 0
		app, _, _ := recoveryTestApp(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.String() == adobeIMSURL {
				ims++
				return adobeTestResponse(401, nil, `{}`), nil
			}
			posts++
			return adobeTestResponse(408, header, `{"error_code":"timeout_error"}`), nil
		})
		response := protocolRequest(app, "POST", "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "歧义响应不重放"})
		if response.Code != 408 || posts != 1 || ims != 0 {
			t.Fatal("歧义拒绝仍触发整单重放")
		}
	}
}
