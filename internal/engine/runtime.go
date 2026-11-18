package engine

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/jdk"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/rundist"
	"github.com/captain/ferry/internal/zipx"
)

func (e *Engine) ServerRuntimes(target string) ([]proto.InstalledRuntime, error) {
	var out []proto.InstalledRuntime
	err := e.withRuntimeSession(target, func(s *session) error {
		return s.callData("runtimelist", &proto.Request{}, &out)
	})
	return out, err
}

func (e *Engine) RemoveServerRuntime(target, program string) (*proto.InstalledRuntime, error) {
	if !path.IsAbs(program) || path.Clean(program) != program {
		return nil, errors.New("请指定运行环境清单中的程序绝对路径")
	}
	if strings.HasSuffix(program, "/bin/java") {
		row, err := e.RemoveServerJDK(target, program)
		if err != nil {
			return nil, err
		}
		return &proto.InstalledRuntime{Kind: "jdk", ID: row.ID, Version: row.Version, Arch: row.Arch,
			Home: strings.TrimSuffix(row.Path, "/bin/java"), Path: row.Path, Java: row.Java, Shared: row.Shared, Custom: row.Custom}, nil
	}
	var out proto.InstalledRuntime
	err := e.withRuntimeSession(target, func(s *session) error {
		return s.callData("runtimeremove", &proto.Request{RuntimePath: program}, &out)
	})
	return &out, err
}

func (e *Engine) InstallServerRuntime(target string, choice rundist.Selection, dir, local string) (*proto.InstalledRuntime, error) {
	if local != "" && choice.Version == "" {
		return nil, errors.New("使用本机安装包时请填写完整版本")
	}
	dir = strings.TrimSpace(dir)
	if len(dir) > 1 {
		dir = strings.TrimRight(dir, "/")
	}
	var result proto.InstalledRuntime
	var spec proto.RuntimeSpec
	err := e.withRuntimeSession(target, func(s *session) error {
		choice.Arch = map[string]string{"amd64": "x64", "arm64": "arm64"}[s.arch]
		if err := choice.Validate(); err != nil {
			return err
		}
		base := path.Join(s.ferryDir, "runtimes")
		if dir != "" {
			base = dir
		}
		spec = proto.RuntimeSpec{Kind: choice.Kind, Arch: choice.Arch, Component: choice.Component, Dir: dir}
		if err := s.callData("runtimecheck", &proto.Request{Runtime: &spec}, nil); err != nil {
			return err
		}
		var items []proto.InstalledRuntime
		if err := s.callData("runtimelist", &proto.Request{}, &items); err != nil {
			return err
		}
		for _, item := range items {
			if item.Kind == choice.Kind && item.Component == choice.Component && item.Arch == choice.Arch &&
				path.Dir(item.Home) == base && choice.Matches(item.Version) {
				if !item.Unavailable && (result.ID == "" || rundist.Newer(item.Version, result.Version)) {
					result = item
				}
			}
		}
		if result.ID != "" {
			e.Rep.Info("复用服务器运行环境 %s", result.Path)
		}
		return nil
	})
	if err != nil || result.ID != "" {
		return &result, err
	}
	cache := filepath.Join(e.Settings.RuntimeDir(), "distributions")
	pkg, file, temporary, err := e.runtimePackage(cache, choice, local)
	if err != nil {
		return nil, err
	}
	if temporary {
		defer os.Remove(file)
	}
	spec.ID, spec.Version, spec.SHA256, spec.Size, spec.File = pkg.ID, pkg.Version, pkg.SHA256, pkg.Size, "runtime.tar.gz"
	err = e.withRuntimeSession(target, func(s *session) error {
		var prep proto.Prepared
		if err := s.callData("prepare", &proto.Request{}, &prep); err != nil {
			return err
		}
		e.Rep.Step("上传 %s 到 %s", pkg.ID, target)
		item := remote.UploadItem{Local: file, Remote: remote.Join(prep.Stage, spec.File), Mode: 0o640, Size: pkg.Size}
		if err := e.uploadItems(s, []remote.UploadItem{item}, pkg.Size, pkg.Size); err != nil {
			s.discardStage(prep.Stage)
			return err
		}
		if err := s.callData("runtime", &proto.Request{Stage: prep.Stage, Runtime: &spec}, &result); err != nil {
			return err
		}
		if temporary && result.SHA256 == pkg.SHA256 {
			if _, err := publishRuntimePackage(cache, file, pkg); err != nil {
				e.Rep.Warn("已安装运行环境，本机缓存写入失败：%v", err)
			}
		}
		return nil
	})
	return &result, err
}

type runtimePackage struct {
	ID, Kind, Component, Version, Arch, SHA256 string
	Size                                       int64
}

func (p runtimePackage) archiveIn(dir string) string {
	return filepath.Join(dir, p.ID, p.SHA256+".tar.gz")
}

func cachedRuntime(cache string, choice rundist.Selection) (runtimePackage, string) {
	entries, _ := os.ReadDir(cache)
	var best runtimePackage
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), choice.Kind+"-") {
			continue
		}
		var pkg runtimePackage
		data, err := os.ReadFile(filepath.Join(cache, entry.Name(), "package.json"))
		if err != nil || json.Unmarshal(data, &pkg) != nil || !choice.Matches(pkg.Version) {
			continue
		}
		exact := choice
		exact.Channel, exact.Version = "", pkg.Version
		if pkg.ID != entry.Name() || pkg.ID != exact.ID() || pkg.Kind != choice.Kind || pkg.Component != choice.Component ||
			pkg.Arch != choice.Arch || pkg.Size <= 0 || pkg.Size > rundist.MaxArchive || !zipx.IsSHA256(pkg.SHA256) ||
			(best.ID != "" && !rundist.Newer(pkg.Version, best.Version)) {
			continue
		}
		if sum, size, err := zipx.FileSHA256(pkg.archiveIn(cache)); err == nil && sum == pkg.SHA256 && size == pkg.Size {
			best = pkg
		}
	}
	if best.ID == "" {
		return best, ""
	}
	return best, best.archiveIn(cache)
}

func (e *Engine) runtimePackage(cache string, choice rundist.Selection, local string) (runtimePackage, string, bool, error) {
	var pkg runtimePackage
	var asset rundist.Asset
	if local == "" {
		if saved, file := cachedRuntime(cache, choice); file != "" {
			e.Rep.Info("使用本机缓存 %s", saved.ID)
			return saved, file, false, nil
		}
		var err error
		asset, err = rundist.Resolve(e.ctx(), choice)
		if err != nil {
			return pkg, "", false, fmt.Errorf("查询运行环境安装包失败：%w（也可以选择本机 .tar.gz 安装包）", err)
		}
		choice.Version = asset.Version
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return pkg, "", false, err
	}
	tmp, err := os.CreateTemp(cache, ".runtime-package-")
	if err != nil {
		return pkg, "", false, err
	}
	file := tmp.Name()
	keep := false
	defer func() {
		tmp.Close()
		if !keep {
			os.Remove(file)
		}
	}()
	var sum string
	if local != "" {
		src, err := os.Open(local)
		if err != nil {
			return pkg, "", false, err
		}
		if st, err := src.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > rundist.MaxArchive {
			src.Close()
			return pkg, "", false, errors.New("请选择不超过 1 GiB 的普通 .tar.gz 文件")
		}
		digest := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, digest), io.LimitReader(src, rundist.MaxArchive+1))
		src.Close()
		if err != nil {
			return pkg, "", false, err
		}
		sum = hex.EncodeToString(digest.Sum(nil))
	} else {
		e.Rep.Step("下载 %s", choice.ID())
		var dest io.Writer = &runtimeDownload{file: tmp}
		digest512 := sha512.New()
		if len(asset.Digest) == 128 {
			dest = io.MultiWriter(dest, digest512)
		}
		downloadURL := e.SoftwareSources().Rewrite(asset.URL)
		ctx, cancel := context.WithTimeout(e.ctx(), 20*time.Minute)
		defer cancel()
		// 大包单独限制响应头等待时间，保持慢速下载的总时限。
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 30 * time.Second
		transport.DisableCompression = true
		defer transport.CloseIdleConnections()
		sum, err = jdk.FetchWithClient(ctx, &http.Client{Transport: transport}, downloadURL, dest, func(done, total int64) {
			e.Rep.Progress(choice.ID(), done, total)
		})
		got := sum
		if len(asset.Digest) == 128 {
			got = hex.EncodeToString(digest512.Sum(nil))
		}
		if err == nil && !strings.EqualFold(got, asset.Digest) {
			err = errors.New("运行环境安装包与发行清单的摘要不符")
		}
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				if downloadURL != asset.URL && strings.HasPrefix(asset.URL, "https://github.com/") {
					err = fmt.Errorf("使用 GitHub 加速地址下载 %s 失败：%w（检查设置 → 运行环境 → 安装包中的 GitHub 加速，或清空后重试）", choice.ID(), err)
				}
				err = fmt.Errorf("%w（也可以选择本机 .tar.gz 安装包）", err)
			}
			return pkg, "", false, err
		}
	}
	if err := tmp.Close(); err != nil {
		return pkg, "", false, err
	}
	st, err := os.Stat(file)
	if err != nil {
		return pkg, "", false, err
	}
	size := st.Size()
	if size <= 0 || size > rundist.MaxArchive {
		return pkg, "", false, errors.New("运行环境安装包为空或超过 1 GiB")
	}
	pkg = runtimePackage{ID: choice.ID(), Kind: choice.Kind, Component: choice.Component, Version: choice.Version,
		Arch: choice.Arch, SHA256: sum, Size: size}
	if local != "" {
		keep = true
		return pkg, file, true, nil
	}
	published, err := publishRuntimePackage(cache, file, pkg)
	return pkg, published, false, err
}

type runtimeDownload struct {
	file *os.File
	size int64
}

func (r *runtimeDownload) Write(data []byte) (int, error) {
	if int64(len(data)) > rundist.MaxArchive-r.size {
		return 0, errors.New("运行环境安装包超过 1 GiB")
	}
	n, err := r.file.Write(data)
	r.size += int64(n)
	return n, err
}

func publishRuntimePackage(cache, file string, pkg runtimePackage) (string, error) {
	dest := pkg.archiveIn(cache)
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(file, dest); err != nil {
		if sum, size, check := zipx.FileSHA256(dest); check != nil || sum != pkg.SHA256 || size != pkg.Size {
			return "", err
		}
	}
	data, err := json.Marshal(pkg)
	if err != nil {
		return "", err
	}
	return dest, appdir.WriteFileAtomic(filepath.Join(dir, "package.json"), data, 0o600)
}
