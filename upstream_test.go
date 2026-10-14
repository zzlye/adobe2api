package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type adobeTestTransport func(*http.Request) (*http.Response, error)

func (transport adobeTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func adobeTestResponse(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func newAdobeTestGenerator(t *testing.T, transport adobeTestTransport) *AdobeGenerator {
	t.Helper()
	store, err := NewStore(storageTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
		// 原有生成链路测试不提供额度快照，辅助查询失败时应保持消耗未知。
		if request.URL.String() == adobeCreditsURL {
			return adobeTestResponse(http.StatusNotFound, nil, `{}`), nil
		}
		return transport.RoundTrip(request)
	})}
	return &AdobeGenerator{store: store, client: client, downloadClient: client}
}

func TestAdobeNonceMatchesJavaScriptUTF16(t *testing.T) {
	// 预期值由页面相同的 TextEncoder/SHA-256 流程生成，覆盖代理对截断边界。
	tests := []struct{ prompt, expected string }{
		{"一只猫", "5d70ff8ceac75a75bdf2fc5c3c0f39d5cb4aa38d05d6b432f61b3353a1977898"},
		{strings.Repeat("a", 255) + "😀尾巴", "b67188b9034865b7fec4d968c2cebddd255e809d60b749e007d2ea6173a87528"},
	}
	for _, test := range tests {
		if got := adobeNonce("user@AdobeID", test.prompt); got != test.expected {
			t.Errorf("nonce 错误：%s，预期 %s", got, test.expected)
		}
	}
}

func TestAdobeModelTemplates(t *testing.T) {
	for _, modelID := range []string{"nano-banana-pro", "nano-banana-2"} {
		model, _ := resolveModel(modelID)
		body, err := adobeGenerationBody(model, GenerateInput{Prompt: "猫", AspectRatio: "16:9", Resolution: "2K"}, nil, 42)
		if err != nil {
			t.Fatal(err)
		}
		size := body["size"].(map[string]int)
		if size["width"] != 2048 || size["height"] != 2048 {
			t.Fatalf("档位应使用 2048 方形占位，实际 %v", size)
		}
		payload := body["modelSpecificPayload"].(map[string]any)
		if payload["aspectRatio"] != "16:9" || body["modelId"] != "gemini-flash" || body["modelVersion"] != model.Version {
			t.Fatalf("模型模板错误：%v", body)
		}
		body, err = adobeGenerationBody(model, GenerateInput{Prompt: "猫", AspectRatio: "auto", Resolution: "4K"}, nil, 42)
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := body["modelSpecificPayload"].(map[string]any)["aspectRatio"]; exists {
			t.Fatal("自动比例不应传 aspectRatio")
		}
	}
	model, _ := resolveModel("gpt-image-2")
	body, err := adobeGenerationBody(model, GenerateInput{Prompt: "猫", Size: "auto", Quality: "high"}, nil, 42)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := body["size"]; exists || body["modelSpecificPayload"].(map[string]any)["size"] != "auto" {
		t.Fatalf("GPT 自动尺寸应省略输出宽高：%v", body)
	}
	if body["generationSettings"].(map[string]int)["detailLevel"] != 5 {
		t.Fatal("高画质应转换为 detailLevel 5")
	}
	pro, _ := resolveModel("nano-banana-pro")
	if _, err := adobeGenerationBody(pro, GenerateInput{Prompt: "猫", Resolution: "512"}, nil, 0); err == nil {
		t.Fatal("香蕉 Pro 不应接受 512 档位")
	}
	if _, err := adobeGenerationBody(model, GenerateInput{Prompt: "猫", Size: "999x123"}, nil, 0); err == nil {
		t.Fatal("不支持的比例应返回错误")
	}
}

func TestAdobeCurrentBananaModelDirectory(t *testing.T) {
	for _, id := range []string{"nano-banana", "gemini-2.5-flash-image", "gemini-2.5-flash-image-preview", "models/gemini-2.5-flash-image"} {
		if _, ok := resolveModel(id); ok {
			t.Errorf("页面未提供的旧版模型不能解析为可用模型：%s", id)
		}
	}
	bananaCount := 0
	for _, model := range availableModels {
		if model.AdobeID == "gemini-flash" {
			bananaCount++
			if model.Version != "nano-banana-2" && model.Version != "nano-banana-3" {
				t.Fatalf("目录出现未经当前页面确认的香蕉版本：%s", model.Version)
			}
		}
	}
	if bananaCount != 2 {
		t.Fatalf("当前页面应提供香蕉 2 和香蕉 Pro，实际有 %d 个", bananaCount)
	}
}

func TestAdobeGenerationUploadsPollsAndDownloadsWithoutCredentials(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jfGQAAAAASUVORK5CYII=")
	account := Account{AccessToken: "test-token", UserID: "user@AdobeID", APIKey: "test-api", ARPSession: "test-arp"}
	uploads, posts, polls, downloads := 0, 0, 0, 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Path == "/v2/storage/image":
			uploads++
			body, _ := io.ReadAll(request.Body)
			if !bytes.Equal(body, png) || request.ContentLength != int64(len(png)) || request.Header.Get("Content-Type") != "image/png" {
				t.Fatal("参考图应按原始字节和明确长度上传")
			}
			return adobeTestResponse(200, nil, `{"images":[{"id":"reference-id"}]}`), nil
		case request.URL.Path == "/v2/3p-images/generate-async":
			posts++
			if request.Header.Get("Authorization") != "Bearer test-token" || request.Header.Get("X-Nonce") != adobeNonce(account.UserID, "画猫") || request.Header.Get("X-Arp-Session-Id") != "test-arp" {
				t.Fatal("上游认证与新 nonce 错误")
			}
			var body map[string]any
			if json.NewDecoder(request.Body).Decode(&body) != nil {
				t.Fatal("生图请求应使用 JSON")
			}
			ref := body["referenceBlobs"].([]any)[0].(map[string]any)
			if ref["id"] != "reference-id" || ref["usage"] != "general" {
				t.Fatal("香蕉参考图应使用 general 用途")
			}
			header := http.Header{"X-Task-Status": {"ACCEPTED"}, "Retry-After": {"0"}, "X-Override-Status-Link": {"https://bks-epo8552.adobe.io/v2/jobs/result/job"}}
			return adobeTestResponse(200, header, `{"links":{"result":{"href":"https://firefly-epo855232.adobe.io/jobs/result/job"}}}`), nil
		case request.URL.Host == "bks-epo8552.adobe.io":
			polls++
			if request.Header.Get("Authorization") != "Bearer test-token" {
				t.Fatal("轮询需要正确的访问令牌")
			}
			status, progress := "IN_PROGRESS", 80
			if polls == 2 {
				status, progress = "COMPLETED", 100
			}
			body, _ := json.Marshal(map[string]any{"progress": progress, "contentType": "image/png", "outputs": []any{map[string]any{"image": map[string]string{"presignedUrl": "https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/images/image?X-Amz-Signature=secret"}}}})
			return adobeTestResponse(200, http.Header{"X-Task-Status": {status}, "Retry-After": {"0"}}, string(body)), nil
		case request.URL.Host == "pre-signed-firefly-prod.s3-accelerate.amazonaws.com":
			downloads++
			if polls != 2 {
				t.Fatal("进度 80 的图片不应提前下载")
			}
			if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("X-Api-Key") != "" {
				t.Fatal("图片下载不能携带账号凭据")
			}
			return adobeTestResponse(200, http.Header{"Content-Type": {"image/png"}}, string(png)), nil
		default:
			t.Fatalf("收到意外请求：%s", request.URL.Host+request.URL.Path)
			return nil, errors.New("意外请求")
		}
	})
	reference, err := generator.store.SaveImage(bytes.NewReader(png), "image/png", "reference")
	if err != nil {
		t.Fatal(err)
	}
	log := &RequestLog{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := generator.Generate(ctx, account, GenerateInput{Model: "nano-banana-2", Prompt: "画猫", References: []ImageRecord{reference}}, log)
	if err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || posts != 1 || polls != 2 || downloads != 1 || len(result.Images) != 1 || result.Images[0].Width != 1 || result.Credits != nil {
		t.Fatalf("生成流程或真实计费处理错误：%d/%d/%d/%d %+v", uploads, posts, polls, downloads, result)
	}
	if len(log.Upstream) != 6 || strings.Contains(log.Upstream[5].URL, "secret") {
		t.Fatalf("日志记录不完整或包含图片签名：%+v", log.Upstream)
	}
}

func TestAdobeGenerationDoesNotRetryPost(t *testing.T) {
	posts := 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		posts++
		return adobeTestResponse(429, nil, `{"error_code":"quota_exhausted","message":"没有可用积分"}`), nil
	})
	_, err := generator.Generate(context.Background(), Account{AccessToken: "token", UserID: "user@AdobeID"}, GenerateInput{Model: "nano-banana-2", Prompt: "猫"}, &RequestLog{})
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.StatusCode != 429 || posts != 1 {
		t.Fatalf("失败请求不应重试或隐藏状态：%d %v", posts, err)
	}
}

func TestAdobeCanceledPollingCancelsJob(t *testing.T) {
	canceled := 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPut {
			canceled++
			return adobeTestResponse(200, nil, `{}`), nil
		}
		return adobeTestResponse(200, http.Header{"X-Task-Status": {"ACCEPTED"}, "Retry-After": {"5"}}, `{"links":{"result":{"href":"https://firefly.adobe.io/jobs/result/job"},"cancel":{"href":"https://firefly.adobe.io/jobs/cancel/job"}}}`), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := generator.Generate(ctx, Account{AccessToken: "token", UserID: "user@AdobeID"}, GenerateInput{Model: "nano-banana-2", Prompt: "猫"}, &RequestLog{})
	if !errors.Is(err, context.DeadlineExceeded) || canceled != 1 {
		t.Fatalf("取消请求应停止轮询并通知上游：%d %v", canceled, err)
	}
}

func TestAdobeTaskLinksDoNotLeakCredentials(t *testing.T) {
	for _, raw := range []string{"http://firefly.adobe.io/job", "https://adobe.io.evil.test/job", "https://evil.test/job", "https://user:pass@firefly.adobe.io/job", "https://127.0.0.1/job", "https://firefly.adobe.io:8443/job"} {
		if _, err := resolveAdobeLink(raw, adobeUpstreamURL); err == nil {
			t.Errorf("危险任务地址应拒绝：%s", raw)
		}
	}
	if got, err := resolveAdobeLink("/jobs/result/job", adobeUpstreamURL); err != nil || got != adobeUpstreamURL+"/jobs/result/job" {
		t.Fatalf("合法相对链接应正常解析：%s %v", got, err)
	}
	requests := 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		requests++
		return adobeTestResponse(200, http.Header{"X-Task-Status": {"ACCEPTED"}, "X-Override-Status-Link": {"https://evil.test/job"}}, `{}`), nil
	})
	_, err := generator.Generate(context.Background(), Account{AccessToken: "token", UserID: "user@AdobeID"}, GenerateInput{Model: "nano-banana-2", Prompt: "猫"}, &RequestLog{})
	if err == nil || requests != 1 {
		t.Fatal("无效轮询链接不应获得访问令牌")
	}
}

func TestAdobeIMSRefreshUsesCookieAndMillisecondExpiry(t *testing.T) {
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != adobeIMSURL || request.Method != http.MethodPost || request.Header.Get("Authorization") != "" || !strings.Contains(request.Header.Get("Cookie"), "ims_sid=old") {
			t.Fatal("IMS 刷新应使用官方 Cookie 接口")
		}
		if err := request.ParseForm(); err != nil || request.Form.Get("user_id") != "user@AdobeID" || request.Form.Get("client_id") != "clio-playground-web" || !strings.Contains(request.Form.Get("scope"), "firefly_api") || request.Form.Get("client_secret") != "" {
			t.Fatal("IMS 刷新参数无效")
		}
		return adobeTestResponse(200, http.Header{"Set-Cookie": {"ims_sid=new; Secure; HttpOnly", "aux_sid=; Max-Age=0"}}, `{"access_token":"new-token","expires_in":3600000,"userId":"user@AdobeID"}`), nil
	})
	before := time.Now()
	account, err := generator.Refresh(context.Background(), Account{UserID: "user@AdobeID", Cookie: "ims_sid=old; aux_sid=remove; bims_sid=keep", ARPSession: "existing-arp"})
	if err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != "new-token" || !strings.Contains(account.Cookie, "ims_sid=new") || strings.Contains(account.Cookie, "aux_sid=") || !strings.Contains(account.Cookie, "bims_sid=keep") {
		t.Fatalf("刷新凭据应合并 Cookie：%+v", account)
	}
	if account.ExpiresAt.Before(before.Add(59*time.Minute)) || account.ExpiresAt.After(before.Add(61*time.Minute)) {
		t.Fatalf("expires_in 必须使用毫秒：%s", account.ExpiresAt)
	}
}

func TestAdobeRefreshFallsBackToBrowser(t *testing.T) {
	called := 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		return adobeTestResponse(401, nil, `{"error":"invalid_cookie"}`), nil
	})
	generator.browserRefresh = func(ctx context.Context, account Account) (Account, error) {
		called++
		account.AccessToken = "browser-token"
		return account, nil
	}
	account, err := generator.Refresh(context.Background(), Account{UserID: "user@AdobeID", Cookie: "ims_sid=expired"})
	if err != nil || account.AccessToken != "browser-token" || called != 1 {
		t.Fatalf("Cookie 失效应尝试浏览器刷新：%+v %v", account, err)
	}
}

func TestAdobeNewCookieAccountObtainsARPAfterTokenRefresh(t *testing.T) {
	called := false
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		return adobeTestResponse(200, nil, `{"access_token":"new-token","expires_in":86400000,"userId":"profile@AdobeID"}`), nil
	})
	generator.browserRefresh = func(ctx context.Context, account Account) (Account, error) {
		called = true
		if account.AccessToken != "new-token" || account.UserID != "profile@AdobeID" {
			t.Fatal("浏览器同步应接收 IMS 更新后的账号")
		}
		account.ARPSession = "fresh-arp"
		return account, nil
	}
	account, err := generator.Refresh(context.Background(), Account{UserID: "user@AdobeID", Cookie: "ims_sid=old"})
	if err != nil || !called || account.ARPSession != "fresh-arp" {
		t.Fatalf("只有 Cookie 的账号必须获取风险会话：%+v %v", account, err)
	}
}

func TestAdobeRequestedImageCountIsHonored(t *testing.T) {
	posts, downloads := 0, 0
	generator := newAdobeTestGenerator(t, func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost {
			posts++
			var body map[string]any
			if json.NewDecoder(request.Body).Decode(&body) != nil || body["n"] != float64(1) {
				t.Fatal("每次上游提交应遵守网页单图约定")
			}
			return adobeTestResponse(200, http.Header{"X-Task-Status": {"COMPLETED"}}, `{"contentType":"image/png","outputs":[{"image":{"presignedUrl":"https://pre-signed-firefly-prod.s3-accelerate.amazonaws.com/images/image"}}]}`), nil
		}
		downloads++
		return adobeTestResponse(200, http.Header{"Content-Type": {"image/png"}}, "图片内容"), nil
	})
	result, err := generator.Generate(context.Background(), Account{AccessToken: "token", UserID: "user@AdobeID"}, GenerateInput{Model: "nano-banana-2", Prompt: "猫", N: 3}, &RequestLog{})
	if err != nil || posts != 3 || downloads != 3 || len(result.Images) != 3 {
		t.Fatalf("生成数量未落实到上游请求：%d/%d/%d %v", posts, downloads, len(result.Images), err)
	}
}

func TestAdobeProxySupportsHTTPAndSOCKS5(t *testing.T) {
	generator := newAdobeTestGenerator(t, nil)
	for _, address := range []string{"http://proxy.test:8080", "socks5://proxy.test:1080", "socks5h://proxy.test:1080"} {
		settings := generator.store.Settings()
		settings.ProxyURL = address
		if err := generator.store.SaveSettings(settings); err != nil {
			t.Fatal(err)
		}
		client, _, err := generator.clients()
		if err != nil {
			t.Fatal(err)
		}
		proxy, err := client.Transport.(*http.Transport).Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "firefly.adobe.io"}})
		if err != nil || proxy.String() != address {
			t.Fatalf("代理设置未应用：%s %v", proxy, err)
		}
	}
	generator.Close()
}
