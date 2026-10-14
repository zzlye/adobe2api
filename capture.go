package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// parseCapture 接受浏览器复制的 cURL、HAR、Cookie 导出或账号对象。
func parseCapture(raw string) (Account, error) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
	if len(raw) > 8<<20 {
		return Account{}, errors.New("导入内容超过 8 MB")
	}
	account := Account{Enabled: true, ClientID: "clio-playground-web", APIKey: "clio-playground-web"}
	var har struct {
		Log struct {
			Entries []struct {
				Request struct {
					URL     string `json:"url"`
					Headers []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"headers"`
					Cookies []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"cookies"`
				} `json:"request"`
			} `json:"entries"`
		} `json:"log"`
	}
	if json.Unmarshal([]byte(raw), &har) == nil && len(har.Log.Entries) > 0 {
		bestScore := -1
		for _, entry := range har.Log.Entries {
			if !trustedCaptureURL(entry.Request.URL) {
				continue
			}
			candidate := Account{Enabled: true, ClientID: "clio-playground-web", APIKey: "clio-playground-web"}
			for _, header := range entry.Request.Headers {
				setCaptureHeader(&candidate, header.Name, header.Value)
			}
			if candidate.Cookie == "" {
				var cookies []string
				for _, cookie := range entry.Request.Cookies {
					if validCaptureCookie(cookie.Name, cookie.Value) {
						cookies = append(cookies, cookie.Name+"="+cookie.Value)
					}
				}
				candidate.Cookie = strings.Join(cookies, "; ")
			}
			// token 与风险会话必须来自同一请求，不能被后续统计或认证请求覆盖。
			score := captureAccountScore(candidate)
			if score >= bestScore {
				account, bestScore = candidate, score
			}
		}
	} else if strings.HasPrefix(raw, "[") {
		var cookies []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
		}
		if json.Unmarshal([]byte(raw), &cookies) != nil {
			return Account{}, errors.New("Cookie 导出格式无效")
		}
		var parts []string
		for _, c := range cookies {
			if trustedCaptureDomain(c.Domain) && validCaptureCookie(c.Name, c.Value) {
				parts = append(parts, c.Name+"="+c.Value)
			}
		}
		account.Cookie = strings.Join(parts, "; ")
	} else if strings.HasPrefix(raw, "{") {
		if err := json.Unmarshal([]byte(raw), &account); err != nil {
			return Account{}, errors.New("账号内容格式无效")
		}
		account.Enabled = true
	} else if captureCurlPrefix.MatchString(raw) {
		var err error
		account, err = parseCaptureCurl(raw)
		if err != nil {
			return Account{}, err
		}
	} else {
		if prefix, cookie, ok := strings.Cut(raw, "----"); ok {
			account.Email = prefix
			raw = cookie
		}
		if strings.Contains(raw, "ims_sid=") || strings.Contains(raw, "sso_sid=") {
			account.Cookie = normalizedCaptureCookie(raw)
		} else {
			return Account{}, errors.New("请导入 cURL、HAR、Cookie 或账号 JSON")
		}
	}
	if account.Cookie == "" && account.AccessToken == "" && account.RefreshToken == "" {
		return Account{}, errors.New("未找到账号登录凭据")
	}
	account.AccessToken = captureBearer(account.AccessToken)
	if account.ClientID == "" {
		account.ClientID = "clio-playground-web"
	}
	if account.APIKey == "" {
		account.APIKey = account.ClientID
	}
	// 导出的 nonce 绑定原提示词，不能用于后续不同的请求。
	account.Nonce = ""
	account.Status = "pending"
	return account, nil
}

var captureCurlPrefix = regexp.MustCompile(`(?i)^curl(?:\.exe)?(?:\s|$)`)

// 域名必须有完整后缀边界，避免把包含 adobe 字样的第三方站点当成登录来源。
func trustedCaptureDomain(domain string) bool {
	host := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	for _, allowed := range []string{"adobe.com", "adobe.io", "adobelogin.com"} {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

func trustedCaptureURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Host != "" && trustedCaptureDomain(parsed.Hostname()) && (parsed.Port() == "" || parsed.Port() == "443")
}

func captureBearer(value string) string {
	value = strings.TrimSpace(value)
	if prefix, token, ok := strings.Cut(value, " "); ok {
		if !strings.EqualFold(prefix, "bearer") {
			return ""
		}
		value = strings.TrimSpace(token)
	}
	if strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return ""
	}
	return value
}

func validCaptureCookie(name, value string) bool {
	if name == "" || strings.ContainsAny(name, "=;\r\n\t ") || strings.ContainsAny(value, ";\r\n") {
		return false
	}
	return true
}

func normalizedCaptureCookie(value string) string {
	request := &http.Request{Header: http.Header{"Cookie": {value}}}
	values := make(map[string]string)
	for _, cookie := range request.Cookies() {
		if validCaptureCookie(cookie.Name, cookie.Value) {
			values[cookie.Name] = cookie.Value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+values[name])
	}
	return strings.Join(parts, "; ")
}

func setCaptureHeader(account *Account, name, value string) {
	value = strings.TrimSpace(value)
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization":
		account.AccessToken = captureBearer(value)
	case "cookie":
		account.Cookie = normalizedCaptureCookie(value)
	case "x-api-key":
		account.APIKey = value
	case "x-arp-session-id":
		account.ARPSession = value
	case "x-ims-client-id", "x-adobe-client-id":
		account.ClientID = value
	}
}

func captureAccountScore(account Account) int {
	score := 0
	if account.Cookie != "" {
		score += 20
	}
	if account.AccessToken != "" {
		score += 10
	}
	if account.AccessToken != "" && account.ARPSession != "" {
		score += 100
	}
	return score
}

func parseCaptureCurl(raw string) (Account, error) {
	words, err := captureShellWords(raw)
	if err != nil || len(words) < 2 {
		return Account{}, errors.New("cURL 格式无效")
	}
	account := Account{Enabled: true, ClientID: "clio-playground-web", APIKey: "clio-playground-web"}
	urls := []string{}
	for i := 1; i < len(words); i++ {
		word := words[i]
		var header, cookie string
		switch {
		case word == "-H" || word == "--header" || word == "-b" || word == "--cookie" || word == "--url":
			if i+1 >= len(words) {
				return Account{}, errors.New("cURL 选项缺少内容")
			}
			i++
			if word == "--url" {
				urls = append(urls, words[i])
			} else if word == "-b" || word == "--cookie" {
				cookie = words[i]
			} else {
				header = words[i]
			}
		case strings.HasPrefix(word, "--header="):
			header = strings.TrimPrefix(word, "--header=")
		case strings.HasPrefix(word, "--cookie="):
			cookie = strings.TrimPrefix(word, "--cookie=")
		case strings.HasPrefix(word, "--url="):
			urls = append(urls, strings.TrimPrefix(word, "--url="))
		case strings.HasPrefix(word, "-H") && len(word) > 2:
			header = word[2:]
		case strings.HasPrefix(word, "-b") && len(word) > 2:
			cookie = word[2:]
		case word == "--data" || word == "--data-raw" || word == "--data-binary" || word == "--data-urlencode" || word == "-d" || word == "--request" || word == "-X" || word == "--user-agent" || word == "-A" || word == "--referer" || word == "-e" || word == "--proxy" || word == "-x":
			if i+1 >= len(words) {
				return Account{}, errors.New("cURL 选项缺少内容")
			}
			i++
		case strings.HasPrefix(word, "https://") || strings.HasPrefix(word, "http://"):
			urls = append(urls, word)
		case captureCurlPrefix.MatchString(word):
			return Account{}, errors.New("只能导入一个 cURL 请求")
		}
		if header != "" {
			if name, value, ok := strings.Cut(header, ":"); ok {
				setCaptureHeader(&account, name, value)
			}
		}
		if cookie != "" {
			setCaptureHeader(&account, "cookie", cookie)
		}
	}
	if len(urls) == 0 {
		return Account{}, errors.New("cURL 缺少 Adobe 请求地址")
	}
	for _, rawURL := range urls {
		if !trustedCaptureURL(rawURL) {
			return Account{}, errors.New("只能导入 Adobe 认证或生图请求")
		}
	}
	return account, nil
}

// 仅拆分复制出的参数，保留引号中的凭据；不会执行命令、变量或文件引用。
func captureShellWords(raw string) ([]string, error) {
	runes := []rune(raw)
	var words []string
	var current strings.Builder
	var quote rune
	started := false
	ansiQuoted := false
	for i := 0; i < len(runes); i++ {
		character := runes[i]
		if quote == '\'' {
			if ansiQuoted && character == '\\' && i+1 < len(runes) {
				i++
				switch runes[i] {
				case 'n':
					current.WriteRune('\n')
				case 'r':
					current.WriteRune('\r')
				case 't':
					current.WriteRune('\t')
				default:
					current.WriteRune(runes[i])
				}
			} else if character == quote {
				quote = 0
				ansiQuoted = false
			} else {
				current.WriteRune(character)
			}
			continue
		}
		if character == '$' && quote == 0 && i+1 < len(runes) && runes[i+1] == '\'' {
			quote, started, ansiQuoted = '\'', true, true
			i++
			continue
		}
		// 部分 Windows 导出使用 ^" 包裹参数，仅在参数边界识别包装引号。
		if character == '^' && i+1 < len(runes) && runes[i+1] == '"' {
			if quote == 0 && current.Len() == 0 {
				quote, started = '"', true
			} else if quote == '"' && (i+2 == len(runes) || unicode.IsSpace(runes[i+2])) {
				quote = 0
			} else {
				current.WriteRune('"')
			}
			i++
			continue
		}
		if character == '\\' || character == '^' || character == '`' {
			if i+1 < len(runes) && (runes[i+1] == '\n' || (runes[i+1] == '\r' && i+2 < len(runes) && runes[i+2] == '\n')) {
				i++
				if runes[i] == '\r' {
					i++
				}
				continue
			}
			if i+1 < len(runes) && (quote == 0 || runes[i+1] == '"' || runes[i+1] == '\\' || runes[i+1] == '^') {
				i++
				current.WriteRune(runes[i])
				started = true
				continue
			}
		}
		if quote == '"' {
			if character == quote {
				quote = 0
			} else {
				current.WriteRune(character)
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote, started = character, true
			continue
		}
		if unicode.IsSpace(character) {
			if started {
				words = append(words, current.String())
				current.Reset()
				started = false
			}
			continue
		}
		if strings.ContainsRune(";|&<>", character) {
			return nil, errors.New("cURL 不能包含其他命令")
		}
		current.WriteRune(character)
		started = true
	}
	if quote != 0 {
		return nil, errors.New("cURL 引号未闭合")
	}
	if started {
		words = append(words, current.String())
	}
	return words, nil
}

var sensitiveValue = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/=-]+`)

func safeError(err error) string {
	if err == nil {
		return ""
	}
	value := sensitiveValue.ReplaceAllString(err.Error(), "${1}[已隐藏]")
	if len(value) > 1200 {
		value = value[:1200]
	}
	return value
}
