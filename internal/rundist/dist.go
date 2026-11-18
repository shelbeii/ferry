// Package rundist selects official Linux runtime distributions.
package rundist

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/captain/ferry/internal/jdk"
)

const MaxArchive = 1 << 30

type Selection struct {
	Kind, Component, Channel, Version, Arch string
}

func (s *Selection) Validate() error {
	switch s.Kind {
	case "node", "python":
		if s.Component != "" {
			return fmt.Errorf("只有 .NET 支持选择安装组件")
		}
	case "dotnet":
		if s.Component == "" {
			s.Component = "aspnetcore"
		}
		if s.Component != "aspnetcore" && s.Component != "runtime" && s.Component != "sdk" {
			return fmt.Errorf(".NET 组件只能是 aspnetcore、runtime 或 sdk")
		}
	default:
		return fmt.Errorf("运行环境类型只能是 node、python 或 dotnet")
	}
	if s.Arch != "x64" && s.Arch != "arm64" {
		return fmt.Errorf("运行环境支持 Linux x64 与 arm64，目标架构为 %s", s.Arch)
	}
	if s.Version != "" {
		if s.Channel != "" || !ValidVersion(s.Version) {
			return fmt.Errorf("请单独指定完整版本，例如 --version 10.0.15")
		}
	} else if !channelPattern.MatchString(s.Channel) || strings.Contains(s.Channel, ".") != (s.Kind != "node") {
		return fmt.Errorf("请指定版本系列（Node.js 如 24，Python 如 3.14，.NET 如 10.0），或指定完整版本")
	}
	return nil
}

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,4})\.(0|[1-9][0-9]{0,4})\.(0|[1-9][0-9]{0,4})$`)
	channelPattern = regexp.MustCompile(`^[1-9][0-9]{0,4}(\.(0|[1-9][0-9]{0,4}))?$`)
)

func ValidVersion(version string) bool { return versionPattern.MatchString(version) }

func VersionParts(version string) [3]int {
	var out [3]int
	for i, v := range strings.SplitN(version, ".", 3) {
		out[i], _ = strconv.Atoi(v)
	}
	return out
}

func Newer(version, other string) bool {
	a, b := VersionParts(version), VersionParts(other)
	return slices.Compare(a[:], b[:]) > 0
}

func (s Selection) Matches(version string) bool {
	if !ValidVersion(version) {
		return false
	}
	if s.Version != "" {
		return s.Version == version
	}
	return strings.HasPrefix(version, s.Channel+".")
}

func (s Selection) ID() string {
	prefix := s.Kind
	if s.Component != "" {
		prefix += "-" + s.Component
	}
	return prefix + "-" + s.Version + "-" + s.Arch
}

func Executable(kind string) string {
	switch kind {
	case "node":
		return "bin/node"
	case "python":
		return "bin/python3"
	case "dotnet":
		return "dotnet"
	}
	return ""
}

type Asset struct {
	Version, URL, Digest string
}

func (a Asset) Valid() bool {
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || !strings.HasSuffix(u.Path, ".tar.gz") {
		return false
	}
	_, err = hex.DecodeString(a.Digest)
	return err == nil && (len(a.Digest) == 64 || len(a.Digest) == 128)
}

func Resolve(ctx context.Context, s Selection) (Asset, error) {
	if err := s.Validate(); err != nil {
		return Asset{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	switch s.Kind {
	case "node":
		return resolveNode(ctx, s)
	case "python":
		return resolvePython(ctx, s)
	case "dotnet":
		return resolveDotnet(ctx, s)
	}
	return Asset{}, fmt.Errorf("未知的运行环境类型 %s", s.Kind)
}

func resolveNode(ctx context.Context, s Selection) (Asset, error) {
	const base = "https://nodejs.org/dist/"
	version := s.Version
	if version == "" {
		var releases []struct {
			Version string
			Files   []string
			LTS     json.RawMessage
		}
		if err := readJSON(ctx, base+"index.json", &releases); err != nil {
			return Asset{}, err
		}
		for _, release := range releases {
			v := strings.TrimPrefix(release.Version, "v")
			if s.Matches(v) && len(release.LTS) > 0 && release.LTS[0] == '"' &&
				slices.Contains(release.Files, "linux-"+s.Arch) && Newer(v, version) {
				version = v
			}
		}
	}
	if version == "" {
		return Asset{}, fmt.Errorf("没有找到 Node.js %s 系列的 Linux %s LTS 发行包", s.Channel, s.Arch)
	}
	dir := base + "v" + version + "/"
	file := "node-v" + version + "-linux-" + s.Arch + ".tar.gz"
	data, err := jdk.Get(ctx, dir+"SHASUMS256.txt", 1<<20)
	if err != nil {
		return Asset{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == file && len(fields[0]) == 64 {
			if a := (Asset{version, dir + file, fields[0]}); a.Valid() {
				return a, nil
			}
		}
	}
	return Asset{}, fmt.Errorf("Node.js 发布清单没有 %s 的 SHA-256", file)
}

func resolvePython(ctx context.Context, s Selection) (Asset, error) {
	const index = "https://releases.astral.sh/github/versions/main/v1/python-build-standalone.ndjson"
	const limit = 96 << 20
	data, err := jdk.Get(ctx, index, limit+1)
	if err != nil {
		return Asset{}, err
	}
	if len(data) > limit {
		return Asset{}, fmt.Errorf("Astral 版本清单超出大小限制")
	}
	return pythonAsset(data, s)
}

func pythonAsset(data []byte, s Selection) (Asset, error) {
	arch := map[string]string{"x64": "x86_64", "arm64": "aarch64"}[s.Arch]
	platform := arch + "-unknown-linux-gnu"
	var best Asset
	bestDate := ""
	lines := bufio.NewScanner(bytes.NewReader(data))
	lines.Buffer(make([]byte, 64<<10), 2<<20)
	for lines.Scan() {
		if len(bytes.TrimSpace(lines.Bytes())) == 0 {
			continue
		}
		var release struct {
			Version, Date string
			Artifacts     []struct {
				Platform, Variant, URL, SHA256 string
				Format                         string `json:"archive_format"`
			}
		}
		if err := json.Unmarshal(lines.Bytes(), &release); err != nil {
			return Asset{}, fmt.Errorf("读取 Astral 版本清单：%w", err)
		}
		v, _, _ := strings.Cut(release.Version, "+")
		if !s.Matches(v) || (best.Version != "" && Newer(best.Version, v)) ||
			(v == best.Version && release.Date <= bestDate) {
			continue
		}
		for _, item := range release.Artifacts {
			if item.Platform != platform || item.Format != "tar.gz" ||
				(item.Variant != "install_only_stripped" && item.Variant != "install_only") || len(item.SHA256) != 64 {
				continue
			}
			if a := (Asset{v, item.URL, item.SHA256}); a.Valid() {
				best, bestDate = a, release.Date
				if item.Variant == "install_only_stripped" {
					break
				}
			}
		}
	}
	if err := lines.Err(); err != nil {
		return Asset{}, err
	}
	if best.Version == "" {
		return Asset{}, fmt.Errorf("Astral 清单没有符合条件的 Python Linux %s 安装包", s.Arch)
	}
	return best, nil
}

type dotnetRelease struct {
	Version string
	Files   []struct {
		Name, RID, URL, Hash string
	}
}

func resolveDotnet(ctx context.Context, s Selection) (Asset, error) {
	channel := s.Channel
	if channel == "" {
		channel = s.Version[:strings.LastIndexByte(s.Version, '.')]
	}
	var data struct {
		Releases []struct {
			ASPNet  dotnetRelease `json:"aspnetcore-runtime"`
			Runtime dotnetRelease
			SDK     dotnetRelease
			SDKs    []dotnetRelease
		}
	}
	const base = "https://builds.dotnet.microsoft.com/dotnet/release-metadata/"
	if err := readJSON(ctx, base+channel+"/releases.json", &data); err != nil {
		return Asset{}, err
	}
	name := map[string]string{"aspnetcore": "aspnetcore-runtime", "runtime": "dotnet-runtime", "sdk": "dotnet-sdk"}[s.Component] +
		"-linux-" + s.Arch + ".tar.gz"
	var best Asset
	for _, r := range data.Releases {
		var options []dotnetRelease
		switch s.Component {
		case "aspnetcore":
			options = []dotnetRelease{r.ASPNet}
		case "runtime":
			options = []dotnetRelease{r.Runtime}
		case "sdk":
			options = append(r.SDKs, r.SDK)
		}
		for _, option := range options {
			if !s.Matches(option.Version) || !Newer(option.Version, best.Version) {
				continue
			}
			for _, f := range option.Files {
				if f.RID == "linux-"+s.Arch && f.Name == name && len(f.Hash) == 128 {
					if a := (Asset{option.Version, f.URL, f.Hash}); a.Valid() {
						best = a
						break
					}
				}
			}
		}
	}
	if best.Version == "" {
		return Asset{}, fmt.Errorf(".NET %s 清单没有符合条件的 %s Linux %s 安装包", channel, s.Component, s.Arch)
	}
	return best, nil
}

func readJSON(ctx context.Context, addr string, out any) error {
	data, err := jdk.Get(ctx, addr, 24<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("读取发行清单 %s：%w", path.Base(addr), err)
	}
	return nil
}
