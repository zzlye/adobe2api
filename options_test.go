package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 同时检查协议输入与最终 Adobe 请求，防止只接受参数而没有发送到上游。
func TestExpandedOptionsReachAdobeAcrossProtocols(t *testing.T) {
	for _, route := range []string{"images", "chat", "gemini", "multipart"} {
		t.Run(route, func(t *testing.T) {
			app, fake, data := newProtocolApp(t)
			body := map[string]any{"model": "gpt-image-2.5-sunburst", "prompt": "陶瓷茶壶", "quality": "max", "background": "transparent", "aspect_ratio": "7:5", "resolution": "2K"}
			var response *httptest.ResponseRecorder
			switch route {
			case "images":
				response = protocolRequest(app, "POST", "/v1/images/generations", body)
			case "chat":
				body["messages"] = []map[string]string{{"role": "user", "content": "陶瓷茶壶"}}
				response = protocolRequest(app, "POST", "/v1/chat/completions", body)
			case "gemini":
				response = protocolRequest(app, "POST", "/v1beta/models/gpt-image-2.5-sunburst:generateContent", map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": "陶瓷茶壶"}}}}, "generationConfig": map[string]any{"imageConfig": map[string]any{"quality": "max", "background": "transparent", "aspectRatio": "7:5", "imageSize": "2K"}}})
			case "multipart":
				var buf bytes.Buffer
				writer := multipart.NewWriter(&buf)
				for key, value := range body {
					_ = writer.WriteField(key, value.(string))
				}
				part, _ := writer.CreateFormFile("image", "source.png")
				_, _ = part.Write(data)
				_ = writer.Close()
				req := httptest.NewRequest("POST", "/v1/images/edits", &buf)
				req.Header.Set("Content-Type", writer.FormDataContentType())
				req.Header.Set("Authorization", "Bearer sk-protocol-test-12345")
				response = httptest.NewRecorder()
				app.Handler().ServeHTTP(response, req)
			}
			requireProtocolStatus(t, response, 200)
			model, _ := resolveModel(fake.input.Model)
			upstream, err := adobeGenerationBody(model, fake.input, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			settings := upstream["generationSettings"].(map[string]any)
			if settings["detailLevel"] != 7 || settings["transparentBackground"] != true {
				t.Fatalf("参数未到达上游：%v", settings)
			}
			size := upstream["size"].(map[string]int)
			if size["width"] <= 2048 || size["height"] >= 2048 || validGPTSize(size["width"], size["height"]) != nil {
				t.Fatalf("自定义比例未生效：%v", size)
			}
			logs, _, _ := app.store.Logs(1, 1, "", "")
			entry, _ := app.store.Log(logs[0].ID)
			if !bytes.Contains(entry.Parameters, []byte(`"quality":"max"`)) || !bytes.Contains(entry.Parameters, []byte(`"background":"transparent"`)) {
				t.Fatalf("日志遗漏参数：%s", entry.Parameters)
			}
		})
	}
}

func TestModelOptionBoundariesAndConflicts(t *testing.T) {
	integer := func(v int) *int { return &v }
	boolean := func(v bool) *bool { return &v }
	seed := int64(42)
	for _, input := range []GenerateInput{
		{Model: "nano-banana-2", Quality: "max"}, {Model: "nano-banana-pro", Background: "transparent"},
		{Model: "gpt-image-2", Background: "transparent"}, {Model: "gpt-image-2", DetailLevel: integer(6)},
		{Model: "gpt-image-2.5-flare", DetailLevel: integer(8)}, {Model: "gpt-image-2.5-flare", Quality: "max", DetailLevel: integer(5)},
		{Model: "gpt-image-2.5-flare", Background: "opaque", TransparentBackground: boolean(true)},
		{Model: "gpt-image-2.5-flare", Seed: &seed}, {Model: "gpt-image-2", GroundSearch: boolean(false)},
		{Model: "nano-banana-2", Seed: &seed, Seeds: []int64{1}}, {Model: "nano-banana-2", N: 2, Seeds: []int64{1}},
		{Model: "gpt-image-2", Size: "480x480"}, {Model: "gpt-image-2", Size: "1000x1000"},
		{Model: "gpt-image-2", AspectRatio: "4:1"}, {Model: "gpt-image-2", AspectRatio: "NaN:1"},
		{Model: "gpt-image-1.5", Size: "1280x1280"},
		{Model: "gpt-image-2", ModelSpecificPayload: map[string]any{"size": "auto"}},
	} {
		input.Prompt = "测试"
		if err := validateInput(&input); err == nil {
			t.Errorf("未拒绝不支持或冲突参数：%+v", input)
		}
	}
	for _, id := range []string{"gpt-image-1.5", "gpt-image-2", "gpt-image-2.5-flare"} {
		model, _ := resolveModel(id)
		body, err := adobeGenerationBody(model, GenerateInput{Prompt: "测试", Quality: "max"}, nil, 42)
		if err != nil || body["generationSettings"].(map[string]any)["detailLevel"] != model.MaxDetailLevel {
			t.Fatalf("最高品质未映射当前模型上限：%s %v", id, err)
		}
		if _, exists := body["seeds"]; exists {
			t.Fatal("GPT 模型未声明种子能力")
		}
	}
	input := GenerateInput{Model: "nano-banana-2", Prompt: "测试", Size: "1024x1024", Resolution: "4K", Seed: &seed, GroundSearch: boolean(true)}
	if err := validateInput(&input); err != nil {
		t.Fatal(err)
	}
	model, _ := resolveModel(input.Model)
	body, err := adobeGenerationBody(model, input, nil, 0)
	if err != nil || body["resolution"] != "4K" || body["groundSearch"] != true || body["seeds"].([]int64)[0] != 42 {
		t.Fatalf("香蕉显式参数丢失：%v %v", body, err)
	}
}

func TestMaskValidationAndReferenceLimits(t *testing.T) {
	app, fake, data := newProtocolApp(t)
	source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
	config, _, _ := image.DecodeConfig(bytes.NewReader(data))
	mask := image.NewNRGBA(image.Rect(0, 0, config.Width, config.Height))
	mask.SetNRGBA(0, 0, color.NRGBA{A: 128})
	var buf bytes.Buffer
	_ = png.Encode(&buf, mask)
	maskURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	body := map[string]any{"model": "gpt-image-2.5-flare", "prompt": "修改中央物体", "image": source, "mask": maskURL}
	response := protocolRequest(app, "POST", "/v1/images/edits", body)
	requireProtocolStatus(t, response, 200)
	if fake.input.MaskReference == nil || len(fake.input.Mask) > 0 {
		t.Fatal("遮罩未落盘或仍保留编码")
	}
	logs, _, _ := app.store.Logs(1, 1, "", "")
	entry, _ := app.store.Log(logs[0].ID)
	if len(entry.References) != 2 || bytes.Contains(entry.Parameters, []byte("base64,")) {
		t.Fatal("遮罩应关联日志且不记录编码")
	}
	body["mask"] = source
	// 无透明通道的普通 RGB 图片不能被当作编辑遮罩。
	opaque := image.NewRGBA(image.Rect(0, 0, config.Width, config.Height))
	for y := 0; y < config.Height; y++ {
		for x := 0; x < config.Width; x++ {
			opaque.Set(x, y, color.White)
		}
	}
	buf.Reset()
	_ = png.Encode(&buf, opaque)
	body["mask"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	requireProtocolStatus(t, protocolRequest(app, "POST", "/v1/images/edits", body), 400)
	for _, item := range []struct {
		id    string
		limit int
	}{{"nano-banana-2", 14}, {"gpt-image-2", 16}} {
		refs := make([]ImageRecord, item.limit)
		for i := range refs {
			refs[i] = ImageRecord{MIME: "image/png", Bytes: 100}
		}
		input := GenerateInput{Model: item.id, Prompt: "测试", References: refs}
		if err := validateInput(&input); err != nil {
			t.Fatal(err)
		}
		input.References = append(input.References, refs[0])
		if err := validateInput(&input); err == nil {
			t.Fatal("参考图数量越界未被拒绝")
		}
	}
	for _, item := range []struct {
		id    string
		bytes int64
	}{{"nano-banana-2", 20 << 20}, {"gpt-image-2", protocolBodyLimit}} {
		input := GenerateInput{Model: item.id, Prompt: "测试", References: []ImageRecord{{MIME: "image/png", Bytes: item.bytes + 1}}}
		if err := validateInput(&input); err == nil {
			t.Fatalf("单张参考图大小越界未被拒绝：%s", item.id)
		}
	}
}

func TestReferenceUsageOptionsFollowModelSchema(t *testing.T) {
	for _, item := range []struct {
		model string
		usage string
	}{
		{"nano-banana-2", "element"},
		{"gpt-image-2", "element"},
		{"gpt-image-2", "source"},
	} {
		input := GenerateInput{Model: item.model, Prompt: "测试", References: []ImageRecord{{MIME: "image/png", Bytes: 100}}, ReferenceUsages: []string{item.usage}}
		if err := validateInput(&input); err != nil {
			t.Fatalf("应接受页面 Schema 声明的参考图用途：%s/%s: %v", item.model, item.usage, err)
		}
		model, _ := resolveModel(item.model)
		if actual := referenceUsage(model, input, 0); actual != item.usage {
			t.Fatalf("参考图用途未保留：%s", actual)
		}
	}
	for _, input := range []GenerateInput{
		{Model: "nano-banana-2", Prompt: "测试", References: []ImageRecord{{MIME: "image/png", Bytes: 100}}, ReferenceUsages: []string{"source"}},
		{Model: "gpt-image-2", Prompt: "测试", References: []ImageRecord{{MIME: "image/png", Bytes: 100}}, ReferenceUsages: []string{"mask"}},
		{Model: "gpt-image-2", Prompt: "测试", References: []ImageRecord{{MIME: "image/png", Bytes: 100}}, ReferenceUsages: []string{"source", "element"}},
	} {
		if err := validateInput(&input); err == nil {
			t.Fatalf("未拒绝无效参考图用途：%+v", input)
		}
	}
}

// 各协议的参考图用途必须在规范化输入和 Adobe 请求中保持一致。
func TestReferenceUsagesReachAdobeAcrossProtocols(t *testing.T) {
	for _, route := range []string{"images", "chat", "gemini", "multipart"} {
		t.Run(route, func(t *testing.T) {
			app, fake, data := newProtocolApp(t)
			source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
			const usage = "element"
			var response *httptest.ResponseRecorder

			switch route {
			case "images":
				response = protocolRequest(app, "POST", "/v1/images/generations", map[string]any{
					"model":            "gpt-image-2",
					"prompt":           "调整画面",
					"images":           []any{map[string]string{"image_url": source}},
					"reference_usages": []string{usage},
				})
			case "chat":
				response = protocolRequest(app, "POST", "/v1/chat/completions", map[string]any{
					"model":            "gpt-image-2",
					"reference_usages": []string{usage},
					"messages": []any{map[string]any{"role": "user", "content": []any{
						map[string]string{"type": "text", "text": "调整画面"},
						map[string]any{"type": "image_url", "image_url": map[string]string{"url": source}},
					}}},
				})
			case "gemini":
				response = protocolRequest(app, "POST", "/v1beta/models/gpt-image-2:generateContent", map[string]any{
					"contents": []any{map[string]any{"parts": []any{
						map[string]string{"text": "调整画面"},
						map[string]any{"inlineData": map[string]string{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(data)}},
					}}},
					"generationConfig": map[string]any{"imageConfig": map[string]any{"referenceUsages": []string{usage}}},
				})
			case "multipart":
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				_ = writer.WriteField("model", "gpt-image-2")
				_ = writer.WriteField("prompt", "调整画面")
				_ = writer.WriteField("reference_usages", `["element"]`)
				part, err := writer.CreateFormFile("image", "source.png")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = part.Write(data); err != nil {
					t.Fatal(err)
				}
				if err = writer.Close(); err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest("POST", "/v1/images/edits", &body)
				request.Header.Set("Content-Type", writer.FormDataContentType())
				request.Header.Set("Authorization", "Bearer sk-protocol-test-12345")
				response = httptest.NewRecorder()
				app.Handler().ServeHTTP(response, request)
			}

			requireProtocolStatus(t, response, 200)
			fake.mu.Lock()
			input := fake.input
			fake.mu.Unlock()
			if len(input.References) != 1 || len(input.ReferenceUsages) != 1 || input.ReferenceUsages[0] != usage {
				t.Fatalf("参考图用途未进入规范化输入：%+v", input)
			}
			model, _ := resolveModel(input.Model)
			body, err := adobeGenerationBody(model, input, []map[string]string{{"id": "reference-id", "usage": referenceUsage(model, input, 0)}}, 0)
			if err != nil {
				t.Fatal(err)
			}
			references := body["referenceBlobs"].([]map[string]string)
			if len(references) != 1 || references[0]["usage"] != usage {
				t.Fatalf("参考图用途未到达 Adobe 请求：%v", references)
			}
		})
	}
}

// 随机调度的测试检查排除条件和并发独占，不依赖偶然的随机分布。
func TestAccountStrategiesPersistAndExcludeBusyAccounts(t *testing.T) {
	for _, strategy := range []string{"round_robin", "random"} {
		t.Run(strategy, func(t *testing.T) {
			store := newStorageTestStore(t)
			settings := store.Settings()
			if settings.AccountStrategy != "round_robin" {
				t.Fatal("旧设置应默认轮询")
			}
			settings.AccountStrategy, settings.MaxConcurrent = strategy, 3
			if err := store.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			for _, account := range []Account{{ID: "a", Name: "A", Enabled: true}, {ID: "b", Name: "B", Enabled: false}, {ID: "c", Name: "C", Enabled: true}, {ID: "d", Name: "D", Enabled: true}} {
				if err := store.SaveAccount(account); err != nil {
					t.Fatal(err)
				}
			}
			app := &App{store: store, busy: map[string]bool{"d": true}}
			if strategy == "round_robin" {
				for _, id := range []string{"a", "c", "a", "c"} {
					acc, release, err := app.reserveAccount()
					if err != nil || acc.ID != id {
						t.Fatalf("轮询顺序错误：%s %v", acc.ID, err)
					}
					release()
				}
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			ids := map[string]bool{}
			releases := []func(){}
			for range 2 {
				wg.Go(func() {
					account, release, err := app.reserveAccount()
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						t.Error(err)
						return
					}
					if ids[account.ID] || account.ID == "b" || account.ID == "d" {
						t.Errorf("选到不可用账号 %s", account.ID)
					}
					ids[account.ID] = true
					releases = append(releases, release)
				})
			}
			wg.Wait()
			if _, _, err := app.reserveAccount(); err == nil {
				t.Fatal("所有账号占用时应拒绝请求")
			}
			for _, release := range releases {
				release()
			}
			if app.active != 0 {
				t.Fatal("账号占用未释放")
			}
			reopened, err := NewStore(store.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.Settings().AccountStrategy != strategy {
				t.Fatal("调度方式未持久化")
			}
		})
	}
}

func TestGeminiSearchSeedAndMultipartRejectMalformedOptions(t *testing.T) {
	app, fake, _ := newProtocolApp(t)
	body := map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": "地球"}}}}, "generationConfig": map[string]any{"seed": 42, "imageConfig": map[string]string{"aspectRatio": "4:1", "imageSize": "2K"}}, "tools": []any{map[string]any{"googleSearch": map[string]any{}}}}
	requireProtocolStatus(t, protocolRequest(app, "POST", "/v1beta/models/nano-banana-2:generateContent", body), 200)
	if fake.input.Seed == nil || *fake.input.Seed != 42 || fake.input.GroundSearch == nil || !*fake.input.GroundSearch {
		t.Fatal("Gemini 搜索或种子丢失")
	}
	for _, field := range []string{"seed", "ground_search", "detail_level", "seeds", "model_specific_payload"} {
		var buf bytes.Buffer
		writer := multipart.NewWriter(&buf)
		_ = writer.WriteField(field, "invalid")
		_ = writer.Close()
		req := httptest.NewRequest("POST", "/v1/images/edits", &buf)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		_, err := app.imageMultipart(httptest.NewRecorder(), req)
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s 无效值未被拒绝：%v", field, err)
		}
	}
	input := GenerateInput{ModelSpecificPayload: map[string]any{"api_key": "secret", "value": strings.Repeat("x", 32700)}}
	if !json.Valid(input.parameterJSON()) || bytes.Contains(input.parameterJSON(), []byte("secret")) {
		t.Fatal("扩展参数日志无效或泄露凭据")
	}
}

// 兼容网关渠道别名、output_format 及扩展画质参数。
func TestGatewayCompatibleOutputFormatAndModelAliases(t *testing.T) {
	app, fake, data := newProtocolApp(t)
	body := map[string]any{
		"model":         "gpt-image-2.5-sunburst-满血",
		"prompt":        "中转渠道兼容性测试",
		"size":          "3840x2160",
		"quality":       "xhigh",
		"background":    "transparent",
		"output_format": "png",
		"moderation":    "auto",
	}
	requireProtocolStatus(t, protocolRequest(app, "POST", "/v1/images/generations", body), 200)
	if fake.input.Model != "gpt-image-2.5-sunburst" || fake.input.Quality != "max" || fake.input.Background != "transparent" {
		t.Fatalf("网关文生图参数映射错误: %+v", fake.input)
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "xia-gpt-image-2.5-flare-官渠")
	_ = writer.WriteField("prompt", "中转渠道图生图测试")
	_ = writer.WriteField("size", "2880x2880")
	_ = writer.WriteField("quality", "max")
	_ = writer.WriteField("background", "transparent")
	_ = writer.WriteField("output_format", "png")
	_ = writer.WriteField("moderation", "auto")
	part, _ := writer.CreateFormFile("image[]", "input-1.png")
	_, _ = part.Write(data)
	_ = writer.Close()
	req := httptest.NewRequest("POST", "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer sk-protocol-test-12345")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	requireProtocolStatus(t, rec, 200)
	if fake.input.Model != "gpt-image-2.5-flare" || fake.input.Quality != "max" || fake.input.Resolution != "4K" {
		t.Fatalf("网关图生图参数映射错误: %+v", fake.input)
	}
}

