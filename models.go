package main

import "strings"

// 模型标识来自实际页面菜单，GPT 高分辨率另经真实输出验证；旧版香蕉不作为可用模型发布。
var availableModels = []Model{
	{ID: "gemini-3.1-flash-image-preview", Name: "Nano Banana 2", AdobeID: "gemini-flash", Version: "nano-banana-3", Ratios: []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9", "8:1", "4:1", "1:4", "1:8"}, Resolutions: []string{"512", "1K", "2K", "4K"}},
	{ID: "gemini-3-pro-image-preview", Name: "Nano Banana Pro", AdobeID: "gemini-flash", Version: "nano-banana-2", Ratios: []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9"}, Resolutions: []string{"1K", "2K", "4K"}},
	{ID: "gpt-image-2.5-sunburst", Name: "GPT Image 2.5 Sunburst", AdobeID: "gpt-image", Version: "gpt-image-2.5-prism", Ratios: []string{"auto", "1:1", "3:2", "2:3"}, Resolutions: []string{"1K", "2K", "4K"}, HighResolutionRequiresRatio: true},
	{ID: "gpt-image-2.5-flare", Name: "GPT Image 2.5 Flare", AdobeID: "gpt-image", Version: "gpt-image-2.5-flare", Ratios: []string{"auto", "1:1", "3:2", "2:3"}, Resolutions: []string{"1K", "2K", "4K"}, HighResolutionRequiresRatio: true},
	{ID: "gpt-image-2", Name: "GPT Image 2", AdobeID: "gpt-image", Version: "2", Ratios: []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9"}, Resolutions: []string{"1K", "2K", "4K"}, HighResolutionRequiresRatio: true},
	{ID: "gpt-image-1.5", Name: "GPT Image 1.5", AdobeID: "gpt-image", Version: "1.5", Ratios: []string{"1:1", "3:2", "2:3"}, Resolutions: []string{"1K"}},
}

// 能力来自 Adobe 模型目录，生成、协议校验和页面选项共用同一份定义。
func init() {
	for i := range availableModels {
		m := &availableModels[i]
		m.Qualities = []string{"auto"}
		m.MaxReferences = 14
		m.MaxReferenceBytes = 20 << 20
		m.ReferenceUsages = []string{"general", "element"}
		m.SupportsGroundSearch = m.AdobeID == "gemini-flash"
		m.SupportsSeed = m.SupportsGroundSearch
		if m.AdobeID == "gpt-image" {
			m.MaxReferences, m.MaxDetailLevel = 16, 5
			// GPT 上游单图上限高于服务请求体上限，因此目录展示服务可接受值。
			m.MaxReferenceBytes = protocolBodyLimit
			m.ReferenceUsages = []string{"source", "element"}
			m.SupportsMask = true
			m.Qualities = []string{"auto", "low", "medium", "high", "max"}
			m.SupportsCustomSize = m.Version != "1.5"
			if strings.HasPrefix(m.Version, "gpt-image-2.5-") {
				m.MaxDetailLevel, m.SupportsTransparency = 7, true
				m.Ratios = []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "5:4", "4:5", "21:9"}
			}
		}
	}
}

func resolveModel(id string) (Model, bool) {
	id = strings.TrimSpace(strings.TrimPrefix(id, "models/"))
	// 兼容网关渠道转发时附带的上下游前缀与规格后缀。
	for _, prefix := range []string{"xia-", "xin-"} {
		id = strings.TrimPrefix(id, prefix)
	}
	for _, suffix := range []string{"-满血", "-官渠", "-专线", "-vip", "-4k", "-4K", "-2k", "-2K", "-1k", "-1K"} {
		id = strings.TrimSuffix(id, suffix)
	}
	aliases := map[string]string{
		"nano-banana-2":          "gemini-3.1-flash-image-preview",
		"nano-banana-2.1":        "gemini-3.1-flash-image-preview",
		"gemini-3.1-flash-image": "gemini-3.1-flash-image-preview",
		"nano-banana-pro":        "gemini-3-pro-image-preview",
		"gemini-3-pro-image":     "gemini-3-pro-image-preview",
		"gpt-image-2.5-prism":    "gpt-image-2.5-sunburst",
	}
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
