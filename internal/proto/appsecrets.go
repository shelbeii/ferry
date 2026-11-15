package proto

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const AppSecretsFile = ".ferry-secrets.env"

// IsAppSecretsPath 包括原子更新期间暂存的同目录文件。
func IsAppSecretsPath(p string) bool {
	name := path.Base(p)
	return name == AppSecretsFile || strings.HasPrefix(name, AppSecretsFile+".tmp-")
}

func AppSecretsDirective(running string) string {
	return "EnvironmentFile=" + strings.ReplaceAll(path.Join(running, AppSecretsFile), "%", "%%")
}

var appSecretName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func ValidAppSecretName(name string) bool { return appSecretName.MatchString(name) }

// ValidateAppSecretValue 检查 systemd 环境文件允许的字符。报错不能包含值。
func ValidateAppSecretValue(value string) error {
	switch {
	case value == "":
		return errors.New("密钥值不能为空")
	case strings.ContainsAny(value, "\r\n"):
		return errors.New("密钥值必须是单行；证书、私钥等多行内容请用附带文件")
	case !utf8.ValidString(value) || strings.ContainsFunc(value, func(r rune) bool {
		return r == 0 || r == '\uFEFF' || 0xFDD0 <= r && r <= 0xFDEF || r&0xFFFF >= 0xFFFE
	}):
		return errors.New("密钥值含有 systemd 环境文件不支持的字符")
	}
	return nil
}

// AppSecretValues 仅经部署和 secretset 请求的 SSH stdin 传输。非 nil 空映射用于只有可选密钥且均未设置的应用。
type AppSecretValues map[string]string

func (AppSecretValues) String() string   { return "[应用密钥]" }
func (AppSecretValues) GoString() string { return "[应用密钥]" }

// AppSecretStatus 是应用密钥的本机状态，供 CLI、桌面、MCP 和 AI 读取。
type AppSecretStatus struct {
	App       string `json:"app"`
	Name      string `json:"name"`
	Doc       string `json:"doc,omitempty"`
	Optional  bool   `json:"optional,omitempty"`
	Available bool   `json:"available"`
	Source    string `json:"source,omitempty"` // env | target | shared | team | team-unavailable | unavailable | invalid
}
