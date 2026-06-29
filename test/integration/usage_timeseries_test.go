// 时间序列/key维度聚合测试(v2 M2-1):验证 AggregateUsageByTime 按 UTC+8 自然日/周/月边界正确分桶,
// 以及 AggregateUsageLedgerByKey 按平台 key_id 聚合。重点:构造两条 UTC 同一天、但 UTC+8 跨天的 ledger 行,
// 断言被分到相邻两天(证明 +8h 边界逻辑)。纯读 ledger,不需 new-api,隔离库 nexus_ts。
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/nexusapi-platform/enterprise/repo"
)

func TestIntegration_UsageTimeSeries(t *testing.T) {
	dsn := os.Getenv("NEXUS_IT_DSN")
	newapiSQLDSN := os.Getenv("NEXUS_IT_NEWAPI_SQL_DSN")
	if dsn == "" || newapiSQLDSN == "" {
		t.Skip("跳过:需 NEXUS_IT_DSN / NEXUS_IT_NEWAPI_SQL_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ensureDatabase(t, newapiSQLDSN, "nexus_ts") // 用始终存在的 newapi 库建隔离库
	if !strings.Contains(dsn, "/nexus?") {
		t.Fatalf("NEXUS_IT_DSN 不含 /nexus?: %q", dsn)
	}
	store, err := repo.Open(ctx, strings.Replace(dsn, "/nexus?", "/nexus_ts?", 1))
	if err != nil {
		t.Fatalf("连 nexus_ts 失败: %v", err)
	}
	defer store.Close()
	defer func() { _, _ = store.DB().Exec("DROP DATABASE IF EXISTS nexus_ts") }()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	const orgID, userID = int64(1), int64(1000)
	ins := func(tb time.Time, keyID, quota int64) {
		if _, err := store.DB().ExecContext(ctx,
			`INSERT INTO usage_ledger (org_id, member_id, newapi_user_id, key_id, model_name, time_bucket, consumed_quota, log_max_ts)
			 VALUES (?, 1, ?, ?, 'm', ?, ?, ?)`, orgID, userID, keyID, tb, quota, tb); err != nil {
			t.Fatalf("插 ledger 失败: %v", err)
		}
	}
	// A、B 是 UTC 同一天(06-01),但 UTC+8 跨天:A=23:00(06-01)、B=次日00:00(06-02)。C 落 06-03。
	utc := func(y int, mo time.Month, d, h int) time.Time { return time.Date(y, mo, d, h, 0, 0, 0, time.UTC) }
	ins(utc(2026, 6, 1, 15), 1, 100) // UTC 06-01 15:00 → UTC+8 06-01 23:00 → day 2026-06-01
	ins(utc(2026, 6, 1, 16), 1, 200) // UTC 06-01 16:00 → UTC+8 06-02 00:00 → day 2026-06-02
	ins(utc(2026, 6, 2, 16), 2, 50)  // UTC 06-02 16:00 → UTC+8 06-03 00:00 → day 2026-06-03(key_id=2)

	since := utc(2026, 5, 1, 0)

	// 1) 按天:应 3 个点,且 A、B 分到相邻两天(UTC+8 边界生效),升序。
	day, err := store.AggregateUsageByTime(ctx, orgID, since, "day", nil, nil)
	if err != nil {
		t.Fatalf("按天聚合失败: %v", err)
	}
	wantDay := []repo.UsageTimePoint{
		{Period: "2026-06-01", Consumed: 100},
		{Period: "2026-06-02", Consumed: 200},
		{Period: "2026-06-03", Consumed: 50},
	}
	if len(day) != 3 {
		t.Fatalf("按天应 3 点,实得 %d: %+v", len(day), day)
	}
	for i, w := range wantDay {
		if day[i] != w {
			t.Fatalf("按天[%d]=%+v 应=%+v(UTC+8 边界/升序错)", i, day[i], w)
		}
	}

	// 2) 按月:全在 UTC+8 2026-06 → 1 点 350。
	mon, err := store.AggregateUsageByTime(ctx, orgID, since, "month", nil, nil)
	if err != nil {
		t.Fatalf("按月聚合失败: %v", err)
	}
	if len(mon) != 1 || mon[0].Period != "2026-06" || mon[0].Consumed != 350 {
		t.Fatalf("按月应 [{2026-06,350}],实得 %+v", mon)
	}

	// 3) keyFilter=1:只 A、B(key_id=1),C(key_id=2)排除。
	k1day, err := store.AggregateUsageByTime(ctx, orgID, since, "day", nil, ptr(int64(1)))
	if err != nil {
		t.Fatalf("按天+keyFilter 失败: %v", err)
	}
	if len(k1day) != 2 || k1day[0].Consumed != 100 || k1day[1].Consumed != 200 {
		t.Fatalf("keyFilter=1 应只 A/B 两天(100/200),实得 %+v", k1day)
	}

	// 4) 按 key_id 聚合:{1:300, 2:50}。
	byKey, err := store.AggregateUsageLedgerByKey(ctx, orgID, since, nil)
	if err != nil {
		t.Fatalf("按 key 聚合失败: %v", err)
	}
	if byKey[1] != 300 || byKey[2] != 50 || len(byKey) != 2 {
		t.Fatalf("按 key 应 {1:300,2:50},实得 %+v", byKey)
	}

	t.Logf("M2-1 时间序列 ok: UTC+8 日边界(A/B 同UTC日跨UTC+8两天)正确;月聚合 350;keyFilter/byKey 正确")
}

func ptr(v int64) *int64 { return &v }
