package agent

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var systemBundleFilename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~:%-]{0,239}$`)

func checkSystemBundle(manager, bundle string) error {
	switch bundle {
	case "":
		return nil
	case "rpm":
		if manager == "dnf" || manager == "yum" {
			return nil
		}
	case "deb":
		if manager == "apt-get" {
			return nil
		}
	}
	return fmt.Errorf("%s 离线包不适用于此服务器的软件包管理器 %s", strings.ToUpper(bundle), manager)
}

func unpackSystemBundle(src, dst, bundle string) ([]string, error) {
	z, err := zip.OpenReader(src)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	var files []string
	var size uint64
	seen := map[string]bool{}
	for _, entry := range z.File {
		if !strings.EqualFold(path.Ext(entry.Name), "."+bundle) {
			continue
		}
		name := path.Base(entry.Name)
		if !entry.Mode().IsRegular() || !systemBundleFilename.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("离线软件包文件名无效或重复：%q", name)
		}
		if len(files) >= 512 || entry.UncompressedSize64 > (2<<30)-size {
			return nil, errors.New("离线包集合超过 512 个包或 2 GiB 解压大小")
		}
		size += entry.UncompressedSize64
		seen[name] = true
		local := filepath.Join(dst, name)
		if err := extractSystemPackage(entry, local, bundle); err != nil {
			return nil, fmt.Errorf("%s：%w", name, err)
		}
		files = append(files, local)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("离线包集合里没有 .%s 文件", bundle)
	}
	return files, nil
}

func extractSystemPackage(entry *zip.File, dst, bundle string) error {
	r, err := entry.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	magic := []byte("!<arch>\n")
	if bundle == "rpm" {
		magic = []byte{0xed, 0xab, 0xee, 0xdb}
	}
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(r, head); err != nil || !bytes.Equal(head, magic) {
		return fmt.Errorf("文件不是有效的 %s 软件包", strings.ToUpper(bundle))
	}
	return writeEntry(dst, io.MultiReader(bytes.NewReader(head), r), 0o600)
}

// checkSingleSystemPackage 校验单个 .rpm / .deb 离线包文件头并复制到临时目录供包管理器使用。
func prepareSingleSystemPackage(src, dst, kind, fileName string) ([]string, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	magic := []byte("!<arch>\n")
	if kind == "rpm" {
		magic = []byte{0xed, 0xab, 0xee, 0xdb}
	}
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, magic) {
		return nil, fmt.Errorf("文件不是有效的 %s 软件包", strings.ToUpper(kind))
	}
	name := path.Base(fileName)
	if !systemBundleFilename.MatchString(name) {
		name = "package." + kind
	}
	local := filepath.Join(dst, name)
	if err := writeEntry(local, io.MultiReader(bytes.NewReader(head), f), 0o600); err != nil {
		return nil, err
	}
	return []string{local}, nil
}

func packageArguments(pkgs, offline []string) string {
	if len(offline) == 0 {
		return strings.Join(pkgs, " ")
	}
	args := make([]string, 0, len(offline))
	for _, path := range offline {
		args = append(args, shellQuote(path))
	}
	return strings.Join(args, " ")
}

// packageManagerArgs 离线包安装时默认关掉软件源（只用包里的本地文件）；当 useRepos 为 true 时允许结合软件源补齐基础依赖。
// apt 的 --no-download 在 apt 2.4（Ubuntu 22.04）里和本地 .deb 一起用会报
// "Pathname to install is not absolute"，所以 apt 改用不加载任何软件源。
func packageManagerArgs(manager string, offline []string, useRepos bool) string {
	if len(offline) == 0 || useRepos {
		return manager
	}
	if manager == "apt-get" {
		return manager + " -o Dir::Etc::sourcelist=/dev/null -o Dir::Etc::sourceparts=/dev/null"
	}
	return manager + " --disablerepo='*'"
}
