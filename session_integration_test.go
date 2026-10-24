package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 独立适配器控制生成和续期的交错，不访问外部服务或消耗账号积分。
type sessionIntegrationGenerator struct {
	generate func(context.Context, Account, GenerateInput, *RequestLog) (GenerateResult, error)
	refresh  func(context.Context, Account) (Account, error)
}

func (g *sessionIntegrationGenerator) Generate(ctx context.Context, account Account, input GenerateInput, entry *RequestLog) (GenerateResult, error) {
	return g.generate(ctx, account, input, entry)
}

func (g *sessionIntegrationGenerator) Refresh(ctx context.Context, account Account) (Account, error) {
	return g.refresh(ctx, account)
}

func sessionIntegrationApp(t *testing.T) (*App, Account) {
	t.Helper()
	app, _, _ := newProtocolApp(t)
	settings := app.store.Settings()
	settings.MaxConcurrent = 4
	if err := app.store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	accounts, err := app.store.Accounts()
	if err != nil || len(accounts) != 1 {
		t.Fatal("没有建立唯一的测试账号")
	}
	account := accounts[0]
	account.Cookie, account.ARPSession, account.Status = "ims_sid=original", recoveryTestARP("original", 0), "ready"
	account.LastRefresh, account.RequestsPerMinute = time.Now().Add(-time.Hour), 4
	if err := app.store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	return app, account
}

// 一个请求被拒绝时，同账号已被接受的慢任务必须先结束，不能在轮询过程中轮换会话。
func TestSessionIntegrationRefreshWaitsForActualGeneration(t *testing.T) {
	app, original := sessionIntegrationApp(t)
	slowEntered, slowRelease := make(chan struct{}), make(chan struct{})
	var refreshed atomic.Bool
	app.generator = &sessionIntegrationGenerator{
		generate: func(ctx context.Context, account Account, input GenerateInput, _ *RequestLog) (GenerateResult, error) {
			if input.Prompt == "慢任务" {
				close(slowEntered)
				select {
				case <-slowRelease:
					return GenerateResult{}, nil
				case <-ctx.Done():
					return GenerateResult{}, ctx.Err()
				}
			}
			if account.ARPSession == original.ARPSession {
				return GenerateResult{}, &UpstreamError{StatusCode: 408, Code: "timeout_error", Message: "风险会话被拒绝", RetrySafe: true}
			}
			return GenerateResult{}, nil
		},
		refresh: func(_ context.Context, account Account) (Account, error) {
			refreshed.Store(true)
			account.ARPSession, account.LastRefresh = recoveryTestARP("renewed", 0), time.Now()
			account.RiskRefreshRequired = false
			return account, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var requests sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		requests.Wait()
	})
	finished := make(chan error, 2)
	start := func(prompt string) {
		requests.Go(func() {
			_, _, err := app.runGenerate(ctx, GenerateInput{Model: "nano-banana-2", Prompt: prompt}, "openai", "/v1/images/generations")
			finished <- err
		})
	}
	start("慢任务")
	select {
	case <-slowEntered:
	case <-ctx.Done():
		t.Fatal("慢生成没有开始")
	}
	start("失败任务")
	waitSessionGateWriters(t, app.sessionGate(original.ID), 1)
	if refreshed.Load() {
		t.Fatal("真实生成尚未完成就开始轮换会话")
	}
	close(slowRelease)
	for range 2 {
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("在途生成退出后，恢复仍没有完成")
		}
	}
	if !refreshed.Load() {
		t.Fatal("慢任务结束后没有恢复被拒绝的会话")
	}
}

// 显式重现旧生成先释放读侧、恢复失败先保存、旧成功结果最后落盘的收尾顺序。
func TestSessionIntegrationOldResultCannotUndoFailedRecovery(t *testing.T) {
	app, original := sessionIntegrationApp(t)
	app.generator = &sessionIntegrationGenerator{refresh: func(_ context.Context, account Account) (Account, error) {
		return account, errors.New("IMS 暂时不可用")
	}}
	old, release, err := app.readGenerationAccount(context.Background(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	updated, err := app.refreshGenerationAccount(context.Background(), original, &UpstreamError{StatusCode: 408, Code: "timeout_error", RetrySafe: true})
	if err == nil {
		t.Fatal("测试没有进入首次 IMS 失败分支")
	}
	updated.Status, updated.LastError = "error", err.Error()
	if err := app.saveGenerationAccount(updated); err != nil {
		t.Fatal(err)
	}
	old.Status, old.LastError = "ready", ""
	if err := app.saveGenerationAccount(old); err != nil {
		t.Fatal(err)
	}
	saved, err := app.findAdminAccount(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.RiskRefreshRequired || saved.Status != "error" || saved.LastError != updated.LastError || !saved.LastRefresh.After(original.LastRefresh) {
		t.Fatal("旧生成结果撤销了新一轮失败恢复的风险意图或状态")
	}
}

// 模拟真实数据库写入失败，确认恢复返回错误后独占写侧仍能正常释放。
func TestSessionIntegrationSaveFailureReleasesWriter(t *testing.T) {
	app, original := sessionIntegrationApp(t)
	app.generator = &sessionIntegrationGenerator{refresh: func(_ context.Context, account Account) (Account, error) {
		account.ARPSession, account.LastRefresh = recoveryTestARP("renewed", 0), time.Now()
		account.RiskRefreshRequired = false
		return account, nil
	}}
	if _, err := app.store.db.Exec("CREATE TRIGGER session_test_fail_save BEFORE UPDATE ON accounts BEGIN SELECT RAISE(ABORT, 'session-test-save-failure'); END;"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := app.store.db.Exec("DROP TRIGGER session_test_fail_save"); err != nil {
			t.Error(err)
		}
	}()
	if _, err := app.refreshGenerationAccount(context.Background(), original, &UpstreamError{StatusCode: 408, Code: "timeout_error", RetrySafe: true}); err == nil {
		t.Fatal("恢复没有传递数据库保存失败")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := app.sessionGate(original.ID).acquire(ctx, true)
	if err != nil {
		t.Fatal("保存失败后写侧未释放", err)
	}
	release()
}

// 页面中间状态不能仅凭一个供应商字段通过后端检查，已确认的 IMS 轮换仍需保留。
func TestSessionIntegrationBrowserPartialARPIsRejected(t *testing.T) {
	imsRequests := 0
	_, generator, original := recoveryTestApp(t, func(*http.Request) (*http.Response, error) {
		imsRequests++
		return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=confirmed-rotation"}}, `{"access_token":"confirmed-token","userId":"test-user","expires_in":86400000}`), nil
	})
	original.RiskRefreshRequired = true
	raw, _ := json.Marshal(map[string]string{"sid": "new-sid", "bfp": "new-bfp"})
	partialARP := base64.StdEncoding.EncodeToString(raw)
	generator.browserRefresh = func(_ context.Context, account Account) (Account, error) {
		account.ARPSession = partialARP
		return account, nil
	}
	updated, err := generator.Refresh(context.Background(), original)
	if err == nil || imsRequests != 1 {
		t.Fatal("不完整的浏览器风险会话被当作完整凭据继续续期")
	}
	if updated.ARPSession != original.ARPSession || !updated.RiskRefreshRequired || updated.AccessToken != "confirmed-token" || updated.Cookie == original.Cookie {
		t.Fatal("拒绝中间态后未保留原风险会话、恢复意图和已经确认的 IMS 轮换")
	}
}
