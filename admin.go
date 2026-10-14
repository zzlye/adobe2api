package main

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const adminCookieName = "adobe2api_session"

type adminSession struct {
	expires      time.Time
	passwordHash string
}

type loginAttempt struct {
	count   int
	started time.Time
}

// adminAuth 的会话只留在内存中，重启和修改密码后旧会话立即失效。
type adminAuth struct {
	mu       sync.Mutex
	sessions map[string]adminSession
	attempts map[string]loginAttempt
}

func (a *App) registerAdmin(mux *http.ServeMux) {
	a.adminAuth = &adminAuth{sessions: make(map[string]adminSession), attempts: make(map[string]loginAttempt)}
	mux.HandleFunc("GET /api/auth/status", a.authStatus)
	mux.HandleFunc("POST /api/auth/setup", a.authSetup)
	mux.HandleFunc("POST /api/auth/login", a.authLogin)
	mux.HandleFunc("POST /api/auth/logout", a.authLogout)
	mux.HandleFunc("GET /api/admin/stats", a.adminOnly(a.adminStats))
	mux.HandleFunc("GET /api/admin/accounts", a.adminOnly(a.adminAccounts))
	mux.HandleFunc("POST /api/admin/accounts", a.adminOnly(a.adminCreateAccount))
	mux.HandleFunc("POST /api/admin/accounts/import", a.adminOnly(a.adminImportAccount))
	mux.HandleFunc("POST /api/admin/accounts/browser", a.adminOnly(a.adminBrowserAccount))
	mux.HandleFunc("PUT /api/admin/accounts/{id}", a.adminOnly(a.adminUpdateAccount))
	mux.HandleFunc("DELETE /api/admin/accounts/{id}", a.adminOnly(a.adminDeleteAccount))
	mux.HandleFunc("POST /api/admin/accounts/{id}/refresh", a.adminOnly(a.adminRefreshAccount))
	mux.HandleFunc("GET /api/admin/logs", a.adminOnly(a.adminLogs))
	mux.HandleFunc("GET /api/admin/logs/{id}", a.adminOnly(a.adminLog))
	mux.HandleFunc("GET /api/admin/settings", a.adminOnly(a.adminGetSettings))
	mux.HandleFunc("PUT /api/admin/settings", a.adminOnly(a.adminSaveSettings))
	mux.HandleFunc("POST /api/admin/cleanup", a.adminOnly(a.adminCleanup))
}

func (a *App) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !a.adminAuthenticated(r) {
			writeError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !adminSameOrigin(r) {
			writeError(w, http.StatusForbidden, "请求来源不匹配")
			return
		}
		next(w, r)
	}
}

func (a *App) adminAuthenticated(r *http.Request) bool {
	if a.adminAuth == nil {
		return false
	}
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || len(cookie.Value) > 128 {
		return false
	}
	settings := a.store.Settings()
	a.adminAuth.mu.Lock()
	defer a.adminAuth.mu.Unlock()
	session, ok := a.adminAuth.sessions[cookie.Value]
	if !ok || time.Now().After(session.expires) || session.passwordHash != settings.AdminPasswordHash {
		delete(a.adminAuth.sessions, cookie.Value)
		return false
	}
	return true
}

// SameSite Cookie 配合同源检查阻止外部网页提交后台变更。
func adminSameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && strings.EqualFold(u.Host, r.Host)
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, value any, limit int64) error {
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return errors.New("请使用 JSON 请求")
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		return errors.New("请求内容无效或过大")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("请求只允许一个 JSON 对象")
	}
	return nil
}

func (a *App) authStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	settings := a.store.Settings()
	writeJSON(w, http.StatusOK, map[string]any{"initialized": settings.AdminPasswordHash != "", "authenticated": a.adminAuthenticated(r), "username": settings.AdminUsername})
}

func hashAdminPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	derived, err := pbkdf2.Key(sha256.New, password, salt, 210000, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$210000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(derived), nil
}

func verifyAdminPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 100000 || iterations > 1000000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) != 32 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func validAdminCredentials(username, password string) bool {
	return len(strings.TrimSpace(username)) >= 1 && len(username) <= 64 && len(password) >= 10 && len(password) <= 1024
}

func (a *App) authSetup(w http.ResponseWriter, r *http.Request) {
	if !adminSameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源不匹配")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeAdminJSON(w, r, &input, 16384); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if !validAdminCredentials(input.Username, input.Password) {
		writeError(w, http.StatusBadRequest, "账号不能为空，密码至少 10 位")
		return
	}
	// 首次初始化与会话创建共享锁，防止两个并发请求覆盖管理员。
	a.adminAuth.mu.Lock()
	settings := a.store.Settings()
	if settings.AdminPasswordHash != "" {
		a.adminAuth.mu.Unlock()
		writeError(w, http.StatusConflict, "管理员已设置")
		return
	}
	passwordHash, err := hashAdminPassword(input.Password)
	if err != nil {
		a.adminAuth.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "保存管理员失败")
		return
	}
	settings.AdminUsername, settings.AdminPasswordHash = input.Username, passwordHash
	if len(settings.APIKeys) == 0 {
		settings.APIKeys = []string{"sk-" + newID()}
	}
	if err := a.store.SaveSettings(settings); err != nil {
		a.adminAuth.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "保存管理员失败")
		return
	}
	token := a.newAdminSessionLocked(settings.AdminPasswordHash)
	a.adminAuth.mu.Unlock()
	a.setAdminCookie(w, r, token, 86400)
	writeJSON(w, http.StatusOK, map[string]any{"initialized": true, "authenticated": true, "username": settings.AdminUsername, "api_key": settings.APIKeys[0]})
}

func (a *App) allowLoginAttempt(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if ip == "" {
		ip = "unknown"
	}
	now := time.Now()
	a.adminAuth.mu.Lock()
	defer a.adminAuth.mu.Unlock()
	for key, attempt := range a.adminAuth.attempts {
		if now.Sub(attempt.started) > 15*time.Minute {
			delete(a.adminAuth.attempts, key)
		}
	}
	if len(a.adminAuth.attempts) >= 4096 {
		return false
	}
	attempt := a.adminAuth.attempts[ip]
	if attempt.started.IsZero() {
		attempt.started = now
	}
	if attempt.count >= 10 {
		return false
	}
	attempt.count++
	a.adminAuth.attempts[ip] = attempt
	return true
}

func (a *App) authLogin(w http.ResponseWriter, r *http.Request) {
	if !adminSameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源不匹配")
		return
	}
	if !a.allowLoginAttempt(r) {
		w.Header().Set("Retry-After", "900")
		writeError(w, http.StatusTooManyRequests, "登录尝试过多，请稍后重试")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeAdminJSON(w, r, &input, 16384); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	settings := a.store.Settings()
	if settings.AdminPasswordHash == "" {
		writeError(w, http.StatusConflict, "请先设置管理员")
		return
	}
	if len(input.Password) > 1024 || !verifyAdminPassword(input.Password, settings.AdminPasswordHash) || subtle.ConstantTimeCompare([]byte(input.Username), []byte(settings.AdminUsername)) != 1 {
		writeError(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}
	a.adminAuth.mu.Lock()
	token := a.newAdminSessionLocked(settings.AdminPasswordHash)
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	delete(a.adminAuth.attempts, ip)
	a.adminAuth.mu.Unlock()
	a.setAdminCookie(w, r, token, 86400)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": settings.AdminUsername})
}

func (a *App) newAdminSessionLocked(passwordHash string) string {
	now := time.Now()
	for key, session := range a.adminAuth.sessions {
		if now.After(session.expires) {
			delete(a.adminAuth.sessions, key)
		}
	}
	if len(a.adminAuth.sessions) >= 1024 {
		var oldestKey string
		var oldest time.Time
		for key, session := range a.adminAuth.sessions {
			if oldest.IsZero() || session.expires.Before(oldest) {
				oldest, oldestKey = session.expires, key
			}
		}
		delete(a.adminAuth.sessions, oldestKey)
	}
	token := newID()
	a.adminAuth.sessions[token] = adminSession{expires: now.Add(24 * time.Hour), passwordHash: passwordHash}
	return token
}

func (a *App) setAdminCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	// 反向代理终止 HTTPS 时仍设置安全 Cookie，避免登录信息经明文连接发送。
	forwardedProtocol := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	secure := r.TLS != nil || strings.EqualFold(forwardedProtocol, "https") || strings.HasPrefix(a.store.Settings().PublicURL, "https://")
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func (a *App) authLogout(w http.ResponseWriter, r *http.Request) {
	if !adminSameOrigin(r) {
		writeError(w, http.StatusForbidden, "请求来源不匹配")
		return
	}
	if cookie, err := r.Cookie(adminCookieName); err == nil {
		a.adminAuth.mu.Lock()
		delete(a.adminAuth.sessions, cookie.Value)
		a.adminAuth.mu.Unlock()
	}
	a.setAdminCookie(w, r, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

type adminAccount struct {
	Account
	HasCredentials bool `json:"has_credentials"`
}

func publicAdminAccount(account Account) adminAccount {
	hasCredentials := account.Cookie != "" || account.AccessToken != "" || account.RefreshToken != ""
	account.Cookie, account.AccessToken, account.RefreshToken = "", "", ""
	account.ClientID, account.APIKey, account.ARPSession, account.Nonce, account.BrowserProfile = "", "", "", "", ""
	account.UserID = ""
	return adminAccount{Account: account, HasCredentials: hasCredentials}
}

func (a *App) adminStats(w http.ResponseWriter, r *http.Request) {
	stats, err := a.store.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取统计失败")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (a *App) adminAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.store.Accounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账号失败")
		return
	}
	items := make([]adminAccount, 0, len(accounts))
	for _, account := range accounts {
		items = append(items, publicAdminAccount(account))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
	var account Account
	if err := decodeAdminJSON(w, r, &account, 2<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account.ID = newID()
	account.Name = strings.TrimSpace(account.Name)
	if account.Name == "" {
		account.Name = "Adobe 账号"
	}
	if len(account.Name) > 128 || len(account.Email) > 254 {
		writeError(w, http.StatusBadRequest, "账号名称或邮箱过长")
		return
	}
	account.Status = "unverified"
	if account.Cookie == "" && account.AccessToken == "" && account.RefreshToken == "" {
		account.Status = "missing_credentials"
	} else if account.AccessToken == "" || account.ARPSession == "" || account.UserID == "" {
		account.Status = "pending"
	}
	if err := a.store.SaveAccount(account); err != nil {
		writeError(w, http.StatusInternalServerError, "保存账号失败")
		return
	}
	writeJSON(w, http.StatusCreated, publicAdminAccount(account))
}

func (a *App) findAdminAccount(id string) (Account, error) {
	accounts, err := a.store.Accounts()
	if err != nil {
		return Account{}, err
	}
	for _, account := range accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return Account{}, sql.ErrNoRows
}

func (a *App) adminUpdateAccount(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
		Cookie  string  `json:"cookie"`
	}
	if err := decodeAdminJSON(w, r, &patch, 2<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if a.busy[r.PathValue("id")] {
		writeError(w, http.StatusConflict, "账号正在使用，请稍后修改")
		return
	}
	account, err := a.findAdminAccount(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	// 空 Cookie 保留会话；同一登录仅换 Cookie，身份变化时清除旧凭据。
	if patch.Name != nil {
		account.Name = strings.TrimSpace(*patch.Name)
	}
	if patch.Enabled != nil {
		account.Enabled = *patch.Enabled
	}
	if strings.TrimSpace(patch.Cookie) != "" {
		cookie, email, err := parseAdminCookie(patch.Cookie)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if cookie != account.Cookie {
			if sameAdobeCookieSession(account.Cookie, cookie) {
				account.Cookie, account.Status, account.LastError = cookie, "pending", ""
			} else {
				account.Cookie, account.Email, account.UserID = cookie, email, ""
				account.AccessToken, account.RefreshToken, account.ARPSession, account.Nonce, account.BrowserProfile = "", "", "", "", ""
				account.ExpiresAt, account.LastRefresh = time.Time{}, time.Time{}
				account.Credits, account.LastError, account.Status = nil, "", "pending"
			}
		}
	}
	account.Name = strings.TrimSpace(account.Name)
	if account.Name == "" || len(account.Name) > 128 || len(account.Email) > 254 {
		writeError(w, http.StatusBadRequest, "请填写有效的账号名称和邮箱")
		return
	}
	if err := a.store.SaveAccount(account); err != nil {
		writeError(w, http.StatusInternalServerError, "保存账号失败")
		return
	}
	writeJSON(w, http.StatusOK, publicAdminAccount(account))
}

func (a *App) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if a.busy[r.PathValue("id")] {
		writeError(w, http.StatusConflict, "账号正在使用，请稍后删除")
		return
	}
	if err := a.store.DeleteAccount(r.PathValue("id")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "账号不存在")
		} else {
			writeError(w, http.StatusInternalServerError, "删除账号失败")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (a *App) adminImportAccount(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name    string `json:"name"`
		Cookie  string `json:"cookie"`
		Capture string `json:"capture"`
	}
	if err := decodeAdminJSON(w, r, &input, 8<<20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var account Account
	var err error
	if strings.TrimSpace(input.Cookie) != "" {
		account.Cookie, account.Email, err = parseAdminCookie(input.Cookie)
		account.ClientID, account.APIKey = "clio-playground-web", "clio-playground-web"
	} else {
		// 保留早期抓包导入兼容，新的页面只需要提供 Cookie。
		account, err = parseCapture(input.Capture)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "没有找到可用的 Adobe 登录凭据")
		return
	}
	account.ID = ""
	input.Name = strings.TrimSpace(input.Name)
	if len(input.Name) > 128 {
		writeError(w, http.StatusBadRequest, "账号名称过长")
		return
	}
	account.Enabled, account.Status = true, "pending"
	if account.AccessToken != "" && account.ARPSession != "" && account.UserID != "" {
		account.Status = "unverified"
	}
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	accounts, err := a.store.Accounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账号池失败")
		return
	}
	status := http.StatusCreated
	for _, previous := range accounts {
		if !(account.UserID != "" && account.UserID == previous.UserID) && !sameAdobeCookieSession(previous.Cookie, account.Cookie) {
			continue
		}
		if a.busy[previous.ID] {
			writeError(w, http.StatusConflict, "账号正在使用，请稍后导入")
			return
		}
		// 同账号更新必须保留经真实生成使用的风险会话，只令 Cookie 触发 IMS 续期。
		cookie := account.Cookie
		account = previous
		if cookie != "" {
			account.Cookie = cookie
		}
		account.Status, account.LastError = "pending", ""
		status = http.StatusOK
		break
	}
	if account.ID == "" {
		account.ID = newID()
	}
	if input.Name != "" {
		account.Name = input.Name
	}
	if account.Name == "" {
		account.Name = "Adobe 账号"
	}
	if err := a.store.SaveAccount(account); err != nil {
		writeError(w, http.StatusInternalServerError, "导入账号失败")
		return
	}
	writeJSON(w, status, publicAdminAccount(account))
}

// Cookie 输入允许完整请求头或邮箱分隔行，保存前规范化，拒绝多行请求注入。
func parseAdminCookie(raw string) (string, string, error) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
	if len(raw) == 0 || len(raw) > 1<<20 || strings.ContainsAny(raw, "\r\n") {
		return "", "", errors.New("请填写有效的 Cookie 文本")
	}
	var email string
	if prefix, value, ok := strings.Cut(raw, "----"); ok {
		email, raw = strings.TrimSpace(prefix), strings.TrimSpace(value)
		if len(email) > 254 || !strings.Contains(email, "@") {
			return "", "", errors.New("Cookie 前的邮箱无效")
		}
	}
	if len(raw) >= len("Cookie:") && strings.EqualFold(raw[:len("Cookie:")], "Cookie:") {
		raw = strings.TrimSpace(raw[len("Cookie:"):])
	}
	for _, pair := range strings.Split(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, "=")
		if !ok || !validCaptureCookie(name, value) {
			return "", "", errors.New("Cookie 格式无效")
		}
	}
	cookie := normalizedCaptureCookie(raw)
	if cookie == "" {
		return "", "", errors.New("没有找到有效的 Cookie")
	}
	return cookie, email, nil
}

func (a *App) adminBrowserAccount(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeAdminJSON(w, r, &input, 4096); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if len(input.Name) > 128 {
		writeError(w, http.StatusBadRequest, "账号名称过长")
		return
	}
	if strings.TrimSpace(os.Getenv("ADOBE2API_CDP_URL")) == "" {
		writeError(w, http.StatusServiceUnavailable, "浏览器同步尚未连接，请配置已登录浏览器")
		return
	}
	if a.generator == nil {
		writeError(w, http.StatusServiceUnavailable, "浏览器同步暂不可用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	// 空账号通过刷新适配器读取当前已登录页面；这里不创建登录页，也不导航用户页面。
	account, err := a.generator.Refresh(ctx, Account{})
	if err != nil {
		writeError(w, http.StatusBadGateway, safeError(err))
		return
	}
	if account.Cookie == "" || account.AccessToken == "" || account.ARPSession == "" || account.UserID == "" {
		writeError(w, http.StatusBadGateway, "浏览器没有返回完整的 Adobe 登录会话")
		return
	}
	a.poolMu.Lock()
	accounts, err := a.store.Accounts()
	if err != nil {
		a.poolMu.Unlock()
		writeError(w, http.StatusInternalServerError, "读取账号池失败")
		return
	}
	created := true
	for _, previous := range accounts {
		if previous.UserID != account.UserID && !(previous.UserID == "" && ((previous.Email != "" && strings.EqualFold(previous.Email, account.Email)) || sameAdobeCookieSession(previous.Cookie, account.Cookie))) {
			continue
		}
		if a.busy[previous.ID] {
			a.poolMu.Unlock()
			writeError(w, http.StatusConflict, "账号正在使用，请稍后同步")
			return
		}
		account.ID, account.Name, account.Enabled = previous.ID, previous.Name, previous.Enabled
		// 显式同步以本次页面的风险会话为准，不能因续期清空错误而沿用失效的旧会话。
		created = false
		break
	}
	if created {
		account.ID, account.Enabled = newID(), true
	}
	if a.busy == nil {
		a.busy = make(map[string]bool)
	}
	a.busy[account.ID] = true
	reservedID := account.ID
	a.poolMu.Unlock()
	defer func() {
		a.poolMu.Lock()
		delete(a.busy, reservedID)
		a.poolMu.Unlock()
	}()
	// 页面令牌的权限可能不适合生图，再由真实 Cookie 换取 Firefly IMS 令牌。
	refreshBase := account
	account, err = a.generator.Refresh(ctx, refreshBase)
	if err != nil {
		failed := mergeConfirmedRefresh(refreshBase, account)
		failed.Status, failed.LastError = "expired", safeError(err)
		if failed.Name == "" {
			failed.Name = "Adobe 账号"
		}
		if err := a.store.SaveAccount(failed); err != nil {
			writeError(w, http.StatusInternalServerError, "保存续期登录信息失败")
			return
		}
		writeError(w, http.StatusBadGateway, failed.LastError)
		return
	}
	account.ID = reservedID
	if account.AccessToken == "" || account.UserID == "" || account.ARPSession == "" {
		writeError(w, http.StatusBadGateway, "Adobe 没有返回完整的生图登录会话")
		return
	}
	if input.Name != "" {
		account.Name = input.Name
	}
	if account.Name == "" {
		account.Name = "Adobe 账号"
	}
	account.Status, account.LastError, account.LastRefresh = "unverified", "", time.Now().UTC()
	account.Nonce = ""
	if err := a.store.SaveAccount(account); err != nil {
		writeError(w, http.StatusInternalServerError, "保存浏览器账号失败")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"account": publicAdminAccount(account), "created": created})
}

// 新导入账号尚无用户标识时，用同一登录会话去重，其他统计 Cookie 不参与身份判断。
func sameAdobeCookieSession(previous, current string) bool {
	if previous == "" || current == "" {
		return false
	}
	first := (&http.Request{Header: http.Header{"Cookie": {previous}}}).Cookies()
	second := &http.Request{Header: http.Header{"Cookie": {current}}}
	for _, cookie := range first {
		if cookie.Name != "ims_sid" && cookie.Name != "sso_sid" {
			continue
		}
		other, err := second.Cookie(cookie.Name)
		if err == nil && cookie.Value != "" && cookie.Value == other.Value {
			return true
		}
	}
	return false
}

// 续期后半程失败也保留已确认同账号的轮换凭据，不接受空账号或其他用户的返回值。
func mergeConfirmedRefresh(original, partial Account) Account {
	if original.ID == "" || partial.ID != original.ID || partial.UserID == "" {
		return original
	}
	if expected := adobeUserID(original); expected != "" && expected != partial.UserID {
		return original
	}
	if tokenUser := adobeUserID(Account{AccessToken: partial.AccessToken}); tokenUser != "" && tokenUser != partial.UserID {
		return original
	}
	merged := original
	merged.UserID = partial.UserID
	if partial.Cookie != "" {
		merged.Cookie = partial.Cookie
	}
	if partial.AccessToken != "" {
		merged.AccessToken, merged.ExpiresAt = partial.AccessToken, partial.ExpiresAt
	}
	if partial.RefreshToken != "" {
		merged.RefreshToken = partial.RefreshToken
	}
	if partial.ClientID != "" {
		merged.ClientID = partial.ClientID
	}
	if partial.APIKey != "" {
		merged.APIKey = partial.APIKey
	}
	if merged.ARPSession == "" && partial.ARPSession != "" {
		merged.ARPSession = partial.ARPSession
	}
	if merged.Email == "" && partial.Email != "" {
		merged.Email = partial.Email
	}
	if !partial.LastRefresh.IsZero() {
		merged.LastRefresh = partial.LastRefresh
	}
	return merged
}

func (a *App) adminRefreshAccount(w http.ResponseWriter, r *http.Request) {
	a.poolMu.Lock()
	if a.busy[r.PathValue("id")] {
		a.poolMu.Unlock()
		writeError(w, http.StatusConflict, "账号正在使用，请稍后刷新")
		return
	}
	account, err := a.findAdminAccount(r.PathValue("id"))
	if err != nil {
		a.poolMu.Unlock()
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if a.busy == nil {
		a.busy = make(map[string]bool)
	}
	a.busy[account.ID] = true
	a.poolMu.Unlock()
	defer func() {
		a.poolMu.Lock()
		delete(a.busy, account.ID)
		a.poolMu.Unlock()
	}()
	if a.generator == nil {
		writeError(w, http.StatusServiceUnavailable, "账号刷新暂不可用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	updated, err := a.generator.Refresh(ctx, account)
	if err != nil {
		account = mergeConfirmedRefresh(account, updated)
		account.Status, account.LastError = "expired", safeError(err)
		if errors.Is(err, context.DeadlineExceeded) {
			account.LastError = "登录刷新超时，请稍后重试"
		} else if errors.Is(err, context.Canceled) {
			account.LastError = "登录刷新已取消"
		}
		_ = a.store.SaveAccount(account)
		writeError(w, http.StatusBadGateway, account.LastError)
		return
	}
	updated.ID, updated.LastRefresh = account.ID, time.Now().UTC()
	updated.LastError, updated.Status = "", "unverified"
	if err := a.store.SaveAccount(updated); err != nil {
		writeError(w, http.StatusInternalServerError, "保存刷新结果失败")
		return
	}
	writeJSON(w, http.StatusOK, publicAdminAccount(updated))
}

func (a *App) adminLogs(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	items, total, err := a.store.LogSummaries(page, limit, r.URL.Query().Get("status"), r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取日志失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "limit": limit})
}

func (a *App) adminLog(w http.ResponseWriter, r *http.Request) {
	item, err := a.store.Log(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "日志不存在")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *App) adminGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.store.Settings())
}

func validateAdminSettings(settings Settings) error {
	if settings.PublicURL != "" {
		u, err := url.Parse(settings.PublicURL)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("图片访问地址必须是有效的 HTTP 或 HTTPS 地址")
		}
	}
	if settings.ProxyURL != "" {
		u, err := url.Parse(settings.ProxyURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return errors.New("出口地址必须是 HTTP、HTTPS 或 SOCKS5 代理地址")
		}
	}
	if settings.RefreshMinutes < 0 || settings.RefreshMinutes > 10080 || settings.RetentionHours < 0 || settings.RetentionHours > 87600 || settings.MaxStorageMB < 0 || settings.MaxStorageMB > 10485760 || settings.LogRetentionDays < 0 || settings.LogRetentionDays > 3650 {
		return errors.New("保存期限或容量设置超出范围")
	}
	if settings.MaxConcurrent < 1 || settings.MaxConcurrent > 32 || settings.RequestTimeoutSeconds < 30 || settings.RequestTimeoutSeconds > 3600 {
		return errors.New("并发数应为 1 至 32，生成超时应为 30 至 3600 秒")
	}
	if strings.TrimSpace(settings.AdminUsername) == "" || len(settings.AdminUsername) > 64 {
		return errors.New("管理员账号不能为空或超过 64 位")
	}
	if len(settings.APIKeys) == 0 || len(settings.APIKeys) > 100 {
		return errors.New("请至少设置一个调用密钥，最多 100 个")
	}
	seen := make(map[string]bool)
	for _, key := range settings.APIKeys {
		if len(key) < 16 || len(key) > 256 || strings.TrimSpace(key) != key || seen[key] {
			return errors.New("调用密钥至少 16 位且不能重复或包含首尾空白")
		}
		seen[key] = true
	}
	return nil
}

func (a *App) adminSaveSettings(w http.ResponseWriter, r *http.Request) {
	settings := a.store.Settings()
	previousHash := settings.AdminPasswordHash
	input := struct {
		*Settings
		Password string `json:"password"`
	}{Settings: &settings}
	if err := decodeAdminJSON(w, r, &input, 65536); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	settings.PublicURL = strings.TrimRight(strings.TrimSpace(settings.PublicURL), "/")
	settings.ProxyURL = strings.TrimSpace(settings.ProxyURL)
	settings.AdminUsername = strings.TrimSpace(settings.AdminUsername)
	if err := validateAdminSettings(settings); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.Password != "" {
		if !validAdminCredentials(settings.AdminUsername, input.Password) {
			writeError(w, http.StatusBadRequest, "新密码至少 10 位")
			return
		}
		var err error
		settings.AdminPasswordHash, err = hashAdminPassword(input.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "保存密码失败")
			return
		}
	}
	if err := a.store.SaveSettings(settings); err != nil {
		writeError(w, http.StatusInternalServerError, "保存设置失败")
		return
	}
	if previousHash != settings.AdminPasswordHash {
		// 当前管理员获得新会话，其他设备的旧会话在密码修改后失效。
		a.adminAuth.mu.Lock()
		a.adminAuth.sessions = make(map[string]adminSession)
		token := a.newAdminSessionLocked(settings.AdminPasswordHash)
		a.adminAuth.mu.Unlock()
		a.setAdminCookie(w, r, token, 86400)
	}
	writeJSON(w, http.StatusOK, settings)
}

func (a *App) adminCleanup(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Cleanup(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "图片清理失败")
		return
	}
	a.adminStats(w, r)
}
