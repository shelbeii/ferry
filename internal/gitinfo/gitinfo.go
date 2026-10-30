// Package gitinfo 读取本地工作区的提交信息（写进部署记录）。
package gitinfo

import (
	"os/exec"
	"strconv"
	"strings"
)

// Info 工作区状态。不是 git 仓库时 Commit 为空。
type Info struct {
	Commit string   `json:"commit"`
	Branch string   `json:"branch"`
	Dirty  bool     `json:"dirty"`
	Files  []string `json:"files,omitempty"` // 未提交的文件（最多 20 个）
	Top    string   `json:"top,omitempty"`   // 仓库根目录
}

// Read 读取 dir 所在仓库的信息。
func Read(dir string) Info {
	var in Info
	out, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return in
	}
	in.Commit = out
	in.Branch, _ = git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	in.Top, _ = git(dir, "rev-parse", "--show-toplevel")
	if st, err := git(dir, "status", "--porcelain", "--untracked-files=no"); err == nil && st != "" {
		in.Dirty = true
		for _, l := range strings.Split(st, "\n") {
			if len(in.Files) == 20 {
				break
			}
			in.Files = append(in.Files, strings.TrimSpace(l))
		}
	}
	return in
}

// Short 提交号前 8 位（带 * 表示有未提交修改）。
func (i Info) Short() string {
	if i.Commit == "" {
		return "-"
	}
	s := i.Commit
	if len(s) > 8 {
		s = s[:8]
	}
	if i.Dirty {
		s += "*"
	}
	return s
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Commit 一个提交。
type Commit struct {
	Hash    string `json:"hash"` // 前 8 位
	Author  string `json:"author"`
	Date    string `json:"date"` // 2006-01-02
	Subject string `json:"subject"`
}

// Range from（线上的提交）到 to（本地的）之间：to 比 from 多的提交（新的在前，最多 max 个）、各自多几个。
// known：from 在本地仓库里（别的电脑上提交、还没拉下来的不在）。
type Range struct {
	Known   bool     `json:"known"`
	Ahead   int      `json:"ahead"`  // 这次要上线、线上还没有的提交
	Behind  int      `json:"behind"` // 线上有、本地没有的提交（线上是别的分支、或比本地新）
	Commits []Commit `json:"commits"`
}

// Between 两个提交之间的提交。
func Between(dir, from, to string, max int) Range {
	var r Range
	if _, err := git(dir, "cat-file", "-e", from+"^{commit}"); err != nil {
		return r
	}
	r.Known = true
	count := func(a, b string) int {
		out, err := git(dir, "rev-list", "--count", a+".."+b)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(out)
		return n
	}
	r.Ahead, r.Behind = count(from, to), count(to, from)
	out, err := git(dir, "log", "--no-merges", "-n", strconv.Itoa(max), "--date=short", "--format=%h%x09%an%x09%ad%x09%s", from+".."+to)
	if err != nil || out == "" {
		return r
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.SplitN(l, "\t", 4)
		if len(f) == 4 {
			h := f[0]
			if len(h) > 8 {
				h = h[:8]
			}
			r.Commits = append(r.Commits, Commit{Hash: h, Author: f[1], Date: f[2], Subject: f[3]})
		}
	}
	return r
}
