package newapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestReadConsumptionLogsByUsername_ExactFilter 锁定回填拉日志的两条契约(24-§7.2/§8):
//  1. 请求按 new-api username **精确等值**过滤(镜像 model/log.go:310 `logs.username = ?`,非 LIKE)——
//     ent_abc 绝不得误命中超串 ent_abcdef,否则会把别的组织的日志灌进本组织(防御性归属的第一道)。
//  2. URL 正确携带 &username=<urlencoded> 且我方 parseLogPage 解析 total/items 无误。
func TestReadConsumptionLogsByUsername_ExactFilter(t *testing.T) {
	type row struct {
		id       int64
		user     int
		username string
		ts       int64
		quota    int64
	}
	seed := []row{
		{11, 7, "ent_abc", 1000, 5},
		{12, 7, "ent_abc", 1001, 6},
		{99, 8, "ent_abcdef", 1002, 9}, // 超串:精确匹配下绝不能被 ent_abc 命中
	}

	var gotUsername, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotUsername, gotType = q.Get("username"), q.Get("type")
		var items []map[string]any
		for _, e := range seed {
			if e.username != gotUsername { // 精确等值,镜像 new-api model/log.go:310
				continue
			}
			items = append(items, map[string]any{
				"id": e.id, "user_id": e.user, "created_at": e.ts,
				"model_name": "gpt-x", "quota": e.quota, "token_id": int64(71),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"items": items, "total": len(items)},
		})
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL, AdminToken: "x", AdminUserID: 1, Timeout: 5 * time.Second}, nil)
	ctx := context.Background()

	// 拉 ent_abc:只应拿到它自己的两条,total=2;超串 ent_abcdef 的那条不得混入。
	got, total, err := a.ReadConsumptionLogsByUsername(ctx, "ent_abc", 0, 2000, 1, 100)
	if err != nil {
		t.Fatalf("ReadConsumptionLogsByUsername 失败: %v", err)
	}
	if gotUsername != "ent_abc" {
		t.Errorf("请求未精确携带 username:got=%q want=ent_abc", gotUsername)
	}
	if gotType != "2" {
		t.Errorf("回填只读消费日志,type 应为 2,got=%q", gotType)
	}
	if total != 2 || len(got) != 2 {
		t.Fatalf("精确过滤应得 2 条(不含超串 ent_abcdef):total=%d len=%d", total, len(got))
	}
	for _, e := range got {
		if e.ID == 99 {
			t.Errorf("超串组织的日志(id=99, ent_abcdef)被误拉进 ent_abc —— 精确匹配被破坏")
		}
	}
	var sum int64
	for _, e := range got {
		sum += e.Quota
	}
	if sum != 11 { // 5 + 6
		t.Errorf("解析 quota 有误:sum=%d want=11", sum)
	}
}
