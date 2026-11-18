package dbx

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SQL Server 的事务没有只读选项。这里只接受 SELECT 和以 WITH 开头的 SELECT，
// 按 T-SQL 的引号、括号标识符及嵌套注释扫描，也检查省略分号的语句和 SELECT 的写入形式。
var sqlServerBlocked = func() map[string]bool {
	words := strings.Fields(`ADD ALTER BACKUP BEGIN BREAK BULK CHECKPOINT CLOSE COMMIT CONTINUE CREATE DBCC
DEALLOCATE DECLARE DELETE DENY DISABLE DROP DUMP ENABLE EXEC EXECUTE GO GOTO GRANT IF INSERT
INTO KILL LOAD MERGE OPEN PRINT RAISERROR RECONFIGURE RENAME RESTORE RETURN REVERT REVOKE ROLLBACK SAVE
SEND SET SETUSER SHUTDOWN THROW TRUNCATE UPDATE UPDATETEXT USE WAITFOR WHILE WRITETEXT CONVERSATION
CURSOR NEXT OPENQUERY OPENROWSET OPENDATASOURCE READTEXT RECEIVE UPDLOCK XLOCK TABLOCKX`)
	blocked := make(map[string]bool, len(words))
	for _, word := range words {
		blocked[word] = true
	}
	return blocked
}()

func sqlServerStatements(src string) ([]string, error) {
	return scanSQLServer(src, false)
}

func scanSQLServer(src string, write bool) ([]string, error) {
	var stmts []string
	start, depth := 0, 0
	first, previous := "", ""
	foundSelect, foundSet, foundAlter := false, false, false
	finish := func(end int) error {
		if first == "" {
			return nil
		}
		if depth != 0 || (!write || first == "WITH") && !foundSelect {
			return errors.New("SQL Server 只接受完整的 SELECT 查询")
		}
		stmts = append(stmts, strings.TrimSpace(src[start:end]))
		first, previous, foundSelect, foundSet, foundAlter = "", "", false, false, false
		return nil
	}
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "--"):
			i += 2
			for i < len(src) && src[i] != '\r' && src[i] != '\n' {
				i++
			}
			continue
		case strings.HasPrefix(src[i:], "/*"):
			n := 1
			i += 2
			for i < len(src) && n > 0 {
				switch {
				case strings.HasPrefix(src[i:], "/*"):
					n++
					i += 2
				case strings.HasPrefix(src[i:], "*/"):
					n--
					i += 2
				default:
					i++
				}
			}
			if n != 0 {
				return nil, errors.New("SQL Server 查询里的注释没有结束")
			}
			continue
		}
		c, size := utf8.DecodeRuneInString(src[i:])
		if unicode.IsSpace(c) {
			i += size
			continue
		}
		if unicode.IsControl(c) {
			return nil, errors.New("SQL Server 查询里有无效控制字符")
		}
		if c == ';' {
			if depth != 0 {
				return nil, errors.New("请用分号分隔完整的 SQL Server 查询")
			}
			if err := finish(i); err != nil {
				return nil, err
			}
			i += size
			start = i
			continue
		}
		if c == '\'' || c == '"' || c == '[' {
			if first == "" {
				return nil, errors.New("SQL Server 查询必须以 SELECT 或 WITH 开头")
			}
			end := byte(c)
			if c == '[' {
				end = ']'
			}
			next, err := sqlServerQuotedEnd(src, i, end)
			if err != nil {
				return nil, err
			}
			i, previous = next, ""
			continue
		}
		if c >= '0' && c <= '9' {
			if first == "" {
				return nil, errors.New("SQL Server 查询必须以 SELECT 或 WITH 开头")
			}
			i, previous = sqlServerNumberEnd(src, i), ""
			continue
		}
		if sqlServerWordChar(c) && c != '$' {
			j := i + size
			for j < len(src) {
				r, n := utf8.DecodeRuneInString(src[j:])
				if !sqlServerWordChar(r) {
					break
				}
				j += n
			}
			word := strings.ToUpper(src[i:j])
			if first == "" {
				if word != "SELECT" && word != "WITH" && !(write && sqlServerWriteStart(word)) {
					return nil, errors.New("SQL Server 查询必须以 SELECT 或 WITH 开头")
				}
				first = word
				switch word {
				case "SELECT", "WITH":
				default:
					i, previous = j, word
					continue
				}
			}
			// NEXT 是分页语法的一部分，也用于会改变序列的 NEXT VALUE FOR。
			if sqlServerBlocked[word] && (word != "NEXT" || previous != "FETCH") {
				switch {
				case write && depth == 0 && first == "UPDATE" && word == "SET" && !foundSet:
					foundSet = true
				case write && depth == 0 && first == "INSERT" && (word == "INTO" && previous == "INSERT" || word == "WITH"):
				case write && depth == 0 && first == "ALTER" && (word == "ADD" || word == "DROP" || word == "ALTER") && !foundAlter:
					foundAlter = true
				default:
					return nil, fmt.Errorf("SQL Server 不允许 %s；同名列请用方括号引用", word)
				}
			}
			if depth == 0 && word == "SELECT" {
				if foundSelect && previous != "UNION" && previous != "ALL" && previous != "INTERSECT" && previous != "EXCEPT" {
					return nil, errors.New("请用分号分隔 SQL Server 查询")
				}
				foundSelect = true
			}
			i, previous = j, word
			continue
		}
		if first == "" {
			return nil, errors.New("SQL Server 查询必须以 SELECT 或 WITH 开头")
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, errors.New("SQL Server 查询里的括号不匹配")
			}
		}
		i, previous = i+size, ""
	}
	if err := finish(len(src)); err != nil {
		return nil, err
	}
	if len(stmts) == 0 {
		return nil, errors.New("没有要执行的 SQL Server 查询")
	}
	return stmts, nil
}

func sqlServerWriteStart(word string) bool {
	switch word {
	case "INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "DROP", "TRUNCATE":
		return true
	}
	return false
}

func sqlServerWordChar(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c) || unicode.IsMark(c) || strings.ContainsRune("_@#$", c)
}

func sqlServerNumberEnd(src string, start int) int {
	i := start
	isDigit := func(b byte) bool { return b >= '0' && b <= '9' }
	if strings.HasPrefix(src[start:], "0x") || strings.HasPrefix(src[start:], "0X") {
		i += 2
		for i < len(src) && (isDigit(src[i]) || src[i] >= 'A' && src[i] <= 'F' || src[i] >= 'a' && src[i] <= 'f') {
			i++
		}
		return i
	}
	for i < len(src) && isDigit(src[i]) {
		i++
	}
	if i < len(src) && src[i] == '.' {
		i++
		for i < len(src) && isDigit(src[i]) {
			i++
		}
	}
	if i < len(src) && (src[i] == 'E' || src[i] == 'e') {
		j := i + 1
		if j < len(src) && (src[j] == '+' || src[j] == '-') {
			j++
		}
		if j < len(src) && isDigit(src[j]) {
			for j < len(src) && isDigit(src[j]) {
				j++
			}
			return j
		}
	}
	return i
}

func sqlServerQuotedEnd(src string, start int, end byte) (int, error) {
	for i := start + 1; i < len(src); i++ {
		if src[i] != end {
			continue
		}
		if i+1 < len(src) && src[i+1] == end {
			i++
			continue
		}
		return i + 1, nil
	}
	return 0, errors.New("SQL Server 查询里的字符串或标识符没有结束")
}
