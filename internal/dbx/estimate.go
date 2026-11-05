package dbx

// 改动执行前估算会改、删多少行（给确认框看）：用 EXPLAIN，不执行语句；放在事务里、最后撤回，几秒内没估出来就算了。
// 只是数据库按统计信息的估计，可能和实际差不少；表在这批语句里才建、语法 EXPLAIN 不支持的跳过。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Estimate 一条语句预计影响的行数。
type Estimate struct {
	Index int    `json:"index"` // 第几条（从 0 开始）
	Verb  string `json:"verb"`  // 改 | 删
	Rows  int64  `json:"rows"`
}

// EstimateTimeout 估算最多用多久。
var EstimateTimeout = 8 * time.Second

// EstimateRows 估算这些语句里的 UPDATE、DELETE 各会影响多少行。
func (cl *Client) EstimateRows(ctx context.Context, dbName string, stmts []string) ([]Estimate, error) {
	kind := cl.Conn.Kind
	out := []Estimate{}
	todo := EstimateCandidates(kind, stmts)
	if len(todo) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, EstimateTimeout)
	defer cancel()
	db, err := cl.DB(ctx, dbName)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, cl.explain(err)
	}
	defer conn.Close()
	// PostgreSQL 放在只读事务里；MySQL、MariaDB 的只读事务里不让 EXPLAIN UPDATE，用普通事务，最后总是撤回
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: kind == Postgres})
	if err != nil {
		return nil, cl.explain(err)
	}
	defer tx.Rollback()
	if kind == Postgres {
		tx.ExecContext(ctx, "SET LOCAL statement_timeout = 5000")
	}
	for _, i := range todo {
		// 去掉注释再加 EXPLAIN：MySQL 的 /*! ANALYZE */ 会让它真的执行
		stmt := strings.TrimSpace(stripComments(kind, stmts[i]))
		verb := "改"
		if FirstWord(kind, stmts[i]) == "DELETE" {
			verb = "删"
		}
		var n int64
		var ok bool
		if kind == Postgres {
			// 一条估不了（表还没建）会让事务作废：每条前设保存点
			tx.ExecContext(ctx, "SAVEPOINT ferry_est")
			n, ok = pgEstimate(ctx, tx, stmt)
			if !ok {
				tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT ferry_est")
			}
		} else {
			n, ok = mysqlEstimate(ctx, tx, stmt)
		}
		if ctx.Err() != nil {
			break
		}
		if ok {
			out = append(out, Estimate{Index: i, Verb: verb, Rows: n})
		}
	}
	return out, nil
}

// EstimateCandidates 要估算的语句（UPDATE、DELETE）是第几条。
func EstimateCandidates(kind string, stmts []string) []int {
	var out []int
	if kind == Redis {
		return out
	}
	for i, s := range stmts {
		switch FirstWord(kind, s) {
		case "UPDATE", "DELETE":
			out = append(out, i)
		}
	}
	return out
}

// pgEstimate EXPLAIN (FORMAT JSON)：改、删的节点（ModifyTable）下面那个节点的行数。
func pgEstimate(ctx context.Context, tx *sql.Tx, stmt string) (int64, bool) {
	var js string
	if err := tx.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) "+stmt).Scan(&js); err != nil {
		return 0, false
	}
	type node struct {
		Type  string  `json:"Node Type"`
		Rows  float64 `json:"Plan Rows"`
		Plans []node  `json:"Plans"`
	}
	var plans []struct {
		Plan node `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(js), &plans); err != nil || len(plans) == 0 {
		return 0, false
	}
	p := plans[0].Plan
	if p.Type == "ModifyTable" && len(p.Plans) > 0 {
		p = p.Plans[0]
	}
	return int64(p.Rows + 0.5), true
}

// mysqlEstimate EXPLAIN：第一行（要改的表）的 rows × filtered%。
func mysqlEstimate(ctx context.Context, tx *sql.Tx, stmt string) (int64, bool) {
	rows, err := tx.QueryContext(ctx, "EXPLAIN "+stmt)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	cols, data, _, err := readRows(rows, 1)
	if err != nil || len(data) == 0 {
		return 0, false
	}
	var n, pct float64 = -1, 100
	for i, c := range cols {
		s, _ := data[0][i].(string)
		switch strings.ToLower(c.Name) {
		case "rows":
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				n = v
			}
		case "filtered":
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				pct = v
			}
		}
	}
	if n < 0 {
		return 0, false
	}
	return int64(n*pct/100 + 0.5), true
}

// EstimateText 给人看的估算：「第 2 条预计改约 1,234 行」，没有估算时为空。
func EstimateText(stmts []string, est []Estimate) string {
	var parts []string
	for _, e := range est {
		s := fmt.Sprintf("预计%s约 %s 行", e.Verb, groupDigits(e.Rows))
		if len(stmts) > 1 {
			s = fmt.Sprintf("第 %d 条%s", e.Index+1, s)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "；")
}

func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
