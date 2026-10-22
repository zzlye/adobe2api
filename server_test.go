package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 上游繁忙与认证失效必须分开，避免一次超时触发不必要的重新登录。
func TestGenerationTimeoutPreservesSessionAndUnauthorizedExpiresAccount(t *testing.T) {
	for _, status := range []int{408, 401} {
		app, generator, _ := newProtocolApp(t)
		accounts, err := app.store.Accounts()
		if err != nil || len(accounts) != 1 {
			t.Fatal("测试账号创建失败")
		}
		account := accounts[0]
		account.ARPSession = "已验证的风险会话"
		if err := app.store.SaveAccount(account); err != nil {
			t.Fatal(err)
		}
		generator.failure = &UpstreamError{StatusCode: status, Message: "上游错误"}
		response := protocolRequest(app, "POST", "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "测试图片"})
		if response.Code != status {
			t.Fatalf("上游状态未保留: %d", response.Code)
		}
		accounts, err = app.store.Accounts()
		if err != nil || accounts[0].ARPSession != account.ARPSession {
			t.Fatal("上游错误擦除了风险会话")
		}
		if (accounts[0].Status == "expired") != (status == 401) {
			t.Fatalf("账号认证状态错误: %d %s", status, accounts[0].Status)
		}
	}
}

type blockedImageBody struct {
	reader  io.Reader
	started chan struct{}
	unblock <-chan struct{}
	once    sync.Once
}

func (b *blockedImageBody) Read(data []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.unblock
	return b.reader.Read(data)
}

// 慢速大型输入占用解析名额时及时拒绝新输入，完成后恢复接收，避免无限累积解码缓冲。
func TestLargeImageInputAdmissionIsBoundedAndReleased(t *testing.T) {
	app, _, _ := newProtocolApp(t)
	handler := app.Handler()
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	finished := make(chan struct{}, 2)
	body := `{"model":"nano-banana-2","prompt":"测试图片"}`
	for i := 0; i < 2; i++ {
		started := make(chan struct{})
		reader := &blockedImageBody{reader: strings.NewReader(body), started: started, unblock: unblock}
		request := httptest.NewRequest("POST", "/v1/images/generations", reader)
		request.Header.Set("Authorization", "Bearer sk-protocol-test-12345")
		request.Header.Set("Content-Type", "application/json")
		go func() { handler.ServeHTTP(httptest.NewRecorder(), request); finished <- struct{}{} }()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("输入读取没有启动")
		}
	}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk-protocol-test-12345")
		r.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		return response
	}
	if response := request(); response.Code != 503 {
		t.Fatalf("慢输入没有约束并发解析: %d", response.Code)
	}
	release()
	for i := 0; i < 2; i++ {
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Fatal("输入完成后名额未释放")
		}
	}
	if response := request(); response.Code != 200 {
		t.Fatalf("解析名额未恢复: %d %s", response.Code, response.Body.String())
	}
}

// 单账号每分钟请求上限超出时直接返回 429，无启用账号时保持 503。
func TestAccountRequestsPerMinuteAndRateLimit429(t *testing.T) {
	app, handler, cookie, generator := cookieAdminTestApp(t)
	generator.image = storageTestPNG(t)
	response := adminTestRequest(handler, "POST", "/api/admin/accounts/import", `{"name":"限流账号","cookie":"ims_sid=rpm-session; sso_sid=rpm-sso","requests_per_minute":1}`, cookie, "")
	if response.Code != 201 || !strings.Contains(response.Body.String(), `"requests_per_minute":1`) {
		t.Fatalf("导入时每分钟请求上限未保存: %d %s", response.Code, response.Body.String())
	}
	accounts, err := app.store.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].RequestsPerMinute != 1 {
		t.Fatal("数据库中每分钟请求上限不一致")
	}
	callGen := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"nano-banana-2","prompt":"限流测试"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+app.store.Settings().APIKeys[0])
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := callGen(); rec.Code != 200 {
		t.Fatalf("首次请求应成功: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callGen(); rec.Code != 429 {
		t.Fatalf("超过单账号每分钟请求上限应返回 429，实际: %d %s", rec.Code, rec.Body.String())
	}
	// 在账号列表直接调高每分钟请求上限后应立即放行。
	update := adminTestRequest(handler, "PUT", "/api/admin/accounts/"+accounts[0].ID, `{"requests_per_minute":2}`, cookie, "")
	if update.Code != 200 || !strings.Contains(update.Body.String(), `"requests_per_minute":2`) {
		t.Fatalf("更新每分钟请求上限失败: %d %s", update.Code, update.Body.String())
	}
	if rec := callGen(); rec.Code != 200 {
		t.Fatalf("调高每分钟请求上限后应放行第二次请求: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callGen(); rec.Code != 429 {
		t.Fatalf("再次超过每分钟上限应返回 429: %d %s", rec.Code, rec.Body.String())
	}
	listResp := adminTestRequest(handler, "GET", "/api/admin/accounts", "", cookie, "")
	if listResp.Code != 200 || !strings.Contains(listResp.Body.String(), `"generated_images":2`) || !strings.Contains(listResp.Body.String(), `"model":"gemini-3.1-flash-image-preview"`) {
		t.Fatalf("账号池统计未返回生成张数与分模型统计: %d %s", listResp.Code, listResp.Body.String())
	}
	// 停用全部账号时返回 503。
	adminTestRequest(handler, "PUT", "/api/admin/accounts/"+accounts[0].ID, `{"enabled":false}`, cookie, "")
	if rec := callGen(); rec.Code != 503 {
		t.Fatalf("无启用账号时应返回 503: %d %s", rec.Code, rec.Body.String())
	}
}

