package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/zipx"
)

// KeepItemReport 一项保留在服务器上、本机包里的情况，和这次部署会怎么做（Note）。
type KeepItemReport struct {
	proto.KeepState
	// Package 本机产物里：file | dir | none；通配符的、java / binary（包里只有程序和附带文件）为空
	Package string `json:"package,omitempty"`
	Differs bool   `json:"differs,omitempty"` // 服务器上的文件和包里的不同：源码里改的这次不会上线
	Note    string `json:"note"`
}

// KeepReport 一个应用部署时保留的（keep scan）：各项的情况；Dir 不为空时还有服务器上运行中的版本里这个目录下的。
type KeepReport struct {
	App     string            `json:"app"`
	Type    string            `json:"type"`
	Release string            `json:"release,omitempty"` // 服务器上运行中的版本；空 = 还没部署过
	Items   []KeepItemReport  `json:"items"`
	Entries []proto.KeepEntry `json:"entries,omitempty"`
	More    bool              `json:"more,omitempty"`
}

// KeepScan 部署前看要保留的在服务器上的情况（只读）。keep 为 nil 时按 ferry.yaml；dir 是要列出的目录（"." = 最上层，空 = 不列）。
func (e *Engine) KeepScan(target, app string, keep []*config.KeepItem, dir string) (*KeepReport, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	apps, err := e.File.Select(t, []string{app})
	if err != nil {
		return nil, err
	}
	if len(apps) != 1 {
		return nil, fmt.Errorf("没有应用 %s", app)
	}
	a := apps[0]
	spec, err := Spec(e.File, t, a)
	if err != nil {
		return nil, err
	}
	if keep != nil {
		if spec.Keep, err = KeepSpec(a, spec.FileName, keep); err != nil {
			return nil, err
		}
	}
	if dir != "" && dir != "." {
		dir = proto.CleanKeepPath(dir)
	}
	s, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	var out []proto.KeepScan
	if err := s.callData("keepscan", &proto.Request{Apps: []proto.AppSpec{spec}, KeepDir: dir}, &out); err != nil {
		return nil, err
	}
	r := &KeepReport{App: a.Name, Type: a.Type}
	if len(out) == 1 {
		r.Release, r.Entries, r.More = out[0].Release, out[0].Entries, out[0].More
		r.Items = keepReports(e.File, t, a, out[0])
	}
	return r, nil
}

// keepReports 服务器上的情况加上本机包里的，写好说明。
func keepReports(f *config.File, t *config.Target, a *config.App, ks proto.KeepScan) []KeepItemReport {
	local := ""
	if a.Type == proto.TypeStatic || proto.DirService(a.Type) {
		local, _ = LocalArtifact(f, t, a)
	}
	var out []KeepItemReport
	for _, st := range ks.States {
		r := KeepItemReport{KeepState: st}
		if local != "" && !strings.ContainsAny(st.Path, "*?[") {
			r.Package = "none"
			p := filepath.Join(local, filepath.FromSlash(st.Path))
			if fi, err := os.Stat(p); err == nil {
				if fi.IsDir() {
					r.Package = "dir"
				} else {
					r.Package = "file"
					if st.Server == "file" && st.SHA256 != "" {
						if sha, _, err := zipx.FileSHA256(p); err == nil && sha != st.SHA256 {
							r.Differs = true
						}
					}
				}
			}
		}
		r.Note = keepNote(r, ks.Release != "")
		out = append(out, r)
	}
	return out
}

// keepNote 这次部署对这一项会怎么做。
func keepNote(r KeepItemReport, deployed bool) string {
	inPkg := r.Package == "file" || r.Package == "dir"
	if r.Shared {
		switch {
		case r.Data:
			return "用共用目录里的（data/" + r.Path + "），新版本里放一个链接"
		case r.Server == "file" || r.Server == "dir":
			return "第一次共用：复制服务器上的到 data/" + r.Path
		case inPkg:
			return "第一次共用：用包里的放进 data/" + r.Path
		}
		return "第一次共用：建一个空目录 data/" + r.Path
	}
	switch r.Server {
	case "file":
		switch {
		case r.Differs:
			return "用服务器上的（和包里的不同：包里改的这次不会上线）"
		case r.Package == "file":
			return "用服务器上的（和包里的一样）"
		case r.Package == "none":
			return "用服务器上的（包里没有）"
		}
		return "用服务器上的"
	case "dir":
		return fmt.Sprintf("用服务器上的目录（%d 个文件）", r.Size)
	case "link":
		return "用服务器上的链接"
	case "many":
		return fmt.Sprintf("用服务器上匹配到的 %d 个", r.Matches)
	}
	switch {
	case !deployed:
		return "还没部署过：用包里的"
	case inPkg:
		return "服务器上没有：用包里的"
	case r.Package == "none":
		return "服务器上和包里都没有"
	}
	return "服务器上没有匹配的：用包里的"
}

// SetKeep 把这次选的保留写回 ferry.yaml（应用的 artifact.keep）。
func (e *Engine) SetKeep(app string, keep []*config.KeepItem) error {
	var a *config.App
	for _, x := range e.File.Apps {
		if strings.EqualFold(x.Name, app) {
			a = x
		}
	}
	if a == nil {
		return fmt.Errorf("没有应用 %s", app)
	}
	if _, err := KeepSpec(a, "", keep); err != nil {
		return err
	}
	return config.SetAppKeep(e.File.Path, a.Name, keep)
}
