package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// 静态站点只传变化的文件：prepare 时给出当前版本的文件清单；部署时在上传目录里拼出完整的站点
// （新传的文件 + 从当前版本复制没变的），逐个核对 sha256，全部对上才放进版本目录。
// 任何一个对不上（比如有人在服务器上改过文件）都按「上传文件校验未通过」处理：不动应用，本机改为全量上传重试。

const (
	siteManifest = "site.json" // 版本记录里站点的文件清单
	siteMaxFiles = 50000       // 文件比这还多的站点不算清单（一次全量上传）
)

// siteDir 版本里站点文件所在的目录：由 Ferry 的 nginx 运行的在 www/ 下。
func (a *Agent) siteDir(app, id string) string {
	dir := a.releaseDir(app, id)
	if www := filepath.Join(dir, dirWWW); realDir(www) && exists(filepath.Join(a.verMetaDir(app, id), "nginx.conf")) {
		return www
	}
	return dir
}

// siteFiles 应用当前版本的文件清单：版本记录里存了就用它，没有（以前部署的版本）就现算。没有当前版本时为空。
func (a *Agent) siteFiles(app string) []proto.SiteFile {
	cur := a.current(app)
	if cur == "" {
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(a.verMetaDir(app, cur), siteManifest)); err == nil {
		var files []proto.SiteFile
		if json.Unmarshal(b, &files) == nil {
			return files
		}
	}
	files, err := hashSite(a.siteDir(app, cur))
	if err != nil {
		return nil
	}
	return files
}

var errTooManyFiles = errors.New("文件太多")

// hashSite 算目录里各文件的 sha256（只看普通文件，不跟符号链接）。
func hashSite(dir string) ([]proto.SiteFile, error) {
	var files []proto.SiteFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(files) >= siteMaxFiles {
			return errTooManyFiles
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		sha, size, err := zipx.FileSHA256(p)
		if err != nil {
			return err
		}
		files = append(files, proto.SiteFile{Path: filepath.ToSlash(rel), SHA256: sha, Size: size})
		return nil
	})
	return files, err
}

// readSiteManifest 读上传的文件清单并检查路径（相对、不越界）。
func readSiteManifest(p string) ([]proto.SiteFile, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var files []proto.SiteFile
	if err := json.Unmarshal(b, &files); err != nil {
		return nil, fmt.Errorf("文件清单格式不对：%v", err)
	}
	for _, f := range files {
		c := path.Clean(f.Path)
		if c != f.Path || c == "." || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") || strings.Contains(c, `\`) {
			return nil, fmt.Errorf("文件清单里有无效的路径：%q", f.Path)
		}
	}
	return files, nil
}

// siteStage 拼装站点用的目录（在上传目录里）。
func siteStage(stage, app string) string { return filepath.Join(stage, app+".site") }

// assembleSite 在上传目录里拼出完整站点：解开新传的文件，没传的从当前版本复制，再按清单逐个核对。
func (a *Agent) assembleSite(s proto.AppSpec, stage string, r proto.ReleaseSpec) error {
	files, err := readSiteManifest(filepath.Join(stage, r.Site))
	if err != nil {
		return err
	}
	cur := a.current(s.Name)
	if cur == "" {
		return errors.New("服务器上没有当前版本，没法沿用没变的文件")
	}
	from := a.siteDir(s.Name, cur)
	dst := siteStage(stage, s.Name)
	os.RemoveAll(dst)
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	if err := extractTarGz(filepath.Join(stage, r.File), dst); err != nil {
		return fmt.Errorf("解压失败：%v", err)
	}
	want := make(map[string]bool, len(files))
	copied := 0
	for _, f := range files {
		want[f.Path] = true
		p := filepath.Join(dst, filepath.FromSlash(f.Path))
		if exists(p) {
			continue
		}
		src := filepath.Join(from, filepath.FromSlash(f.Path))
		if !within(from, src) {
			return fmt.Errorf("路径越界：%s", f.Path)
		}
		if err := copySiteFile(src, p); err != nil {
			return fmt.Errorf("当前版本里没有 %s 或读不了（%v）", f.Path, err)
		}
		copied++
	}
	// 逐个核对：新传的、复制来的都要和本机的一样；多出来的文件也不行
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		if !want[filepath.ToSlash(rel)] {
			return fmt.Errorf("多出来的文件：%s", filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, f := range files {
		sha, size, err := zipx.FileSHA256(filepath.Join(dst, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		if sha != f.SHA256 || size != f.Size {
			return fmt.Errorf("%s 和本机的不一样（服务器上的文件被改过？）", f.Path)
		}
	}
	a.info(proto.EvAssemble, s.Name, "%d 个文件沿用当前版本，%d 个新传，全部核对一致", copied, len(files)-copied)
	return nil
}

// copySiteFile 复制一个普通文件（不跟符号链接），权限 0644，需要时建上层目录（0755）。
func copySiteFile(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("不是普通文件")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// placeSite 把拼好的站点移进版本目录的站点目录 site（已建好的空目录）。
func placeSite(stage, app, site string) error {
	src := siteStage(stage, app)
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := moveTree(filepath.Join(src, e.Name()), filepath.Join(site, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
