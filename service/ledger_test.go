// 架构B 阶段1(BE②)钱核心纯函数单测:周期桶标签 / manage 日志金额无损还原 / 多重集证据判定。
// 判定函数是对账环"绝不多退/少退"的裁决核心,失败分支逐一覆盖(34 §5.1-⑤)。
package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/nexusapi-platform/enterprise/repo"
)

// ── subscriptionBucket:组织时区自然边界(裁定 33-§12-11 冻结格式) ──

func TestSubscriptionBucket_FormatsAndTimezoneBoundary(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	// UTC 2026-07-06 17:30 = 上海 2026-07-07 01:30:同一时刻,时区不同,桶必须不同(自然边界按组织时区)。
	at := time.Date(2026, 7, 6, 17, 30, 0, 0, time.UTC)
	cases := []struct {
		period string
		loc    *time.Location
		want   string
	}{
		{"daily", time.UTC, "2026-07-06"},
		{"daily", sh, "2026-07-07"},
		{"monthly", time.UTC, "2026-07"},
		{"weekly", sh, "2026-W28"}, // 2026-07-07 是周二,ISO 第 28 周
	}
	for _, c := range cases {
		got, ok := subscriptionBucket(c.period, at, c.loc)
		if !ok || got != c.want {
			t.Errorf("subscriptionBucket(%s,%s) = %q,%v; want %q", c.period, c.loc, got, ok, c.want)
		}
	}
	// 月末跨界:UTC 7/31 23:00 = 上海 8/1 07:00 → monthly 桶分属两月。
	eom := time.Date(2026, 7, 31, 23, 0, 0, 0, time.UTC)
	if b, _ := subscriptionBucket("monthly", eom, sh); b != "2026-08" {
		t.Errorf("上海月末跨界: got %q want 2026-08", b)
	}
	// ISO 周年边界:2027-01-01(周五)属 2026 年最后一周?→ ISOWeek 自洽即可,断言格式合法且稳定。
	ny := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	b1, _ := subscriptionBucket("weekly", ny, time.UTC)
	y, w := ny.ISOWeek()
	if b1 != fmt.Sprintf("%d-W%02d", y, w) {
		t.Errorf("ISO 周年边界格式漂移: %q", b1)
	}
	// 非法周期拒。
	if _, ok := subscriptionBucket("hourly", at, time.UTC); ok {
		t.Error("非法周期应返回 ok=false")
	}
}

// ── parseManageLog:rc.4 manage 日志金额无损还原 ──

func TestParseManageLog(t *testing.T) {
	const qpu = int64(500000)
	cases := []struct {
		name    string
		content string
		wantRaw int64
		wantOp  int
		wantCls int
	}{
		{"USD增加", "管理员增加用户额度 ＄4.000000 额度", 2_000_000, repo.OpCredit, manageLogQuotaOp},
		{"USD减少", "管理员减少用户额度 ＄4.000000 额度", 2_000_000, repo.OpDebit, manageLogQuotaOp},
		{"USD最小raw", "管理员增加用户额度 ＄0.000002 额度", 1, repo.OpCredit, manageLogQuotaOp}, // raw=1 无损
		{"USD_int32上限", "管理员增加用户额度 ＄4294.967294 额度", 2_147_483_647, repo.OpCredit, manageLogQuotaOp},
		{"Tokens直读", "管理员减少用户额度 2000000 点额度", 2_000_000, repo.OpDebit, manageLogQuotaOp},
		{"override违规", "管理员覆盖用户额度从 ＄1.000000 额度 为 ＄2.000000 额度", 0, 0, manageLogOverride},
		{"CNY不可逆", "管理员增加用户额度 ¥28.000000 额度", 0, 0, manageLogUnparseable},
		{"自定义币不可逆", "管理员增加用户额度 ¤4.000000 额度", 0, 0, manageLogUnparseable},
		{"非额度manage日志", "管理员将用户状态修改为禁用", 0, 0, manageLogIrrelevant},
		{"金额残缺", "管理员增加用户额度 ＄abc 额度", 0, 0, manageLogUnparseable},
		{"负数拒", "管理员增加用户额度 -100 点额度", 0, 0, manageLogUnparseable},
	}
	for _, c := range cases {
		raw, op, cls := parseManageLog(c.content, qpu)
		if cls != c.wantCls || (cls == manageLogQuotaOp && (raw != c.wantRaw || op != c.wantOp)) {
			t.Errorf("%s: parseManageLog=%d,%d,%d; want %d,%d,%d", c.name, raw, op, cls, c.wantRaw, c.wantOp, c.wantCls)
		}
	}
}

// TestParseManageLog_RoundTripExact USD 显示制式对 QPU=500000 全程无损(raw→＄%.6f→raw 恒等)。
func TestParseManageLog_RoundTripExact(t *testing.T) {
	const qpu = int64(500000)
	for _, raw := range []int64{1, 2, 3, 499999, 500000, 123456789, 2_000_000_000, 2_147_483_647} {
		content := fmt.Sprintf("管理员增加用户额度 ＄%.6f 额度", float64(raw)/float64(qpu))
		got, op, cls := parseManageLog(content, qpu)
		if cls != manageLogQuotaOp || op != repo.OpCredit || got != raw {
			t.Errorf("round-trip raw=%d: got %d (cls=%d)", raw, got, cls)
		}
	}
}

// ── judgeManageOps:多重集证据判定(对账环裁决核心) ──

func TestJudgeManageOps(t *testing.T) {
	const amount = int64(2_000_000)
	cases := []struct {
		name        string
		observed    []int64 // 日志出现的同方向操作
		journaled   []int64 // 账本已确认的同方向操作
		unparseable bool
		want        opVerdict
	}{
		{"空窗口=未落地", nil, nil, false, opNotLanded},
		{"仅本笔=落地", []int64{amount}, nil, false, opLanded},
		{"兄弟操作全对账,残差空=未落地", []int64{500, 800}, []int64{800, 500}, false, opNotLanded},
		{"兄弟操作对账后剩本笔=落地", []int64{500, amount}, []int64{500}, false, opLanded},
		{"同额两笔,账本记一笔,残差一笔=落地", []int64{amount, amount}, []int64{amount}, false, opLanded},
		{"残差有但非本笔金额=unknown", []int64{500, 999}, []int64{500}, false, opUnknown},
		{"负残差(账本有日志无=窗口裁边/清日志)=unknown", []int64{}, []int64{500}, false, opUnknown},
		{"证据不完整=unknown(即使显式匹配)", []int64{amount}, nil, true, opUnknown},
		{"混合:负残差优先于命中=unknown", []int64{amount}, []int64{500}, false, opUnknown},
	}
	for _, c := range cases {
		if got := judgeManageOps(c.observed, c.journaled, amount, c.unparseable); got != c.want {
			t.Errorf("%s: judgeManageOps=%v want %v", c.name, got, c.want)
		}
	}
}

// TestJudgeManageOps_NeverLandsWithoutExactAmount 对抗性:任何"凑不出本笔金额"的残差组合都不得判 landed
// (判 landed 意味着对账环不会补发;误判 landed=少发,误判 notLanded=可能多发——两个方向都在此闸住)。
func TestJudgeManageOps_NeverLandsWithoutExactAmount(t *testing.T) {
	const amount = int64(777)
	combos := [][]int64{{}, {1}, {776}, {778}, {776, 1}, {amount - 1, 1}}
	for _, obs := range combos {
		if got := judgeManageOps(obs, nil, amount, false); got == opLanded {
			t.Errorf("observed=%v 不含本笔金额却判 landed", obs)
		}
	}
	// notLanded 只允许在残差完全为空时出现。
	if got := judgeManageOps([]int64{776}, nil, amount, false); got != opUnknown {
		t.Errorf("非空残差不含本笔应 unknown,got %v", got)
	}
}

// ── ledgerRuntime:桶去重 ──

func TestLedgerRuntime_BucketDedup(t *testing.T) {
	var lr ledgerRuntime
	if lr.seenBucket(1) != "" {
		t.Fatal("零值应无记录")
	}
	lr.markBucket(1, "2026-07-07")
	if lr.seenBucket(1) != "2026-07-07" {
		t.Fatal("markBucket 未生效")
	}
	lr.markBucket(1, "2026-07-08") // 桶翻转覆盖
	if lr.seenBucket(1) != "2026-07-08" {
		t.Fatal("桶翻转未覆盖")
	}
}
