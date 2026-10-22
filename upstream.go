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

var errAdobeAccountMismatch = errors.New("Adobe 返回的登录账号与账号池记录不一致，请检查 Cookie")

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
	// AccessError 保留脱敏后的权限原因，区分模型无权限与登录失效。
	AccessError string
}

func (e *UpstreamError) Error() string {
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Code == "" {
		return fmt.Sprintf("请求失败（%d）", e.StatusCode)
	}
	return fmt.Sprintf("请求失败（%d/%s）", e.StatusCode, e.Code)
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
			// 浏览器签发的会话作为不透明值传递，不删改其中的供应商数据。
			request.Header.Set("X-Arp-Session-Id", account.ARPSession)
		}
	}
}

func (g *AdobeGenerator) do(client *http.Client, request *http.Request, log *RequestLog) (*http.Response, error) {
	if log == nil {
		// Refresh 保持原接口，通过上下文接入触发它的生成日志。
		log, _ = request.Context().Value(requestLogContextKey{}).(*RequestLog)
	}
	var record UpstreamRecord
	var secrets []string
	recording := log != nil && len(log.Upstream) < 4096
	details := recording && upstreamLogCanRecordDetails(log)
	if recording {
		secrets = upstreamHeaderSecrets(request.Header, nil)
		record = UpstreamRecord{Method: request.Method, URL: upstreamLogURL(upstreamLogText(request.URL.String(), secrets))}
		if details {
			record.RequestHeaders = sanitizeUpstreamHeaders(request.Header, secrets)
			record.RequestBody, secrets = captureUpstreamJSON(&request.Body, request.Header.Get("Content-Type"), request.ContentLength, secrets)
		}
	}
	started := time.Now()
	response, err := client.Do(request)
	if response != nil && response.Request == nil {
		response.Request = request
	}
	if recording {
		record.DurationMS = time.Since(started).Milliseconds()
		if response != nil {
			secrets = upstreamHeaderSecrets(response.Header, secrets)
			record.Status = response.StatusCode
			record.RequestID = trimUpstreamLogText(upstreamLogText(response.Header.Get("X-Request-Id"), secrets), 1024)
			if details {
				record.ResponseHeaders = sanitizeUpstreamHeaders(response.Header, secrets)
			}
			if err == nil {
				if details {
					record.ResponseBody, secrets = captureUpstreamJSON(&response.Body, response.Header.Get("Content-Type"), response.ContentLength, secrets)
				}
				response.Body = attachUpstreamLogSecrets(response.Body, secrets)
			}
		}
		appendUpstreamLogRecord(log, sanitizeUpstreamRecordWithSecrets(record, secrets))
	}
	if err != nil {
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		// 网络错误不返回原始 URL，避免签名参数出现在错误日志中。
		return nil, errors.New("连接上游失败，请检查网络与代理设置")
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
		// 只传递标准上下文错误，既保留超时与取消分类，也避免原始读取错误泄露签名地址。
		if errors.Is(err, context.DeadlineExceeded) {
			return adobeGenerationResponse{}, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return adobeGenerationResponse{}, context.Canceled
		}
		return adobeGenerationResponse{}, errors.New("读取上游响应失败")
	}
	if len(body) > 2<<20 {
		return adobeGenerationResponse{}, errors.New("上游响应超过允许大小")
	}
	var payload adobeGenerationResponse
	decodeErr := json.Unmarshal(body, &payload)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := payload.Message
		if message == "" || len(message) > 1024 {
			message = http.StatusText(response.StatusCode)
		}
		return payload, adobeUpstreamError(response, response.StatusCode, payload.ErrorCode, message)
	}
	if decodeErr != nil {
		return payload, errors.New("上游返回了无效响应")
	}
	if payload.ErrorCode != "" {
		return payload, adobeUpstreamError(response, http.StatusBadGateway, payload.ErrorCode, payload.Message)
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
	if err != nil || stat.Size() == 0 || stat.Size() > maxImageBytes {
		return "", errors.New("参考图为空或超过 64 MB 上传限制")
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
		return "", errors.New("参考图上传响应无效")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", adobeUpstreamError(response, response.StatusCode, payload.ErrorCode, payload.Message)
	}
	if len(payload.Images) == 0 || payload.Images[0].ID == "" {
		return "", errors.New("上游未返回参考图标识")
	}
	return payload.Images[0].ID, nil
}

var adobeGPTSizes = map[string][2]int{
	"21:9": {1584, 672}, "16:9": {1376, 768}, "5:4": {1152, 928}, "4:3": {1200, 896}, "3:2": {1264, 848},
	"1:1": {1024, 1024}, "4:5": {928, 1152}, "3:4": {896, 1200}, "2:3": {848, 1264}, "9:16": {768, 1376},
}

// GPT 的档位需按比例映射为输出宽高，不能采用香蕉的方形分辨率占位。
var adobeGPTSizes2K = map[string][2]int{
	"1:1": {2048, 2048}, "5:4": {2240, 1792}, "9:16": {1440, 2560}, "21:9": {3024, 1296}, "16:9": {2560, 1440},
	"4:3": {2304, 1728}, "3:2": {2496, 1664}, "4:5": {1792, 2240}, "3:4": {1728, 2304}, "2:3": {1664, 2496},
}

var adobeGPTSizes4K = map[string][2]int{
	"1:1": {2880, 2880}, "5:4": {3200, 2560}, "9:16": {2160, 3840}, "21:9": {3696, 1584}, "16:9": {3840, 2160},
	"4:3": {3264, 2448}, "3:2": {3504, 2336}, "4:5": {2560, 3200}, "3:4": {2448, 3264}, "2:3": {2336, 3504},
}

const adobeGPTMaxEdge = 3840
const adobeGPTMaxPixels = 8294400

func parseAdobeSize(raw string) (int, int, error) {
	parts := strings.Split(strings.ToLower(raw), "x")
	if len(parts) != 2 {
		return 0, 0, errors.New("图片尺寸应使用宽x高格式")
	}
	width, err1 := strconv.Atoi(parts[0])
	height, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || width < 128 || height < 128 || width > 16384 || height > 16384 {
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

// adobeSizeResolution 统一接口和上游的尺寸档位推断，避免 2880 方形被误判成 2K。
func adobeSizeResolution(model Model, width, height int) string {
	pixels := int64(width) * int64(height)
	if model.AdobeID == "gpt-image" && model.Version != "1.5" {
		if pixels > 2048*2048 {
			return "4K"
		}
	} else if pixels >= 8<<20 {
		return "4K"
	}
	if pixels >= 2<<20 {
		return "2K"
	}
	if pixels < 512*768 {
		return "512"
	}
	return "1K"
}

// adobeParameters 按模型目录校验尺寸，自定义比例计算结果仍遵守上游像素约束。
func adobeParameters(model Model, input GenerateInput) (string, string, int, int, error) {
	ratio, resolution := strings.TrimSpace(input.AspectRatio), strings.ToUpper(strings.TrimSpace(input.Resolution))
	width, height := 0, 0
	if input.Size != "" && input.Size != "auto" {
		var err error
		width, height, err = parseAdobeSize(input.Size)
		if err != nil {
			return "", "", 0, 0, err
		}
		best := closestAdobeRatio(width, height, model.Ratios)
		if model.SupportsCustomSize {
			if err := validGPTSize(width, height); err != nil {
				return "", "", 0, 0, err
			}
			if ratio != "" && ratio != "auto" {
				r, err := ratioNumber(ratio)
				if err != nil || math.Abs(math.Log(float64(width)/float64(height)/r)) > 0.035 {
					return "", "", 0, 0, errors.New("尺寸与画面比例不一致")
				}
				best = ratio
			} else if best == "" {
				best = fmt.Sprintf("%d:%d", width, height)
			}
		}
		if model.Version == "1.5" && !((width == 1024 && height == 1024) || (width == 1536 && height == 1024) || (width == 1024 && height == 1536)) {
			return "", "", 0, 0, errors.New("GPT Image 1.5 仅支持 1024x1024、1536x1024、1024x1536")
		}
		if best == "" {
			return "", "", 0, 0, errors.New("该模型不支持请求的图片比例")
		}
		if ratio != "" && ratio != "auto" && ratio != best {
			return "", "", 0, 0, errors.New("尺寸与画面比例不一致")
		}
		inferred := adobeSizeResolution(model, width, height)
		// 香蕉的显式分辨率直接决定输出档位，size 只用于推断比例。
		if resolution != "" && resolution != inferred && model.AdobeID != "gemini-flash" {
			return "", "", 0, 0, errors.New("尺寸与分辨率档位不一致")
		}
		ratio = best
		if resolution == "" {
			resolution = inferred
		}
	}
	if ratio == "" {
		if hasString(model.Ratios, "auto") {
			ratio = "auto"
		} else {
			ratio = "1:1"
		}
	}
	if resolution == "" {
		resolution = "1K"
	}
	if !hasString(model.Resolutions, resolution) {
		return "", "", 0, 0, errors.New("该模型不支持指定的分辨率")
	}
	if !hasString(model.Ratios, ratio) {
		if !model.SupportsCustomSize {
			return "", "", 0, 0, errors.New("该模型不支持指定的图片比例")
		}
		if _, err := ratioNumber(ratio); err != nil {
			return "", "", 0, 0, err
		}
		if width == 0 {
			var err error
			width, height, err = customGPTSize(ratio, resolution)
			if err != nil {
				return "", "", 0, 0, err
			}
		}
	}
	if model.HighResolutionRequiresRatio && resolution != "1K" && ratio == "auto" {
		return "", "", 0, 0, errors.New("该模型在 2K、4K 分辨率下需要指定画面比例")
	}
	return ratio, resolution, width, height, nil
}

func adobeGenerationBody(model Model, input GenerateInput, references []map[string]string, seed uint32) (map[string]any, error) {
	if err := validateModelOptions(model, input); err != nil {
		return nil, err
	}
	ratio, resolution, width, height, err := adobeParameters(model, input)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"n": 1, "prompt": input.Prompt,
		"output": map[string]any{"storeInputs": false}, "referenceBlobs": references,
		"modelId": model.AdobeID, "modelVersion": model.Version,
		"generationMetadata": map[string]string{"module": "text2image", "submodule": "ff-image-generate"},
	}
	if input.Output != nil {
		body["output"] = input.Output
	}
	payload := make(map[string]any, len(input.ModelSpecificPayload)+2)
	for key, value := range input.ModelSpecificPayload {
		payload[key] = value
	}
	body["modelSpecificPayload"] = payload
	if model.AdobeID == "gemini-flash" {
		size := map[string]int{"512": 512, "1K": 1024, "2K": 2048, "4K": 4096}[resolution]
		if _, exists := payload["parameters"]; !exists {
			payload["parameters"] = map[string]bool{"addWatermark": false}
		}
		if ratio != "auto" {
			payload["aspectRatio"] = ratio
			body["generationSettings"] = map[string]any{"aspectRatio": ratio}
		}
		body["size"] = map[string]int{"width": size, "height": size}
		body["resolution"] = resolution
		search := false
		if input.GroundSearch != nil {
			search = *input.GroundSearch
		}
		body["groundSearch"] = search
		value := int64(seed)
		if input.Seed != nil {
			value = *input.Seed
		}
		if len(input.Seeds) > 0 {
			value = input.Seeds[0]
		}
		body["seeds"] = []int64{value}
		return body, nil
	}
	if model.AdobeID != "gpt-image" {
		return nil, errors.New("暂不支持该上游模型")
	}
	if ratio == "auto" {
		payload["size"] = "auto"
	} else {
		actual := adobeGPTSizes[ratio]
		if model.Version == "1.5" {
			actual = map[string][2]int{"1:1": {1024, 1024}, "3:2": {1536, 1024}, "2:3": {1024, 1536}}[ratio]
		}
		if resolution == "2K" {
			actual = adobeGPTSizes2K[ratio]
		} else if resolution == "4K" {
			actual = adobeGPTSizes4K[ratio]
		}
		if width > 0 && height > 0 {
			actual = [2]int{width, height}
		}
		if model.SupportsCustomSize {
			if err := validGPTSize(actual[0], actual[1]); err != nil {
				return nil, err
			}
		}
		body["size"] = map[string]int{"width": actual[0], "height": actual[1]}
	}
	detail := qualityDetail(model, input.Quality)
	if input.DetailLevel != nil {
		detail = *input.DetailLevel
	}
	settings := map[string]any{"detailLevel": detail}
	if input.TransparentBackground != nil {
		settings["transparentBackground"] = *input.TransparentBackground
	} else if input.Background != "" && input.Background != "auto" {
		settings["transparentBackground"] = input.Background == "transparent"
	}
	body["generationSettings"] = settings
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
	if count < 1 || count > 4 {
		return result, errors.New("生成数量应在 1 到 4 之间")
	}
	if _, err := adobeGenerationBody(model, input, nil, 0); err != nil {
		return result, err
	}
	client, download, err := g.clients()
	if err != nil {
		return result, err
	}
	references := make([]map[string]string, 0, len(input.References))
	for index, reference := range input.References {
		id, err := g.uploadReference(ctx, client, account, reference, log)
		if err != nil {
			return result, err
		}
		references = append(references, map[string]string{"id": id, "usage": referenceUsage(model, input, index)})
	}
	if input.MaskReference != nil {
		id, err := g.uploadReference(ctx, client, account, *input.MaskReference, log)
		if err != nil {
			return result, err
		}
		references = append(references, map[string]string{"id": id, "usage": "mask"})
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
		single := input
		if len(input.Seeds) > 0 {
			single.Seeds = nil
			single.Seed = &input.Seeds[index]
		}
		body, err := adobeGenerationBody(model, single, references, binary.BigEndian.Uint32(random[:])&0x7fffffff)
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
			return result, errors.New("上游未返回生成图片")
		}
		for _, output := range payload.Outputs {
			if len(output.Error) > 0 && string(output.Error) != "null" {
				return result, errors.New("未能生成该图片")
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
			return payload, errors.New("上游未返回图片生成任务地址")
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
		return ImageRecord{}, errors.New("生成的图片地址无效")
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
	if err == nil && updated.ARPSession != "" {
		return updated, nil
	}
	if ctx.Err() != nil {
		// 取消前已完成的 IMS 轮换仍需交给调用方保存，避免下次再次使用旧 Cookie。
		return updated, ctx.Err()
	}
	if errors.Is(err, errAdobeAccountMismatch) {
		// 身份已明确不一致时禁止使用其他浏览器会话掩盖错误。
		return account, err
	}
	if g.browserRefresh == nil {
		if err == nil {
			return updated, errors.New("登录令牌已刷新，但缺少 Adobe 风险会话，需要浏览器同步")
		}
		return account, err
	}
	browserAccount := account
	if err == nil {
		browserAccount = updated
	}
	browsed, browserErr := g.browserRefresh(ctx, browserAccount)
	if browserErr != nil {
		return browserAccount, browserErr
	}
	expectedUserID := adobeUserID(browserAccount)
	if expectedUserID != "" && browsed.UserID != "" && expectedUserID != browsed.UserID {
		return browserAccount, errAdobeAccountMismatch
	}
	if browsed.UserID == "" {
		browsed.UserID = expectedUserID
	}
	if browsed.ARPSession == "" {
		return browserAccount, errors.New("浏览器没有返回 Adobe 风险会话，请重新同步登录状态")
	}
	// 浏览器缓存的令牌可能属于其他权限范围，完成 Cookie 和风险会话后再次由 IMS 规范化。
	refreshed, refreshErr := g.refreshIMS(ctx, browsed)
	if refreshErr != nil {
		// 最后一次 IMS 未确认页面凭据时，只返回前一次已确认的会话供失败分支保存。
		return browserAccount, refreshErr
	}
	return refreshed, nil
}

func (g *AdobeGenerator) refreshIMS(ctx context.Context, account Account) (Account, error) {
	if account.Cookie == "" {
		return account, errors.New("账号缺少 Adobe Cookie，需要重新导入登录状态")
	}
	userID := adobeUserID(account)
	client, _, err := g.clients()
	if err != nil {
		return account, err
	}
	clientID := account.ClientID
	if clientID == "" {
		clientID = "clio-playground-web"
	}
	// 登录 Cookie 本身即可恢复真实用户，不传猜测的用户标识或访客参数。
	form := url.Values{"client_id": {clientID}, "scope": {adobeIMSScope}}
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
	if payload.ErrorCode != "" {
		// IMS 会在 HTTP 200 中返回认证失败，不能把业务错误误报为缺少令牌。
		return account, &UpstreamError{StatusCode: http.StatusUnauthorized, Code: payload.ErrorCode, Message: "Adobe 登录状态已失效，请重新登录后同步 Cookie"}
	}
	token := payload.AccessToken
	if token == "" {
		token = payload.Token
	}
	if token == "" {
		return account, errors.New("Adobe 登录刷新未返回访问令牌")
	}
	refreshedUserID := payload.ProfileUserID
	if refreshedUserID == "" {
		refreshedUserID = payload.UserID
	} else if payload.UserID != "" && payload.UserID != refreshedUserID {
		return account, errAdobeAccountMismatch
	}
	if refreshedUserID == "" {
		refreshedUserID = adobeUserID(Account{AccessToken: token})
	}
	if refreshedUserID == "" {
		return account, errors.New("Adobe 登录刷新未返回用户身份，请重新登录或同步浏览器")
	}
	if userID != "" && userID != refreshedUserID {
		return account, errAdobeAccountMismatch
	}
	account.Cookie = mergeAdobeCookies(account.Cookie, response.Cookies())
	if account.ARPSession == "" {
		account.ARPSession = cookieARPSession(account.Cookie)
	}
	account.AccessToken, account.UserID = token, refreshedUserID
	if payload.RefreshToken != "" {
		account.RefreshToken = payload.RefreshToken
	}
	account.ClientID = clientID
	account.LastRefresh = time.Now().UTC()
	account.LastError, account.Status = "", "unverified"
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

// cookieARPSession 只校验会话基本结构，返回原值，不改写供应商签发的数据。
func cookieARPSession(raw string) string {
	request := &http.Request{Header: http.Header{"Cookie": []string{raw}}}
	cookie, err := request.Cookie("sherlockToken")
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return ""
	}
	value := strings.TrimSpace(cookie.Value)
	if value == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "/").Replace(value))
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "/").Replace(strings.TrimRight(value, "=")))
		if err != nil {
			return ""
		}
	}
	var payload struct {
		SID string `json:"sid"`
		FTR string `json:"ftr"`
		ARK string `json:"ark"`
		BFP string `json:"bfp"`
	}
	if json.Unmarshal(decoded, &payload) != nil {
		return ""
	}
	if strings.TrimSpace(payload.SID) == "" {
		return ""
	}
	if strings.TrimSpace(payload.FTR) == "" && strings.TrimSpace(payload.BFP) == "" && strings.TrimSpace(payload.ARK) == "" {
		return ""
	}
	return value
}
