package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// Ferry 提供的 nginx（静态站点 static.port）：装在部署根目录下，与系统里的 nginx、其他项目互不影响。
//
//	<root>/runtimes/nginx-1.30.5-x86_64/nginx                 静态链接的单个程序（root 所有）
//	<root>/runtimes/nginx-1.30.5-x86_64/.ferry-runtime.json
//	<root>/runtimes/nginx-1.30 → nginx-1.30.5-x86_64           指定了系列（static.nginx）的应用用它
//	<root>/runtimes/nginx      → nginx-1.30.5-x86_64           没指定系列的应用用它

var (
	reNginxID   = regexp.MustCompile(`^nginx-\d+\.\d+\.\d+-[a-z0-9_]+$`)
	reNginxLink = regexp.MustCompile(`^nginx(-\d+\.\d+)?$`)
)

func (a *Agent) nginxDir() string { return filepath.Join(filepath.Clean(a.req.Root), "runtimes") }

// installedNginx 部署根目录下已装的 nginx：链接名 → 指向的版本。
func (a *Agent) installedNginx() map[string]proto.Runtime {
	out := map[string]proto.Runtime{}
	dir := a.nginxDir()
	ents, _ := os.ReadDir(dir)
	links := map[string]bool{"nginx": true}
	for _, e := range ents {
		if !e.IsDir() || !reNginxID.MatchString(e.Name()) {
			continue
		}
		if m := readNginxMeta(filepath.Join(dir, e.Name())); m.Version != "" {
			if p := strings.SplitN(m.Version, ".", 3); len(p) == 3 {
				links["nginx-"+p[0]+"."+p[1]] = true
			}
		}
	}
	for l := range links {
		target, err := a.Sys.ReadLink(filepath.Join(dir, l))
		if err != nil || !reNginxID.MatchString(target) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, target, "nginx")); err != nil {
			continue
		}
		out[l] = proto.Runtime{ID: target, Version: readNginxMeta(filepath.Join(dir, target)).Version}
	}
	return out
}

func readNginxMeta(dir string) proto.Runtime {
	var m proto.Runtime
	if b, err := os.ReadFile(filepath.Join(dir, ".ferry-runtime.json")); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// installNginx 安装上传目录里的 nginx：校验 sha256 与架构，放进版本目录，再更新链接。
func (a *Agent) installNginx(r *proto.RuntimeSpec) int {
	if !reNginxID.MatchString(r.ID) || !reNginxLink.MatchString(r.Link) || !reSafe.MatchString(r.File) || !zipx.IsSHA256(r.SHA256) {
		return a.result(proto.CodeBadInput, "nginx 信息无效", nil)
	}
	if err := validateRoot(a.req.Root); err != nil {
		return a.result(proto.CodeBadInput, err.Error(), nil)
	}
	stage := filepath.Clean(a.req.Stage)
	if filepath.Dir(stage) != a.incomingDir() {
		return a.result(proto.CodeBadInput, "上传目录不在 "+a.incomingDir()+" 下", nil)
	}
	defer os.RemoveAll(stage)
	dir := a.nginxDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return a.result(proto.CodeFailed, err.Error(), nil)
	}
	unlock, err := a.Sys.Lock(filepath.Join(dir, ".lock"), 10*time.Minute)
	if err != nil {
		return a.result(proto.CodeLocked, "另一个部署正在安装运行环境："+err.Error(), nil)
	}
	defer unlock()

	final := filepath.Join(dir, r.ID)
	if _, err := os.Stat(filepath.Join(final, "nginx")); err != nil {
		src := filepath.Join(stage, r.File)
		got, size, err := zipx.FileSHA256(src)
		if err != nil || got != r.SHA256 || size != r.Size {
			return a.result(proto.CodeVerify, "上传的 nginx 校验失败（传输中损坏），请重新部署", nil)
		}
		if err := checkELFArch(src); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		tmp := filepath.Join(dir, ".tmp-"+r.ID)
		os.RemoveAll(tmp)
		if err := os.Mkdir(tmp, 0o755); err != nil {
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		if err := moveFile(src, filepath.Join(tmp, "nginx")); err != nil {
			os.RemoveAll(tmp)
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
		os.Chmod(filepath.Join(tmp, "nginx"), 0o755)
		a.Sys.Lchown(filepath.Join(tmp, "nginx"), 0, 0)
		meta, _ := json.Marshal(proto.Runtime{ID: r.ID, Version: r.Version})
		os.WriteFile(filepath.Join(tmp, ".ferry-runtime.json"), meta, 0o644)
		os.RemoveAll(final)
		if err := os.Rename(tmp, final); err != nil {
			os.RemoveAll(tmp)
			return a.result(proto.CodeFailed, err.Error(), nil)
		}
	}
	if err := a.Sys.SwitchLink(filepath.Join(dir, r.Link), r.ID); err != nil {
		return a.result(proto.CodeFailed, "更新 "+r.Link+" 链接失败："+err.Error(), nil)
	}
	a.info(proto.EvRuntime, "", "nginx %s 已安装：%s", r.Version, final)
	return a.result(proto.CodeOK, "", proto.Runtime{ID: r.ID, Version: r.Version})
}

// checkELFArch 必须是这台服务器能运行的 Linux 程序（看 ELF 头里的机器类型）。
func checkELFArch(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	h := make([]byte, 20)
	if _, err := io.ReadFull(f, h); err != nil || string(h[:4]) != "\x7fELF" {
		return fmt.Errorf("上传的 nginx 不是 Linux 程序")
	}
	machine := map[uint16]string{0x3E: "amd64", 0xB7: "arm64", 0x15: "ppc64le"}[binary.LittleEndian.Uint16(h[18:20])]
	if goruntime.GOOS == "linux" && machine != goruntime.GOARCH {
		return fmt.Errorf("上传的 nginx 是 %s 的程序，这台服务器是 %s", orNone(machine), goruntime.GOARCH)
	}
	return nil
}
