package ui

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Table 简单的对齐表格（按显示宽度对齐，中文占两格）。
type Table struct {
	Header []string
	Rows   [][]string
	// Color 可选：给某个单元格着色（返回着色后的字符串，不影响宽度计算）
	Color func(row, col int, s string) string
}

// Write 输出表格。
func (t *Table) Write(w io.Writer) {
	widths := make([]int, len(t.Header))
	for i, h := range t.Header {
		widths[i] = Width(h)
	}
	for _, r := range t.Rows {
		for i, c := range r {
			if i < len(widths) && Width(c) > widths[i] {
				widths[i] = Width(c)
			}
		}
	}
	line := func(cells []string, row int) {
		var b strings.Builder
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			s := c
			if t.Color != nil && row >= 0 {
				s = t.Color(row, i, c)
			}
			b.WriteString(s)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-Width(c)+2))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(t.Header, -1)
	for i, r := range t.Rows {
		line(r, i)
	}
}

// Width 显示宽度：东亚宽字符算 2。
func Width(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r == 0x2026 || r == 0x2714 || r == 0x2717:
			n++
		case unicode.Is(unicode.Han, r) || (r >= 0x3000 && r <= 0x30ff) || (r >= 0xff00 && r <= 0xffef) || (r >= 0xac00 && r <= 0xd7a3):
			n += 2
		default:
			n++
		}
	}
	return n
}

func padRight(s string, n int) string {
	if w := Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}
