package remote

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// linkInfo 链接指向的东西：是不是目录、多大。
type linkInfo struct {
	dir  bool
	size int64
}

// linkTargetsCommand 一条命令列出目录里各项「类型 大小 名字」，-L 跟着链接看它指向的东西。
// -maxdepth 1 只看这一层，不进目录；远端也设时限，通道关了命令不会一直跑。
func linkTargetsCommand(dir string) string {
	quoted := "'" + strings.ReplaceAll(dir, "'", `'\''`) + "'"
	return "LC_ALL=C timeout -k 5s 60s find -L " + quoted + ` -maxdepth 1 -mindepth 1 -printf '%y\t%s\t%f\n' 2>/dev/null`
}

// parseLinkTargets 解析上面那条命令的输出。名字里有制表符也没关系：只按前两个字段切。
func parseLinkTargets(lines []string) map[string]linkInfo {
	out := make(map[string]linkInfo, len(lines))
	for _, line := range lines {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		out[f[2]] = linkInfo{dir: f[0] == "d", size: n}
	}
	return out
}

// linkTargets 用一条命令问清这些链接指向目录还是文件，返回没问到的下标（调用方对这些再逐个 Stat）。
// 不能执行命令（提权的文件会话）、没有 find、没权限看这个目录时，全部退回逐个问，结果和以前一样。
func (c *FileClient) linkTargets(dir string, out []FileEntry, links []int) []int {
	if c.run == nil || len(links) == 0 {
		return links
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var lines []string
	res, err := c.run(ctx, linkTargetsCommand(dir), nil, func(line string) { lines = append(lines, line) })
	if err != nil {
		return links
	}
	_ = res // 断链、没权限时 find 会返回非零，但已经列出来的照样能用；没列到的下面逐个问
	got := parseLinkTargets(lines)
	var rest []int
	for _, i := range links {
		v, ok := got[out[i].Name]
		if !ok {
			rest = append(rest, i)
			continue
		}
		out[i].Dir, out[i].Size = v.dir, v.size
	}
	return rest
}
