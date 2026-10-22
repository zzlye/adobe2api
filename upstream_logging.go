package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const upstreamLogBodyLimit = 32 << 10
const upstreamLogDetailBudget = 2 << 20
const upstreamLogMaxRecordDetails = 4 * upstreamLogBodyLimit
const upstreamLogHidden = "[已隐藏]"
const upstreamLogOversized = `{"note":"内容过长已省略"}`
const upstreamLogInvalid = `{"note":"无法记录完整 JSON 内容"}`

var upstreamLogURLPattern = regexp.MustCompile(`(?i)(?:https?://|//)[^\s"'<>]+`)

// upstreamLogHeaderBytes 按 JSON 编码后的大小计数，不额外编码或复制已有头部内容。
func upstreamLogHeaderBytes(headers map[string]string) int {
	if len(headers) == 0 {
		return 0
	}
	bytes := 2
	for key, value := range headers {
		bytes += upstreamLogJSONStringBytes(key) + upstreamLogJSONStringBytes(value) + 2
	}
	return bytes
}

func upstreamLogJSONStringBytes(value string) int {
	bytes := 2
	for _, r := range value {
		switch {
		case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t' || r == '\b' || r == '\f':
			bytes += 2
		case r < 0x20 || r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029':
			bytes += 6
		default:
			bytes += utf8.RuneLen(r)
		}
	}
	return bytes
}

func upstreamLogDetailBytes(record UpstreamRecord) int {
	return len(record.RequestBody) + len(record.ResponseBody) + upstreamLogHeaderBytes(record.RequestHeaders) + upstreamLogHeaderBytes(record.ResponseHeaders)
}

// initializeUpstreamLogBudget 兼容已导入的记录，只初始化一次；已有详情超限则停止新增详情。
func initializeUpstreamLogBudget(log *RequestLog) {
	if log.upstreamDetailInitialized {
		return
	}
	log.upstreamDetailInitialized = true
	for _, record := range log.Upstream {
		size := upstreamLogDetailBytes(record)
		if size >= upstreamLogDetailBudget-log.upstreamDetailBytes {
			log.upstreamDetailBytes = upstreamLogDetailBudget
			return
		}
		log.upstreamDetailBytes += size
	}
}

func upstreamLogCanRecordDetails(log *RequestLog) bool {
	initializeUpstreamLogBudget(log)
	// 为整条调用预留最大容量；临界调用也不能先读取再突破总预算。
	return upstreamLogDetailBudget-log.upstreamDetailBytes >= upstreamLogMaxRecordDetails
}

func appendUpstreamLogRecord(log *RequestLog, record UpstreamRecord) {
	initializeUpstreamLogBudget(log)
	size := upstreamLogDetailBytes(record)
	if size > upstreamLogDetailBudget-log.upstreamDetailBytes {
		record.RequestHeaders, record.ResponseHeaders = nil, nil
		record.RequestBody, record.ResponseBody = "", ""
	} else {
		log.upstreamDetailBytes += size
	}
	log.Upstream = append(log.Upstream, record)
}

// normalizeUpstreamLogKey 统一不同协议的字段写法，避免大小写或分隔符绕过脱敏。
func normalizeUpstreamLogKey(key string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			return -1
		}
		return r
	}, key))
}

func sensitiveUpstreamLogKey(key string) bool {
	key = normalizeUpstreamLogKey(key)
	for _, part := range []string{"token", "cookie", "password", "secret", "auth", "credential", "signature", "nonce", "arpsession", "apikey", "accesskey"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	switch key {
	case "key", "jwt", "bearer", "session", "sessionid", "sid", "ftr", "ark", "bfp", "arp", "b64json", "base64", "data", "bytes", "inlinedata":
		return true
	}
	return false
}

func upstreamLogJSONType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json")
}

// upstreamLogURL 复用已有 URL 脱敏，再去除详情中的查询参数，避免签名形式变化泄露凭据。
func upstreamLogURL(raw string) string {
	safe := sanitizeLoggedURL(raw)
	parsed, err := url.Parse(safe)
	if err != nil || parsed.Scheme != "" && !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return ""
	}
	parsed.RawQuery, parsed.ForceQuery = "", false
	return parsed.String()
}

func appendUpstreamLogSecret(secrets []string, value string) []string {
	if value != "" && value != upstreamLogHidden && value != "[已省略]" {
		secrets = append(secrets, value)
	}
	return secrets
}

// upstreamHeaderSecrets 同时收集完整凭据与 Cookie 值，拦截上游在错误文本中回显凭据。
func upstreamHeaderSecrets(headers http.Header, secrets []string) []string {
	for name, values := range headers {
		if !sensitiveUpstreamLogKey(name) {
			continue
		}
		key := normalizeUpstreamLogKey(name)
		for _, value := range values {
			secrets = appendUpstreamLogSecret(secrets, value)
			if key == "authorization" || key == "proxyauthorization" {
				if _, credential, found := strings.Cut(value, " "); found {
					secrets = appendUpstreamLogSecret(secrets, strings.TrimSpace(credential))
				}
			}
			if strings.Contains(key, "cookie") {
				parts := strings.Split(value, ";")
				if key == "setcookie" && len(parts) > 1 {
					parts = parts[:1]
				}
				for _, part := range parts {
					if _, credential, found := strings.Cut(part, "="); found {
						secrets = appendUpstreamLogSecret(secrets, strings.TrimSpace(credential))
						// 合法 Cookie 可使用引号，错误响应回显的值通常不带引号。
						secrets = appendUpstreamLogSecret(secrets, strings.Trim(strings.TrimSpace(credential), `"`))
					}
				}
			}
		}
	}
	return secrets
}

func upstreamLogText(value string, secrets []string) string {
	return newUpstreamLogRedactor(secrets)(value)
}

// newUpstreamLogRedactor 为一次 JSON 或头部记录编译替换器，避免逐字段重复排序与分配。
func newUpstreamLogRedactor(secrets []string) func(string) string {
	unique := make(map[string]bool, len(secrets))
	ordered := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret != "" && secret != upstreamLogHidden && !unique[secret] {
			unique[secret] = true
			ordered = append(ordered, secret)
		}
	}
	// 长凭据先匹配，且不对替换后的占位文字重复替换。
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	pairs := make([]string, 0, len(ordered)*2)
	for _, secret := range ordered {
		pairs = append(pairs, secret, upstreamLogHidden)
	}
	replacer := strings.NewReplacer(pairs...)
	return func(value string) string {
		return upstreamLogURLPattern.ReplaceAllStringFunc(replacer.Replace(value), upstreamLogURL)
	}
}

func upstreamLogURLKey(key string) bool {
	key = normalizeUpstreamLogKey(key)
	return strings.HasSuffix(key, "url") || strings.HasSuffix(key, "uri") || key == "href" || key == "location" || key == "origin" || key == "referer" || key == "xoverridestatuslink"
}

func safeUpstreamLogHeader(name string) bool {
	switch normalizeUpstreamLogKey(name) {
	case "accept", "acceptencoding", "acceptlanguage", "contenttype", "contentlength", "contentencoding", "origin", "referer", "useragent", "date", "retryafter", "xtaskstatus", "xrequestid", "xoverridestatuslink", "xaccesserror", "location", "cachecontrol":
		return true
	}
	return false
}

// sanitizeUpstreamHeaders 只记录诊断需要的安全头；认证头只保留存在指示。
func sanitizeUpstreamHeaders(headers http.Header, secrets []string) map[string]string {
	redact := newUpstreamLogRedactor(secrets)
	result := make(map[string]string)
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	used := 2
	for _, name := range names {
		value := ""
		if sensitiveUpstreamLogKey(name) {
			value = upstreamLogHidden
		} else if safeUpstreamLogHeader(name) {
			length := 0
			for _, item := range headers[name] {
				length += len(item)
			}
			if length > 2048 {
				// 完整省略超长头部，不能截取凭据的一部分后失去脱敏匹配。
				value = "[内容过长已省略]"
			} else if upstreamLogURLKey(name) {
				values := make([]string, 0, len(headers[name]))
				for _, item := range headers[name] {
					values = append(values, upstreamLogURL(redact(item)))
				}
				value = strings.Join(values, ", ")
			} else {
				value = redact(strings.Join(headers[name], ", "))
			}
		} else {
			continue
		}
		if len(value) > 2048 {
			value = trimUpstreamLogText(value, 2048)
		}
		safeName := http.CanonicalHeaderKey(redact(name))
		encodedName, _ := json.Marshal(safeName)
		encodedValue, _ := json.Marshal(value)
		entrySize := len(encodedName) + len(encodedValue) + 2
		if used+entrySize > upstreamLogBodyLimit {
			break
		}
		result[safeName] = value
		used += entrySize
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func trimUpstreamLogText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func collectUpstreamJSONSecrets(value any, secrets []string) []string {
	// 使用显式栈遍历有限 JSON；敏感对象和数组内的字符串也要拦截回显。
	type pendingValue struct {
		value     any
		sensitive bool
	}
	pending := []pendingValue{{value: value}}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch object := current.value.(type) {
		case map[string]any:
			for key, item := range object {
				pending = append(pending, pendingValue{item, current.sensitive || sensitiveUpstreamLogKey(key)})
			}
		case []any:
			for _, item := range object {
				pending = append(pending, pendingValue{item, current.sensitive})
			}
		case string:
			if current.sensitive {
				secrets = appendUpstreamLogSecret(secrets, object)
			}
		}
	}
	return secrets
}

func sanitizeUpstreamJSONValue(value any, redact func(string) string, depth int) any {
	if depth > 16 {
		return "[内容过深已省略]"
	}
	switch object := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(object))
		for key, item := range object {
			safeKey := redact(key)
			if sensitiveUpstreamLogKey(key) {
				result[safeKey] = upstreamLogHidden
			} else if text, ok := item.(string); ok && upstreamLogURLKey(key) {
				result[safeKey] = upstreamLogURL(redact(text))
			} else {
				result[safeKey] = sanitizeUpstreamJSONValue(item, redact, depth+1)
			}
		}
		return result
	case []any:
		result := make([]any, len(object))
		for index, item := range object {
			result[index] = sanitizeUpstreamJSONValue(item, redact, depth+1)
		}
		return result
	case string:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(object)), "data:") {
			return "[图片数据已省略]"
		}
		return redact(object)
	default:
		return value
	}
}

func sanitizeUpstreamJSON(raw []byte, secrets []string) string {
	text, _ := sanitizeUpstreamJSONWithSecrets(raw, secrets)
	return text
}

func sanitizeUpstreamJSONWithSecrets(raw []byte, secrets []string) (string, []string) {
	if len(raw) == 0 {
		return "", secrets
	}
	if len(raw) > upstreamLogBodyLimit {
		return upstreamLogOversized, secrets
	}
	value, valid := decodeUpstreamLogJSON(raw)
	if !valid {
		return upstreamLogInvalid, secrets
	}
	secrets = collectUpstreamJSONSecrets(value, secrets)
	encoded, err := json.Marshal(sanitizeUpstreamJSONValue(value, newUpstreamLogRedactor(secrets), 0))
	if err != nil {
		return upstreamLogInvalid, secrets
	}
	if len(encoded) > upstreamLogBodyLimit {
		return upstreamLogOversized, secrets
	}
	return string(encoded), secrets
}

func decodeUpstreamLogJSON(raw []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, false
	}
	return value, true
}

func collectUpstreamJSONTextSecrets(text string, secrets []string) []string {
	if len(text) == 0 || len(text) > upstreamLogBodyLimit {
		return secrets
	}
	if value, valid := decodeUpstreamLogJSON([]byte(text)); valid {
		return collectUpstreamJSONSecrets(value, secrets)
	}
	return secrets
}

func sanitizeUpstreamJSONText(text string, secrets []string) string {
	if len(text) > upstreamLogBodyLimit {
		return upstreamLogOversized
	}
	return sanitizeUpstreamJSON([]byte(text), secrets)
}

type upstreamLogReplayBody struct {
	io.Reader
	closer  io.Closer
	secrets []string
}

func (body *upstreamLogReplayBody) Close() error { return body.closer.Close() }

func (body *upstreamLogReplayBody) upstreamLogSecrets() []string { return body.secrets }

// attachUpstreamLogSecrets 对非 JSON 流仅附带脱敏依据，不读取或缓冲图片字节。
func attachUpstreamLogSecrets(body io.ReadCloser, secrets []string) io.ReadCloser {
	if body == nil {
		return nil
	}
	if replay, ok := body.(*upstreamLogReplayBody); ok {
		replay.secrets = secrets
		return replay
	}
	return &upstreamLogReplayBody{Reader: body, closer: body, secrets: secrets}
}

type upstreamLogReadError struct{ err error }

func (failure upstreamLogReadError) Read([]byte) (int, error) { return 0, failure.err }

// captureUpstreamJSON 只取有限 JSON 前缀，随后完整回放，调用方仍读取原始响应和原始错误。
func captureUpstreamJSON(body *io.ReadCloser, contentType string, length int64, secrets []string) (string, []string) {
	if body == nil || *body == nil || !upstreamLogJSONType(contentType) {
		return "", secrets
	}
	if length > upstreamLogBodyLimit {
		return upstreamLogOversized, secrets
	}
	original := *body
	prefix, readErr := io.ReadAll(io.LimitReader(original, upstreamLogBodyLimit+1))
	readers := []io.Reader{bytes.NewReader(prefix)}
	if readErr != nil {
		readers = append(readers, upstreamLogReadError{readErr})
	}
	readers = append(readers, original)
	replay := &upstreamLogReplayBody{Reader: io.MultiReader(readers...), closer: original, secrets: secrets}
	*body = replay
	if readErr != nil {
		return upstreamLogInvalid, secrets
	}
	text, secrets := sanitizeUpstreamJSONWithSecrets(prefix, secrets)
	replay.secrets = secrets
	return text, secrets
}

func upstreamLogHeaderMap(headers map[string]string) http.Header {
	result := make(http.Header, len(headers))
	for key, value := range headers {
		result[key] = []string{value}
	}
	return result
}

// sanitizeUpstreamRecord 在持久化前再次收紧记录，覆盖直接写入或历史调用方产生的日志。
func sanitizeUpstreamRecord(record UpstreamRecord) UpstreamRecord {
	return sanitizeUpstreamRecordWithSecrets(record, nil)
}

func sanitizeUpstreamRecordWithSecrets(record UpstreamRecord, secrets []string) UpstreamRecord {
	requestHeaders := upstreamLogHeaderMap(record.RequestHeaders)
	responseHeaders := upstreamLogHeaderMap(record.ResponseHeaders)
	secrets = upstreamHeaderSecrets(responseHeaders, upstreamHeaderSecrets(requestHeaders, secrets))
	// 两侧一起收集凭据，避免响应在普通错误字段中回显请求 JSON 内的令牌。
	secrets = collectUpstreamJSONTextSecrets(record.RequestBody, secrets)
	secrets = collectUpstreamJSONTextSecrets(record.ResponseBody, secrets)
	record.Method = trimUpstreamLogText(upstreamLogText(record.Method, secrets), 32)
	record.URL = trimUpstreamLogText(upstreamLogURL(upstreamLogText(record.URL, secrets)), 4096)
	record.RequestID = trimUpstreamLogText(upstreamLogText(record.RequestID, secrets), 1024)
	record.RequestHeaders = sanitizeUpstreamHeaders(requestHeaders, secrets)
	record.ResponseHeaders = sanitizeUpstreamHeaders(responseHeaders, secrets)
	record.RequestBody = sanitizeUpstreamJSONText(record.RequestBody, secrets)
	record.ResponseBody = sanitizeUpstreamJSONText(record.ResponseBody, secrets)
	return record
}

// adobeUpstreamError 让日志主错误和账号错误也使用脱敏文本，不能绕过响应详情的保护。
func adobeUpstreamError(response *http.Response, status int, code, message string) *UpstreamError {
	var secrets []string
	var accessError string
	if response != nil {
		accessError = strings.TrimSpace(response.Header.Get("X-Access-Error"))
		secrets = upstreamHeaderSecrets(response.Header, secrets)
		if response.Request != nil {
			secrets = upstreamHeaderSecrets(response.Request.Header, secrets)
		}
		if replay, ok := response.Body.(interface{ upstreamLogSecrets() []string }); ok {
			secrets = append(secrets, replay.upstreamLogSecrets()...)
		}
	}
	redact := newUpstreamLogRedactor(secrets)
	accessError = trimUpstreamLogText(redact(accessError), 256)
	if status == http.StatusForbidden && accessError == "user_not_entitled" {
		// 只替换对外错误，原始响应正文仍保留在脱敏后的接口日志中。
		message = "当前账号没有所选模型的使用权限"
	}
	return &UpstreamError{StatusCode: status, Code: trimUpstreamLogText(redact(code), 256), Message: trimUpstreamLogText(redact(message), 1024), AccessError: accessError}
}
