package config

import (
	"errors"
	"path"
	"path/filepath"
	"strings"

	"github.com/captain/ferry/internal/proto"
	"gopkg.in/yaml.v3"
)

// AppSecretsFile 是相对 ferry.yaml 的路径；空表示使用系统凭据库。
func (t *Target) AppSecretsFile() string {
	file, ok := strings.CutPrefix(t.SecretsFrom, "file:")
	if !ok {
		return ""
	}
	return file
}

func checkAppSecretsSource(source string) error {
	if source == "" || source == "keyring" {
		return nil
	}
	file, ok := strings.CutPrefix(source, "file:")
	if !ok {
		return errors.New("secretsFrom 只支持 keyring 或 file:secrets/<目标>.age")
	}
	if !filepath.IsLocal(filepath.FromSlash(file)) || path.Clean(file) != file ||
		strings.ContainsAny(file, "\\:\x00\r\n") || strings.Contains(file, "${") || path.Ext(file) != ".age" {
		return errors.New("secretsFrom 的文件须是配置目录内的 .age 相对路径（使用 /）")
	}
	for _, part := range strings.Split(file, "/") {
		if strings.EqualFold(part, ".git") {
			return errors.New("secretsFrom 的文件须位于项目工作目录")
		}
	}
	return nil
}

// SecretRef 声明应用需要的环境变量；值由部署时的环境或凭据库提供。
type SecretRef struct {
	Name     string `yaml:"name" json:"name"`
	Optional bool   `yaml:"optional" json:"optional,omitempty"`
	Doc      string `yaml:"doc" json:"doc,omitempty"`
}

func (s *SecretRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		s.Name = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return errors.New("service.secrets 的每项请写变量名或 {name, optional, doc}")
	}
	if err := checkKeys(n, "name", "optional", "doc"); err != nil {
		return err
	}
	type ref SecretRef
	return n.Decode((*ref)(s))
}

func checkAppSecrets(s *Service, where string, bad func(string, ...any)) {
	seen := make(map[string]bool, len(s.Secrets))
	for _, ref := range s.Secrets {
		switch {
		case !proto.ValidAppSecretName(ref.Name):
			bad("%s：service.secrets 的变量名 %q 无效（须符合 ^[A-Z_][A-Z0-9_]*$）", where, ref.Name)
		case seen[ref.Name]:
			bad("%s：service.secrets 重复声明 %s", where, ref.Name)
		default:
			if _, exists := s.Env[ref.Name]; exists {
				bad("%s：service.secrets 的 %s 已在 service.env 声明", where, ref.Name)
			}
		}
		seen[ref.Name] = true
	}
}
