package proto

import (
	"fmt"
	"path"
	"strings"
)

type AppRuntimeChoice struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Version  string `json:"version,omitempty"`
	Source   string `json:"source"`
	Unit     string `json:"unit,omitempty"`
	State    string `json:"state,omitempty"`
	Current  bool   `json:"current,omitempty"`
	Disabled string `json:"disabled,omitempty"`
	// Site 静态站点：选的是服务器上已有的站点（只换网站目录里的文件）
	Site *NginxSite `json:"site,omitempty"`
}

type AppRuntimeChoices struct {
	App       string             `json:"app"`
	Type      string             `json:"type"`
	CanSwitch bool               `json:"canSwitch"`
	Reason    string             `json:"reason,omitempty"`
	Port      string             `json:"port,omitempty"`
	Current   string             `json:"current,omitempty"`
	Choices   []AppRuntimeChoice `json:"choices"`
	Notes     []string           `json:"notes,omitempty"`
}

// NginxSite 服务器上已有的 nginx / OpenResty（服务器上直接运行的、容器里的）里的一个站点。
// 选它部署：Ferry 只把每一版的文件换进它的网站目录，不改 nginx 配置。
type NginxSite struct {
	Server    string   `json:"server"`              // 给人看的：OpenResty（容器 1Panel-openresty-pPgs）
	Container string   `json:"container,omitempty"` // 所在的 Docker 容器名
	Unit      string   `json:"unit,omitempty"`      // 所在的 systemd 服务
	PID       int      `json:"pid,omitempty"`       // nginx 主进程
	Ports     []int    `json:"ports"`               // 在服务器上访问的端口（容器映射过的）
	SSL       bool     `json:"ssl,omitempty"`
	Names     []string `json:"names,omitempty"`  // server_name
	Root      string   `json:"root"`             // nginx 配置里写的网站目录（容器里的路径）
	Dir       string   `json:"dir,omitempty"`    // 在服务器上的网站目录
	Owner     string   `json:"owner,omitempty"`  // 网站目录的属主（用户:组）
	Conf      string   `json:"conf,omitempty"`   // 写在哪个配置文件里
	Shared    bool     `json:"shared,omitempty"` // 同一个端口上还有别的站点（按域名区分）
	Disabled  string   `json:"disabled,omitempty"`
}

// PortConflict 部署前检查：应用要用的端口已经被别的程序占着。
type PortConflict struct {
	App   string      `json:"app"`
	Port  int         `json:"port"`
	Owner string      `json:"owner"`           // 谁占着（给人看的）
	Sites []NginxSite `json:"sites,omitempty"` // 占着的是 nginx / OpenResty 时，这个端口上已有的站点
}

// CheckSiteDir 已有站点的网站目录：绝对路径、至少两级、不能是系统目录。部署时整个目录的内容会换成这一版的。
func CheckSiteDir(p string) error {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return fmt.Errorf("网站目录 %q 要是规范的绝对路径", p)
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 2 {
		return fmt.Errorf("网站目录 %q 太浅，至少两级目录", p)
	}
	switch parts[0] {
	case "bin", "boot", "dev", "etc", "lib", "lib32", "lib64", "proc", "sbin", "sys", "run":
		return fmt.Errorf("网站目录 %q 在系统目录下", p)
	case "usr":
		if len(parts) < 3 || (parts[1] != "local" && parts[1] != "share") {
			return fmt.Errorf("网站目录 %q 在系统目录下", p)
		}
	}
	if strings.ContainsAny(p, "\x00\r\n") {
		return fmt.Errorf("网站目录 %q 无效", p)
	}
	return nil
}
