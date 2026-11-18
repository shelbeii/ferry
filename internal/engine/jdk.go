package engine

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/jdk"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
)

func (e *Engine) jdkPackage(s *session, java int, version string) (*jdk.Meta, error) {
	arch, lib := jdk.ArchOf(s.arch), e.Settings.RuntimeDir()
	label := version
	var m *jdk.Meta
	if version == "" {
		label = strconv.Itoa(java)
		m = jdk.Find(lib, java, arch)
	} else {
		m = jdk.FindVersion(lib, version, arch)
	}
	if m != nil {
		return m, nil
	}
	src := e.Settings.Runtime.Source
	if src == "" {
		src = jdk.DefaultSource
	}
	e.Rep.Step("下载 JDK %s（linux/%s，%s）", label, arch, src)
	progress := func(done, total int64) { e.Rep.Progress("JDK "+label, done, total) }
	var err error
	if version == "" {
		m, err = jdk.Download(e.ctx(), src, java, arch, lib, progress)
	} else {
		m, err = jdk.DownloadVersion(e.ctx(), src, version, arch, lib, progress)
	}
	if err != nil {
		return nil, fmt.Errorf("下载 JDK %s 失败：%w（可以在设置 → 运行环境里换下载源，或手动导入下载好的压缩包）", label, err)
	}
	e.Rep.Info("已下载 %s（%s），存放在 %s", m.Version, humanBytes(m.Size), lib)
	return m, nil
}

func (e *Engine) uploadJDK(s *session, m *jdk.Meta, spec proto.RuntimeSpec) (proto.Runtime, error) {
	e.Rep.Step("上传 JDK %s 到 %s", m.Version, s.t.Name)
	var prep proto.Prepared
	if err := s.callData("prepare", &proto.Request{}, &prep); err != nil {
		return proto.Runtime{}, err
	}
	item := remote.UploadItem{Local: filepath.Join(e.Settings.RuntimeDir(), m.File), Remote: remote.Join(prep.Stage, m.File), Mode: 0o640, Size: m.Size}
	if err := e.uploadItems(s, []remote.UploadItem{item}, m.Size, m.Size); err != nil {
		s.discardStage(prep.Stage)
		return proto.Runtime{}, fmt.Errorf("上传 JDK 失败：%w", err)
	}
	spec.ID, spec.Java, spec.Version = m.ID, m.Java, m.Version
	spec.File, spec.SHA256, spec.Size = m.File, m.SHA256, m.Size
	var r proto.Runtime
	if err := s.callData("runtime", &proto.Request{Stage: prep.Stage, Runtime: &spec}, &r); err != nil {
		return proto.Runtime{}, fmt.Errorf("安装 JDK %s 失败：%w", m.Version, err)
	}
	return r, nil
}

// JDKs 列出目标服务器上 Ferry 管理的 JDK。
func (e *Engine) JDKs(target string) ([]proto.InstalledJDK, error) {
	var out []proto.InstalledJDK
	err := e.withRuntimeSession(target, func(s *session) error {
		return s.callData("jdklist", &proto.Request{}, &out)
	})
	return out, err
}

// RemoveServerJDK 使用清单中的实际路径区分同版本的多个安装。
func (e *Engine) RemoveServerJDK(target, javaPath string) (*proto.InstalledJDK, error) {
	home, ok := strings.CutSuffix(javaPath, "/bin/java")
	if !ok || !path.IsAbs(home) || path.Clean(home) != home {
		return nil, errors.New("请指定运行环境清单中的 Java 绝对路径")
	}
	var out proto.InstalledJDK
	err := e.withRuntimeSession(target, func(s *session) error {
		r := &proto.RuntimeSpec{Dir: path.Dir(home), ID: path.Base(home)}
		return s.callData("jdkremove", &proto.Request{Runtime: r}, &out)
	})
	return &out, err
}

// InstallServerJDK 单独预装 JDK。选大版本时复用共享入口或服务器上已有的版本。
func (e *Engine) InstallServerJDK(target string, java int, version, dir string) (*proto.InstalledJDK, error) {
	if version != "" {
		if java != 0 || !jdk.ValidVersion(version) {
			return nil, errors.New("请单独指定完整 JDK 版本，例如 --version 17.0.13+11")
		}
		java = jdk.Major(version)
	}
	if java < 8 || java > 99 {
		return nil, errors.New("请指定 JDK 大版本（如 --java 21），或填写完整版本（如 --version 21.0.5+11）")
	}
	dir = strings.TrimSpace(dir)
	if strings.HasPrefix(dir, "/") {
		dir = "/" + strings.TrimRight(dir[1:], "/")
	}
	var result *proto.InstalledJDK
	err := e.withRuntimeSession(target, func(s *session) error {
		sharedDir := path.Join(s.ferryDir, "runtimes")
		if dir == sharedDir {
			dir = ""
		}
		read := &proto.Request{Runtime: &proto.RuntimeSpec{Dir: dir}}
		var list []proto.InstalledJDK
		if err := s.callData("jdklist", read, &list); err != nil {
			return err
		}
		arch := jdk.ArchOf(s.arch)
		var existing *proto.InstalledJDK
		for i := range list {
			r := &list[i]
			if r.Java != java || r.Arch != arch || (dir == "" && r.Custom) {
				continue
			}
			if version != "" {
				if jdk.SameVersion(r.Version, version) || jdk.MatchID(r.ID, version, arch) {
					if dir == "" {
						result = r
						return nil
					}
					existing = r
					break
				}
				continue
			}
			if r.Shared {
				result = r
				return nil
			}
			if existing == nil {
				existing = r
			}
		}
		var id string
		spec := proto.RuntimeSpec{KeepDefault: true, Dir: dir}
		if existing != nil {
			spec.ID, spec.Java, spec.Version = existing.ID, existing.Java, existing.Version
			if err := s.callData("runtime", &proto.Request{Runtime: &spec}, nil); err != nil {
				return err
			}
			id = existing.ID
		} else {
			m, err := e.jdkPackage(s, java, version)
			if err != nil {
				return err
			}
			if _, err := e.uploadJDK(s, m, spec); err != nil {
				return err
			}
			id = m.ID
		}
		if err := s.callData("jdklist", read, &list); err != nil {
			return err
		}
		for i := range list {
			if list[i].ID == id && (dir != "" || !list[i].Custom) {
				result = &list[i]
				return nil
			}
		}
		return fmt.Errorf("服务器未返回已安装的 %s", id)
	})
	return result, err
}

func (e *Engine) withRuntimeSession(target string, f func(*session) error) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	s, err := e.connect(t)
	if err != nil {
		return err
	}
	defer s.close()
	return f(s)
}
