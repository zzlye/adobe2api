package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// upstreamLoggingBody 用于确认日志没有提前读取图片，且关闭动作仍传递到原始流。
type upstreamLoggingBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (body *upstreamLoggingBody) Read(p []byte) (int, error) {
	n, err := body.reader.Read(p)
	body.read += n
	return n, err
}

func (body *upstreamLoggingBody) Close() error {
	body.closed = true
	return nil
}

func TestUpstreamLoggingRedactsJSONAndPreservesBodies(t *testing.T) {
	requestJSON := `{"prompt":"猫","size":{"width":2048,"height":2048},"nested":{"access_token":"body-secret","echo":"body-secret"},"number":9007199254740993}`
	responseJSON := `{"outputs":[{"image":{"presignedUrl":"https://images.adobe.io/a.png?X-Amz-Signature=signed-secret&Policy=policy-secret"}}],"nested":{"refreshToken":"refresh-secret","echo":"refresh-secret"},"message":"Bearer auth-secret cookie-secret nonce-secret arp-secret new-cookie-secret body-secret"}`
	responseBody := &upstreamLoggingBody{reader: strings.NewReader(responseJSON)}
	client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
		actual, err := io.ReadAll(request.Body)
		if err != nil || string(actual) != requestJSON {
			t.Fatalf("日志不应改变上游请求内容：%s %v", actual, err)
		}
		return &http.Response{StatusCode: 422, Header: http.Header{"Content-Type": {"application/problem+json; charset=utf-8"}, "X-Request-Id": {"trace-123"}, "Set-Cookie": {"sid=new-cookie-secret; Secure; HttpOnly"}, "X-Override-Status-Link": {"https://jobs.adobe.io/result?a=1&signature=link-secret"}, "X-Debug-Secret": {"header-secret"}, "X-Unlisted": {"unused-secret"}}, Body: responseBody, ContentLength: -1}, nil
	})}
	request, _ := http.NewRequest(http.MethodPost, "https://firefly.adobe.io/generate?nonce=nonce-secret&safe=value", strings.NewReader(requestJSON))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer auth-secret")
	request.Header.Set("Cookie", "ims_sid=cookie-secret")
	request.Header.Set("X-Nonce", "nonce-secret")
	request.Header.Set("X-Arp-Session-Id", "arp-secret")
	request.Header.Set("X-Api-Key", "api-secret")
	request.Header.Set("X-Unlisted", "unlisted-secret")
	log := &RequestLog{}
	response, err := (&AdobeGenerator{}).do(client, request, log)
	if err != nil || response.StatusCode != 422 || len(log.Upstream) != 1 {
		t.Fatalf("原始 HTTP 状态与日志数量应保持：%v %+v", err, log.Upstream)
	}
	actual, err := io.ReadAll(response.Body)
	if err != nil || string(actual) != responseJSON {
		t.Fatalf("业务解析必须收到完整原始响应：%s %v", actual, err)
	}
	response.Body.Close()
	if !responseBody.closed {
		t.Fatal("响应关闭必须传递至原始流")
	}
	record := log.Upstream[0]
	if record.URL != "https://firefly.adobe.io/generate" || record.Status != 422 || record.RequestID != "trace-123" {
		t.Fatalf("基本诊断字段错误：%+v", record)
	}
	for _, name := range []string{"Authorization", "Cookie", "X-Nonce", "X-Arp-Session-Id", "X-Api-Key"} {
		if record.RequestHeaders[name] != upstreamLogHidden {
			t.Fatalf("认证头应仅保留存在指示：%s", name)
		}
	}
	if _, exists := record.RequestHeaders["X-Unlisted"]; exists {
		t.Fatal("白名单外的请求头不能记录原值")
	}
	if record.ResponseHeaders["Set-Cookie"] != upstreamLogHidden || record.ResponseHeaders["X-Debug-Secret"] != upstreamLogHidden || record.ResponseHeaders["X-Override-Status-Link"] != "https://jobs.adobe.io/result" {
		t.Fatalf("响应头未安全脱敏：%v", record.ResponseHeaders)
	}
	encoded, _ := json.Marshal(record)
	for _, secret := range []string{"body-secret", "auth-secret", "cookie-secret", "nonce-secret", "arp-secret", "new-cookie-secret", "api-secret", "signed-secret", "policy-secret", "refresh-secret", "header-secret", "unused-secret", "unlisted-secret", "link-secret"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("日志不得出现凭据：%s", secret)
		}
	}
	if !strings.Contains(record.RequestBody, `"width":2048`) || !strings.Contains(record.RequestBody, `9007199254740993`) || !strings.Contains(record.ResponseBody, "https://images.adobe.io/a.png") {
		t.Fatalf("应保留生成参数、大整数和可诊断图片路径：%+v", record)
	}
}

func TestUpstreamLoggingDoesNotBufferBinaryBodies(t *testing.T) {
	requestBody := &upstreamLoggingBody{reader: strings.NewReader("binary-request")}
	responseBody := &upstreamLoggingBody{reader: strings.NewReader("binary-response")}
	client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
		if requestBody.read != 0 {
			t.Fatal("图片上传不能被日志预读")
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != "binary-request" {
			t.Fatal("图片上传内容应完整")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, Body: responseBody, ContentLength: -1}, nil
	})}
	request, _ := http.NewRequest(http.MethodPost, "https://firefly.adobe.io/storage", requestBody)
	request.Header.Set("Content-Type", "image/png")
	log := &RequestLog{}
	response, err := (&AdobeGenerator{}).do(client, request, log)
	if err != nil || responseBody.read != 0 || log.Upstream[0].RequestBody != "" || log.Upstream[0].ResponseBody != "" {
		t.Fatalf("图片下载不能缓冲或记录：%d %v %+v", responseBody.read, err, log.Upstream)
	}
	actual, _ := io.ReadAll(response.Body)
	if string(actual) != "binary-response" {
		t.Fatal("图片下载内容应完整")
	}
}

func TestUpstreamLoggingBoundsJSONWithoutTruncatingResponse(t *testing.T) {
	large := `{"prompt":"` + strings.Repeat("猫", upstreamLogBodyLimit) + `"}`
	for _, contentLength := range []int64{-1, int64(len(large))} {
		body := &upstreamLoggingBody{reader: strings.NewReader(large)}
		logged := captureUpstreamJSONBodyForTest(body, contentLength)
		if len(logged.text) > upstreamLogBodyLimit || logged.text != upstreamLogOversized {
			t.Fatalf("超限 JSON 必须省略：%s", logged.text)
		}
		if contentLength > upstreamLogBodyLimit && body.read != 0 || body.read > upstreamLogBodyLimit+1 {
			t.Fatalf("日志读取必须有界：%d", body.read)
		}
		actual, err := io.ReadAll(logged.body)
		if err != nil || string(actual) != large {
			t.Fatalf("超限响应仍应原样供业务读取：%d %v", len(actual), err)
		}
	}
	request := http.Header{}
	for index := 0; index < 100; index++ {
		request.Set("X-Auth-"+strings.Repeat("a", index), strings.Repeat("s", 4096))
	}
	request.Set("User-Agent", strings.Repeat("猫", 5000))
	encoded, _ := json.Marshal(sanitizeUpstreamHeaders(request, nil))
	if len(encoded) > upstreamLogBodyLimit || !json.Valid(encoded) || !utf8.Valid(encoded) {
		t.Fatalf("头部日志也必须限制大小并保持有效字符：%d", len(encoded))
	}
}

func captureUpstreamJSONBodyForTest(original io.ReadCloser, length int64) struct {
	text string
	body io.ReadCloser
} {
	body := original
	text, _ := captureUpstreamJSON(&body, "application/json", length, nil)
	return struct {
		text string
		body io.ReadCloser
	}{text, body}
}

type upstreamLoggingFailReader struct {
	used bool
	err  error
}

func (reader *upstreamLoggingFailReader) Read(p []byte) (int, error) {
	if reader.used {
		return 0, io.EOF
	}
	reader.used = true
	return copy(p, `{"partial":`), reader.err
}

func TestUpstreamLoggingPreservesReadErrorsAndCancellation(t *testing.T) {
	readErr := errors.New("读取失败")
	logged := captureUpstreamJSONBodyForTest(io.NopCloser(&upstreamLoggingFailReader{err: readErr}), -1)
	actual, err := io.ReadAll(logged.body)
	if string(actual) != `{"partial":` || !errors.Is(err, readErr) || logged.text != upstreamLogInvalid {
		t.Fatalf("日志必须回放原始部分内容与读取错误：%s %v", actual, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://firefly.adobe.io/result?token=secret", nil)
	client := &http.Client{Transport: adobeTestTransport(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })}
	log := &RequestLog{}
	if _, err := (&AdobeGenerator{}).do(client, request, log); !errors.Is(err, context.Canceled) || len(log.Upstream) != 1 || log.Upstream[0].Status != 0 {
		t.Fatalf("取消语义和失败日志必须保留：%v %+v", err, log.Upstream)
	}
}

func TestUpstreamLoggingPersistenceSanitizesImportedRecords(t *testing.T) {
	record := sanitizeUpstreamRecord(UpstreamRecord{
		URL:             "https://firefly.adobe.io/job?nonce=nonce-secret&cookie=cookie-secret",
		RequestHeaders:  map[string]string{"Authorization": "Bearer access-secret", "Cookie": "sid=cookie-secret", "X-Nonce": "nonce-secret"},
		ResponseHeaders: map[string]string{"Set-Cookie": "sid=updated-secret; Secure", "Location": "https://cdn.adobe.io/image?Policy=policy-secret"},
		RequestBody:     `{"prompt":"access-secret","nested":{"X_ARP_Session_ID":"arp-secret","inlineData":{"data":"encoded-image"}},"accessToken":"json-only-secret"}`,
		ResponseBody:    `{"message":"updated-secret json-only-secret","auth":{"token":"response-secret"},"url":"https://cdn.adobe.io/image?sig=sign-secret"}`,
	})
	encoded, _ := json.Marshal(record)
	for _, secret := range []string{"nonce-secret", "cookie-secret", "access-secret", "updated-secret", "policy-secret", "arp-secret", "encoded-image", "response-secret", "sign-secret", "json-only-secret"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("持久化入口不得保留导入的敏感信息：%s", secret)
		}
	}
	for _, raw := range []string{`{"token":"raw-secret"`, `{"a":1}{"token":"raw-secret"}`, strings.Repeat("x", upstreamLogBodyLimit+1)} {
		text := sanitizeUpstreamJSON([]byte(raw), nil)
		if !json.Valid([]byte(text)) || strings.Contains(text, "raw-secret") || len(text) > upstreamLogBodyLimit {
			t.Fatalf("无效或超限 JSON 不能按原始文本记录：%s", text)
		}
	}
	for _, contentType := range []string{"image/png", "text/html", "application/x-www-form-urlencoded", "text/json", ""} {
		if upstreamLogJSONType(contentType) {
			t.Errorf("不能记录非 JSON 内容：%s", contentType)
		}
	}
}

func TestUpstreamLoggingRedactsRelativeRedirectsAndStructuredCredentials(t *testing.T) {
	deep := strings.Repeat(`{"nested":`, 24) + `{"token":"deep-secret"}` + strings.Repeat("}", 24)
	raw := `{"href":"/jobs/result?sig=relative-secret#fragment-secret","url":"HTTPS://cdn.adobe.io/image?Policy=upper-secret","cookie":["array-secret"],"auth":{"items":[{"value":"object-secret"}]},"deep":` + deep + `,"message":"array-secret object-secret deep-secret","image":" data:image/png;base64,encoded-secret"}`
	text := sanitizeUpstreamJSON([]byte(raw), nil)
	for _, secret := range []string{"relative-secret", "fragment-secret", "upper-secret", "array-secret", "object-secret", "deep-secret", "encoded-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("相对链接或敏感结构不能在其他字段中泄露：%s", secret)
		}
	}
	var value map[string]any
	if json.Unmarshal([]byte(text), &value) != nil || value["href"] != "/jobs/result" {
		t.Fatalf("相对地址应保留路径并清除签名：%s", text)
	}
	headers := sanitizeUpstreamHeaders(http.Header{"Location": {"/jobs/result?Policy=location-secret"}, "X-Override-Status-Link": {"//jobs.adobe.io/result?sig=override-secret"}, "User-Agent": {strings.Repeat("a", 1<<20)}}, nil)
	if headers["Location"] != "/jobs/result" || headers["X-Override-Status-Link"] != "//jobs.adobe.io/result" || headers["User-Agent"] != "[内容过长已省略]" {
		t.Fatalf("重定向与超长头应安全处理：%v", headers)
	}
	if got := upstreamLogText("abcdef abc", []string{"abc", "abcdef", "abc"}); got != upstreamLogHidden+" "+upstreamLogHidden {
		t.Fatalf("重叠凭据必须整体隐藏：%s", got)
	}
}

func TestUpstreamLoggingProtectsPrimaryErrorsAndWrongJSONContentType(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/plain"} {
		requestJSON := `{"prompt":"猫","credentials":{"items":[{"value":"body-only-secret"}]}}`
		responseJSON := `{"error_code":"bad_request","message":"拒绝 auth-secret quoted-cookie-secret body-only-secret https://cdn.adobe.io/image?sig=signed-error-secret"}`
		client := &http.Client{Transport: adobeTestTransport(func(*http.Request) (*http.Response, error) {
			return adobeTestResponse(400, http.Header{"Content-Type": {contentType}}, responseJSON), nil
		})}
		request, _ := http.NewRequest(http.MethodPost, "https://firefly.adobe.io/generate", strings.NewReader(requestJSON))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer auth-secret")
		request.Header.Set("Cookie", `sid="quoted-cookie-secret"`)
		log := &RequestLog{}
		response, err := (&AdobeGenerator{}).do(client, request, log)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := decodeAdobeResponse(response)
		var upstream *UpstreamError
		if !errors.As(err, &upstream) || upstream.StatusCode != 400 || upstream.Code != "bad_request" || !strings.Contains(upstream.Message, "拒绝") {
			t.Fatalf("状态、错误码与诊断文字应保留：%v", err)
		}
		for _, secret := range []string{"auth-secret", "quoted-cookie-secret", "body-only-secret", "signed-error-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("日志主错误与账号错误不能绕过脱敏：%s", secret)
			}
		}
		if payload.Message != "拒绝 auth-secret quoted-cookie-secret body-only-secret https://cdn.adobe.io/image?sig=signed-error-secret" {
			t.Fatal("错误脱敏不能破坏完整原始响应解析")
		}
		if contentType == "text/plain" && log.Upstream[0].ResponseBody != "" {
			t.Fatal("非 JSON 类型仍不能被当作详情内容记录")
		}
	}
}

func TestUpstreamLoggingTotalBudgetSkipsPreReadsAndKeepsBasicRecords(t *testing.T) {
	for _, oldSize := range []int{upstreamLogDetailBudget - 1, upstreamLogDetailBudget, upstreamLogDetailBudget + 100} {
		log := &RequestLog{Upstream: []UpstreamRecord{{ResponseBody: strings.Repeat("x", oldSize)}}}
		requestBody := &upstreamLoggingBody{reader: strings.NewReader(`{"prompt":"猫"}`)}
		responseBody := &upstreamLoggingBody{reader: strings.NewReader(`{"status":"done"}`)}
		client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
			if requestBody.read != 0 {
				t.Fatal("预算耗尽后不能预读 JSON 请求")
			}
			io.Copy(io.Discard, request.Body)
			return &http.Response{StatusCode: 202, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"trace-budget"}}, Body: responseBody, ContentLength: -1}, nil
		})}
		request, _ := http.NewRequest(http.MethodPost, "https://firefly.adobe.io/generate?token=secret", requestBody)
		request.Header.Set("Content-Type", "application/json")
		response, err := (&AdobeGenerator{}).do(client, request, log)
		if err != nil || responseBody.read != 0 || len(log.Upstream) != 2 || log.upstreamDetailBytes > upstreamLogDetailBudget {
			t.Fatalf("预算耗尽后应保持零预读与基本记录：%d %d %v", responseBody.read, log.upstreamDetailBytes, err)
		}
		record := log.Upstream[1]
		if record.Method != "POST" || record.URL != "https://firefly.adobe.io/generate" || record.Status != 202 || record.RequestID != "trace-budget" || record.RequestHeaders != nil || record.ResponseHeaders != nil || record.RequestBody != "" || record.ResponseBody != "" {
			t.Fatalf("详情省略后仍应保留请求定位信息：%+v", record)
		}
		actual, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || string(actual) != `{"status":"done"}` || !responseBody.closed {
			t.Fatalf("预算限制不得影响响应和关闭：%s %v", actual, readErr)
		}
	}
}

func TestUpstreamLoggingCumulativeDetailsStayBounded(t *testing.T) {
	jsonBody := `{"text":"` + strings.Repeat("x", upstreamLogBodyLimit-100) + `"}`
	var lastBody *upstreamLoggingBody
	client := &http.Client{Transport: adobeTestTransport(func(*http.Request) (*http.Response, error) {
		lastBody = &upstreamLoggingBody{reader: strings.NewReader(jsonBody)}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: lastBody, ContentLength: -1}, nil
	})}
	log := &RequestLog{}
	captured, omitted := 0, 0
	for index := 0; index < 100; index++ {
		request, _ := http.NewRequest(http.MethodGet, "https://firefly.adobe.io/result", nil)
		response, err := (&AdobeGenerator{}).do(client, request, log)
		if err != nil {
			t.Fatal(err)
		}
		if lastBody.read == 0 {
			omitted++
		} else {
			captured++
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	total := 0
	for _, record := range log.Upstream {
		total += upstreamLogDetailBytes(record)
	}
	if len(log.Upstream) != 100 || captured == 0 || omitted == 0 || total > upstreamLogDetailBudget || total != log.upstreamDetailBytes {
		t.Fatalf("累计详情应受总预算约束，后续保留基本调用：%d %d/%d %d/%d", len(log.Upstream), captured, omitted, total, log.upstreamDetailBytes)
	}
	encoded, _ := json.Marshal(log)
	if bytes.Contains(encoded, []byte("upstreamDetailBytes")) || bytes.Contains(encoded, []byte("upstream_detail_bytes")) {
		t.Fatal("内存计数不能出现在管理接口或持久化内容中")
	}
}
