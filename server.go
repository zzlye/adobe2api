package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
)

//go:embed web/*
var frontend embed.FS

// App 的并发计数同时约束请求总量、单账号任务与每分钟请求上限，避免重复扣费和失控排队。
type App struct {
	store           *Store
	generator       Generator
	adminAuth       *adminAuth
	poolMu          sync.Mutex
	busy            map[string]bool
	activeByAccount map[string]int
	requestHistory  map[string][]time.Time
	active          int
	next            int
	inputOnce       sync.Once
	inputGate       chan struct{}
	jobs            sync.WaitGroup
}

var errRateLimited = errors.New("当前请求已超过并发或单账号每分钟请求上限，请稍后重试")

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": "api_error", "code": status}})
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	a.registerAdmin(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/admin/models", a.adminOnly(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"items": availableModels})
	}))
	mux.HandleFunc("POST /api/admin/uploads", a.adminOnly(a.imageInput(a.upload)))
	mux.HandleFunc("POST /api/admin/generate", a.adminOnly(a.imageInput(a.adminGenerate)))
	mux.HandleFunc("GET /v1/models", a.apiOnly(a.openAIModels))
	mux.HandleFunc("POST /v1/images/generations", a.apiOnly(a.imageInput(a.openAIImages)))
	mux.HandleFunc("POST /v1/images/edits", a.apiOnly(a.imageInput(a.openAIEdits)))
	mux.HandleFunc("POST /v1/chat/completions", a.apiOnly(a.imageInput(a.openAIChat)))
	mux.HandleFunc("GET /v1beta/models", a.apiOnly(a.geminiModels))
	mux.HandleFunc("POST /v1beta/models/{action}", a.apiOnly(a.imageInput(a.geminiGenerate)))
	mux.HandleFunc("POST /v1/models/{action}", a.apiOnly(a.imageInput(a.geminiGenerate)))
	mux.HandleFunc("GET /images/{id}", a.serveImage)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeError(w, 404, "接口不存在") })
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) { writeError(w, 404, "接口不存在") })
	mux.HandleFunc("/v1beta/", func(w http.ResponseWriter, r *http.Request) { writeError(w, 404, "接口不存在") })
	web, _ := fs.Sub(frontend, "web")
	files := http.FileServer(http.FS(web))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "请求方法不支持")
			return
		}
		files.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		defer func() {
			if recover() != nil {
				log.Print("处理请求时发生异常")
				writeError(w, 500, "处理请求失败")
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

type imageInputReleaseKey struct{}

// 生成中触发的续期等内部请求共享同一条日志，管理后台独立续期不绑定生成记录。
type requestLogContextKey struct{}

// 大型 JSON 编码只允许少量请求同时解析，转换为磁盘图片后立即释放名额。
func (a *App) imageInput(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.inputOnce.Do(func() { a.inputGate = make(chan struct{}, 2) })
		select {
		case a.inputGate <- struct{}{}:
		default:
			if strings.HasPrefix(r.URL.Path, "/v1beta/") || strings.HasPrefix(r.URL.Path, "/v1/models/") {
				writeGeminiError(w, 503, "图片输入处理中，请稍后重试")
			} else {
				writeError(w, 503, "图片输入处理中，请稍后重试")
			}
			return
		}
		var once sync.Once
		release := func() { once.Do(func() { <-a.inputGate }) }
		defer release()
		next(w, r.WithContext(context.WithValue(r.Context(), imageInputReleaseKey{}, release)))
	}
}

func (a *App) apiOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" {
			key = r.Header.Get("X-Goog-Api-Key")
		}
		if key == "" {
			key = r.URL.Query().Get("key")
		}
		valid := false
		for _, allowed := range a.store.Settings().APIKeys {
			if len(allowed) > 0 && subtle.ConstantTimeCompare([]byte(key), []byte(allowed)) == 1 {
				valid = true
			}
		}
		if !valid {
			if strings.HasPrefix(r.URL.Path, "/v1beta/") || strings.HasPrefix(r.URL.Path, "/v1/models/") {
				writeGeminiError(w, 401, "API Key 无效")
				return
			}
			writeError(w, 401, "API Key 无效")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

// accountAvailableLocked 同时检查账号启用状态、后台刷新占用与一分钟滑动窗口内的请求配额。
func (a *App) accountAvailableLocked(account Account, now time.Time) bool {
	if !account.Enabled {
		return false
	}
	inFlight := a.activeByAccount[account.ID]
	if a.busy[account.ID] && inFlight == 0 {
		return false
	}
	history := a.requestHistory[account.ID]
	valid := history[:0]
	for _, ts := range history {
		if now.Sub(ts) < time.Minute {
			valid = append(valid, ts)
		}
	}
	if len(valid) == 0 {
		delete(a.requestHistory, account.ID)
	} else {
		a.requestHistory[account.ID] = valid
	}
	if account.RequestsPerMinute > 0 {
		return len(valid) < account.RequestsPerMinute && inFlight < account.RequestsPerMinute
	}
	return !a.busy[account.ID] && inFlight == 0
}

func (a *App) reserveAccount() (Account, func(), error) {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	accounts, err := a.store.Accounts()
	if err != nil {
		return Account{}, nil, err
	}
	hasEnabled := false
	for _, account := range accounts {
		if account.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return Account{}, nil, errors.New("暂无可用账号，请检查账号池")
	}
	if a.active >= a.store.Settings().MaxConcurrent {
		return Account{}, nil, errRateLimited
	}
	if a.busy == nil {
		a.busy = make(map[string]bool)
	}
	if a.activeByAccount == nil {
		a.activeByAccount = make(map[string]int)
	}
	if a.requestHistory == nil {
		a.requestHistory = make(map[string][]time.Time)
	}
	now := time.Now()
	// 先从全部可接收请求的启用账号中均匀抽样，不能随机起点后顺延造成权重偏差。
	if a.store.Settings().AccountStrategy == "random" {
		candidates := make([]int, 0, len(accounts))
		for i, account := range accounts {
			if a.accountAvailableLocked(account, now) {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) > 0 {
			a.next = candidates[rand.IntN(len(candidates))]
		}
	}
	for i := 0; i < len(accounts); i++ {
		index := (a.next + i) % len(accounts)
		account := accounts[index]
		if !a.accountAvailableLocked(account, now) {
			continue
		}
		a.next = index + 1
		a.active++
		a.activeByAccount[account.ID]++
		a.busy[account.ID] = true
		if account.RequestsPerMinute > 0 {
			a.requestHistory[account.ID] = append(a.requestHistory[account.ID], now)
		}
		return account, func() {
			a.poolMu.Lock()
			a.activeByAccount[account.ID]--
			if a.activeByAccount[account.ID] <= 0 {
				delete(a.activeByAccount, account.ID)
				delete(a.busy, account.ID)
			}
			a.active--
			a.poolMu.Unlock()
		}, nil
	}
	return Account{}, nil, errRateLimited
}

func (a *App) runGenerate(ctx context.Context, input GenerateInput, protocol, endpoint string) (GenerateResult, RequestLog, error) {
	if release, ok := ctx.Value(imageInputReleaseKey{}).(func()); ok {
		release()
	}
	started := time.Now()
	entry := RequestLog{ID: newID(), CreatedAt: started, Protocol: protocol, Endpoint: endpoint, Model: input.Model, Prompt: input.Prompt, Status: "failed", HTTPStatus: 502, Images: []ImageRecord{}, References: input.allReferences(), Upstream: []UpstreamRecord{}}
	entry.Parameters = input.parameterJSON()
	finish := func(result GenerateResult, err error) (GenerateResult, RequestLog, error) {
		entry.FinishedAt = time.Now()
		entry.DurationMS = entry.FinishedAt.Sub(started).Milliseconds()
		entry.Error = safeError(err)
		entry.Images = result.Images
		entry.Credits = result.Credits
		if err == nil {
			entry.Status = "success"
			entry.HTTPStatus = 200
		}
		if errors.Is(err, context.Canceled) {
			entry.Status = "cancelled"
			entry.HTTPStatus = 499
		}
		if errors.Is(err, context.DeadlineExceeded) {
			entry.HTTPStatus = 504
		}
		if saveErr := a.store.SaveLog(entry); saveErr != nil {
			log.Print("保存生成记录失败")
		}
		return result, entry, err
	}
	if err := validateInput(&input); err != nil {
		entry.HTTPStatus = 400
		return finish(GenerateResult{}, err)
	}
	entry.Model = input.Model
	entry.Parameters = input.parameterJSON()
	account, release, err := a.reserveAccount()
	if err != nil {
		if errors.Is(err, errRateLimited) {
			entry.HTTPStatus = http.StatusTooManyRequests
		} else {
			entry.HTTPStatus = http.StatusServiceUnavailable
		}
		return finish(GenerateResult{}, err)
	}
	defer release()
	saveAccountState := func(acc Account) error {
		if current, findErr := a.findAdminAccount(acc.ID); findErr == nil {
			acc.Name, acc.Enabled, acc.RequestsPerMinute = current.Name, current.Enabled, current.RequestsPerMinute
		}
		return a.store.SaveAccount(acc)
	}
	entry.AccountID = account.ID
	entry.AccountName = account.Name
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.store.Settings().RequestTimeoutSeconds)*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, requestLogContextKey{}, &entry)
	unpin := a.store.PinImages(input.allReferences())
	defer unpin()
	if account.AccessToken == "" || account.UserID == "" || account.Status == "pending" || account.Status == "expired" || (!account.ExpiresAt.IsZero() && time.Until(account.ExpiresAt) < time.Minute) {
		refreshed, err := a.generator.Refresh(ctx, account)
		if err != nil {
			account = mergeConfirmedRefresh(account, refreshed)
			account.LastError = safeError(err)
			account.Status = "expired"
			_ = saveAccountState(account)
			entry.HTTPStatus = 503
			return finish(GenerateResult{}, err)
		}
		account = refreshed
		if err := saveAccountState(account); err != nil {
			return finish(GenerateResult{}, err)
		}
	}
	result, err := a.generator.Generate(ctx, account, input, &entry)
	if err != nil {
		account.LastError = safeError(err)
		account.Status = "error"
		var upstream *UpstreamError
		if errors.As(err, &upstream) {
			entry.HTTPStatus = upstream.StatusCode
			// 模型无权限属于账号套餐错误，不能要求有效登录重新续期。
			if upstream.StatusCode == 401 || upstream.StatusCode == 403 && upstream.AccessError != "user_not_entitled" {
				account.Status = "expired"
			}
		}
		_ = saveAccountState(account)
	} else {
		// 只有真实生成成功才标记可用，令牌续期不代表生图能力已验证。
		account.LastError = ""
		account.Status = "ready"
		_ = saveAccountState(account)
	}
	return finish(result, err)
}

// maintenance 顺序处理续期与清理，刷新中的账号不会参与生成。
func (a *App) maintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.store.Cleanup(ctx); err != nil {
				log.Print("图片清理失败")
			}
			settings := a.store.Settings()
			if settings.RefreshMinutes == 0 {
				continue
			}
			accounts, err := a.store.Accounts()
			if err != nil {
				continue
			}
			for _, account := range accounts {
				if !account.Enabled || time.Since(account.LastRefresh) < time.Duration(settings.RefreshMinutes)*time.Minute {
					continue
				}
				a.poolMu.Lock()
				if a.busy == nil {
					a.busy = make(map[string]bool)
				}
				inUse := a.busy[account.ID]
				if !inUse {
					current, findErr := a.findAdminAccount(account.ID)
					if findErr != nil || !current.Enabled {
						a.poolMu.Unlock()
						continue
					}
					account = current
					a.busy[account.ID] = true
				}
				a.poolMu.Unlock()
				if inUse {
					continue
				}
				refreshCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
				refreshed, refreshErr := a.generator.Refresh(refreshCtx, account)
				cancel()
				if refreshErr == nil {
					_ = a.store.SaveAccount(refreshed)
				} else {
					account = mergeConfirmedRefresh(account, refreshed)
					account.Status = "expired"
					account.LastError = safeError(refreshErr)
					account.LastRefresh = time.Now()
					_ = a.store.SaveAccount(account)
				}
				a.poolMu.Lock()
				delete(a.busy, account.ID)
				a.poolMu.Unlock()
				if ctx.Err() != nil {
					return
				}
			}
		}
	}
}

func main() {
	data := flag.String("data", envOr("ADOBE2API_DATA_DIR", "data"), "数据保存目录")
	addr := flag.String("addr", envOr("ADOBE2API_ADDR", ":8080"), "监听地址")
	importPath := flag.String("import-account", "", "导入账号文件后退出")
	flag.Parse()
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(256 << 20)
	}
	store, err := NewStore(*data)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	if *importPath != "" {
		raw, err := os.ReadFile(*importPath)
		if err != nil {
			log.Fatal("读取账号文件失败")
		}
		account, err := parseCapture(string(raw))
		if err != nil {
			log.Fatal(err)
		}
		account.ID = newID()
		if err := store.SaveAccount(account); err != nil {
			log.Fatal(err)
		}
		fmt.Println("账号已导入")
		return
	}
	app := &App{store: store, busy: make(map[string]bool)}
	generator := NewAdobeGenerator(store)
	defer generator.Close()
	generator.browserRefresh = app.refreshBrowser
	app.generator = generator
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *addr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second, IdleTimeout: 75 * time.Second, MaxHeaderBytes: 32 << 10,
		// 停机信号同时取消在途任务，使上游轮询可以及时终止。
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go app.maintenance(ctx)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()
	log.Printf("Adobe2API 已启动：http://%s", *addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	// 等待请求和流式连接背后的任务落日志后，再关闭存储。
	<-shutdownDone
	app.jobs.Wait()
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
