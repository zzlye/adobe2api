package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 列表分页不加载大型接口正文，过滤、排序和图片信息仍与完整查询一致。
func TestLogSummariesKeepPaginationAndExcludeUpstreamAtDatabase(t *testing.T) {
	store := newStorageTestStore(t)
	image, err := store.SaveImage(bytes.NewReader(storageTestPNG(t)), "image/png", "generated")
	if err != nil {
		t.Fatal(err)
	}
	largeBody := `{"message":"diagnostic-marker-` + strings.Repeat("详细响应", 900) + `"}`
	for i := 0; i < 7; i++ {
		entry := RequestLog{ID: string(rune('a' + i)), CreatedAt: time.Now().Add(time.Duration(i) * time.Second), Status: "failed", Model: "香蕉", Prompt: "山间小屋", AccountName: "测试账号", Parameters: json.RawMessage(`{"aspect_ratio":"16:9","resolution":"4K"}`), Images: []ImageRecord{image}, References: []ImageRecord{image}, Upstream: []UpstreamRecord{{Method: "POST", URL: adobeUpstreamURL, Status: 408, ResponseBody: largeBody}}}
		if err := store.SaveLog(entry); err != nil {
			t.Fatal(err)
		}
	}
	full, total, err := store.Logs(2, 3, "failed", "山间")
	if err != nil || total != 7 || len(full) != 3 {
		t.Fatal("完整日志查询失败")
	}
	summaries, summaryTotal, err := store.LogSummaries(2, 3, "failed", "山间")
	if err != nil || summaryTotal != total || len(summaries) != len(full) {
		t.Fatal("摘要列表破坏了分页或搜索")
	}
	for i, summary := range summaries {
		if len(summary.Upstream) != 0 || len(full[i].Upstream) != 1 {
			t.Fatal("摘要仍加载完整接口正文，或原完整查询被削减")
		}
		expected := full[i]
		expected.Upstream = nil
		if !reflect.DeepEqual(summary, expected) {
			t.Fatal("摘要列表丢失了基本字段、参数或图片索引")
		}
	}
	// 直接读取投影长度，验证完整正文没有离开数据库查询层，而非在 Go 解码后再置空。
	var fullBytes, projectedBytes int
	if err := store.db.QueryRow("SELECT length(payload),length(json_remove(CAST(payload AS TEXT), '$.upstream')) FROM logs WHERE id='a'").Scan(&fullBytes, &projectedBytes); err != nil || projectedBytes*4 >= fullBytes {
		t.Fatal("数据库摘要投影没有显著缩小读取内容")
	}
	detail, err := store.Log("a")
	if err != nil || len(detail.Upstream) != 1 || !strings.Contains(detail.Upstream[0].ResponseBody, "diagnostic-marker") {
		t.Fatal("摘要查询破坏了原始详情记录")
	}
}

// 正常管理登录后的列表仅返回摘要，打开详情时仍可读取同一条完整诊断记录。
func TestAdminLogListIsSmallAndDetailStillComplete(t *testing.T) {
	app, handler, cookie, _ := cookieAdminTestApp(t)
	entry := RequestLog{ID: "summary-log-fixture", CreatedAt: time.Now(), Status: "failed", Model: "香蕉", Prompt: "诊断记录", Upstream: []UpstreamRecord{{Method: "POST", URL: adobeUpstreamURL, Status: 408, ResponseHeaders: map[string]string{"Content-Type": "application/json"}, ResponseBody: `{"message":"detail-marker-` + strings.Repeat("响应内容", 1000) + `"}`}}}
	if err := app.store.SaveLog(entry); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := app.store.db.QueryRow("SELECT payload FROM logs WHERE id=?", entry.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	list := adminTestRequest(handler, http.MethodGet, "/api/admin/logs?limit=20", "", cookie, "")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "detail-marker") || !strings.Contains(list.Body.String(), "summary-log-fixture") || list.Body.Len() >= 2048 {
		t.Fatal("管理日志列表仍返回完整诊断正文")
	}
	detail := adminTestRequest(handler, http.MethodGet, "/api/admin/logs/"+entry.ID, "", cookie, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "detail-marker") || !strings.Contains(detail.Body.String(), "response_headers") {
		t.Fatal("日志详情没有保留接口响应")
	}
	var after []byte
	if err := app.store.db.QueryRow("SELECT payload FROM logs WHERE id=?", entry.ID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatal("列表或详情查询修改了数据库原始日志")
	}
}
