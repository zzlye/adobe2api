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
