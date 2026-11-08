package store

import (
	"database/sql"
	"errors"
)

// LogRow 是一条运行日志。
type LogRow struct {
	ID       int64  `json:"id"`
	At       int64  `json:"at"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

// AppendLog 追加一条日志。
func (d *DB) AppendLog(at int64, category, message string) error {
	_, err := d.db.Exec(
		`INSERT INTO logs (at, category, message) VALUES (?, ?, ?)`,
		at, category, message)
	return err
}

// ListLogs 读最近的日志（按时间倒序）。
//
// ⚠️ **categories 为空表示不限类目** ——
// 调用方（server 层）负责把"已关闭的类目"过滤掉，
// 这样**历史数据不会被删**，只是不再显示。
func (d *DB) ListLogs(categories []string, limit int) ([]LogRow, error) {
	if limit <= 0 {
		limit = 300
	}
	var rows *sql.Rows
	var err error
	if len(categories) == 0 {
		rows, err = d.db.Query(
			`SELECT id, at, category, message FROM logs ORDER BY id DESC LIMIT ?`, limit)
	} else {
		// ⚠️ 类目是**代码里定义的常量**，不是用户输入，
		//   所以拼进 IN 子句是安全的；这里仍然用占位符。
		query := `SELECT id, at, category, message FROM logs WHERE category IN (`
		args := make([]any, 0, len(categories))
		for i, c := range categories {
			if i > 0 {
				query += ","
			}
			query += "?"
			args = append(args, c)
		}
		query += `) ORDER BY id DESC LIMIT ?`
		args = append(args, limit)
		rows, err = d.db.Query(query, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogRow{}
	for rows.Next() {
		var r LogRow
		if err := rows.Scan(&r.ID, &r.At, &r.Category, &r.Message); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountLogsByCategory 统计各类目的条数（给日志页显示"每类多少条"）。
func (d *DB) CountLogsByCategory() (map[string]int, error) {
	rows, err := d.db.Query(`SELECT category, COUNT(*) FROM logs GROUP BY category`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		out[c] = n
	}
	return out, rows.Err()
}

// ClearLogs 清空日志（只清某个类目，或全部）。
func (d *DB) ClearLogs(category string) error {
	if category == "" {
		_, err := d.db.Exec(`DELETE FROM logs`)
		return err
	}
	_, err := d.db.Exec(`DELETE FROM logs WHERE category = ?`, category)
	return err
}

// TrimLogs 只保留最近 n 条日志（防止日志表无限膨胀）。
func (d *DB) TrimLogs(keep int) error {
	if keep <= 0 {
		return nil
	}
	_, err := d.db.Exec(
		`DELETE FROM logs WHERE id NOT IN (SELECT id FROM logs ORDER BY id DESC LIMIT ?)`, keep)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}
