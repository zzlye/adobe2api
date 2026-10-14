package main

import "strings"

// 模型标识与参数来自实际页面菜单；旧版香蕉仅留在静态脚本中，不作为可用模型发布。
var availableModels = []Model{
	{ID: "gemini-3.1-flash-image-preview", Name: "Nano Banana 2", AdobeID: "gemini-flash", Version: "nano-banana-3", Ratios: []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9", "8:1", "4:1", "1:4", "1:8"}, Resolutions: []string{"512", "1K", "2K", "4K"}},
	{ID: "gemini-3-pro-image-preview", Name: "Nano Banana Pro", AdobeID: "gemini-flash", Version: "nano-banana-2", Ratios: []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9"}, Resolutions: []string{"1K", "2K", "4K"}},
	{ID: "gpt-image-2.5-sunburst", Name: "GPT Image 2.5 Sunburst", AdobeID: "gpt-image", Version: "gpt-image-2.5-prism", Ratios: []string{"auto", "1:1", "3:2", "2:3"}, Resolutions: []string{"1K"}},
	{ID: "gpt-image-2.5-flare", Name: "GPT Image 2.5 Flare", AdobeID: "gpt-image", Version: "gpt-image-2.5-flare", Ratios: []string{"auto", "1:1", "3:2", "2:3"}, Resolutions: []string{"1K"}},
	{ID: "gpt-image-2", Name: "GPT Image 2", AdobeID: "gpt-image", Version: "2", Ratios: []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9"}, Resolutions: []string{"1K"}},
	{ID: "gpt-image-1.5", Name: "GPT Image 1.5", AdobeID: "gpt-image", Version: "1.5", Ratios: []string{"1:1", "3:2", "2:3"}, Resolutions: []string{"1K"}},
}

func resolveModel(id string) (Model, bool) {
	id = strings.TrimPrefix(id, "models/")
	aliases := map[string]string{"nano-banana-2": "gemini-3.1-flash-image-preview", "gemini-3.1-flash-image": "gemini-3.1-flash-image-preview", "nano-banana-pro": "gemini-3-pro-image-preview", "gemini-3-pro-image": "gemini-3-pro-image-preview", "gpt-image-2.5-prism": "gpt-image-2.5-sunburst"}
	if alias, ok := aliases[id]; ok {
		id = alias
	}
	for _, m := range availableModels {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}
