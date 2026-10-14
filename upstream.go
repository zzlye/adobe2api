package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const adobeUpstreamURL = "https://firefly-3p.ff.adobe.io"
const adobeIMSURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token"
const adobeIMSScope = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext,additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles,account_cluster.read,creative_production,tk_platform,tk_platform_sync,af_byof,profile,dii_lr_ml_unlimited"

// AdobeGenerator 复用短连接池；图片在磁盘与网络间传输，不在内存中保存编码。
type AdobeGenerator struct {
	store          *Store
	browserRefresh func(context.Context, Account) (Account, error)
	mu             sync.Mutex
	client         *http.Client
	downloadClient *http.Client
	transport      *http.Transport
	proxyURL       string
}

func NewAdobeGenerator(store *Store) *AdobeGenerator {
	return &AdobeGenerator{store: store}
}

// UpstreamError 保留状态与错误码，供协议层选择对应的失败响应。
type UpstreamError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *UpstreamError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("Adobe 请求失败（%d）：%s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("Adobe 请求失败（%d/%s）：%s", e.StatusCode, e.Code, e.Message)
}

func (g *AdobeGenerator) clients() (*http.Client, *http.Client, error) {
	settings := g.store.Settings()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.client != nil && g.downloadClient != nil && g.proxyURL == settings.ProxyURL {
		return g.client, g.downloadClient, nil
	}
	var proxy func(*http.Request) (*url.URL, error)
	if settings.ProxyURL != "" {
		parsed, err := url.Parse(settings.ProxyURL)
		if err != nil || parsed.Host == "" || !hasString([]string{"http", "https", "socks5", "socks5h"}, parsed.Scheme) {
			return nil, nil, errors.New("代理地址无效，仅支持 HTTP、HTTPS 和 SOCKS5")
		}
		proxy = http.ProxyURL(parsed)
	}
	if g.transport != nil {
		g.transport.CloseIdleConnections()
	}
	transport := &http.Transport{
		Proxy:             proxy,
		DialContext:       (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true,
		MaxIdleConns:      16, MaxIdleConnsPerHost: 4, MaxConnsPerHost: 16,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second, ExpectContinueTimeout: time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 90 * time.Second}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !isAdobeURL(request.URL) {
			return errors.New("Adobe 请求重定向地址无效")
		}
		return nil
	}
	download := &http.Client{Transport: transport, Timeout: 90 * time.Second}
	download.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !isAdobeImageURL(request.URL) {
			return errors.New("图片重定向地址无效")
		}
		return nil
	}
	g.transport, g.client, g.downloadClient, g.proxyURL = transport, client, download, settings.ProxyURL
	return client, download, nil
}

func (g *AdobeGenerator) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.transport != nil {
		g.transport.CloseIdleConnections()
	}
}

func hasString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func validHTTPSURL(parsed *url.URL) bool {
	return parsed != nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Hostname() != "" && (parsed.Port() == "" || parsed.Port() == "443")
}

func hostInDomains(host string, domains ...string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func isAdobeURL(parsed *url.URL) bool {
	return validHTTPSURL(parsed) && hostInDomains(parsed.Hostname(), "adobe.com", "adobe.io")
}

func isAdobeImageURL(parsed *url.URL) bool {
	return validHTTPSURL(parsed) && hostInDomains(parsed.Hostname(), "adobe.com", "adobe.io", "adobecc.com", "adobestatic.com", "amazonaws.com", "cloudfront.net")
}

// resolveAdobeLink 只接受 Adobe 的 HTTPS 地址，防止轮询链接带走账号凭据。
func resolveAdobeLink(raw, base string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || raw == "" {
		return "", errors.New("Adobe 未返回有效任务地址")
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", errors.New("Adobe 任务来源地址无效")
	}
	parsed = baseURL.ResolveReference(parsed)
	if !isAdobeURL(parsed) {
		return "", errors.New("Adobe 返回的任务地址不在受信任域名内")
	}
	return parsed.String(), nil
}

// adobeNonce 与页面实现一致：先按 UTF-16 截取提示前 256 个码元，再计算摘要。
func adobeNonce(userID, prompt string) string {
	units := utf16.Encode([]rune(prompt))
	if len(units) > 256 {
		units = units[:256]
	}
	digest := sha256.Sum256([]byte(userID + "-" + string(utf16.Decode(units))))
	return hex.EncodeToString(digest[:])
}

func adobeUserID(account Account) string {
	if account.UserID != "" {
		return account.UserID
	}
	// 部分 Adobe 令牌含可读取的身份声明；不把解码结果当作认证依据。
	for _, segment := range strings.Split(account.AccessToken, ".") {
		decoded, err := base64.RawURLEncoding.DecodeString(segment)
		if err != nil || len(decoded) > 64<<10 {
			continue
		}
		var claims map[string]json.RawMessage
		if json.Unmarshal(decoded, &claims) != nil {
			continue
		}
		for _, key := range []string{"AdoBEID", "adobeId", "adobe_id", "user_id", "userId", "sub"} {
			var value string
			if json.Unmarshal(claims[key], &value) == nil && strings.Contains(value, "@AdobeID") && len(value) < 256 {
				return value
			}
		}
		// Firefly 的 IMS 访问令牌将用户标识放在 adobe.AdoBEID 中。
		var adobe map[string]string
		if json.Unmarshal(claims["adobe"], &adobe) == nil && adobe["AdoBEID"] != "" {
			return adobe["AdoBEID"]
		}
	}
	return ""
}

func adobeHeaders(request *http.Request, account Account, prompt string, generation bool) {
	request.Header.Set("Authorization", "Bearer "+account.AccessToken)
	key := account.APIKey
	if key == "" {
		key = account.ClientID
	}
	if key == "" {
		key = "clio-playground-web"
	}
	request.Header.Set("X-Api-Key", key)
	request.Header.Set("Origin", "https://firefly.adobe.com")
	request.Header.Set("Referer", "https://firefly.adobe.com/")
	if generation {
		request.Header.Set("X-Nonce", adobeNonce(adobeUserID(account), prompt))
		if account.ARPSession != "" {
			request.Header.Set("X-Arp-Session-Id", account.ARPSession)
		}
	}
}

func (g *AdobeGenerator) do(client *http.Client, request *http.Request, log *RequestLog) (*http.Response, error) {
	started := time.Now()
	response, err := client.Do(request)
	if log != nil && len(log.Upstream) < 4096 {
		record := UpstreamRecord{Method: request.Method, URL: sanitizeLoggedURL(request.URL.String()), DurationMS: time.Since(started).Milliseconds()}
		if response != nil {
			record.Status = response.StatusCode
			record.RequestID = response.Header.Get("X-Request-Id")
		}
		log.Upstream = append(log.Upstream, record)
	}
	if err != nil {
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		// 网络错误不返回原始 URL，避免签名参数出现在错误日志中。
		return nil, errors.New("连接 Adobe 失败，请检查网络与代理设置")
	}
	return response, nil
}

type adobeLink struct {
	Href string `json:"href"`
}

type adobeImageOutput struct {
	Seed  uint32 `json:"seed"`
	Image struct {
		ID           string `json:"id"`
		PresignedURL string `json:"presignedUrl"`
	} `json:"image"`
	Error json.RawMessage `json:"error"`
}

type adobeGenerationResponse struct {
	Links       map[string]adobeLink `json:"links"`
	Outputs     []adobeImageOutput   `json:"outputs"`
	Progress    float64              `json:"progress"`
	ContentType string               `json:"contentType"`
	ErrorCode   string               `json:"error_code"`
	Message     string               `json:"message"`
}

func decodeAdobeResponse(response *http.Response) (adobeGenerationResponse, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return adobeGenerationResponse{}, errors.New("读取 Adobe 响应失败")
	}
	if len(body) > 2<<20 {
		return adobeGenerationResponse{}, errors.New("Adobe 响应超过允许大小")
	}
	var payload adobeGenerationResponse
	decodeErr := json.Unmarshal(body, &payload)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := payload.Message
		if message == "" || len(message) > 1024 {
			message = http.StatusText(response.StatusCode)
		}
		return payload, &UpstreamError{StatusCode: response.StatusCode, Code: payload.ErrorCode, Message: message}
	}
	if decodeErr != nil {
		return payload, errors.New("Adobe 返回了无效响应")
	}
	if payload.ErrorCode != "" {
		return payload, &UpstreamError{StatusCode: http.StatusBadGateway, Code: payload.ErrorCode, Message: payload.Message}
	}
	return payload, nil
}

func adobeWaitDuration(header http.Header) time.Duration {
	duration := 2 * time.Second
	if seconds, err := strconv.ParseFloat(header.Get("Retry-After"), 64); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
		duration = time.Duration(seconds * float64(time.Second))
	} else if date, err := http.ParseTime(header.Get("Retry-After")); err == nil {
		duration = time.Until(date)
	}
	if duration < time.Second {
		return time.Second
	}
	if duration > 60*time.Second {
		return 60 * time.Second
	}
	return duration
}

func waitAdobe(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func generationCompleted(header http.Header, payload adobeGenerationResponse) bool {
	status := strings.ToUpper(header.Get("X-Task-Status"))
	return status == "COMPLETED" || payload.Progress >= 100 || (status == "" && len(payload.Outputs) > 0 && len(payload.Links) == 0)
}

func (g *AdobeGenerator) uploadReference(ctx context.Context, client *http.Client, account Account, image ImageRecord, log *RequestLog) (string, error) {
	file, err := os.Open(image.Path)
	if err != nil {
		return "", errors.New("参考图文件不可用")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.Size() == 0 || stat.Size() > 48<<20 {
		return "", errors.New("参考图为空或超过 Adobe 的 48 MB 上传限制")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, adobeUpstreamURL+"/v2/storage/image", file)
	if err != nil {
		return "", err
	}
	request.ContentLength = stat.Size()
	request.Header.Set("Content-Type", image.MIME)
	adobeHeaders(request, account, "", false)
	response, err := g.do(client, request, log)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var payload struct {
		Images []struct {
			ID string `json:"id"`
		} `json:"images"`
		ErrorCode string `json:"error_code"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", errors.New("Adobe 参考图上传响应无效")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &UpstreamError{StatusCode: response.StatusCode, Code: payload.ErrorCode, Message: payload.Message}
	}
	if len(payload.Images) == 0 || payload.Images[0].ID == "" {
		return "", errors.New("Adobe 未返回参考图标识")
	}
	return payload.Images[0].ID, nil
}

var adobeGPTSizes = map[string][2]int{
	"21:9": {1584, 672}, "16:9": {1376, 768}, "5:4": {1152, 928}, "4:3": {1200, 896}, "3:2": {1264, 848},
	"1:1": {1024, 1024}, "4:5": {928, 1152}, "3:4": {896, 1200}, "2:3": {848, 1264}, "9:16": {768, 1376},
}

func parseAdobeSize(raw string) (int, int, error) {
	parts := strings.Split(strings.ToLower(raw), "x")
	if len(parts) != 2 {
		return 0, 0, errors.New("图片尺寸应使用宽x高格式")
	}
	width, err1 := strconv.Atoi(parts[0])
	height, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return 0, 0, errors.New("图片尺寸无效")
	}
	return width, height, nil
}

func closestAdobeRatio(width, height int, ratios []string) string {
	best, difference := "", math.Inf(1)
	for _, ratio := range ratios {
		parts := strings.Split(ratio, ":")
		if len(parts) != 2 {
			continue
		}
		x, _ := strconv.ParseFloat(parts[0], 64)
		y, _ := strconv.ParseFloat(parts[1], 64)
		delta := math.Abs(math.Log((float64(width) / float64(height)) / (x / y)))
		if delta < difference {
			best, difference = ratio, delta
		}
	}
	if difference > 0.035 {
		return ""
	}
	return best
}

// adobeParameters 按页面模型能力转换参数；方形分辨率档位与输出比例分别传递。
func adobeParameters(model Model, input GenerateInput) (string, string, int, int, error) {
	ratio, resolution := input.AspectRatio, strings.ToUpper(input.Resolution)
	width, height := 0, 0
	if input.Size != "" && input.Size != "auto" {
		var err error
		width, height, err = parseAdobeSize(input.Size)
		if err != nil {
			return "", "", 0, 0, err
		}
		if ratio == "" {
			ratio = closestAdobeRatio(width, height, model.Ratios)
			if ratio == "" {
				return "", "", 0, 0, errors.New("该模型不支持请求的图片比例")
			}
		}
		if resolution == "" {
			pixels := int64(width) * int64(height)
			switch {
			case pixels >= 8<<20 && hasString(model.Resolutions, "4K"):
				resolution = "4K"
			case pixels >= 2<<20 && hasString(model.Resolutions, "2K"):
				resolution = "2K"
			case pixels < 512*768 && hasString(model.Resolutions, "512"):
				resolution = "512"
			default:
				resolution = "1K"
			}
		}
	}
	if ratio == "" {
		if input.Size == "auto" && hasString(model.Ratios, "auto") {
			ratio = "auto"
		} else {
			ratio = "1:1"
		}
	}
	if resolution == "" {
		resolution = "1K"
	}
	if !hasString(model.Ratios, ratio) {
		return "", "", 0, 0, errors.New("该模型不支持指定的图片比例")
	}
	if !hasString(model.Resolutions, resolution) {
		return "", "", 0, 0, errors.New("该模型不支持指定的分辨率")
	}
	return ratio, resolution, width, height, nil
}

func adobeGenerationBody(model Model, input GenerateInput, references []map[string]string, seed uint32) (map[string]any, error) {
	ratio, resolution, width, height, err := adobeParameters(model, input)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"n": 1, "seeds": []uint32{seed}, "prompt": input.Prompt,
		"output": map[string]any{"storeInputs": false}, "referenceBlobs": references,
		"modelId": model.AdobeID, "modelVersion": model.Version,
		"generationMetadata": map[string]string{"module": "text2image", "submodule": "ff-image-generate"},
	}
	if model.AdobeID == "gemini-flash" {
		size := map[string]int{"512": 512, "1K": 1024, "2K": 2048, "4K": 4096}[resolution]
		payload := map[string]any{"parameters": map[string]bool{"addWatermark": false}}
		if ratio != "auto" {
			payload["aspectRatio"] = ratio
		}
		body["size"] = map[string]int{"width": size, "height": size}
		if model.Version == "nano-banana" {
			// 旧版香蕉按页面定义的实际图片尺寸提交，不使用高分辨率档位。
			sizes := map[string][2]int{"1:1": {1024, 1024}, "4:3": {1184, 864}, "3:4": {864, 1184}, "16:9": {1344, 768}, "9:16": {768, 1344}, "21:9": {1536, 672}, "3:2": {1248, 832}, "2:3": {832, 1248}, "5:4": {1152, 896}, "4:5": {896, 1152}}
			actual := sizes[ratio]
			body["size"] = map[string]int{"width": actual[0], "height": actual[1]}
		} else {
			body["groundSearch"] = false
		}
		body["modelSpecificPayload"] = payload
		return body, nil
	}
	if model.AdobeID != "gpt-image" {
		return nil, errors.New("暂不支持该 Adobe 上游模型")
	}
	payload := map[string]any{}
	if ratio == "auto" {
		payload["size"] = "auto"
	} else {
		actual := adobeGPTSizes[ratio]
		if model.Version != "2" {
			actual = map[string][2]int{"1:1": {1024, 1024}, "3:2": {1536, 1024}, "2:3": {1024, 1536}}[ratio]
		} else if width > 0 && height > 0 {
			actual = [2]int{width, height}
		}
		body["size"] = map[string]int{"width": actual[0], "height": actual[1]}
	}
	body["modelSpecificPayload"] = payload
	if model.Version != "1.5" {
		quality := input.Quality
		if quality == "" || quality == "auto" || quality == "standard" {
			quality = "medium"
		}
		detail := map[string]int{"low": 1, "medium": 3, "high": 5}[quality]
		if detail == 0 {
			return nil, errors.New("该模型画质仅支持 low、medium、high")
		}
		body["generationSettings"] = map[string]int{"detailLevel": detail}
	}
	return body, nil
}

func (g *AdobeGenerator) Generate(ctx context.Context, account Account, input GenerateInput, log *RequestLog) (result GenerateResult, err error) {
	seconds := g.store.Settings().RequestTimeoutSeconds
	if seconds <= 0 {
		seconds = 300
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	result = GenerateResult{Images: []ImageRecord{}}
	model, ok := resolveModel(input.Model)
	if !ok {
		return result, errors.New("模型不存在")
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return result, errors.New("图片描述不能为空")
	}
	if account.AccessToken == "" || adobeUserID(account) == "" {
		return result, errors.New("账号缺少访问令牌或用户标识，请先刷新登录状态")
	}
	count := input.N
	if count == 0 {
		count = 1
	}
	if count < 1 || count > 10 {
		return result, errors.New("生成数量应在 1 到 10 之间")
	}
	limit := 6
	if model.Version == "nano-banana" {
		limit = 4
	}
	if len(input.References) > limit {
		return result, fmt.Errorf("该模型最多支持 %d 张参考图", limit)
	}
	if _, err := adobeGenerationBody(model, input, nil, 0); err != nil {
		return result, err
	}
	client, download, err := g.clients()
	if err != nil {
		return result, err
	}
	references := make([]map[string]string, 0, len(input.References))
	for _, reference := range input.References {
		id, err := g.uploadReference(ctx, client, account, reference, log)
		if err != nil {
			return result, err
		}
		usage := "general"
		if model.AdobeID == "gpt-image" {
			usage = "subject"
		}
		references = append(references, map[string]string{"id": id, "usage": usage})
	}
	// 账号池在一次生成期间独占该账号，以前后余额差记录实际消耗。
	before := g.readCreditBalance(ctx, client, account, log)
	defer func() {
		if before != nil {
			result.Credits = creditConsumption(before, g.readCreditBalance(ctx, client, account, log))
		}
	}()
	// 上游每次只提交一次生图；失败不重试 POST，避免重复消耗积分。
	for index := 0; index < count; index++ {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return result, err
		}
		body, err := adobeGenerationBody(model, input, references, binary.BigEndian.Uint32(random[:])&0x7fffffff)
		if err != nil {
			return result, err
		}
		encoded, _ := json.Marshal(body)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, adobeUpstreamURL+"/v2/3p-images/generate-async", strings.NewReader(string(encoded)))
		if err != nil {
			return result, err
		}
		request.Header.Set("Content-Type", "application/json")
		adobeHeaders(request, account, input.Prompt, true)
		response, err := g.do(client, request, log)
		if err != nil {
			return result, err
		}
		payload, err := decodeAdobeResponse(response)
		if err != nil {
			return result, err
		}
		payload, err = g.poll(ctx, client, account, response.Header, payload, log)
		if err != nil {
			return result, err
		}
		if len(payload.Outputs) == 0 {
			return result, errors.New("Adobe 未返回生成图片")
		}
		for _, output := range payload.Outputs {
			if len(output.Error) > 0 && string(output.Error) != "null" {
				return result, errors.New("Adobe 拒绝或未能生成该图片")
			}
			image, err := g.downloadImage(ctx, download, output.Image.PresignedURL, payload.ContentType, log)
			if err != nil {
				return result, err
			}
			result.Images = append(result.Images, image)
		}
	}
	// 消耗数量在返回前由余额快照核对，积分状态头不作为扣费数字。
	return result, nil
}

func (g *AdobeGenerator) poll(ctx context.Context, client *http.Client, account Account, header http.Header, payload adobeGenerationResponse, log *RequestLog) (adobeGenerationResponse, error) {
	var resultURL, cancelURL string
	for {
		status := strings.ToUpper(header.Get("X-Task-Status"))
		if status == "FAILED" || status == "CANCELED" || status == "CANCELLED" {
			return payload, &UpstreamError{StatusCode: http.StatusBadGateway, Code: "generation_failed", Message: "图片生成任务失败或已取消"}
		}
		if generationCompleted(header, payload) {
			return payload, nil
		}
		if raw := payload.Links["cancel"].Href; raw != "" {
			var err error
			cancelURL, err = resolveAdobeLink(raw, adobeUpstreamURL)
			if err != nil {
				return payload, err
			}
		}
		raw := header.Get("X-Override-Status-Link")
		if raw == "" {
			raw = payload.Links["result"].Href
		}
		if raw != "" {
			var err error
			resultURL, err = resolveAdobeLink(raw, adobeUpstreamURL)
			if err != nil {
				return payload, err
			}
		}
		if resultURL == "" {
			return payload, errors.New("Adobe 未返回图片生成任务地址")
		}
		if err := waitAdobe(ctx, adobeWaitDuration(header)); err != nil {
			g.cancelJob(client, account, cancelURL, log)
			return payload, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, resultURL, nil)
		if err != nil {
			return payload, err
		}
		adobeHeaders(request, account, "", false)
		response, err := g.do(client, request, log)
		if err != nil {
			if ctx.Err() != nil {
				g.cancelJob(client, account, cancelURL, log)
			}
			return payload, err
		}
		header = response.Header
		payload, err = decodeAdobeResponse(response)
		if err != nil {
			return payload, err
		}
	}
}

func (g *AdobeGenerator) cancelJob(client *http.Client, account Account, address string, log *RequestLog) {
	if address == "" {
		return
	}
	// 请求结束后给上游一次取消机会，独立超时保证浏览器连接能及时释放。
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, address, nil)
	if err != nil {
		return
	}
	adobeHeaders(request, account, "", false)
	response, err := g.do(client, request, log)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
	}
}

func (g *AdobeGenerator) downloadImage(ctx context.Context, client *http.Client, address, contentType string, log *RequestLog) (ImageRecord, error) {
	parsed, err := url.Parse(address)
	if err != nil || !isAdobeImageURL(parsed) {
		return ImageRecord{}, errors.New("Adobe 返回的图片地址无效")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return ImageRecord{}, err
	}
	// 签名图片地址自行认证，此请求不携带账号 Cookie 或访问令牌。
	response, err := g.do(client, request, log)
	if err != nil {
		return ImageRecord{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ImageRecord{}, &UpstreamError{StatusCode: response.StatusCode, Code: "image_download_failed", Message: "生成图片下载失败"}
	}
	if response.ContentLength > maxImageBytes {
		return ImageRecord{}, errors.New("生成图片超过 64 MB")
	}
	if mime := strings.Split(response.Header.Get("Content-Type"), ";")[0]; strings.HasPrefix(mime, "image/") {
		contentType = mime
	}
	if contentType == "" {
		contentType = "image/png"
	}
	return g.store.SaveImage(response.Body, contentType, "generated")
}

func (g *AdobeGenerator) Refresh(ctx context.Context, account Account) (updated Account, err error) {
	// 统一在登录状态就绪后读取余额，不额外启动浏览器或覆盖保存中的其他账号字段。
	defer func() {
		if err != nil {
			return
		}
		updated.Credits = nil
		client, _, clientErr := g.clients()
		if clientErr == nil {
			if balance := g.readCreditBalance(ctx, client, updated, nil); balance != nil {
				updated.Credits = balance.Total.Quota.Available
			}
		}
	}()
	updated, err = g.refreshIMS(ctx, account)
	if err == nil {
		// 仅有 Cookie 的新账号还需要页面生成风险会话，不能把刷新令牌视作生图就绪。
		if updated.ARPSession == "" {
			if g.browserRefresh == nil {
				return updated, errors.New("登录令牌已刷新，但缺少 Adobe 风险会话，需要浏览器同步")
			}
			return g.browserRefresh(ctx, updated)
		}
		return updated, nil
	}
	if ctx.Err() != nil {
		return account, ctx.Err()
	}
	if g.browserRefresh != nil {
		return g.browserRefresh(ctx, account)
	}
	return account, err
}

func (g *AdobeGenerator) refreshIMS(ctx context.Context, account Account) (Account, error) {
	if account.Cookie == "" {
		return account, errors.New("账号缺少 Adobe Cookie，需要重新导入登录状态")
	}
	userID := adobeUserID(account)
	if userID == "" {
		return account, errors.New("账号缺少 Adobe 用户标识，无法刷新登录状态")
	}
	client, _, err := g.clients()
	if err != nil {
		return account, err
	}
	clientID := account.ClientID
	if clientID == "" {
		clientID = "clio-playground-web"
	}
	form := url.Values{"client_id": {clientID}, "scope": {adobeIMSScope}, "user_id": {userID}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, adobeIMSURL, strings.NewReader(form.Encode()))
	if err != nil {
		return account, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Cookie", account.Cookie)
	request.Header.Set("Origin", "https://firefly.adobe.com")
	request.Header.Set("Referer", "https://firefly.adobe.com/")
	response, err := g.do(client, request, nil)
	if err != nil {
		return account, err
	}
	defer response.Body.Close()
	var payload struct {
		AccessToken   string          `json:"access_token"`
		Token         string          `json:"token"`
		RefreshToken  string          `json:"refresh_token"`
		ExpiresIn     json.RawMessage `json:"expires_in"`
		UserID        string          `json:"user_id"`
		ProfileUserID string          `json:"userId"`
		ErrorCode     string          `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload) != nil {
		return account, errors.New("Adobe 登录刷新响应无效")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return account, &UpstreamError{StatusCode: response.StatusCode, Code: payload.ErrorCode, Message: "Adobe 登录状态无法刷新，需要重新登录"}
	}
	token := payload.AccessToken
	if token == "" {
		token = payload.Token
	}
	if token == "" {
		return account, errors.New("Adobe 登录刷新未返回访问令牌")
	}
	account.Cookie = mergeAdobeCookies(account.Cookie, response.Cookies())
	account.AccessToken, account.UserID = token, userID
	if payload.UserID != "" {
		account.UserID = payload.UserID
	}
	if payload.ProfileUserID != "" {
		account.UserID = payload.ProfileUserID
	}
	if payload.RefreshToken != "" {
		account.RefreshToken = payload.RefreshToken
	}
	account.ClientID = clientID
	account.LastRefresh = time.Now().UTC()
	account.LastError, account.Status = "", "ready"
	// IMS 的 expires_in 使用毫秒；未知有效期不猜测为固定一小时。
	if milliseconds, err := strconv.ParseFloat(strings.Trim(string(payload.ExpiresIn), "\""), 64); err == nil && milliseconds > 0 && milliseconds <= float64(7*24*time.Hour/time.Millisecond) {
		account.ExpiresAt = account.LastRefresh.Add(time.Duration(milliseconds) * time.Millisecond)
	} else {
		account.ExpiresAt = time.Time{}
	}
	return account, nil
}

func mergeAdobeCookies(raw string, incoming []*http.Cookie) string {
	request := &http.Request{Header: http.Header{"Cookie": []string{raw}}}
	values := make(map[string]string)
	order := []string{}
	for _, cookie := range request.Cookies() {
		if _, exists := values[cookie.Name]; !exists {
			order = append(order, cookie.Name)
		}
		values[cookie.Name] = cookie.Value
	}
	for _, cookie := range incoming {
		if cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && cookie.Expires.Before(time.Now())) {
			delete(values, cookie.Name)
			continue
		}
		if _, exists := values[cookie.Name]; !exists {
			order = append(order, cookie.Name)
		}
		values[cookie.Name] = cookie.Value
	}
	pairs := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, name := range order {
		value, exists := values[name]
		if exists && !seen[name] {
			pairs = append(pairs, name+"="+value)
			seen[name] = true
		}
	}
	return strings.Join(pairs, "; ")
}
