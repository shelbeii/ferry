package dbx

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Column 结果里的一列。
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"` // 数据库的类型名（大写）：VARCHAR、INT、TIMESTAMP……
}

// Binary 二进制值（不直接显示）。
type Binary struct {
	Size int    `json:"bin"`
	Hex  string `json:"hex"` // 前 MaxHex 个字节
}

// Cut 太长被截断的文本。
type Cut struct {
	Text string `json:"cut"`
	Size int    `json:"size"`
}

// 单元格最多返回这么多：更长的截断（界面上不能直接改）。
const (
	MaxCell = 256 << 10
	MaxHex  = 32
)

// Result 一条语句的结果。
type Result struct {
	SQL      string   `json:"sql"`
	Columns  []Column `json:"columns,omitempty"`
	Rows     [][]any  `json:"rows,omitempty"` // 单元格：null、字符串、Binary、Cut
	More     bool     `json:"more,omitempty"` // 行数超过上限，没全部返回
	Affected *int64   `json:"affected,omitempty"`
	LastID   *int64   `json:"lastId,omitempty"`
	Ms       int64    `json:"ms"`
	Error    string   `json:"error,omitempty"`
	Code     string   `json:"code,omitempty"`
}

var binaryTypes = map[string]bool{
	"BLOB": true, "TINYBLOB": true, "MEDIUMBLOB": true, "LONGBLOB": true, "BINARY": true, "VARBINARY": true,
	"GEOMETRY": true, "POINT": true, "LINESTRING": true, "POLYGON": true, "BIT": true, "BYTEA": true,
}

// IsBinaryType 这种类型的值按二进制显示。
func IsBinaryType(t string) bool { return binaryTypes[strings.ToUpper(t)] }

// readRows 读出最多 max 行（max <= 0 不限）。
func readRows(rows *sql.Rows, max int) ([]Column, [][]any, bool, error) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, false, err
	}
	cols := make([]Column, len(cts))
	for i, ct := range cts {
		cols[i] = Column{Name: ct.Name(), Type: strings.ToUpper(ct.DatabaseTypeName())}
	}
	out := [][]any{}
	more := false
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if max > 0 && len(out) >= max {
			more = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return cols, out, more, err
		}
		row := make([]any, len(cols))
		for i, v := range vals {
			row[i] = cell(v, cols[i].Type)
		}
		out = append(out, row)
	}
	return cols, out, more, rows.Err()
}

// cell 驱动给的值 → 界面上显示、修改用的值。
func cell(v any, typ string) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		if IsBinaryType(typ) || !utf8.Valid(x) {
			n := len(x)
			if len(x) > MaxHex {
				x = x[:MaxHex]
			}
			return Binary{Size: n, Hex: hex.EncodeToString(x)}
		}
		return text(string(x))
	case string:
		return text(x)
	case time.Time:
		return formatTime(x, typ)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	default:
		return text(fmt.Sprint(x))
	}
}

func text(s string) any {
	if len(s) > MaxCell {
		cut := s[:MaxCell]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		return Cut{Text: cut, Size: len(s)}
	}
	return s
}

func formatTime(t time.Time, typ string) string {
	switch typ {
	case "DATE":
		return t.Format("2006-01-02")
	case "TIME", "TIMETZ":
		return t.Format("15:04:05.999999")
	case "TIMESTAMPTZ":
		return t.Format("2006-01-02 15:04:05.999999Z07:00")
	}
	return t.Format("2006-01-02 15:04:05.999999")
}
