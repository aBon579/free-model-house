package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// ⚠️ 纯 Go 的 SQLite 驱动 —— 能进 scratch 镜像。
	// mattn/go-sqlite3 需要 CGO，会破坏静态编译。
	// ⚠️ 用 ncruces（SQLite 编译成 WASM）而不是 modernc.org/sqlite：
	//   modernc 是把 C 转译成 Go，**依赖树 292 MB / 5000+ 文件** ——
	//   构建时要下载 360 MB，在 NAS 上能卡十几分钟。
	//   ncruces 只有 **20 MB**，性能实测持平。
	// ⚠️ **不要**再 import `.../embed` —— 那个包已废弃，
	//   import 它会在启动时打一行警告到 stderr。
	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/aBon579/free-model-house/internal/lane"
)

// 本文件是**持久化**层。
// 用 SQLite（纯 Go 的 ncruces/go-sqlite3 —— 能进 scratch 镜像，
// 而 mattn/go-sqlite3 需要 CGO，不行）。
// 存什么：
//	settings   键值设置（本机 Key、LAN Key、开关…）
//	secrets    密封的上游凭据
//	sessions   seed → 上游 session id 的稳定映射
//	throttle   限流账本
//	probes     探测结论
//	usage      用量明细与按天聚合
// ⚠️ **只存元数据，不存提示词与回答**（隐私）。

// DB 是持久化句柄。
type DB struct {
	db *sql.DB
	// mu 保护迁移。
	mu   sync.Mutex
	path string
}

// Open 打开（或创建）数据库。
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "fmh.db")
	// ⚠️ WAL 模式：读写并发更好，NAS 上也更不容易卡。
	// ⚠️ DSN 有两处跟 modernc 不同：
	//   ① 必须带 `file:` 前缀 —— ncruces 用 URI 模式解析查询参数。
	//   ② 驱动名是 "sqlite3"（modernc 叫 "sqlite"）。
	//     ncruces 可以用 -ldflags 改名，但没必要。
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 不适合高并发写 —— 限制连接数。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	d := &DB{db: db, path: path}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Close 关闭数据库。
func (d *DB) Close() error { return d.db.Close() }

// Path 返回数据库文件路径。
func (d *DB) Path() string { return d.path }

// migrate 建表。
// ⚠️ 用 `CREATE TABLE IF NOT EXISTS` —— 迁移是**幂等**的，
// 所以启动时无条件跑一遍就行，不需要版本号。
func (d *DB) migrate() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	stmts := []string{
		// 键值设置。
		`CREATE TABLE IF NOT EXISTS settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		// 密封的上游凭据。
		// ⚠️ 存的是**密文**，明文只在内存里存在。
		`CREATE TABLE IF NOT EXISTS secrets (
			name       TEXT PRIMARY KEY,
			sealed     TEXT NOT NULL,
			hint       TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL
		)`,
		// 上游 session 映射。
		// ⚠️ 必须持久化 —— 重启后复用同一个 session，
		// prompt cache 才不会每次冷启动。
		`CREATE TABLE IF NOT EXISTS sessions (
			seed       TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		// 限流账本。
		`CREATE TABLE IF NOT EXISTS throttle (
			model          TEXT PRIMARY KEY,
			throttled_at   INTEGER NOT NULL DEFAULT 0,
			cooldown_until INTEGER NOT NULL DEFAULT 0,
			last_ok        INTEGER NOT NULL DEFAULT 0,
			episodes       TEXT NOT NULL DEFAULT '[]'
		)`,
		// 探测结论。
		`CREATE TABLE IF NOT EXISTS probes (
			model     TEXT PRIMARY KEY,
			state     TEXT NOT NULL,
			detail    TEXT NOT NULL DEFAULT '',
			latency   INTEGER NOT NULL DEFAULT 0,
			ttft      INTEGER NOT NULL DEFAULT 0,
			probed_at INTEGER NOT NULL
		)`,
		// 用量明细（每行一个物理请求）。
		`CREATE TABLE IF NOT EXISTS usage (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			at            INTEGER NOT NULL,
			day           TEXT NOT NULL,
			lane          TEXT NOT NULL DEFAULT '',
			model         TEXT NOT NULL DEFAULT '',
			actual_model  TEXT NOT NULL DEFAULT '',
			client        TEXT NOT NULL DEFAULT '',
			requests      INTEGER NOT NULL DEFAULT 1,
			turns         INTEGER NOT NULL DEFAULT 0,
			attempts      INTEGER NOT NULL DEFAULT 1,
			input_tokens  INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			failed        INTEGER NOT NULL DEFAULT 0,
			recovered     INTEGER NOT NULL DEFAULT 0,
			cost_usd      REAL NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS usage_day ON usage(day)`,
		`CREATE INDEX IF NOT EXISTS usage_model ON usage(model)`,
		// 用量按天聚合（快查）。
		`CREATE TABLE IF NOT EXISTS usage_daily (
			day            TEXT PRIMARY KEY,
			requests       INTEGER NOT NULL DEFAULT 0,
			turns          INTEGER NOT NULL DEFAULT 0,
			attempts       INTEGER NOT NULL DEFAULT 0,
			input_tokens   INTEGER NOT NULL DEFAULT 0,
			output_tokens  INTEGER NOT NULL DEFAULT 0,
			failed         INTEGER NOT NULL DEFAULT 0,
			cost_usd       REAL NOT NULL DEFAULT 0
		)`,
		// 模型开关（用户禁用了哪些模型）。
		`CREATE TABLE IF NOT EXISTS model_toggles (
			model   TEXT PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 1
		)`,
	}
	for _, stmt := range stmts {
		if _, err := d.db.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}

	// ⚠️ **加列** ——
	// `CREATE TABLE IF NOT EXISTS` **不会**给已存在的表加新列，
	// 所以老数据库必须显式 `ALTER TABLE ADD COLUMN`。
	// ⚠️ 加列在 SQLite 里**不幂等** —— 重复加会报 `duplicate column name: xxx`，
	//   所以这里把"列已存在"**当成功**处理。
	//   不用 `PRAGMA table_info` 先查一遍：那要多一次查询，
	//   而错误信息本身已经够明确（匹配两种措辞，不同 SQLite 版本不一样）。
	// ⚠️ 为什么用 REAL 而不是 INTEGER：
	//   钱不能只按整数记 —— 一次免费调用可能是 $0.0000043。
	for _, col := range []struct {
		table string
		name  string
		def   string
	}{
		{"usage", "cost_usd", "REAL NOT NULL DEFAULT 0"},
		{"usage_daily", "cost_usd", "REAL NOT NULL DEFAULT 0"},
	} {
		_, err := d.db.Exec(fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN %s %s`, col.table, col.name, col.def))
		if err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("加列失败 %s.%s: %w", col.table, col.name, err)
		}
	}
	return nil
}

// isDuplicateColumn 判断错误是不是"列已存在"。
// ⚠️ 不同 SQLite 版本的措辞不一样：
//
//	"duplicate column name: xxx"
//	"column xxx already exists"
func isDuplicateColumn(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists")
}

// GetSetting 读一个设置。
func (d *DB) GetSetting(key string) (string, bool, error) {
	var value string
	err := d.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting 写一个设置。
func (d *DB) SetSetting(key, value string) error {
	_, err := d.db.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli())
	return err
}

// GetBool 读一个布尔设置。
func (d *DB) GetBool(key string, fallback bool) bool {
	v, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		return fallback
	}
	return v == "1" || v == "true"
}

// GetAutoModelSwitch 读「自动跨模型调用」开关，**带旧键迁移**。
//
// ⚠️ 旧键是 `cross_lane_fallback`（"跨车道回落"）。
//
//	老实例的设置存在那个键里 —— 这里读一次就迁过去，
//	免得"升级之后开关自己变回关闭"。
//
// ⚠️ **新键优先** —— 一旦用户在新版本里改过，就不再被旧键覆盖。
func (d *DB) GetAutoModelSwitch(fallback bool) bool {
	if v, ok, err := d.GetSetting(SettingAutoModelSwitch); err == nil && ok {
		return v == "1" || v == "true"
	}
	// 新键还没有 → 看旧键。
	if v, ok, err := d.GetSetting(SettingCrossLane); err == nil && ok {
		migrated := v == "1" || v == "true"
		// 迁移：写到新键。
		_ = d.SetSetting(SettingAutoModelSwitch, boolToDB(migrated))
		return migrated
	}
	return fallback
}

// boolToDB 把 bool 转成设置里存的字符串。
func boolToDB(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// GetInt 读一个整数设置。
func (d *DB) GetInt(key string, fallback int) int {
	v, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}

// GetString 读一个字符串设置。
func (d *DB) GetString(key, fallback string) string {
	v, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		return fallback
	}
	return v
}

// PutSecret 存一个密封的上游凭据。
func (d *DB) PutSecret(name, sealed, hint string) error {
	_, err := d.db.Exec(
		`INSERT INTO secrets (name, sealed, hint, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET sealed = excluded.sealed, hint = excluded.hint, updated_at = excluded.updated_at`,
		name, sealed, hint, time.Now().UnixMilli())
	return err
}

// GetSecret 取一个密封的上游凭据。
func (d *DB) GetSecret(name string) (sealed string, hint string, found bool, err error) {
	err = d.db.QueryRow(`SELECT sealed, hint FROM secrets WHERE name = ?`, name).Scan(&sealed, &hint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return sealed, hint, true, nil
}

// DeleteSecret 删一个密封凭据。
func (d *DB) DeleteSecret(name string) error {
	_, err := d.db.Exec(`DELETE FROM secrets WHERE name = ?`, name)
	return err
}

// PutSession 存一个 session 映射。
func (d *DB) PutSession(seed, sessionID string) error {
	_, err := d.db.Exec(
		`INSERT INTO sessions (seed, session_id, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(seed) DO UPDATE SET session_id = excluded.session_id, updated_at = excluded.updated_at`,
		seed, sessionID, time.Now().UnixMilli())
	return err
}

// LoadSessions 读全部 session 映射。
func (d *DB) LoadSessions() (map[string]string, error) {
	rows, err := d.db.Query(`SELECT seed, session_id FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var seed, id string
		if err := rows.Scan(&seed, &id); err != nil {
			return nil, err
		}
		out[seed] = id
	}
	return out, rows.Err()
}

// ThrottleRow 是限流账本里的一行。
// ⚠️ 这是 `lane.ThrottleRow` 的别名 —— 避免两份形状。
type ThrottleRow = lane.ThrottleRow

// PutThrottle 写一条限流记录。
func (d *DB) PutThrottle(row ThrottleRow) error {
	_, err := d.db.Exec(
		`INSERT INTO throttle (model, throttled_at, cooldown_until, last_ok, episodes)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(model) DO UPDATE SET
		   throttled_at = excluded.throttled_at,
		   cooldown_until = excluded.cooldown_until,
		   last_ok = excluded.last_ok,
		   episodes = excluded.episodes`,
		row.Model, row.ThrottledAt, row.CooldownUntil, row.LastOK, row.Episodes)
	return err
}

// LoadThrottle 读全部限流记录。
func (d *DB) LoadThrottle() (map[string]ThrottleRow, error) {
	rows, err := d.db.Query(`SELECT model, throttled_at, cooldown_until, last_ok, episodes FROM throttle`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ThrottleRow{}
	for rows.Next() {
		var r ThrottleRow
		if err := rows.Scan(&r.Model, &r.ThrottledAt, &r.CooldownUntil, &r.LastOK, &r.Episodes); err != nil {
			return nil, err
		}
		out[r.Model] = r
	}
	return out, rows.Err()
}

// ProbeRow 是探测结论的一行。
type ProbeRow struct {
	Model    string `json:"model"`
	State    string `json:"state"`
	Detail   string `json:"detail,omitempty"`
	Latency  int64  `json:"latency,omitempty"`
	TTFT     int64  `json:"ttft,omitempty"`
	ProbedAt int64  `json:"probedAt"`
}

// PutProbe 写一条探测结论。
func (d *DB) PutProbe(row ProbeRow) error {
	_, err := d.db.Exec(
		`INSERT INTO probes (model, state, detail, latency, ttft, probed_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(model) DO UPDATE SET
		   state = excluded.state, detail = excluded.detail,
		   latency = excluded.latency, ttft = excluded.ttft, probed_at = excluded.probed_at`,
		row.Model, row.State, row.Detail, row.Latency, row.TTFT, row.ProbedAt)
	return err
}

// LoadProbes 读全部探测结论。
func (d *DB) LoadProbes() (map[string]ProbeRow, error) {
	rows, err := d.db.Query(`SELECT model, state, detail, latency, ttft, probed_at FROM probes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProbeRow{}
	for rows.Next() {
		var r ProbeRow
		if err := rows.Scan(&r.Model, &r.State, &r.Detail, &r.Latency, &r.TTFT, &r.ProbedAt); err != nil {
			return nil, err
		}
		out[r.Model] = r
	}
	return out, rows.Err()
}

// SetModelEnabled 启停一个模型。
func (d *DB) SetModelEnabled(model string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := d.db.Exec(
		`INSERT INTO model_toggles (model, enabled) VALUES (?, ?)
		 ON CONFLICT(model) DO UPDATE SET enabled = excluded.enabled`,
		model, v)
	return err
}

// LoadModelToggles 读全部模型开关。
// ⚠️ 只需要**被显式关掉**的 —— 没记录的就是开着的。
func (d *DB) LoadModelToggles() (map[string]bool, error) {
	rows, err := d.db.Query(`SELECT model, enabled FROM model_toggles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var model string
		var enabled int
		if err := rows.Scan(&model, &enabled); err != nil {
			return nil, err
		}
		out[model] = enabled != 0
	}
	return out, rows.Err()
}

// UsageRecord 是一次请求的用量记录。
// ⚠️ 这是 `lane.UsageRecord` 的别名 ——
// 避免两份形状不一样的定义（那样迟早会记错字段）。
type UsageRecord = lane.UsageRecord

// PutUsage 记一次用量。
// ⚠️ 明细与按天聚合一并写 —— 聚合是为了快查，
// 明细是为了"哪个模型/客户端用了多少"。
func (d *DB) PutUsage(r lane.UsageRecord) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO usage (at, day, lane, model, actual_model, client,
		   requests, turns, attempts, input_tokens, output_tokens, reasoning_tokens, failed, recovered, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.At, r.Day, r.Lane, r.Model, r.ActualModel, r.Client,
		r.Requests, r.Turns, r.Attempts, r.InputTokens, r.OutputTokens, r.ReasoningTokens, r.Failed, r.Recovered, r.CostUSD)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`INSERT INTO usage_daily (day, requests, turns, attempts, input_tokens, output_tokens, failed, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(day) DO UPDATE SET
		   requests = usage_daily.requests + excluded.requests,
		   turns = usage_daily.turns + excluded.turns,
		   attempts = usage_daily.attempts + excluded.attempts,
		   input_tokens = usage_daily.input_tokens + excluded.input_tokens,
		   output_tokens = usage_daily.output_tokens + excluded.output_tokens,
		   failed = usage_daily.failed + excluded.failed,
		   cost_usd = usage_daily.cost_usd + excluded.cost_usd`,
		r.Day, r.Requests, r.Turns, r.Attempts, r.InputTokens, r.OutputTokens, r.Failed, r.CostUSD)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DailyUsage 是一天的聚合。
type DailyUsage struct {
	Day          string `json:"day"`
	Requests     int    `json:"requests"`
	Turns        int    `json:"turns"`
	Attempts     int    `json:"attempts"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	Failed       int    `json:"failed"`
	// CostUSD 是这天的总花费（美元）。
	CostUSD float64 `json:"costUsd"`
}

// DailyUsageRange 读一段日期的按天聚合。
func (d *DB) DailyUsageRange(from, to string) ([]DailyUsage, error) {
	rows, err := d.db.Query(
		`SELECT day, requests, turns, attempts, input_tokens, output_tokens, failed, cost_usd
		 FROM usage_daily WHERE day >= ? AND day <= ? ORDER BY day`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailyUsage{}
	for rows.Next() {
		var u DailyUsage
		if err := rows.Scan(&u.Day, &u.Requests, &u.Turns, &u.Attempts,
			&u.InputTokens, &u.OutputTokens, &u.Failed, &u.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ModelUsage 是一个模型的汇总。
type ModelUsage struct {
	Model string `json:"model"`
	Lane  string `json:"lane"`
	// ActualModel 是**实际服务**的模型。
	// ⚠️ 它可能与 Model 不同 —— 故障转移是静默的，
	// UI 要能解释"我请求 A 为什么得到了 B"。
	ActualModel  string `json:"actualModel"`
	Requests     int    `json:"requests"`
	Turns        int    `json:"turns"`
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	Failed       int    `json:"failed"`
	// CostUSD 是这个模型累计花的美元。
	// ⚠️ **只有 OpenRouter 报** —— 免密车道永远是 0。
	CostUSD float64 `json:"costUsd"`
}

// ModelUsageAll 读按模型的汇总。
// ⚠️ 按 (请求模型, 实际模型) 分组 ——
// 这样"请求 A 但由 B 应答"在数据里是**看得见的**，
// 而不是被聚合抹平。
func (d *DB) ModelUsageAll(since string) ([]ModelUsage, error) {
	rows, err := d.db.Query(
		`SELECT model, lane, actual_model, SUM(requests), SUM(turns), SUM(input_tokens), SUM(output_tokens), SUM(failed), SUM(cost_usd)
		 FROM usage WHERE day >= ? GROUP BY model, lane, actual_model ORDER BY SUM(requests) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelUsage{}
	for rows.Next() {
		var u ModelUsage
		if err := rows.Scan(&u.Model, &u.Lane, &u.ActualModel, &u.Requests, &u.Turns,
			&u.InputTokens, &u.OutputTokens, &u.Failed, &u.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TotalUsage 是总量汇总。
type TotalUsage struct {
	Requests     int `json:"requests"`
	Turns        int `json:"turns"`
	Attempts     int `json:"attempts"`
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	Failed       int `json:"failed"`
	Recovered    int `json:"recovered"`
	// CostUSD 是总花费（美元）—— **只有 OpenRouter 报**。
	CostUSD float64 `json:"costUsd"`
}

// Total 读总量汇总。
func (d *DB) Total(since string) (TotalUsage, error) {
	var t TotalUsage
	err := d.db.QueryRow(
		`SELECT COALESCE(SUM(requests),0), COALESCE(SUM(turns),0), COALESCE(SUM(attempts),0),
		        COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
		        COALESCE(SUM(failed),0), COALESCE(SUM(recovered),0),
		        COALESCE(SUM(cost_usd),0)
		 FROM usage WHERE day >= ?`, since).Scan(
		&t.Requests, &t.Turns, &t.Attempts, &t.InputTokens, &t.OutputTokens, &t.Failed, &t.Recovered,
		&t.CostUSD)
	return t, err
}

// timeNowUnixNano 是可注入的时钟。
var timeNowUnixNano = func() int64 { return time.Now().UnixNano() }

// 设置键名。
const (
	// SettingLocalKey 是本机推理 Key。
	SettingLocalKey = "local_key"
	SettingLanKey   = "lan_key"
	// SettingLanEnabled 是局域网推理的开关。
	SettingLanEnabled = "lan_enabled"
	// SettingLanTier 是局域网访问档位（off / limited / lan）。
	//
	// ⚠️ 它**取代** `SettingLanEnabled` —— 那个布尔开关粒度太粗
	//   （要么全开要么全关）。`SettingLanEnabled` 保留只为兼容旧数据。
	SettingLanTier = "lan_tier"
	// SettingLanPerMinute 是 "limited" 档的每分钟请求上限（<= 0 = 不限）。
	SettingLanPerMinute = "lan_per_minute"
	// SettingLanMaxConcurrent 是 "limited" 档的并发上限（<= 0 = 不限）。
	SettingLanMaxConcurrent = "lan_max_concurrent"
	// SettingLanPort 是局域网推理的端口。
	SettingLanPort = "lan_port"
	// SettingAutoModelSwitch 是「自动跨模型调用」的开关。
	//
	// ⚠️ **语义**：
	//
	//	关闭 —— 只重试**当前模型**。重试仍失败就**直接报错**，
	//	        **绝不**换成别的模型。
	//	开启 —— 允许换模型。优先级：
	//	        ① Kilo ↔ Zen（都是免密的，不花任何人的钱）
	//	        ② 两条免密车道都没有可用模型了 → OpenRouter
	//
	// ⚠️ **为什么改名**：旧名"跨车道回落"说的是"换一条车道"，
	//   但实际语义已经是**模型级别**的（同一车道内也能换模型）。
	//   新名字说的是用户真正想要的东西：「自动换个模型试试」。
	SettingAutoModelSwitch = "auto_model_switch"
	// SettingCrossLane 是**旧键名** —— 只用于迁移。
	//
	// ⚠️ 保留是因为老实例的设置存在这个键里。
	//   迁移完就没人读它了，但**不删** —— 删了会让"降级再升级"丢设置。
	SettingCrossLane = "cross_lane_fallback"
	// SettingProbeEnabled 是自动探测的开关。
	SettingProbeEnabled = "probe_enabled"
	// SettingProbeIntervalMin 是自动探测的间隔（分钟）。
	SettingProbeIntervalMin = "probe_interval_min"
	// ⚠️ **没有 SettingProbeBatchSize** —— 探测是**全量**的。
	//   每 15 分钟把每个免费模型都探一遍，没有"每轮探几个"的概念。
	//   而且用用户自己 Key 的车道不参与自动探测，
	//   所以全量探测只花匿名共享池的额度。
	// SettingLaneEnabled 是"某条车道开不开"的开关。
	// ⚠️ 存的是**车道 id → 1/0** 的组合，比如 "zen:1,openrouter:0"。
	//   用一个键存所有车道，比每个车道一个键好扩展。
	SettingLaneEnabled = "lane_enabled"
	// ⚠️ **没有 SettingConformanceIntervalMin** —— 合规检查功能已砍掉。
	//   它每次检查 = 一次真实推理，而用户从"闸门拒绝"的现象里
	//   已经能自己判断了（模型清单上全是"出口被风控"/"未探测"）。
	// SettingAdaptiveProbe 是"探测批量自适应"的开关。
	// ⚠️ 上游越拥挤，探得越少 —— 免密车道的配额是全局共享的，
	//   高峰期继续按正常节奏探测就是拿额度去撞限流墙。
	SettingAdaptiveProbe = "adaptive_probe"
	// SettingSecretOpenRouter 是 OpenRouter 凭据的密封存储名。
	SettingSecretOpenRouter = "openrouter_api_key"
	// SettingModelTogglesApplied 标记模型开关是否已应用过。
	SettingModelTogglesApplied = "model_toggles_applied"
)

// PutJSON 存一个 JSON 值（给设置用）。
func (d *DB) PutJSON(key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return d.SetSetting(key, string(data))
}

// GetJSON 读一个 JSON 值。
func (d *DB) GetJSON(key string, out any) (bool, error) {
	raw, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return false, err
	}
	return true, nil
}
