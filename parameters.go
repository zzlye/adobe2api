package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// validateModelOptions 按上游能力拒绝无效参数，避免接口成功却静默丢弃选项。
func validateModelOptions(model Model, input GenerateInput) error {
	if input.Quality != "" && !hasString(model.Qualities, input.Quality) {
		return fmt.Errorf("该模型画质支持 %s", strings.Join(model.Qualities, "、"))
	}
	if input.DetailLevel != nil {
		if *input.DetailLevel < 1 || *input.DetailLevel > model.MaxDetailLevel {
			return errors.New("detail_level 超出当前模型支持范围")
		}
		if input.Quality != "" && input.Quality != "auto" && qualityDetail(model, input.Quality) != *input.DetailLevel {
			return errors.New("quality 与 detail_level 不一致")
		}
	}
	if input.Background != "" && !hasString([]string{"auto", "opaque", "transparent"}, input.Background) {
		return errors.New("background 应为 auto、opaque 或 transparent")
	}
	if (input.Background != "" && input.Background != "auto" || input.TransparentBackground != nil) && !model.SupportsTransparency {
		return errors.New("该模型未提供背景透明度参数")
	}
	if input.TransparentBackground != nil && input.Background != "" && input.Background != "auto" && *input.TransparentBackground != (input.Background == "transparent") {
		return errors.New("background 与 transparent_background 不一致")
	}
	if input.GroundSearch != nil && !model.SupportsGroundSearch {
		return errors.New("该模型不支持 ground_search")
	}
	if (input.Seed != nil || len(input.Seeds) > 0) && !model.SupportsSeed {
		return errors.New("该模型未提供种子参数")
	}
	if input.Seed != nil && len(input.Seeds) > 0 {
		return errors.New("seed 与 seeds 不能同时指定")
	}
	if len(input.Seeds) > 0 && len(input.Seeds) != max(1, input.N) {
		return errors.New("seeds 数量应与 n 一致")
	}
	if len(input.ModelSpecificPayload) > 0 {
		raw, err := json.Marshal(input.ModelSpecificPayload)
		if err != nil || len(raw) > 32768 {
			return errors.New("model_specific_payload 不能超过 32 KB")
		}
		// 尺寸和比例只保留一个入口，避免扩展参数覆盖已校验的顶层选项。
		for _, key := range []string{"size", "aspectRatio", "resolution", "generationSettings", "detailLevel", "transparentBackground"} {
			if _, exists := input.ModelSpecificPayload[key]; exists {
				return fmt.Errorf("请通过专用参数设置 %s", key)
			}
		}
	}
	if input.Output != nil && input.Output.TargetFolder != nil {
		folder := input.Output.TargetFolder
		if strings.TrimSpace(folder.CreativeCloudProjectID) == "" || len(folder.CreativeCloudProjectID) > 1024 || len(folder.CreativeCloudPath) > 4096 {
			return errors.New("output.targetFolder 需要有效的 creativeCloudProjectId，路径不能超过 4096 字节")
		}
	}
	if len(input.References) > model.MaxReferences {
		return fmt.Errorf("所选模型最多支持 %d 张参考图", model.MaxReferences)
	}
	if len(input.ReferenceUsages) > 0 && len(input.ReferenceUsages) != len(input.References) {
		return errors.New("reference_usages 数量必须与参考图一致")
	}
	for _, usage := range input.ReferenceUsages {
		if !hasString(model.ReferenceUsages, usage) {
			return fmt.Errorf("该模型参考图用途支持 %s", strings.Join(model.ReferenceUsages, "、"))
		}
	}
	var total int64
	for _, reference := range input.allReferences() {
		if !hasString([]string{"image/png", "image/jpeg", "image/webp"}, reference.MIME) {
			return errors.New("该模型参考图仅支持 PNG、JPEG、WebP")
		}
		if reference.Bytes > model.MaxReferenceBytes {
			return fmt.Errorf("该模型单张参考图不能超过 %d MB", model.MaxReferenceBytes>>20)
		}
		total += reference.Bytes
	}
	if total > protocolBodyLimit {
		return errors.New("参考图与遮罩合计不能超过 64 MB")
	}
	if input.MaskReference != nil {
		if !model.SupportsMask || len(input.References) == 0 {
			return errors.New("遮罩需要支持编辑的 GPT 模型和至少一张参考图")
		}
		mask, source := input.MaskReference, input.References[0]
		if mask.Width <= 0 || mask.Height <= 0 || mask.Width != source.Width || mask.Height != source.Height {
			return errors.New("遮罩尺寸必须与第一张参考图相同")
		}
		if !imageHasAlpha(*mask) {
			return errors.New("遮罩必须为包含透明通道的 PNG 或 WebP 图片")
		}
	}
	return nil
}

// referenceUsage 为未显式标记的参考图选择网页端模型的默认用途。
func referenceUsage(model Model, input GenerateInput, index int) string {
	if index < len(input.ReferenceUsages) {
		return input.ReferenceUsages[index]
	}
	if model.AdobeID == "gpt-image" {
		return "source"
	}
	return "general"
}

func qualityDetail(model Model, quality string) int {
	switch quality {
	case "low":
		return 1
	case "high":
		return 5
	case "max":
		return model.MaxDetailLevel
	default:
		return 3
	}
}

func (input GenerateInput) allReferences() []ImageRecord {
	items := append([]ImageRecord(nil), input.References...)
	if input.MaskReference != nil {
		items = append(items, *input.MaskReference)
	}
	return items
}

// 参数日志只保留选项和图片索引，避免持有遮罩编码或泄露扩展参数中的认证信息。
func (input GenerateInput) parameterJSON() json.RawMessage {
	input.Model, input.Prompt, input.References, input.Mask = "", "", nil, nil
	raw, _ := json.Marshal(input)
	decoded, _ := decodeUpstreamLogJSON(raw)
	value, _ := decoded.(map[string]any)
	delete(value, "model")
	delete(value, "prompt")
	delete(value, "references")
	raw, _ = json.Marshal(sanitizeLogValue(value, 0))
	return raw
}

func ratioNumber(ratio string) (float64, error) {
	parts := strings.Split(ratio, ":")
	if len(parts) != 2 {
		return 0, errors.New("比例应为 宽:高")
	}
	w, e1 := strconv.ParseFloat(parts[0], 64)
	h, e2 := strconv.ParseFloat(parts[1], 64)
	r := w / h
	if e1 != nil || e2 != nil || w <= 0 || h <= 0 || math.IsNaN(r) || math.IsInf(r, 0) || r < 1.0/3 || r > 3 {
		return 0, errors.New("GPT 自定义比例应在 1:3 至 3:1 之间")
	}
	return r, nil
}

func validGPTSize(width, height int) error {
	if width < 480 || height < 480 || width > 3840 || height > 3840 || width%16 != 0 || height%16 != 0 || width*height < 655360 || width*height > adobeGPTMaxPixels || width > 3*height || height > 3*width {
		return errors.New("GPT 尺寸每边须为 480～3840 的 16 倍数，总像素 655360～8294400，长宽比不超过 3:1")
	}
	return nil
}

// 自定义比例按目标面积计算并向下对齐，保证不会因四舍五入超过像素上限。
func customGPTSize(ratio, resolution string) (int, int, error) {
	r, err := ratioNumber(ratio)
	if err != nil {
		return 0, 0, err
	}
	pixels := map[string]float64{"1K": 1024 * 1024, "2K": 2048 * 2048, "4K": adobeGPTMaxPixels}[resolution]
	w, h := math.Sqrt(pixels*r), math.Sqrt(pixels/r)
	scale := math.Min(1, 3840/math.Max(w, h))
	width, height := int(w*scale)/16*16, int(h*scale)/16*16
	// 极限比例的向下对齐可能略微越界，再缩短长边一个步长。
	if width > 3*height {
		width = 3 * height
	}
	if height > 3*width {
		height = 3 * width
	}
	return width, height, validGPTSize(width, height)
}

// 只检查容器中的透明通道声明，避免为校验遮罩解码整幅大图。
func imageHasAlpha(record ImageRecord) bool {
	f, err := os.Open(record.Path)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [26]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	if record.MIME == "image/png" {
		if string(head[:8]) != "\x89PNG\r\n\x1a\n" {
			return false
		}
		if head[25] == 4 || head[25] == 6 {
			return true
		}
		if _, err := f.Seek(8, io.SeekStart); err != nil {
			return false
		}
		for i := 0; i < 1024; i++ {
			var chunk [8]byte
			if _, err := io.ReadFull(f, chunk[:]); err != nil {
				return false
			}
			switch string(chunk[4:]) {
			case "tRNS":
				return true
			case "IDAT", "IEND":
				return false
			}
			if _, err := f.Seek(int64(binary.BigEndian.Uint32(chunk[:4]))+4, io.SeekCurrent); err != nil {
				return false
			}
		}
	}
	if record.MIME == "image/webp" && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		switch string(head[12:16]) {
		case "VP8X":
			return head[20]&0x10 != 0
		case "VP8L":
			return head[24]&0x10 != 0
		}
	}
	return false
}
