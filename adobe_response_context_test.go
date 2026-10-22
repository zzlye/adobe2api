package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// 上游读取错误可能携带签名地址，只保留两个标准上下文错误供调用方分类。
func TestDecodeAdobeResponsePreservesSafeContextReadErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"正文超时", context.DeadlineExceeded, context.DeadlineExceeded},
		{"正文取消", context.Canceled, context.Canceled},
		{"其他读取错误", io.ErrUnexpectedEOF, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := &url.Error{Op: "Read", URL: "https://firefly.adobe.io/result?sig=read-url-secret-fixture", Err: test.err}
			response := adobeTestResponse(http.StatusOK, nil, "")
			response.Body = io.NopCloser(&upstreamLoggingFailReader{err: original})
			_, err := decodeAdobeResponse(response)
			if test.want != nil {
				if err != test.want {
					t.Fatal("正文读取未返回标准上下文错误")
				}
			} else if err == nil || err.Error() != "读取上游响应失败" || errors.Is(err, original) {
				t.Fatal("其他正文错误应转换为安全中文消息")
			}
			if strings.Contains(err.Error(), "read-url-secret-fixture") || strings.Contains(err.Error(), "https://") {
				t.Fatal("正文读取失败泄露了上游地址")
			}
		})
	}
}

// 从正式协议入口经过 JSON 日志预读与真实适配器，正文错误必须正确进入响应及持久化日志。
func TestGenerationResponseBodyContextErrorsKeepLogClassification(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		log    string
	}{
		{"正文超时", context.DeadlineExceeded, http.StatusGatewayTimeout, "failed"},
		{"正文取消", context.Canceled, 499, "cancelled"},
		{"其他读取错误", io.ErrUnexpectedEOF, http.StatusBadGateway, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, _ := newProtocolApp(t)
			requests := 0
			client := &http.Client{Transport: adobeTestTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() == adobeCreditsURL {
					return adobeTestResponse(http.StatusNotFound, http.Header{"Content-Type": {"application/json"}}, `{}`), nil
				}
				if request.URL.String() != adobeUpstreamURL+"/v2/3p-images/generate-async" || request.Method != http.MethodPost {
					t.Fatal("测试未经过正式 Adobe 生成适配器")
				}
				requests++
				response := adobeTestResponse(http.StatusOK, http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"body-context-fixture"}}, "")
				response.ContentLength = -1
				response.Body = io.NopCloser(&upstreamLoggingFailReader{err: &url.Error{Op: "Read", URL: "https://firefly.adobe.io/result?sig=read-url-secret-fixture", Err: test.err}})
				return response, nil
			})}
			app.generator = &AdobeGenerator{store: app.store, client: client, downloadClient: client}
			response := protocolRequest(app, http.MethodPost, "/v1/images/generations", map[string]any{"model": "nano-banana-2", "prompt": "正文读取分类测试"})
			if response.Code != test.status || requests != 1 {
				t.Fatal("正文读取错误返回状态不准确或重复提交了生成")
			}
			logs, total, err := app.store.Logs(1, 20, "", "")
			if err != nil || total != 1 || len(logs) != 1 || logs[0].HTTPStatus != test.status || logs[0].Status != test.log || logs[0].Error == "" {
				t.Fatal("正文读取错误没有按取消或超时分类保存日志")
			}
			found := false
			for _, record := range logs[0].Upstream {
				if record.RequestID == "body-context-fixture" {
					found = record.Status == http.StatusOK && record.ResponseBody == upstreamLogInvalid
				}
			}
			if !found {
				t.Fatal("读取错误未保留已收到的上游响应状态及接口记录")
			}
			encoded, _ := json.Marshal(logs)
			if strings.Contains(string(encoded), "read-url-secret-fixture") || strings.Contains(response.Body.String(), "read-url-secret-fixture") {
				t.Fatal("接口响应或持久化日志泄露了读取错误中的签名地址")
			}
		})
	}
}
