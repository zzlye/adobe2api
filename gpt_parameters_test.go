package main

import "testing"

func TestGPTHighResolutionParametersMatchProtocolAndUpstream(t *testing.T) {
	for _, id := range []string{"gpt-image-2", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare"} {
		model, _ := resolveModel(id)
		for _, test := range []struct{ size, ratio, resolution string }{
			{"1024x1024", "1:1", "1K"}, {"2048x2048", "1:1", "2K"}, {"2880x2880", "1:1", "4K"},
			{"2496x1664", "3:2", "2K"}, {"3504x2336", "3:2", "4K"}, {"2336x3504", "2:3", "4K"},
		} {
			input := GenerateInput{Model: id, Prompt: "山间小屋", Size: test.size, AspectRatio: "auto"}
			if err := validateInput(&input); err != nil {
				t.Fatalf("%s/%s 校验失败：%v", id, test.size, err)
			}
			ratio, resolution, width, height, err := adobeParameters(model, input)
			if err != nil || input.AspectRatio != test.ratio || input.Resolution != test.resolution || ratio != input.AspectRatio || resolution != input.Resolution {
				t.Fatalf("协议与上游应使用同一档位：%s/%s %+v %s/%s %v", id, test.size, input, ratio, resolution, err)
			}
			body, err := adobeGenerationBody(model, input, nil, 42)
			if err != nil {
				t.Fatal(err)
			}
			size := body["size"].(map[string]int)
			if size["width"] != width || size["height"] != height {
				t.Fatalf("%s 明确尺寸不能被默认档位覆盖：%v", id, size)
			}
		}
		for _, resolution := range []string{"1K", "2K", "4K"} {
			if !hasString(model.Resolutions, resolution) {
				t.Fatalf("%s 模型目录缺少已验证档位 %s", id, resolution)
			}
		}
		if !model.HighResolutionRequiresRatio {
			t.Fatalf("%s 高分辨率需要明确比例的能力信息不能缺失", id)
		}
	}
	older, _ := resolveModel("gpt-image-1.5")
	if len(older.Resolutions) != 1 || older.Resolutions[0] != "1K" {
		t.Fatal("未经验证的 GPT1.5 高分辨率不能开放")
	}
}

func TestGPTHighResolutionRequiresExplicitRatio(t *testing.T) {
	for _, id := range []string{"gpt-image-2", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare"} {
		model, _ := resolveModel(id)
		for _, resolution := range []string{"2K", "4K"} {
			for _, ratio := range []string{"", "auto"} {
				input := GenerateInput{Model: id, Prompt: "山间小屋", Size: "auto", AspectRatio: ratio, Resolution: resolution}
				if err := validateInput(&input); err == nil {
					t.Fatalf("高分自动比例不得静默退回 1K：%+v", input)
				}
				if _, err := adobeGenerationBody(model, input, nil, 42); err == nil {
					t.Fatalf("直接上游入口应采用相同高分限制：%+v", input)
				}
			}
		}
		body, err := adobeGenerationBody(model, GenerateInput{Prompt: "山间小屋", Size: "auto", Resolution: "1K"}, nil, 42)
		if err != nil || body["modelSpecificPayload"].(map[string]any)["size"] != "auto" {
			t.Fatalf("原有 1K 自动模式仍应可用：%v %v", body, err)
		}
		if _, exists := body["size"]; exists {
			t.Fatal("1K 自动模式不能强加固定比例")
		}
	}
	for _, id := range []string{"nano-banana-2", "nano-banana-pro"} {
		model, _ := resolveModel(id)
		if model.HighResolutionRequiresRatio {
			t.Fatalf("香蕉不能继承 GPT 的未验证自动模式限制：%s", id)
		}
		if _, err := adobeGenerationBody(model, GenerateInput{Prompt: "山间小屋", AspectRatio: "auto", Resolution: "4K"}, nil, 42); err != nil {
			t.Fatalf("香蕉高分自动模式应继续工作：%v", err)
		}
	}
}

func TestGPTResolutionMapsStayWithinVerifiedLimits(t *testing.T) {
	for _, id := range []string{"gpt-image-2", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare"} {
		model, _ := resolveModel(id)
		for _, ratio := range model.Ratios {
			if ratio == "auto" {
				continue
			}
			for _, resolution := range []string{"2K", "4K"} {
				input := GenerateInput{Prompt: "山间小屋", AspectRatio: ratio, Resolution: resolution}
				body, err := adobeGenerationBody(model, input, nil, 42)
				if err != nil {
					t.Fatal(id, ratio, resolution, err)
				}
				size := body["size"].(map[string]int)
				width, height := size["width"], size["height"]
				if width <= 0 || height <= 0 || width > adobeGPTMaxEdge || height > adobeGPTMaxEdge || width*height > adobeGPTMaxPixels || width%16 != 0 || height%16 != 0 {
					t.Fatalf("默认尺寸必须满足实证上限并按 16 像素对齐：%s/%s/%s %v", id, ratio, resolution, size)
				}
				if inferred := adobeSizeResolution(model, width, height); inferred != resolution {
					t.Fatalf("默认档位应与明确尺寸推断一致：%s/%s/%s 推断 %s", id, ratio, resolution, inferred)
				}
			}
		}
	}
	model, _ := resolveModel("gpt-image-2")
	body, err := adobeGenerationBody(model, GenerateInput{Prompt: "山间小屋", Size: "3840x2160", Resolution: "4K"}, nil, 42)
	if err != nil || body["size"].(map[string]int)["width"] != 3840 || body["size"].(map[string]int)["height"] != 2160 {
		t.Fatalf("实证成功的 UHD 请求应原样保留：%v %v", body, err)
	}
}

func TestGPTRejectsOversizedAndContradictoryDimensionsBeforeGeneration(t *testing.T) {
	for _, id := range []string{"gpt-image-2", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare"} {
		model, _ := resolveModel(id)
		for _, input := range []GenerateInput{
			{Size: "4096x4096"}, {Size: "3840x3840"}, {Size: "3856x2168"},
			{Size: "2880x2880", Resolution: "2K"}, {Size: "3504x2336", AspectRatio: "1:1"}, {Size: "64x64"},
		} {
			input.Model, input.Prompt = id, "山间小屋"
			if err := validateInput(&input); err == nil {
				t.Fatalf("协议应拒绝超过上限或矛盾的尺寸：%+v", input)
			}
			if _, err := adobeGenerationBody(model, input, nil, 42); err == nil {
				t.Fatalf("直接上游入口也应拒绝相同尺寸：%+v", input)
			}
		}
	}
	for _, id := range []string{"gpt-image-2.5-sunburst", "gpt-image-2.5-flare"} {
		input := GenerateInput{Model: id, Prompt: "山间小屋", AspectRatio: "16:9", Resolution: "4K"}
		if err := validateInput(&input); err != nil {
			t.Fatalf("GPT2.5 模型目录声明的合法尺寸应开放：%s %v", id, err)
		}
	}
}
