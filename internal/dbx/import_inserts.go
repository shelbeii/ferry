package dbx

import (
	"regexp"
	"strings"
)

var (
	mysqlSingleInsert = singleInsertPattern("(?:[a-z_][a-z0-9_$]*|`(?:[^`\\\\]|``)+`)", `|0x[0-9a-f]+|0b[01]+`)
	pgSingleInsert    = singleInsertPattern(`(?:[a-z_][a-z0-9_$]*|"(?:[^"\\]|"")+")`, "")
)

// Only recognize a single VALUES tuple of literals. Leave SQL expressions, suffixes
// such as ON CONFLICT, and dialect-specific quoting to the ordinary execution path.
func singleInsertPattern(name, extraNumbers string) *regexp.Regexp {
	const ws = `[ \t\r\n]`
	cols := name + `(?:` + ws + `*,` + ws + `*` + name + `)*`
	head := `INSERT` + ws + `+INTO` + ws + `+` + name + `(?:` + ws + `*\.` + ws + `*` + name + `)?` +
		`(?:` + ws + `*\(` + ws + `*` + cols + ws + `*\))?` + ws + `+VALUES` + ws + `*`
	value := `(?:NULL|TRUE|FALSE|'(?:[^'\\]|'')*'|[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:e[+-]?[0-9]+)?` + extraNumbers + `)`
	return regexp.MustCompile(`(?i)^(` + head + `)\(` + ws + `*` + value + `(?:` + ws + `*,` + ws + `*` + value + `)*` + ws + `*\)$`)
}

const maxImportBatchBytes = 256 << 10

func batchSQLInserts(kind string, stmts []string, maxBytes int) (string, int) {
	const maxStatementBytes = 16 << 10
	const maxBatchRows = 500
	pattern := mysqlSingleInsert
	if kind == Postgres {
		pattern = pgSingleInsert
	}
	findHeader := func(s string) int {
		if len(s) >= 6 && len(s) <= maxStatementBytes && strings.EqualFold(s[:6], "INSERT") {
			if idx := pattern.FindStringSubmatchIndex(s); idx != nil {
				return idx[3]
			}
		}
		return 0
	}
	first := stmts[0]
	if len(stmts) < 2 || len(first) >= maxBytes {
		return first, 1
	}
	headerLen := findHeader(first)
	if headerLen == 0 {
		return first, 1
	}
	head := first[:headerLen]
	var batch strings.Builder
	n := 1
	for ; n < min(len(stmts), maxBatchRows); n++ {
		stmt := stmts[n]
		if !strings.HasPrefix(stmt, head) || findHeader(stmt) != headerLen {
			break
		}
		tuple := stmt[headerLen:]
		if n == 1 {
			batch.WriteString(first)
		}
		if batch.Len()+len(tuple)+2 > maxBytes {
			break
		}
		batch.WriteString(",\n")
		batch.WriteString(tuple)
	}
	if n == 1 {
		return first, 1
	}
	return batch.String(), n
}
