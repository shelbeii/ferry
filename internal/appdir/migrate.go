package appdir

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LegacyDir 旧版本的数据目录 ~/.ferry。
func LegacyDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ferry")
}

// MigrateLegacy 首次使用新数据目录时，把旧 ~/.ferry 里的主机公钥记录合并过来（只做一次）。
// 旧目录里的私钥不搬：ferry.yaml 里的 identityFile 可能正指向它们，复制一份只会多一份私钥。
func MigrateLegacy() (string, error) {
	marker := Path(".migrated-v1")
	if _, err := os.Stat(marker); err == nil {
		return "", nil
	}
	defer os.WriteFile(marker, nil, 0o600)
	old := LegacyDir()
	if old == "" || samePath(old, Dir()) {
		return "", nil
	}
	n, err := mergeLines(filepath.Join(old, "known_hosts"), KnownHosts())
	if err != nil || n == 0 {
		return "", err
	}
	return fmt.Sprintf("已从 %s 合并 %d 条主机公钥记录", old, n), nil
}

// MigrateUserDir 默认位置改成安装目录后：把旧版本放在用户目录（%APPDATA%\Ferry 等）的全部数据搬过来，只做一次。
// 安装目录下已经有设置文件时不搬（两边都有数据，让用户自己决定）。
// 返回给人看的说明，以及真的搬了时的原目录（调用方据此更新指向旧位置的私钥路径）。
func MigrateUserDir() (msg, from string, err error) {
	cur := Current()
	if cur.Mode != ModeInstall {
		return "", "", nil
	}
	old := UserDir()
	if samePath(old, cur.Dir) {
		return "", "", nil
	}
	if !hasData(old) {
		removeBookkeeping(old) // 只剩记录文件（如开发版运行时留下的）：清掉，免得以后误以为有旧数据
		return "", "", nil
	}
	// 同一时刻可能有几个请求（桌面版启动时）：只让一个去搬，其余的等它搬完
	var unlock func()
	for i := 0; ; i++ {
		if unlock, err = lockFile(filepath.Join(cur.Dir, ".migrate.lock")); err == nil {
			break
		}
		if i >= 300 {
			return "", "", nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer unlock()
	if !hasData(old) { // 别的进程已经搬完
		return "", "", nil
	}
	if _, err := os.Stat(filepath.Join(cur.Dir, "settings.json")); err == nil {
		return fmt.Sprintf("%s 里还有旧版本的数据，安装目录下也已经有数据，没有自动合并；需要的话手动把它们搬到 %s", old, cur.Dir), "", nil
	}
	if err := Move(old, cur.Dir); err != nil {
		return "", "", fmt.Errorf("把数据从 %s 搬到安装目录失败：%w", old, err)
	}
	os.Remove(filepath.Join(old, "location.json"))
	os.Remove(old) // 空了就删掉
	return fmt.Sprintf("已把数据从 %s 搬到安装目录 %s", old, cur.Dir), old, nil
}

// bookkeeping 数据目录里的记录文件（不是用户数据）。
var bookkeeping = map[string]bool{"location.json": true, ".migrated-v1": true, ".migrate.lock": true}

// hasData 目录里有记录文件以外的东西（空目录不算）。
func hasData(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if bookkeeping[e.Name()] {
			continue
		}
		if e.IsDir() {
			if sub, err := os.ReadDir(filepath.Join(dir, e.Name())); err == nil && len(sub) == 0 {
				continue
			}
		}
		return true
	}
	return false
}

// removeBookkeeping 没有数据的旧目录：删掉其中的记录文件与空目录，目录空了就删掉它。
// location.json 不删（它指向用户自己选的数据目录）。
func removeBookkeeping(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		switch {
		case e.Name() == "location.json":
		case bookkeeping[e.Name()]:
			os.Remove(filepath.Join(dir, e.Name()))
		case e.IsDir():
			os.Remove(filepath.Join(dir, e.Name())) // 只删得掉空目录
		}
	}
	os.Remove(dir)
}

// lockFile 独占地创建锁文件（进程异常退出留下的超过 10 分钟的旧锁会被清掉）。
func lockFile(p string) (func(), error) {
	if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > 10*time.Minute {
		os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return func() { os.Remove(p) }, nil
}

// MigrateProject 把旧版本写在配置文件旁边 .ferry/ 里的构建日志与本机部署历史搬到项目目录，
// 搬完且旧目录里没有别的东西时删除它。
func MigrateProject(project, configPath string) (string, error) {
	old := filepath.Join(filepath.Dir(abs(configPath)), ".ferry")
	st, err := os.Stat(old)
	if err != nil || !st.IsDir() {
		return "", nil
	}
	dst := ProjectDir(project, configPath)
	moved := 0
	if logs, err := os.ReadDir(filepath.Join(old, "logs")); err == nil {
		os.MkdirAll(filepath.Join(dst, "logs"), 0o700)
		for _, e := range logs {
			if e.IsDir() {
				continue
			}
			from := filepath.Join(old, "logs", e.Name())
			to := filepath.Join(dst, "logs", e.Name())
			if _, err := os.Stat(to); err == nil {
				os.Remove(from)
				continue
			}
			if err := moveFile(from, to); err != nil {
				return "", err
			}
			moved++
		}
		os.Remove(filepath.Join(old, "logs"))
	}
	hist := filepath.Join(old, "history.jsonl")
	if _, err := os.Stat(hist); err == nil {
		if err := appendFile(hist, filepath.Join(dst, "history.jsonl")); err != nil {
			return "", err
		}
		os.Remove(hist)
		moved++
	}
	os.Remove(old) // 只有空目录才会删掉
	if moved == 0 {
		return "", nil
	}
	return fmt.Sprintf("已把 %s 里的构建日志与部署历史搬到 %s", old, dst), nil
}

// MoveProject 配置文件从 fromConfig 移到 toConfig 后，把项目目录（本机设置、本机部署历史、构建日志）跟着改名。
// 新位置已经有项目目录（以前在那里用过同名项目）时合并：部署历史接在后面，构建日志同名的保留新位置的，
// 本机设置新位置已有就不动。返回搬过去的项目目录；旧位置没有数据时返回空。
func MoveProject(project, fromConfig, toConfig string) (string, error) {
	from, to := ProjectPath(project, fromConfig), ProjectPath(project, toConfig)
	if samePath(from, to) {
		return "", nil
	}
	if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if _, err := os.Stat(to); errors.Is(err, os.ErrNotExist) {
		return to, os.Rename(from, to)
	}
	if ents, err := os.ReadDir(filepath.Join(from, "logs")); err == nil {
		os.MkdirAll(filepath.Join(to, "logs"), 0o700)
		for _, e := range ents {
			src, dst := filepath.Join(from, "logs", e.Name()), filepath.Join(to, "logs", e.Name())
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := os.Rename(src, dst); err != nil {
				return "", err
			}
		}
	}
	if hist := filepath.Join(from, "history.jsonl"); fileExists(hist) {
		if err := appendFile(hist, filepath.Join(to, "history.jsonl")); err != nil {
			return "", err
		}
	}
	if local := filepath.Join(from, "local.json"); fileExists(local) && !fileExists(filepath.Join(to, "local.json")) {
		if err := os.Rename(local, filepath.Join(to, "local.json")); err != nil {
			return "", err
		}
	}
	return to, os.RemoveAll(from)
}

// MoveFile 移动一个文件；不在同一个盘上时复制后删除原文件。
func MoveFile(src, dst string) error { return moveFile(src, dst) }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Move 把数据从 from 复制到 to（已存在的文件不覆盖），全部成功后删除 from 里的数据。
// 默认位置下的 location.json 保留（它记录数据目录在哪）。
func Move(from, to string) error {
	if samePath(from, to) {
		return nil
	}
	if within(abs(to), abs(from)) {
		return errors.New("新位置不能在当前数据目录里面")
	}
	if err := os.MkdirAll(to, 0o700); err != nil {
		return err
	}
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if rel == "." {
			return nil
		}
		if rel == "location.json" {
			return nil
		}
		// 界面缓存（WebView 正在用，会重新生成）与临时文件不搬
		if top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]; top == "webview" || top == "tmp" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if _, err := os.Stat(target); err == nil {
			if rel == "known_hosts" {
				_, err := mergeLines(p, target)
				return err
			}
			return nil
		}
		return copyFile(p, target)
	})
	if err != nil {
		return fmt.Errorf("复制数据失败（原数据未删除）：%w", err)
	}
	entries, _ := os.ReadDir(from)
	for _, e := range entries {
		if e.Name() == "location.json" || e.Name() == "webview" {
			continue
		}
		os.RemoveAll(filepath.Join(from, e.Name()))
	}
	return nil
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// mergeLines 把 src 里 dst 没有的行追加到 dst，返回追加的行数。
func mergeLines(src, dst string) (int, error) {
	in, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	if b, err := os.ReadFile(dst); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			have[strings.TrimSpace(l)] = true
		}
	}
	var add []string
	sc := bufio.NewScanner(strings.NewReader(string(in)))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !have[l] {
			add = append(add, l)
			have[l] = true
		}
	}
	if len(add) == 0 {
		return 0, nil
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(add, "\n") + "\n")
	return len(add), err
}

func appendFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}
