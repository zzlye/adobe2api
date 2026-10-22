package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// 测试生成器只返回本地图片，协议回归不会访问 Adobe 或消耗账号积分。
type protocolFakeGenerator struct {
	mu       sync.Mutex
	input    GenerateInput
	images   []ImageRecord
	failure  error
	wait     bool
	started  chan struct{}
	canceled chan struct{}
}

func (f *protocolFakeGenerator) Refresh(ctx context.Context, account Account) (Account, error) {
	return account, nil
}
func (f *protocolFakeGenerator) Generate(ctx context.Context, account Account, input GenerateInput, entry *RequestLog) (GenerateResult, error) {
	f.mu.Lock()
	f.input = input
	f.mu.Unlock()
	if f.wait {
		close(f.started)
		<-ctx.Done()
		close(f.canceled)
		return GenerateResult{}, ctx.Err()
	}
	if f.failure != nil {
		return GenerateResult{}, f.failure
	}
	credits := 3.0
	return GenerateResult{Images: f.images, Credits: &credits}, nil
}
func newProtocolApp(t *testing.T) (*App, *protocolFakeGenerator, []byte) {
	t.Helper()
	store := newStorageTestStore(t)
	settings := store.Settings()
	settings.APIKeys = []string{"sk-protocol-test-12345"}
	settings.PublicURL = "https://images.example.com"
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: newID(), Name: "测试账号", Enabled: true, AccessToken: "test-token", UserID: "test-user", Status: "active", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	data := storageTestPNG(t)
	image, err := store.SaveImage(bytes.NewReader(data), "image/png", "generated")
	if err != nil {
		t.Fatal(err)
	}
	fake := &protocolFakeGenerator{images: []ImageRecord{image}}
	return &App{store: store, generator: fake}, fake, data
}
func protocolRequest(app *App, method, path string, body any) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer sk-protocol-test-12345")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder
}
func requireProtocolStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("状态=%d，期望=%d，正文=%s", response.Code, status, response.Body.String())
	}
}

func TestProtocolFullRoutesAuthenticationAndModelDirectories(t *testing.T) {
	app, _, _ := newProtocolApp(t)
	for _, path := range []string{"/v1/models", "/v1beta/models"} {
		request := httptest.NewRequest("GET", path, nil)
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		requireProtocolStatus(t, response, 401)
	}
	openai := protocolRequest(app, "GET", "/v1/models", nil)
	requireProtocolStatus(t, openai, 200)
	if !strings.Contains(openai.Body.String(), `"object":"list"`) {
		t.Fatal(openai.Body.String())
	}
	gemini := protocolRequest(app, "GET", "/v1beta/models", nil)
	requireProtocolStatus(t, gemini, 200)
	if !strings.Contains(gemini.Body.String(), `"supportedGenerationMethods"`) {
		t.Fatal(gemini.Body.String())
	}
	page := protocolRequest(app, "GET", "/", nil)
	requireProtocolStatus(t, page, 200)
	if !strings.Contains(page.Body.String(), "Adobe 2API") {
		t.Fatal("管理页面没有返回")
	}
	requireProtocolStatus(t, protocolRequest(app, "GET", "/api/not-found", nil), 404)
	requireProtocolStatus(t, protocolRequest(app, "GET", "/v1/not-found", nil), 404)
	requireProtocolStatus(t, protocolRequest(app, "POST", "/", map[string]string{}), 405)
}

func TestProtocolOpenAIURLAndBase64RoundTrip(t *testing.T) {
	app, _, data := newProtocolApp(t)
	for _, format := range []string{"url", "b64_json"} {
		t.Run(format, func(t *testing.T) {
			response := protocolRequest(app, "POST", "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "测试图片", "size": "1024x1024", "response_format": format})
			requireProtocolStatus(t, response, 200)
			var body struct {
				Data []struct {
					URL    string `json:"url"`
					Base64 string `json:"b64_json"`
				} `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Data) != 1 {
				t.Fatal("图片数量错误")
			}
			if format == "b64_json" {
				decoded, err := base64.StdEncoding.DecodeString(body.Data[0].Base64)
				if err != nil || !bytes.Equal(decoded, data) {
					t.Fatal("编码图片没有正确往返")
				}
			} else {
				if !strings.HasPrefix(body.Data[0].URL, "https://images.example.com/images/") {
					t.Fatal(body.Data[0].URL)
				}
				path := strings.TrimPrefix(body.Data[0].URL, "https://images.example.com")
				request := httptest.NewRequest("GET", path, nil)
				recorder := httptest.NewRecorder()
				app.Handler().ServeHTTP(recorder, request)
				requireProtocolStatus(t, recorder, 200)
				if !bytes.Equal(recorder.Body.Bytes(), data) || recorder.Header().Get("Content-Type") != "image/png" {
					t.Fatal("公开图片内容错误")
				}
			}
		})
	}
}

func TestProtocolMultipartAndDataURLReferences(t *testing.T) {
	app, fake, data := newProtocolApp(t)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("prompt", "参考图编辑")
	_ = writer.WriteField("model", "nano-banana-pro")
	for range 2 {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="image[]"; filename="参考图.png"`)
		header.Set("Content-Type", "image/png")
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(data)
	}
	_ = writer.Close()
	request := httptest.NewRequest("POST", "/v1/images/edits", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer sk-protocol-test-12345")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	requireProtocolStatus(t, response, 200)
	if len(fake.input.References) != 2 || fake.input.References[0].Path == "" {
		t.Fatal("参考图没有流式落盘")
	}
	response = protocolRequest(app, "POST", "/v1/images/edits", map[string]any{"prompt": "Data URL参考图", "images": []any{map[string]string{"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}}})
	requireProtocolStatus(t, response, 200)
	if len(fake.input.References) != 1 || fake.input.References[0].MIME != "image/png" {
		t.Fatal("Data URL参考图格式错误")
	}
	// 中转或客户端声明的 MIME 与实际图片文件头不一致时，应自动按真实文件头识别而不报错。
	response = protocolRequest(app, "POST", "/v1/images/edits", map[string]any{"prompt": "声明MIME不一致兼容", "images": []any{map[string]string{"image_url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)}}})
	requireProtocolStatus(t, response, 200)
	if len(fake.input.References) != 1 || fake.input.References[0].MIME != "image/png" {
		t.Fatal("未按真实图片文件头修正参考图 MIME")
	}
	response = protocolRequest(app, "POST", "/v1/images/edits", map[string]any{"prompt": "无效遮罩", "mask": "file-mask", "images": []any{map[string]string{"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}}})
	requireProtocolStatus(t, response, 400)
}

func TestProtocolGeminiInlineDataAndStreamFrames(t *testing.T) {
	app, fake, data := newProtocolApp(t)
	request := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "Gemini参考图"}, map[string]any{"inlineData": map[string]string{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(data)}}}}}, "generationConfig": map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}, "imageConfig": map[string]string{"aspectRatio": "16:9", "imageSize": "2K"}}}
	for _, path := range []string{"/v1beta/models/nano-banana-2:generateContent", "/v1/models/nano-banana-2:generateContent", "/v1beta/models/nano-banana-2:streamGenerateContent?alt=sse"} {
		response := protocolRequest(app, "POST", path, request)
		requireProtocolStatus(t, response, 200)
		raw := response.Body.String()
		if strings.Contains(path, "stream") {
			if !strings.HasPrefix(raw, "data: ") || strings.Contains(raw, "[DONE]") {
				t.Fatal(raw)
			}
			raw = strings.TrimSpace(strings.TrimPrefix(raw, "data: "))
		}
		var result struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Inline struct {
							MIME string `json:"mimeType"`
							Data string `json:"data"`
						} `json:"inlineData"`
					} `json:"parts"`
				} `json:"content"`
				Finish string `json:"finishReason"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Candidates) != 1 || result.Candidates[0].Finish != "STOP" {
			t.Fatal(raw)
		}
		decoded, err := base64.StdEncoding.DecodeString(result.Candidates[0].Content.Parts[0].Inline.Data)
		if err != nil || !bytes.Equal(decoded, data) {
			t.Fatal("Gemini图片编码错误")
		}
	}
	if fake.input.AspectRatio != "16:9" || fake.input.Resolution != "2K" || len(fake.input.References) != 1 {
		t.Fatalf("Gemini参数转换错误：%+v", fake.input)
	}
}

func TestProtocolChatSSESuccessAndFailure(t *testing.T) {
	app, fake, data := newProtocolApp(t)
	body := map[string]any{"model": "nano-banana-2", "messages": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": "聊天生图"}, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}}}}}, "stream": true}
	response := protocolRequest(app, "POST", "/v1/chat/completions", body)
	requireProtocolStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), `"object":"chat.completion.chunk"`) || !strings.Contains(response.Body.String(), `"finish_reason":"stop"`) || !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
		t.Fatal(response.Body.String())
	}
	fake.failure = errors.New("测试上游失败")
	response = protocolRequest(app, "POST", "/v1/chat/completions", body)
	requireProtocolStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), "event: error") || !strings.Contains(response.Body.String(), `"code":502`) || !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatal(response.Body.String())
	}
	response = protocolRequest(app, "POST", "/v1beta/models/nano-banana-pro:streamGenerateContent", map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": "流式错误"}}}}})
	requireProtocolStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), `"status":"UNAVAILABLE"`) || strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatal(response.Body.String())
	}
}

func TestProtocolStreamCancellationReleasesTask(t *testing.T) {
	app, fake, _ := newProtocolApp(t)
	fake.wait = true
	fake.started = make(chan struct{})
	fake.canceled = make(chan struct{})
	body := strings.NewReader(`{"model":"nano-banana-2","messages":[{"role":"user","content":"取消生成"}],"stream":true}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("POST", "/v1/chat/completions", body).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer sk-protocol-test-12345")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { app.Handler().ServeHTTP(response, request); close(done) }()
	select {
	case <-fake.started:
	case <-time.After(3 * time.Second):
		t.Fatal("生成没有启动")
	}
	cancel()
	select {
	case <-fake.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("请求取消没有传到生成器")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("流式连接没有退出")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		app.poolMu.Lock()
		active := app.active
		app.poolMu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("账号任务没有释放")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProtocolRejectsPrivateRemoteReferencesAndUsesLocalRecords(t *testing.T) {
	app, fake, _ := newProtocolApp(t)
	for _, raw := range []string{"http://127.0.0.1:1/private", "http://localhost:1/private", "http://169.254.169.254/latest/meta-data/", "file:///etc/passwd", "https://user:password@example.com/image.png"} {
		response := protocolRequest(app, "POST", "/v1/images/edits", map[string]any{"prompt": "拒绝内网图片", "images": []any{map[string]string{"image_url": raw}}})
		requireProtocolStatus(t, response, 400)
	}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.1.1", "0.0.0.0", "::1", "::ffff:127.0.0.1", "fc00::1", "2001:db8::1", "::127.0.0.1"} {
		if publicReferenceIP(netip.MustParseAddr(raw)) {
			t.Errorf("不应允许地址 %s", raw)
		}
	}
	if !publicReferenceIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("公开地址被误拒绝")
	}
	// 即使重定向 URL 通过格式检查，实际连接仍必须重新解析并拒绝内网目标。
	client := referenceClient()
	transport := client.Transport.(*http.Transport)
	connection, err := transport.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil || connection != nil || !strings.Contains(err.Error(), "内网") {
		t.Fatal("重定向连接没有在拨号前拦截内网地址")
	}
	redirect, _ := url.Parse("http://127.0.0.1/private")
	if client.CheckRedirect(&http.Request{URL: redirect}, make([]*http.Request, 4)) == nil {
		t.Fatal("没有限制重定向次数")
	}
	local := fake.images[0]
	response := protocolRequest(app, "POST", "/v1/images/edits", map[string]any{"prompt": "引用本服务图片", "images": []any{map[string]string{"image_url": "https://images.example.com/images/" + local.ID}}})
	requireProtocolStatus(t, response, 200)
	if fake.input.References[0].Path != local.Path {
		t.Fatal("本服务图片没有从本地索引读取")
	}
	response = protocolRequest(app, "POST", "/v1/images/generations", map[string]any{"prompt": "忽略伪造元信息", "references": []any{map[string]any{"id": local.ID, "mime_type": "text/html", "bytes": -100, "url": "http://127.0.0.1/private", "path": "/etc/passwd"}}})
	requireProtocolStatus(t, response, 200)
	if fake.input.References[0].MIME != "image/png" || fake.input.References[0].Bytes != local.Bytes {
		t.Fatal("参考图客户端元信息没有被服务端覆盖")
	}
	if err := appendReference(&GenerateInput{References: []ImageRecord{{Bytes: protocolBodyLimit}}}, ImageRecord{Bytes: 1}); err == nil {
		t.Fatal("参考图总大小限制没有执行")
	}
}

func TestProtocolValidatesModelRatioResolutionAndUltrawideSizes(t *testing.T) {
	valid := []struct{ size, ratio, resolution string }{{"1024x1024", "1:1", "1K"}, {"1024x1792", "9:16", "1K"}, {"2048x2048", "1:1", "2K"}, {"3072x384", "8:1", "1K"}, {"12288x1536", "8:1", "4K"}}
	for _, test := range valid {
		input := GenerateInput{Prompt: "尺寸测试", Size: test.size}
		if err := validateInput(&input); err != nil {
			t.Fatal(test.size, err)
		}
		if input.AspectRatio != test.ratio || input.Resolution != test.resolution {
			t.Fatalf("%s 映射为 %s/%s", test.size, input.AspectRatio, input.Resolution)
		}
	}
	app, _, _ := newProtocolApp(t)
	cases := []map[string]any{{"model": "unknown", "prompt": "测试"}, {"model": "nano-banana-pro", "prompt": "测试", "resolution": "512"}, {"model": "gpt-image-1.5", "prompt": "测试", "aspect_ratio": "8:1"}, {"prompt": "测试", "n": 5}, {"prompt": "测试", "quality": "ultra"}, {"prompt": "测试", "size": "wrong"}, {"prompt": "测试", "response_format": "file"}, {"prompt": ""}}
	for _, body := range cases {
		requireProtocolStatus(t, protocolRequest(app, "POST", "/v1/images/generations", body), 400)
	}
	refs := make([]ImageRecord, 7)
	input := GenerateInput{Model: "nano-banana-2", Prompt: "太多参考图", References: refs}
	if validateInput(&input) == nil {
		t.Fatal("香蕉 2 模型参考图上限没有执行")
	}
}
