package store

import (
	"testing"

	"github.com/aBon579/free-model-house/internal/lane"
)

// ⚠️ 这一组测试跑**内存库**，不是临时文件库。
//
//	原因：本机（Windows 虚拟盘）上 ncruces 的文件 VFS 写入不落盘 ——
//	`CREATE TABLE` 返回 nil 但表不存在（`no such table: main.usage`）。
//	已用最小程序验证：`:memory:` 完全正常，任何文件 DSN 都失败；
//	这与业务代码无关，是运行环境限制。
//
//	内存库能真实覆盖**迁移 DDL、logs 裁剪、usage.key_label 聚合**，
//	在 Linux/容器里同样可跑。

// openMemoryDB 建一个内存库并跑真实迁移。
func openMemoryDB(t *testing.T) *DB {
	t.Helper()
	d, err := OpenMemory()
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestMigrateCreatesLogsAndUsageKeyLabel 验证迁移后的表结构。
func TestMigrateCreatesLogsAndUsageKeyLabel(t *testing.T) {
	d := openMemoryDB(t)

	for _, table := range []string{"settings", "secrets", "sessions", "throttle", "probes", "usage", "usage_daily", "logs"} {
		var name string
		err := d.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("表 %s 不存在: %v", table, err)
		}
	}

	// key_label 是这次新增的列 —— 迁移必须把它建出来。
	rows, err := d.db.Query(`PRAGMA table_info(usage)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	has := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		has[name] = true
	}
	for _, col := range []string{"key_label", "key_id", "key_hint"} {
		if !has[col] {
			t.Fatalf("usage.%s 列缺失", col)
		}
	}
}

// TestLogsTrim 验证 5000 条上限的落库行为。
func TestLogsTrim(t *testing.T) {
	d := openMemoryDB(t)

	const total = 6000
	for i := 0; i < total; i++ {
		cat := "request"
		if i%3 == 0 {
			cat = "probe"
		}
		if err := d.AppendLog(int64(i), cat, "日志内容"); err != nil {
			t.Fatalf("第 %d 条写入失败: %v", i, err)
		}
	}
	if err := d.TrimLogs(5000); err != nil {
		t.Fatalf("裁剪失败: %v", err)
	}

	var kept int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM logs`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 5000 {
		t.Fatalf("裁剪后应有 5000 条，实际 %d", kept)
	}
	// 保留的应该是**最新的** 5000 条 —— 最老的 id 应大于 999。
	var minID int
	if err := d.db.QueryRow(`SELECT MIN(id) FROM logs`).Scan(&minID); err != nil {
		t.Fatal(err)
	}
	if minID <= 999 {
		t.Fatalf("保留了旧记录：min(id)=%d", minID)
	}

	// 类目筛选仍然正确。
	got, err := d.ListLogs([]string{"probe"}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Category != "probe" {
		t.Fatalf("按类目筛选异常: %+v", got)
	}
}

// TestUsageKeyLabelGrouping 验证 key_label 真的落库并按它分组。
func TestUsageKeyLabelGrouping(t *testing.T) {
	d := openMemoryDB(t)
	day := "2026-11-03"

	recs := []lane.UsageRecord{
		{At: 1000, Day: day, Lane: "zen", Model: "zen/m1", ActualModel: "zen/m1", KeyLabel: "主key", Requests: 1, Turns: 1},
		{At: 2000, Day: day, Lane: "zen", Model: "zen/m1", ActualModel: "zen/m1", KeyLabel: "主key", Requests: 1, Turns: 1},
		{At: 3000, Day: day, Lane: "zen", Model: "zen/m1", ActualModel: "zen/m1", KeyLabel: "NAS-agent", Requests: 1, Turns: 1},
		{At: 4000, Day: day, Lane: "zen", Model: "zen/m1", ActualModel: "zen/m1", KeyLabel: "", Requests: 1, Turns: 1},
	}
	for _, r := range recs {
		if err := d.PutUsage(r); err != nil {
			t.Fatalf("写用量失败: %v", err)
		}
	}

	rows, err := d.ModelUsageAll("2026-11-01")
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	byLabel := map[string]int{}
	for _, u := range rows {
		byLabel[u.KeyLabel] += u.Requests
	}
	if byLabel["主key"] != 2 {
		t.Fatalf("「主key」应聚合 2 次，实际 %d（全量 %+v）", byLabel["主key"], rows)
	}
	if byLabel["NAS-agent"] != 1 {
		t.Fatalf("「NAS-agent」应聚合 1 次，实际 %d", byLabel["NAS-agent"])
	}
	if byLabel[""] != 1 {
		t.Fatalf("空标签（老数据）应保留 1 条，实际 %d", byLabel[""])
	}
	if len(rows) != 3 {
		t.Fatalf("应按 (model,lane,actual,key_label) 分成 3 组，实际 %d", len(rows))
	}
}

// TestUsageKeyIDSeparatesSameLabelKeys 验证**同名/无名 Key 不会混成一行**。
//
// ⚠️ 这正是"用量页永远显示主key"的另一半原因：
//
//	如果只按 key_label 聚合，两把都叫「主 Key」的 Key（或两把都没命名）
//	会被合并，界面看起来就像"所有调用都记在同一把上"。
func TestUsageKeyIDSeparatesSameLabelKeys(t *testing.T) {
	d := openMemoryDB(t)
	day := "2026-11-03"

	recs := []lane.UsageRecord{
		// 两把都叫 "same"，但 id 不同 → 必须分开。
		{At: 1000, Day: day, Lane: "zen", Model: "zen/m1", KeyLabel: "same", KeyID: "id-a", KeyHint: "…AAAA", Requests: 1},
		{At: 2000, Day: day, Lane: "zen", Model: "zen/m1", KeyLabel: "same", KeyID: "id-b", KeyHint: "…BBBB", Requests: 1},
		// 两把都没命名 → 靠 key_id / hint 区分。
		{At: 3000, Day: day, Lane: "zen", Model: "zen/m1", KeyLabel: "", KeyID: "id-c", KeyHint: "…CCCC", Requests: 1},
		{At: 4000, Day: day, Lane: "zen", Model: "zen/m1", KeyLabel: "", KeyID: "id-d", KeyHint: "…DDDD", Requests: 1},
		// 同一把 Key 的两条记录 → 合并。
		{At: 5000, Day: day, Lane: "zen", Model: "zen/m1", KeyLabel: "same", KeyID: "id-a", KeyHint: "…AAAA", Requests: 1},
	}
	for _, r := range recs {
		if err := d.PutUsage(r); err != nil {
			t.Fatalf("写用量失败: %v", err)
		}
	}

	rows, err := d.ModelUsageAll("2026-11-01")
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("应按 key_id 分成 4 组，实际 %d（%+v）", len(rows), rows)
	}
	seen := map[string]ModelUsage{}
	for _, u := range rows {
		seen[u.KeyID] = u
	}
	if seen["id-a"].Requests != 2 {
		t.Fatalf("同一把 Key 应聚合 2 次，实际 %d", seen["id-a"].Requests)
	}
	if seen["id-a"].KeyHint != "…AAAA" || seen["id-b"].KeyHint != "…BBBB" {
		t.Fatalf("指纹没有跟随各自的 Key: %+v", rows)
	}
	if seen["id-c"].KeyLabel != "" || seen["id-d"].KeyLabel != "" {
		t.Fatalf("无名 Key 的标签应为空: %+v", rows)
	}
	if seen["id-c"].KeyHint == seen["id-d"].KeyHint {
		t.Fatal("两把无名 Key 的指纹不该相同")
	}
}
