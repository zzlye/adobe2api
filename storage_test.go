package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 测试产生的数据固定放在临时目录，退出时删除本测试创建的子目录。
func storageTestDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS == "windows" {
		base = `D:\tmp`
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "adobe2api-store-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		relative, err := filepath.Rel(base, dir)
		if err != nil || strings.HasPrefix(relative, "..") || relative == "." {
			t.Fatal("临时目录范围不正确")
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func newStorageTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(storageTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func storageTestPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 7, 5))
	picture.Set(0, 0, color.RGBA{R: 20, G: 40, B: 80, A: 255})
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestStorageCredentialsAndSettingsSurviveRestartEncrypted(t *testing.T) {
	dir := storageTestDir(t)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	account := Account{ID: newID(), Name: "测试账号", Enabled: true, Cookie: "secret-cookie-fixture", AccessToken: "secret-token-fixture", RefreshToken: "secret-refresh-fixture", ARPSession: "secret-arp-fixture"}
	if err := store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	settings := store.Settings()
	settings.AdminUsername = "管理者"
	settings.AdminPasswordHash = "password-hash-fixture"
	settings.APIKeys = []string{"api-key-secret-fixture"}
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	var accountPayload, settingsPayload []byte
	if err := store.db.QueryRow("SELECT payload FROM accounts WHERE id=?", account.ID).Scan(&accountPayload); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT payload FROM settings WHERE id=1").Scan(&settingsPayload); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{account.Cookie, account.AccessToken, account.RefreshToken, account.ARPSession, settings.AdminPasswordHash, settings.APIKeys[0]} {
		if bytes.Contains(accountPayload, []byte(secret)) || bytes.Contains(settingsPayload, []byte(secret)) {
			t.Fatalf("数据库保存了明文凭据: %s", secret)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got := store.Settings()
	if got.AdminPasswordHash != settings.AdminPasswordHash || len(got.APIKeys) != 1 || got.APIKeys[0] != settings.APIKeys[0] {
		t.Fatal("设置或密码摘要在重启后丢失")
	}
	accounts, err := store.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].AccessToken != account.AccessToken || accounts[0].Cookie != account.Cookie {
		t.Fatalf("凭据没有正确恢复: %v", err)
	}
	got.APIKeys[0] = "修改快照"
	if store.Settings().APIKeys[0] != settings.APIKeys[0] {
		t.Fatal("设置快照影响了内部状态")
	}
}

func TestStorageLogsRemoveCredentialsAndPageMetadata(t *testing.T) {
	store := newStorageTestStore(t)
	data := storageTestPNG(t)
	imageRecord, err := store.SaveImage(bytes.NewReader(data), "image/png", "generated")
	if err != nil {
		t.Fatal(err)
	}
	if imageRecord.Width != 7 || imageRecord.Height != 5 || imageRecord.Bytes != int64(len(data)) {
		t.Fatal("图片元信息错误")
	}
	for i := 0; i < 7; i++ {
		log := RequestLog{ID: fmt.Sprintf("请求-%d", i), CreatedAt: time.Now().Add(time.Duration(i) * time.Millisecond), Status: "success", Model: "香蕉", Prompt: "山间小屋", Images: []ImageRecord{imageRecord}, Parameters: json.RawMessage(`{"aspect_ratio":"3:2","resolution":"2K","authorization":"credential-marker","nested":{"inline_data":{"data":"base64-marker"},"cookie":"cookie-marker"}}`), Upstream: []UpstreamRecord{{URL: "https://example.test/generate?token=token-marker&X-Amz-Signature=signature-marker&model=banana"}}}
		if err := store.SaveLog(log); err != nil {
			t.Fatal(err)
		}
	}
	items, total, err := store.Logs(2, 3, "success", "山间")
	if err != nil || total != 7 || len(items) != 3 {
		t.Fatalf("分页结果不正确: %d %d %v", total, len(items), err)
	}
	encoded, _ := json.Marshal(items)
	for _, marker := range []string{"credential-marker", "base64-marker", "cookie-marker", "token-marker", "signature-marker"} {
		if bytes.Contains(encoded, []byte(marker)) {
			t.Fatalf("日志泄露敏感信息: %s", marker)
		}
	}
	if !bytes.Contains(encoded, []byte("3:2")) || !bytes.Contains(encoded, []byte("2K")) {
		t.Fatal("正常生成参数没有保留")
	}
	if len(items[0].Images) != 1 || items[0].Images[0].Path == "" {
		t.Fatal("图片索引没有恢复")
	}
}

func TestStorageCleanupKeepsPinnedAndFreshImagesAndDeletesOldest(t *testing.T) {
	dir := storageTestDir(t)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	settings := store.Settings()
	settings.RetentionHours, settings.MaxStorageMB = 0, 1
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	imageRecords := make([]ImageRecord, 4)
	for i := range imageRecords {
		record, err := store.SaveImage(bytes.NewReader(storageTestPNG(t)), "image/png", "reference")
		if err != nil {
			t.Fatal(err)
		}
		imageRecords[i] = record
		created := time.Now().Add(-time.Duration(8-i) * time.Hour)
		if i == 3 {
			created = time.Now()
		}
		if _, err := store.db.Exec("UPDATE images SET created_at=?,bytes=? WHERE id=?", created.UnixMilli(), 400*1024, record.ID); err != nil {
			t.Fatal(err)
		}
	}
	// 恢复已有磁盘数据，模拟数小时前完成的图片，而非刚开始的生成任务。
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	release := store.PinImages([]ImageRecord{imageRecords[0]})
	if err := store.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, record := range imageRecords {
		current, err := store.Image(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantDeleted := i == 1 || i == 2
		if current.Deleted != wantDeleted {
			t.Fatalf("图片 %d 清理状态错误: %v", i, current.Deleted)
		}
		_, err = os.Stat(record.Path)
		if wantDeleted && !os.IsNotExist(err) {
			t.Fatal("已清理图片仍占用磁盘")
		}
	}
	release()
	release()
	settings.RetentionHours = 1
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := store.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	pinned, err := store.Image(imageRecords[0].ID)
	if err != nil || !pinned.Deleted {
		t.Fatal("释放保护后旧图片没有清理")
	}
	fresh, err := store.Image(imageRecords[3].ID)
	if err != nil || fresh.Deleted {
		t.Fatal("近期图片被清理")
	}
}

// 容量设置应清理不足一小时的新图片，并在失败请求结束后释放临时保护。
func TestStorageCapacityCleansRecentImagesAfterResponseLease(t *testing.T) {
	store := newStorageTestStore(t)
	settings := store.Settings()
	settings.RetentionHours, settings.MaxStorageMB = 0, 1
	if err := store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	data := append(storageTestPNG(t), make([]byte, 400*1024)...)
	images := make([]ImageRecord, 0, 3)
	for i := 0; i < 3; i++ {
		image, err := store.SaveImage(bytes.NewReader(data), "image/png", "generated")
		if err != nil {
			t.Fatal(err)
		}
		images = append(images, image)
		time.Sleep(2 * time.Millisecond)
	}
	if err := store.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, record := range images {
		image, err := store.Image(record.ID)
		if err != nil || image.Deleted {
			t.Fatal("尚未结束的生成图片被容量清理")
		}
	}
	if err := store.SaveLog(RequestLog{ID: newID(), Status: "failed", Images: images[:2], References: images[2:]}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10100 * time.Millisecond)
	if err := store.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldest, err := store.Image(images[0].ID)
	if err != nil || !oldest.Deleted {
		t.Fatal("最新一小时内的最旧图片未按容量设置清理")
	}
	stats, err := store.Stats()
	if err != nil || stats["storage_bytes"].(int64) > 1024*1024 || stats["image_count"] != int64(2) {
		t.Fatalf("容量清理没有遵守上限: %v %v", stats, err)
	}
	store.imageMu.Lock()
	leaseCount := len(store.imageLeases)
	store.imageMu.Unlock()
	if leaseCount != 0 {
		t.Fatal("过期图片租约持续占用内存")
	}
}

func TestStorageConcurrentSaveAndCleanup(t *testing.T) {
	store := newStorageTestStore(t)
	data := storageTestPNG(t)
	var group sync.WaitGroup
	errors := make(chan error, 24)
	for i := 0; i < 12; i++ {
		group.Add(2)
		go func() {
			defer group.Done()
			_, err := store.SaveImage(bytes.NewReader(data), "image/png", "generated")
			errors <- err
		}()
		go func() {
			defer group.Done()
			errors <- store.Cleanup(context.Background())
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.Stats()
	if err != nil || stats["image_count"] != int64(12) {
		t.Fatalf("并发保存或清理丢失图片: %v %v", stats, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Cleanup(ctx); err == nil {
		t.Fatal("已取消的清理仍然执行")
	}
}

type storageSlowReader struct {
	data    *bytes.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (reader *storageSlowReader) Read(buffer []byte) (int, error) {
	reader.once.Do(func() { close(reader.entered); <-reader.release })
	return reader.data.Read(buffer)
}

// 慢速图片下载不应阻塞其他已完成图片的清理和图片索引访问。
func TestStorageSlowImageStreamDoesNotBlockCleanup(t *testing.T) {
	store := newStorageTestStore(t)
	reader := &storageSlowReader{data: bytes.NewReader(storageTestPNG(t)), entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(reader.release) }) }
	defer release()
	saved := make(chan error, 1)
	go func() { _, err := store.SaveImage(reader, "image/png", "generated"); saved <- err }()
	select {
	case <-reader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("图片流未开始读取")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cleaned := make(chan error, 1)
	go func() { cleaned <- store.Cleanup(ctx) }()
	select {
	case err := <-cleaned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("慢速下载阻塞了图片清理")
	}
	release()
	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("图片流完成后没有保存")
	}
}

func adminTestRequest(handler http.Handler, method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:1234"
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestAdminAuthenticationSameOriginAndPasswordRevocation(t *testing.T) {
	store := newStorageTestStore(t)
	app := &App{store: store}
	mux := http.NewServeMux()
	app.registerAdmin(mux)
	if result := adminTestRequest(mux, "GET", "/api/admin/stats", "", nil, ""); result.Code != http.StatusUnauthorized {
		t.Fatal("后台允许未登录访问")
	}
	if result := adminTestRequest(mux, "POST", "/api/auth/setup", `{"username":"admin","password":"正确密码abcdefgh"}`, nil, "https://external.test"); result.Code != http.StatusForbidden {
		t.Fatal("外部网页可以初始化管理员")
	}
	result := adminTestRequest(mux, "POST", "/api/auth/setup", `{"username":"admin","password":"正确密码abcdefgh"}`, nil, "http://localhost")
	if result.Code != http.StatusOK {
		t.Fatalf("初始化失败: %s", result.Body.String())
	}
	cookies := result.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("会话 Cookie 缺少保护")
	}
	firstCookie := cookies[0]
	if store.Settings().AdminPasswordHash == "" || len(store.Settings().APIKeys) != 1 {
		t.Fatal("初始化未保存密码或随机调用密钥")
	}
	if result := adminTestRequest(mux, "POST", "/api/auth/setup", `{"username":"other","password":"正确密码abcdefgh"}`, nil, ""); result.Code != http.StatusConflict {
		t.Fatal("第二次初始化覆盖了管理员")
	}
	if result := adminTestRequest(mux, "PUT", "/api/admin/settings", `{"retention_hours":12}`, firstCookie, "https://external.test"); result.Code != http.StatusForbidden {
		t.Fatal("外部网页可以修改设置")
	}
	if result := adminTestRequest(mux, "GET", "/api/admin/settings", "", firstCookie, ""); result.Code != http.StatusOK || strings.Contains(result.Body.String(), "password_hash") {
		t.Fatal("后台设置鉴权或去密失败")
	}
	login := adminTestRequest(mux, "POST", "/api/auth/login", `{"username":"admin","password":"正确密码abcdefgh"}`, nil, "")
	if login.Code != http.StatusOK {
		t.Fatal("管理员登录失败")
	}
	otherCookie := login.Result().Cookies()[0]
	change := adminTestRequest(mux, "PUT", "/api/admin/settings", `{"password":"新密码abcdefghij"}`, firstCookie, "http://localhost")
	if change.Code != http.StatusOK {
		t.Fatalf("修改密码失败: %s", change.Body.String())
	}
	if result := adminTestRequest(mux, "GET", "/api/admin/stats", "", otherCookie, ""); result.Code != http.StatusUnauthorized {
		t.Fatal("修改密码后旧会话仍有效")
	}
	if result := adminTestRequest(mux, "GET", "/api/admin/stats", "", change.Result().Cookies()[0], ""); result.Code != http.StatusOK {
		t.Fatal("修改密码后当前会话失效")
	}
}

func TestAdminAccountUpdateKeepsCredentialsAndLoginRateLimit(t *testing.T) {
	store := newStorageTestStore(t)
	app := &App{store: store}
	mux := http.NewServeMux()
	app.registerAdmin(mux)
	setup := adminTestRequest(mux, "POST", "/api/auth/setup", `{"username":"admin","password":"正确密码abcdefgh"}`, nil, "")
	if setup.Code != http.StatusOK {
		t.Fatal(setup.Body.String())
	}
	cookie := setup.Result().Cookies()[0]
	account := Account{ID: newID(), Name: "原账号", Enabled: true, Cookie: "credential-cookie-marker", AccessToken: "credential-token-marker"}
	if err := store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	result := adminTestRequest(mux, "PUT", "/api/admin/accounts/"+account.ID, `{"name":"新账号","enabled":false,"cookie":"","access_token":""}`, cookie, "")
	if result.Code != http.StatusOK || strings.Contains(result.Body.String(), "credential-") {
		t.Fatal("账号修改失败或返回了凭据")
	}
	accounts, err := store.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].Cookie != account.Cookie || accounts[0].AccessToken != account.AccessToken || accounts[0].Enabled || accounts[0].Name != "新账号" {
		t.Fatal("账号修改擦除了凭据或没有保存字段")
	}
	for i := 0; i < 11; i++ {
		result := adminTestRequest(mux, "POST", "/api/auth/login", `{"username":"admin","password":"错误密码abcdefgh"}`, nil, "")
		want := http.StatusUnauthorized
		if i == 10 {
			want = http.StatusTooManyRequests
		}
		if result.Code != want {
			body, _ := io.ReadAll(result.Result().Body)
			t.Fatalf("登录限速错误，尝试 %d: %d %s", i+1, result.Code, body)
		}
	}
}

type storageBlockedGenerator struct {
	entered chan struct{}
	release chan struct{}
}

func (g *storageBlockedGenerator) Generate(context.Context, Account, GenerateInput, *RequestLog) (GenerateResult, error) {
	return GenerateResult{}, fmt.Errorf("此测试只验证账号刷新")
}

func (g *storageBlockedGenerator) Refresh(ctx context.Context, account Account) (Account, error) {
	close(g.entered)
	select {
	case <-ctx.Done():
		return Account{}, ctx.Err()
	case <-g.release:
		return account, nil
	}
}

// 刷新占用账号期间拒绝修改和删除，避免旧刷新结果重新创建已删除账号。
func TestAdminBusyAccountCannotBeUpdatedDeletedOrRefreshedTwice(t *testing.T) {
	store := newStorageTestStore(t)
	generator := &storageBlockedGenerator{entered: make(chan struct{}), release: make(chan struct{})}
	app := &App{store: store, generator: generator}
	mux := http.NewServeMux()
	app.registerAdmin(mux)
	setup := adminTestRequest(mux, "POST", "/api/auth/setup", `{"username":"admin","password":"正确密码abcdefgh"}`, nil, "")
	if setup.Code != http.StatusOK {
		t.Fatal(setup.Body.String())
	}
	cookie := setup.Result().Cookies()[0]
	account := Account{ID: newID(), Name: "测试账号", Enabled: true, AccessToken: "test-token"}
	if err := store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(generator.release) }) }
	defer release()
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		finished <- adminTestRequest(mux, "POST", "/api/admin/accounts/"+account.ID+"/refresh", "", cookie, "")
	}()
	select {
	case <-generator.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("账号刷新没有启动")
	}
	for _, request := range []struct{ method, path, body string }{
		{"PUT", "/api/admin/accounts/" + account.ID, `{"name":"修改账号"}`},
		{"DELETE", "/api/admin/accounts/" + account.ID, ""},
		{"POST", "/api/admin/accounts/" + account.ID + "/refresh", ""},
	} {
		result := adminTestRequest(mux, request.method, request.path, request.body, cookie, "")
		if result.Code != http.StatusConflict {
			t.Fatalf("占用期间接口 %s %s 未被拒绝: %d", request.method, request.path, result.Code)
		}
	}
	release()
	select {
	case result := <-finished:
		if result.Code != http.StatusOK {
			t.Fatalf("刷新结果错误: %s", result.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("刷新没有完成")
	}
	if result := adminTestRequest(mux, "DELETE", "/api/admin/accounts/"+account.ID, "", cookie, ""); result.Code != http.StatusOK {
		t.Fatal("刷新完成后无法删除账号")
	}
}
