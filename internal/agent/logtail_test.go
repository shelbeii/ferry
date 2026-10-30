package agent

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 从末尾往前读和从头读到尾的结果一样（有没有结尾换行、行数够不够、跨块、空行）
func TestTailEndSameAsTailLines(t *testing.T) {
	dir := t.TempDir()
	rnd := rand.New(rand.NewSource(1))
	cases := []string{"", "\n", "one", "one\n", "a\nb", "a\nb\n", "\n\n\n", "a\r\nb\r\n"}
	for i := 0; i < 30; i++ {
		var b strings.Builder
		lines := rnd.Intn(3000)
		for j := 0; j < lines; j++ {
			b.WriteString(strings.Repeat("x", rnd.Intn(200)))
			fmt.Fprintf(&b, " %d", j)
			if j < lines-1 || rnd.Intn(2) == 0 {
				b.WriteByte('\n')
			}
		}
		cases = append(cases, b.String())
	}
	for i, c := range cases {
		p := filepath.Join(dir, fmt.Sprintf("f%d.log", i))
		os.WriteFile(p, []byte(c), 0o644)
		for _, n := range []int{1, 2, 3, 300, 5000} {
			want := tailLines(strings.NewReader(c), n)
			f, _ := os.Open(p)
			got := tailOpenFile(f, n)
			f.Close()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("文件 %d、n=%d：得到 %d 行（截断 %v），应为 %d 行（截断 %v）", i, n, len(got.Lines), got.Truncated, len(want.Lines), want.Truncated)
			}
		}
	}
}

// 大文件只读末尾：不用从头读
func TestTailEndReadsOnlyTheEnd(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	r := &countingReaderAt{s: b.String()}
	got := tailEnd(r, int64(len(r.s)), 3)
	if strings.Join(got.Lines, "|") != "line 199997|line 199998|line 199999" || !got.Truncated {
		t.Fatalf("%+v", got)
	}
	if r.read > 200<<10 {
		t.Fatalf("读了 %d 字节", r.read)
	}
}

type countingReaderAt struct {
	s    string
	read int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, c.s[off:])
	c.read += n
	return n, nil
}
