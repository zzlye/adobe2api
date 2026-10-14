package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const maxImageBytes int64 = 64 << 20

// Store 将图片留在磁盘，仅在内存中保留设置与正在使用的图片索引。
type Store struct {
	db          *sql.DB
	dataDir     string
	aead        cipher.AEAD
	mu          sync.RWMutex
	settings    Settings
	imageMu     sync.Mutex
	imagePins   map[string]int
	imageLeases map[string]time.Time
}

// settingsEnvelope 显式保存密码摘要，避免接口隐藏字段被 JSON 序列化丢弃。
type settingsEnvelope struct {
	Settings     Settings `json:"settings"`
	PasswordHash string   `json:"password_hash"`
}

func NewStore(dataDir string) (*Store, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "images"), 0700); err != nil {
		return nil, err
	}
	key, err := loadStorageKey(filepath.Join(abs, "master.key"))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// 连接设置随每次重新连接生效；单连接避免并发写入时重复占用缓存。
	dsn := filepath.ToSlash(filepath.Join(abs, "adobe2api.db")) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-4096)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, dataDir: abs, aead: aead, imagePins: make(map[string]int), imageLeases: make(map[string]time.Time)}
	s.settings = Settings{RefreshMinutes: 30, RetentionHours: 168, MaxStorageMB: 1024, MaxConcurrent: 2, RequestTimeoutSeconds: 300, LogRetentionDays: 30, APIKeys: []string{}}
	schema := `
CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK(id = 1), payload BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS accounts (id TEXT PRIMARY KEY, name TEXT NOT NULL, enabled INTEGER NOT NULL, status TEXT NOT NULL, payload BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS images (id TEXT PRIMARY KEY, path TEXT NOT NULL, mime TEXT NOT NULL, bytes INTEGER NOT NULL, width INTEGER NOT NULL, height INTEGER NOT NULL, kind TEXT NOT NULL, created_at INTEGER NOT NULL, deleted INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS images_cleanup ON images(deleted, created_at, id);
CREATE TABLE IF NOT EXISTS logs (id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, status TEXT NOT NULL, model TEXT NOT NULL, prompt TEXT NOT NULL, account_name TEXT NOT NULL, credits REAL, payload BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS logs_created ON logs(created_at DESC);
CREATE INDEX IF NOT EXISTS logs_status_created ON logs(status, created_at DESC);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化数据库失败: %w", err)
	}
	_ = os.Chmod(filepath.Join(abs, "adobe2api.db"), 0600)
	var payload []byte
	err = db.QueryRow("SELECT payload FROM settings WHERE id=1").Scan(&payload)
	if err == nil {
		plain, decryptErr := s.decrypt(payload)
		if decryptErr != nil {
			db.Close()
			return nil, fmt.Errorf("读取设置失败: %w", decryptErr)
		}
		var envelope settingsEnvelope
		if err := json.Unmarshal(plain, &envelope); err != nil {
			db.Close()
			return nil, err
		}
		s.settings = envelope.Settings
		s.settings.AdminPasswordHash = envelope.PasswordHash
	} else if !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, err
	}
	return s, nil
}

func loadStorageKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("数据加密密钥长度错误")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return loadStorageKey(path)
	}
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(key); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *Store) encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, nil), nil
}

func (s *Store) decrypt(payload []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(payload) < n {
		return nil, errors.New("加密数据不完整")
	}
	return s.aead.Open(nil, payload[:n], payload[n:], nil)
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	settings := s.settings
	settings.APIKeys = append([]string{}, settings.APIKeys...)
	return settings
}

func (s *Store) SaveSettings(settings Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plain, err := json.Marshal(settingsEnvelope{Settings: settings, PasswordHash: settings.AdminPasswordHash})
	if err != nil {
		return err
	}
	payload, err := s.encrypt(plain)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec("INSERT INTO settings(id,payload) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload", payload); err != nil {
		return err
	}
	settings.APIKeys = append([]string{}, settings.APIKeys...)
	s.settings = settings
	return nil
}

func (s *Store) Accounts() ([]Account, error) {
	rows, err := s.db.Query("SELECT payload FROM accounts ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := make([]Account, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		plain, err := s.decrypt(payload)
		if err != nil {
			return nil, errors.New("账号凭据解密失败")
		}
		var account Account
		if err := json.Unmarshal(plain, &account); err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (s *Store) SaveAccount(account Account) error {
	if account.ID == "" {
		return errors.New("账号 ID 不能为空")
	}
	plain, err := json.Marshal(account)
	if err != nil {
		return err
	}
	payload, err := s.encrypt(plain)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO accounts(id,name,enabled,status,payload) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,enabled=excluded.enabled,status=excluded.status,payload=excluded.payload", account.ID, account.Name, account.Enabled, account.Status, payload)
	return err
}

func (s *Store) DeleteAccount(id string) error {
	result, err := s.db.Exec("DELETE FROM accounts WHERE id=?", id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// sanitizeLogValue 去除凭据与图片编码，避免日志扩大数据库或泄露登录信息。
func sanitizeLogValue(value any, depth int) any {
	if depth > 16 {
		return "[内容过深已省略]"
	}
	switch v := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			lower := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			if strings.Contains(lower, "token") || strings.Contains(lower, "cookie") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || lower == "apikey" || lower == "key" || lower == "nonce" || lower == "arpsession" || lower == "b64json" || lower == "base64" || lower == "data" {
				result[key] = "[已省略]"
				continue
			}
			result[key] = sanitizeLogValue(item, depth+1)
		}
		return result
	case []any:
		if len(v) > 100 {
			v = v[:100]
		}
		for i := range v {
			v[i] = sanitizeLogValue(v[i], depth+1)
		}
		return v
	case string:
		if strings.HasPrefix(v, "data:") {
			return "[图片数据已省略]"
		}
		if strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") {
			return sanitizeLoggedURL(v)
		}
		if len(v) > 32768 {
			return v[:32768]
		}
		return v
	default:
		return value
	}
}

func sanitizeLoggedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	query := u.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "signature") || strings.HasPrefix(lower, "x-amz-") || strings.Contains(lower, "credential") || strings.Contains(lower, "auth") {
			query.Del(key)
		}
	}
	u.RawQuery = query.Encode()
	u.Fragment = ""
	return u.String()
}

func (s *Store) SaveLog(log RequestLog) error {
	if log.ID == "" {
		log.ID = newID()
	}
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now().UTC()
	}
	if len(log.Prompt) > 32768 {
		log.Prompt = log.Prompt[:32768]
	}
	if len(log.Error) > 8192 {
		log.Error = log.Error[:8192]
	}
	if len(log.Parameters) > 262144 {
		log.Parameters = json.RawMessage(`{"note":"参数过长已省略"}`)
	} else if len(log.Parameters) > 0 {
		var value any
		if err := json.Unmarshal(log.Parameters, &value); err != nil {
			log.Parameters = json.RawMessage(`{}`)
		} else {
			log.Parameters, _ = json.Marshal(sanitizeLogValue(value, 0))
			if len(log.Parameters) > 65536 {
				log.Parameters = json.RawMessage(`{"note":"参数过长已省略"}`)
			}
		}
	}
	log.Upstream = append([]UpstreamRecord(nil), log.Upstream...)
	for i := range log.Upstream {
		log.Upstream[i].URL = sanitizeLoggedURL(log.Upstream[i].URL)
	}
	payload, err := json.Marshal(log)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO logs(id,created_at,status,model,prompt,account_name,credits,payload) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,model=excluded.model,prompt=excluded.prompt,account_name=excluded.account_name,credits=excluded.credits,payload=excluded.payload", log.ID, log.CreatedAt.UnixMilli(), log.Status, log.Model, log.Prompt, log.AccountName, log.Credits, payload)
	if err == nil && log.Status != "pending" && log.Status != "running" {
		// 完成或失败的请求只保留短取图窗口，防止长租约削弱容量清理。
		s.imageMu.Lock()
		deadline := time.Now().Add(10 * time.Second)
		for _, images := range [][]ImageRecord{log.Images, log.References} {
			for _, image := range images {
				if _, exists := s.imageLeases[image.ID]; exists {
					s.imageLeases[image.ID] = deadline
				}
			}
		}
		s.imageMu.Unlock()
	}
	return err
}

func (s *Store) Logs(page, limit int, status, query string) ([]RequestLog, int, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if len(query) > 512 {
		query = query[:512]
	}
	where := " WHERE 1=1"
	args := []any{}
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	if query != "" {
		where += " AND (prompt LIKE ? OR model LIKE ? OR account_name LIKE ? OR id LIKE ?)"
		for i := 0; i < 4; i++ {
			args = append(args, "%"+query+"%")
		}
	}
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM logs"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, (page-1)*limit)
	rows, err := s.db.Query("SELECT payload FROM logs"+where+" ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, err
	}
	items := make([]RequestLog, 0, limit)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return nil, 0, err
		}
		var item RequestLog
		if err := json.Unmarshal(payload, &item); err != nil {
			rows.Close()
			return nil, 0, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	// 关闭分页游标后再读取图片，避免单连接被游标占用造成互相等待。
	for i := range items {
		s.hydrateLogImages(&items[i])
	}
	return items, total, nil
}

func (s *Store) Log(id string) (RequestLog, error) {
	var payload []byte
	if err := s.db.QueryRow("SELECT payload FROM logs WHERE id=?", id).Scan(&payload); err != nil {
		return RequestLog{}, err
	}
	var item RequestLog
	if err := json.Unmarshal(payload, &item); err != nil {
		return RequestLog{}, err
	}
	s.hydrateLogImages(&item)
	return item, nil
}

func (s *Store) hydrateLogImages(log *RequestLog) {
	for _, images := range [][]ImageRecord{log.Images, log.References} {
		for i := range images {
			current, err := s.Image(images[i].ID)
			if err != nil || current.Deleted {
				images[i].Deleted = true
				images[i].URL = ""
			} else {
				images[i] = current
			}
		}
	}
}

func (s *Store) SaveImage(reader io.Reader, mimeType, kind string) (ImageRecord, error) {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return ImageRecord{}, errors.New("图片格式无效")
	}
	extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif", "image/avif": ".avif"}[mediaType]
	if extension == "" {
		return ImageRecord{}, errors.New("仅支持 PNG、JPEG、WebP、GIF、AVIF 图片")
	}
	id := newID()
	path := filepath.Join(s.dataDir, "images", id+extension)
	partial := path + ".part"
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ImageRecord{}, err
	}
	defer os.Remove(partial)
	// 额外读取一个字节确认超限，复制缓冲固定，不随图片大小增长。
	written, copyErr := io.CopyBuffer(file, io.LimitReader(reader, maxImageBytes+1), make([]byte, 32<<10))
	var syncErr error
	if copyErr == nil && written > 0 && written <= maxImageBytes {
		syncErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return ImageRecord{}, copyErr
	}
	if closeErr != nil {
		return ImageRecord{}, closeErr
	}
	if syncErr != nil {
		return ImageRecord{}, syncErr
	}
	if written == 0 || written > maxImageBytes {
		return ImageRecord{}, errors.New("图片为空或超过 64 MB")
	}
	record := ImageRecord{ID: id, Path: path, URL: "/images/" + id, MIME: mediaType, Bytes: written, Kind: kind, CreatedAt: time.Now().UTC()}
	if source, err := os.Open(partial); err == nil {
		config, _, decodeErr := image.DecodeConfig(source)
		source.Close()
		if decodeErr == nil {
			record.Width, record.Height = config.Width, config.Height
		}
	}
	// 网络读取不占用图片锁；发布文件、索引与租约时才互斥，避免慢下载阻塞其他任务。
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	if err := os.Rename(partial, path); err != nil {
		return ImageRecord{}, err
	}
	_, err = s.db.Exec("INSERT INTO images(id,path,mime,bytes,width,height,kind,created_at,deleted) VALUES(?,?,?,?,?,?,?,?,0)", record.ID, record.Path, record.MIME, record.Bytes, record.Width, record.Height, record.Kind, record.CreatedAt.UnixMilli())
	if err != nil {
		os.Remove(path)
		return ImageRecord{}, err
	}
	// 新图片可能仍等待同批结果，保护时间随生成超时设置而非固定一小时。
	leaseSeconds := s.Settings().RequestTimeoutSeconds
	if leaseSeconds < 30 || leaseSeconds > 3600 {
		leaseSeconds = 300
	}
	s.imageLeases[record.ID] = time.Now().Add(time.Duration(leaseSeconds) * time.Second)
	return record, nil
}

func (s *Store) Image(id string) (ImageRecord, error) {
	var image ImageRecord
	var created int64
	err := s.db.QueryRow("SELECT id,path,mime,bytes,width,height,kind,created_at,deleted FROM images WHERE id=?", id).Scan(&image.ID, &image.Path, &image.MIME, &image.Bytes, &image.Width, &image.Height, &image.Kind, &created, &image.Deleted)
	if err != nil {
		return ImageRecord{}, err
	}
	image.CreatedAt = time.UnixMilli(created).UTC()
	if !image.Deleted {
		image.URL = "/images/" + image.ID
	}
	return image, nil
}

// PinImages 保护在途请求使用的参考图，返回释放函数供请求结束时调用。
func (s *Store) PinImages(images []ImageRecord) func() {
	s.imageMu.Lock()
	for _, image := range images {
		s.imagePins[image.ID]++
	}
	s.imageMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.imageMu.Lock()
			defer s.imageMu.Unlock()
			for _, image := range images {
				s.imagePins[image.ID]--
				if s.imagePins[image.ID] <= 0 {
					delete(s.imagePins, image.ID)
				}
			}
		})
	}
}

func (s *Store) Cleanup(ctx context.Context) error {
	settings := s.Settings()
	s.imageMu.Lock()
	defer s.imageMu.Unlock()
	var totalBytes int64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(bytes),0) FROM images WHERE deleted=0").Scan(&totalBytes); err != nil {
		return err
	}
	now := time.Now()
	for id, deadline := range s.imageLeases {
		if !now.Before(deadline) {
			delete(s.imageLeases, id)
		}
	}
	expired := int64(0)
	if settings.RetentionHours > 0 {
		expired = now.Add(-time.Duration(settings.RetentionHours) * time.Hour).UnixMilli()
	}
	maxBytes := settings.MaxStorageMB * 1024 * 1024
	lastCreated, lastID := int64(-1), ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.db.QueryContext(ctx, "SELECT id,path,bytes,created_at FROM images WHERE deleted=0 AND (created_at>? OR (created_at=? AND id>?)) ORDER BY created_at,id LIMIT 128", lastCreated, lastCreated, lastID)
		if err != nil {
			return err
		}
		type candidate struct {
			id, path       string
			bytes, created int64
		}
		batch := make([]candidate, 0, 128)
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.id, &item.path, &item.bytes, &item.created); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, item := range batch {
			lastCreated, lastID = item.created, item.id
			if s.imagePins[item.id] > 0 || now.Before(s.imageLeases[item.id]) || !((expired > 0 && item.created < expired) || (maxBytes > 0 && totalBytes > maxBytes)) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Remove(item.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if _, err := s.db.ExecContext(ctx, "UPDATE images SET deleted=1 WHERE id=?", item.id); err != nil {
				return err
			}
			totalBytes -= item.bytes
		}
	}
	if settings.LogRetentionDays > 0 {
		threshold := now.AddDate(0, 0, -settings.LogRetentionDays).UnixMilli()
		// 分批删除旧日志，使清理额外内存与日志总量无关。
		for {
			result, err := s.db.ExecContext(ctx, "DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE created_at<? LIMIT 500)", threshold)
			if err != nil {
				return err
			}
			count, _ := result.RowsAffected()
			if count < 500 {
				break
			}
		}
		// 旧图片的删除状态已包含在日志中，移除过期索引避免元数据无限增长。
		for {
			result, err := s.db.ExecContext(ctx, "DELETE FROM images WHERE id IN (SELECT id FROM images WHERE deleted=1 AND created_at<? LIMIT 500)", threshold)
			if err != nil {
				return err
			}
			count, _ := result.RowsAffected()
			if count < 500 {
				break
			}
		}
	}
	return nil
}

func (s *Store) Stats() (map[string]any, error) {
	var total, success, failed, today, accounts, enabled, images int64
	var bytes int64
	var credits sql.NullFloat64
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).UnixMilli()
	err := s.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(CASE WHEN status='success' THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN status IN ('failed','error') THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN created_at>=? THEN 1 ELSE 0 END),0),SUM(credits) FROM logs", start).Scan(&total, &success, &failed, &today, &credits)
	if err != nil {
		return nil, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(enabled),0) FROM accounts").Scan(&accounts, &enabled); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(bytes),0) FROM images WHERE deleted=0").Scan(&images, &bytes); err != nil {
		return nil, err
	}
	// 上游未返回积分时保留空值，不能将未知消耗显示为零。
	var creditsUsed any
	if credits.Valid {
		creditsUsed = credits.Float64
	}
	return map[string]any{"total_requests": total, "success_requests": success, "failed_requests": failed, "today_requests": today, "accounts": accounts, "enabled_accounts": enabled, "storage_bytes": bytes, "image_count": images, "credits_used": creditsUsed}, nil
}

func newID() string {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("生成随机标识失败: " + err.Error())
	}
	return hex.EncodeToString(bytes[:])
}
