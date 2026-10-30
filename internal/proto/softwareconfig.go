package proto

// SystemNginxInstance 引用服务器已有的 nginx.service，不写入 Ferry 软件安装记录。
const SystemNginxInstance = "system:nginx.service"

// SoftwareConfigRequest 用软件身份及配置 ID 操作，服务器重新发现路径，不接受客户端任意 root 路径。
type SoftwareConfigRequest struct {
	Instance  string  `json:"instance,omitempty"`
	Unit      string  `json:"unit,omitempty"`
	Container string  `json:"container,omitempty"`
	PID       int     `json:"pid,omitempty"`
	ID        string  `json:"id,omitempty"`
	Operation string  `json:"operation"`
	Revision  string  `json:"revision,omitempty"`
	Content   *string `json:"content,omitempty"`
	FilePath  string  `json:"filePath,omitempty"`
}

type SoftwareConfigFile struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Path     string `json:"path"`
	Format   string `json:"format,omitempty"`
	Source   string `json:"source,omitempty"`
	ReadOnly string `json:"readOnly,omitempty"`
	Apply    string `json:"apply,omitempty"` // reload | restart
	Backup   bool   `json:"backup"`
	InPlace  bool   `json:"inPlace,omitempty"` // Docker 单文件挂载需要保留 inode。
}

type SoftwareConfigs struct {
	Files    []SoftwareConfigFile `json:"files"`
	Notes    []string             `json:"notes,omitempty"`
	Software Software             `json:"software"`
}

// NginxHosting 独立 nginx 的托管能力：站点配置包含在 http 块中。
type NginxHosting struct {
	Binary string `json:"binary" yaml:"binary"`
	Config string `json:"config" yaml:"config"`
	Prefix string `json:"prefix" yaml:"prefix"`
	Sites  string `json:"sites" yaml:"sites"`
	System bool   `json:"system,omitempty" yaml:"system,omitempty"`
}
