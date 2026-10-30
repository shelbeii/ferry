// Package agentbin 内嵌各架构的 ferry-agent（由 scripts/build 先交叉编译到 bin/）。
package agentbin

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed all:bin
var bins embed.FS

// Binary 某个架构的 agent。
type Binary struct {
	Arch   string
	Data   []byte
	SHA256 string
}

// Get 取 linux/<arch> 的 agent。环境变量 FERRY_AGENT_DIR 指向目录时优先从那里读（开发调试用）。
func Get(arch string) (*Binary, error) {
	name := "ferry-agent-linux-" + arch
	var data []byte
	var err error
	if dir := os.Getenv("FERRY_AGENT_DIR"); dir != "" {
		data, err = os.ReadFile(filepath.Join(dir, name))
	} else {
		data, err = bins.ReadFile("bin/" + name)
	}
	if err != nil {
		return nil, fmt.Errorf("没有 %s 的 agent（%s）：先运行 scripts/build.ps1 或 scripts/build.sh 构建 ferry", arch, name)
	}
	sum := sha256.Sum256(data)
	return &Binary{Arch: arch, Data: data, SHA256: hex.EncodeToString(sum[:])}, nil
}
