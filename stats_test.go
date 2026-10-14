package main

import (
	"testing"
	"time"
)

// 未返回积分与明确消耗零积分含义不同，概览不能将二者合并。
func TestStatsKeepsUnknownCreditsDistinctFromZero(t *testing.T) {
	store := newStorageTestStore(t)
	entry := RequestLog{ID: newID(), CreatedAt: time.Now(), Status: "success"}
	if err := store.SaveLog(entry); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats()
	if err != nil || stats["credits_used"] != nil {
		t.Fatalf("未知积分被转换成数字: %v %v", stats["credits_used"], err)
	}
	zero := 0.0
	entry.Credits = &zero
	if err := store.SaveLog(entry); err != nil {
		t.Fatal(err)
	}
	stats, err = store.Stats()
	if err != nil || stats["credits_used"] != zero {
		t.Fatalf("明确的零积分未保留: %v %v", stats["credits_used"], err)
	}
}
