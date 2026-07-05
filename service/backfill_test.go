package service

import "testing"

// TestUnit_belongsToBackfill 锁死回填/forward 的 (ts,id) 词典序切分(24-§3 命根子):
// 与 forward"只处理 > B"必须精确互补 —— 边界错一条,ledger 无按行去重就是真多算/漏算。
func TestUnit_belongsToBackfill(t *testing.T) {
	const bTS, bID = int64(1000), int64(50)
	cases := []struct {
		name   string
		ts, id int64
		want   bool
	}{
		{"更早 ts 属回填", 999, 9999, true},        // ts<B_ts,id 再大也属回填
		{"同 ts 且 id==B_id 属回填(闭区间)", 1000, 50, true}, // 边界含
		{"同 ts 且 id<B_id 属回填", 1000, 49, true},
		{"同 ts 且 id>B_id 属 forward", 1000, 51, false}, // forward 侧
		{"更晚 ts 属 forward", 1001, 1, false},
		{"远古属回填", 1, 1, true},
	}
	for _, c := range cases {
		if got := belongsToBackfill(c.ts, c.id, bTS, bID); got != c.want {
			t.Errorf("%s: belongsToBackfill(%d,%d,%d,%d)=%v 期望 %v", c.name, c.ts, c.id, bTS, bID, got, c.want)
		}
	}

	// B_id=0(forward 首跑未处理任何日志):用纯 id 会漏,故必须 (ts,id) 词典序(24-§3.2)。
	// 同 B_ts 的任何真实 id(>0)都属 forward;更早 ts 仍属回填。
	if belongsToBackfill(1000, 1, 1000, 0) {
		t.Errorf("B_id=0 时同 B_ts 的 id=1 应属 forward(否则会与 forward 重叠双算)")
	}
	if !belongsToBackfill(999, 1, 1000, 0) {
		t.Errorf("B_id=0 时更早 ts 应属回填(否则整段历史被误判 forward 侧而漏)")
	}
}
