package config

import "path/filepath"

// LoadProbeContent 只用编辑器草稿里的连接设置检测环境。应用可能尚未填完，
// 不参与校验；保留原配置路径和项目名，以沿用相对私钥路径、本机登录设置。
func LoadProbeContent(data []byte, path string) (*File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, err
	}
	project := f.Kind != KindConnections
	f.Kind = KindConnections
	f.Apps, f.Builds, f.Backups = nil, nil, nil
	for _, t := range f.Targets {
		if t != nil {
			t.Apps = nil
			t.AppRuntimes = nil
		}
	}
	f.Path, f.Dir = abs, filepath.Dir(abs)
	if err := f.normalize(); err != nil {
		return nil, err
	}
	if project {
		local, err := ReadLocal(f.Project, f.Path)
		if err != nil {
			return nil, err
		}
		f.ApplyLocal(local)
	}
	return f, nil
}
