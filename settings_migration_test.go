package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// 升级只关闭旧自动清理，账号、管理员和历史图片日志必须原样保留。
func TestSettingsMigrationPreservesHistoryAndRunsOnce(t *testing.T) {
	dir := storageTestDir(t)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	settings := store.Settings()
	settings.AdminUsername, settings.AdminPasswordHash = "原管理员", "原密码摘要"
	settings.APIKeys = []string{"原密钥abcdefghijklmnop"}
	settings.ProxyURL, settings.PublicURL = "http://127.0.0.1:7890", "https://images.example.test"
	settings.RefreshMinutes, settings.MaxConcurrent = 17, 3
	settings.RetentionHours, settings.MaxStorageMB, settings.LogRetentionDays = 168, 1024, 30
	account := Account{ID: "原账号", Cookie: "ims_sid=原登录状态", Name: "旧账号", Enabled: true}
	if err := store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	image, err := store.SaveImage(bytes.NewReader(storageTestPNG(t)), "image/png", "generated")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(-1, 0, 0)
	if _, err := store.db.Exec("UPDATE images SET created_at=?,bytes=? WHERE id=?", old.UnixMilli(), int64(2<<30), image.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveLog(RequestLog{ID: "旧请求", CreatedAt: old, Status: "failed", Images: []ImageRecord{image}}); err != nil {
		t.Fatal(err)
	}
	// 直接构造不带版本的旧加密设置，模拟已有服务升级，而不是当前保存接口。
	plain, err := json.Marshal(settingsEnvelope{Settings: settings, PasswordHash: settings.AdminPasswordHash})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := store.encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("INSERT INTO settings(id,payload) VALUES(1,?)", payload); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	expected := settings
	expected.RetentionHours, expected.MaxStorageMB, expected.LogRetentionDays = 0, 0, 0
	if got := store.Settings(); !reflect.DeepEqual(got, expected) {
		t.Fatal("迁移未保留原设置或没有关闭三项旧清理策略")
	}
	if err := store.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Log("旧请求"); err != nil {
		t.Fatal("升级后历史日志被清理")
	}
	if _, err := os.Stat(image.Path); err != nil {
		t.Fatal("升级后历史图片被清理")
	}
	accounts, err := store.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].Cookie != account.Cookie {
		t.Fatal("迁移损坏原账号")
	}
	expected.RetentionHours, expected.MaxStorageMB, expected.LogRetentionDays = 240, 2048, 90
	if err := store.SaveSettings(expected); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Settings(); !reflect.DeepEqual(got, expected) {
		t.Fatal("迁移重复执行并覆盖管理员后续设置")
	}
}

func TestFreshSettingsKeepLogsWithoutDisablingImageLimits(t *testing.T) {
	store := newStorageTestStore(t)
	settings := store.Settings()
	if settings.LogRetentionDays != 0 || settings.RetentionHours != 168 || settings.MaxStorageMB != 1024 {
		t.Fatal("新安装的日志保留或图片保存默认值错误")
	}
}
