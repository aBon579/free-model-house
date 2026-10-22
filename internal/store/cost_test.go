package store

import (
	"path/filepath"
	"testing"

	"github.com/aBon579/free-model-house/internal/lane"
)

// TestMigrateAddsCostColumn 验证**加列**在老数据库上能跑。
//
// ⚠️ `CREATE TABLE IF NOT EXISTS` **不会**给已存在的表加新列，
//
//	所以必须显式 `ALTER TABLE`。
//
// ⚠️ 而且加列**不幂等** —— 重复加会报
//
//	`duplicate column name: cost_usd`。
//	迁移必须把那个错误当成功。
func TestMigrateAddsCostColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fmh.db")

	// 第一次开库 —— 建表 + 加列。
	db, err := Open(path)
	if err != nil {
		t.Fatalf("第一次开库失败: %v", err)
	}
	if err := db.PutUsage(lane.UsageRecord{
		At: 1000, Day: "2026-10-19", Lane: "openrouter",
		Model: "openrouter/m", ActualModel: "openrouter/m",
		Requests: 1, Turns: 1, Attempts: 1,
		InputTokens: 10, OutputTokens: 5, CostUSD: 0.0000043,
	}); err != nil {
		t.Fatalf("写用量失败: %v", err)
	}
	db.Close()

	// 第二次开库 —— 加列要**幂等**（不能报错）。
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("第二次开库失败（加列不幂等）: %v", err)
	}
	defer db2.Close()

	// 数据还在，费用也对。
	rows, err := db2.ModelUsageAll("2000-01-01")
	if err != nil {
		t.Fatalf("读汇总失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("该有 1 行，实际 %d", len(rows))
	}
	if rows[0].CostUSD <= 0 {
		t.Fatalf("费用该保留，实际 %v", rows[0].CostUSD)
	}
	// ⚠️ 钱不能只按整数记 —— 0.0000043 会被截成 0。
	if rows[0].CostUSD == 0 {
		t.Fatal("小额费用被截断了 —— 列类型该是 REAL")
	}
}

// TestCostAccumulatesAcrossDays 验证**按天聚合**会累加费用。
func TestCostAccumulatesAcrossDays(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "fmh.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer db.Close()

	for i := 0; i < 3; i++ {
		if err := db.PutUsage(lane.UsageRecord{
			At: 1000, Day: "2026-10-19", Lane: "openrouter",
			Model: "openrouter/m", ActualModel: "openrouter/m",
			Requests: 1, Turns: 1, Attempts: 1,
			CostUSD: 0.01,
		}); err != nil {
			t.Fatalf("写用量失败: %v", err)
		}
	}
	daily, err := db.DailyUsageRange("2026-10-19", "2026-10-19")
	if err != nil {
		t.Fatalf("读按天聚合失败: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("该有 1 天，实际 %d", len(daily))
	}
	// ⚠️ 浮点累加 —— 0.01×3 = 0.03，允许一点点误差。
	if daily[0].CostUSD < 0.029 || daily[0].CostUSD > 0.031 {
		t.Fatalf("费用该是 0.03，实际 %v", daily[0].CostUSD)
	}
}
