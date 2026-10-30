// Package config 读取并校验 ferry.yaml。
//
// 结构：项目（project） → 构建（builds） → 应用（apps） → 目标服务器（targets）。
// 字符串里可以用 ${变量}，见 vars.go。
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/captain/ferry/internal/proto"
)

// File ferry.yaml 的内容。
type File struct {
	Version int `yaml:"version"`
	// Kind 空 = 项目配置；connections = 桌面版「新建连接」保存的服务器（只有 targets，见 KindConnections）
	Kind    string             `yaml:"kind"`
	Project string             `yaml:"project"`
	Vars    map[string]string  `yaml:"vars"`
	Git     GitPolicy          `yaml:"git"`
	Builds  map[string]*Build  `yaml:"builds"`
	Targets map[string]*Target `yaml:"targets"`
	Apps    []*App             `yaml:"apps"`
	// Backups 数据备份（数据库、目录、自己写的命令），在服务器上打开后才按时执行，见 backup.go
	Backups []*Backup `yaml:"backups"`

	Path string `yaml:"-"` // 配置文件绝对路径
	Dir  string `yaml:"-"` // 配置文件所在目录：相对路径都相对它
	// LocalVars 值来自本机设置的变量（见 Local）
	LocalVars []string `yaml:"-"`
}

// GitPolicy 部署前对本地工作区的要求。
type GitPolicy struct {
	RequireClean bool `yaml:"requireClean"` // 工作区有未提交修改时拒绝部署
}

// Build 一条构建命令。多个应用引用同一条构建时，一次部署只执行一次（${each:变量} 展开成这些应用的值）。
type Build struct {
	Name    string            `yaml:"-"`
	Workdir string            `yaml:"workdir"` // 相对配置目录，默认配置目录
	Command string            `yaml:"command"`
	Shell   string            `yaml:"shell"` // 默认 Windows 为 cmd，其余为 sh
	Env     map[string]string `yaml:"env"`
	Lock    string            `yaml:"lock"` // 构建锁名；同名的构建（跨进程）串行执行。默认按工作目录
}

// BuildRef 应用里的 build：可以是 builds 中的名字，也可以直接写一条构建。
type BuildRef struct {
	Ref    string
	Inline *Build
}

// UnmarshalYAML 支持字符串或对象两种写法。
func (b *BuildRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		b.Ref = n.Value
		return nil
	}
	if err := checkKeys(n, "workdir", "command", "shell", "env", "lock"); err != nil {
		return err
	}
	b.Inline = &Build{}
	return n.Decode(b.Inline)
}

// Target 一台目标服务器。
type Target struct {
	Name          string `yaml:"-"`
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	User          string `yaml:"user"`          // SSH 用户，默认 root
	Auth          string `yaml:"auth"`          // key（默认，私钥 / ssh-agent）或 password（密码不写在这里，见 ferry secret）
	IdentityFile  string `yaml:"identityFile"`  // 私钥；相对配置目录，支持 ~
	HostKey       string `yaml:"hostKey"`       // 可选：固定主机公钥指纹 SHA256:...
	Sudo          bool   `yaml:"sudo"`          // 非 root 时用 sudo -n 执行 agent
	Root          string `yaml:"root"`          // 部署根目录，默认 /opt/<project>
	RunAs         string `yaml:"runAs"`         // 默认运行用户
	Arch          string `yaml:"arch"`          // 可选：amd64 / arm64；与服务器实际不符时报错
	KeepReleases  int    `yaml:"keepReleases"`  // 每个应用保留的版本数，默认 5
	UploadStreams int    `yaml:"uploadStreams"` // 上传并行连接数（0 = 用设置里的默认值）；VPN 等对单条连接限速时调大
	// FerryDir 服务器上存放 ferry 组件的目录：agent 与 Ferry 提供的运行环境（JDK）。默认 /usr/local/lib/ferry；必须归 root、他人不可写
	FerryDir string            `yaml:"ferryDir"`
	Confirm  *bool             `yaml:"confirm"` // 部署前是否要求确认，默认 true
	Apps     []string          `yaml:"apps"`    // 可选：只允许部署这些应用
	Vars     map[string]string `yaml:"vars"`
	// Title 显示名（「新建连接」保存的服务器，可以是中文）
	Title string `yaml:"title"`
	// Production 生产服务器：AI 助手的每个改动都要确认（不能「同类不再询问」），界面上标出「生产」
	Production bool `yaml:"production"`
	// Deployers 只有这些人能部署、回滚、启停（各自在设置里填的名字，或 用户@电脑名）；空 = 谁都可以。
	// 是防止误操作的简单名单，不是权限控制：能登录服务器的人都能绕过
	Deployers []string `yaml:"deployers"`
	// AIChanges 是否允许 AI 助手改动这台服务器（逐条确认），默认 true；false = 只能查看
	AIChanges *bool `yaml:"aiChanges"`

	// Plain 只登录、不用 agent（「新建连接」的服务器：终端、文件），不要求 root 或 sudo
	Plain bool `yaml:"-"`
	// Local 套用了的本机设置（登录方式、私钥来自这台电脑，见 Local）；nil = 都按 ferry.yaml
	Local *LocalTarget `yaml:"-"`
	// LocalVars 本机设置里只对这台服务器改了值的变量（已合进 Vars）
	LocalVars []string `yaml:"-"`
}

// AllowAIChanges AI 助手能不能改动这台服务器。
func (t *Target) AllowAIChanges() bool { return t.AIChanges == nil || *t.AIChanges }

// NeedConfirm 部署前是否需要确认。
func (t *Target) NeedConfirm() bool { return t.Confirm == nil || *t.Confirm }

// App 一个可部署的应用。
type App struct {
	Name     string    `yaml:"name"`
	Type     string    `yaml:"type"` // java | binary | static | python | node | dotnet
	Order    int       `yaml:"order"`
	Build    *BuildRef `yaml:"build"`
	Artifact Artifact  `yaml:"artifact"`
	Service  Service   `yaml:"service"`
	Static   Static    `yaml:"static"`
	// Deps python、node：部署时在服务器上装依赖（requirements.txt / package.json），依赖文件没变就用上次装好的
	Deps         Deps              `yaml:"deps"`
	KeepReleases int               `yaml:"keepReleases"`
	Vars         map[string]string `yaml:"vars"`
	// Root 应用自己的部署根目录（应用目录为 <root>/apps/<应用>），可用 ${变量}；不设置 = 目标的 root
	Root string `yaml:"root"`
	// Hooks 部署时在服务器上执行的命令
	Hooks Hooks `yaml:"hooks"`

	index int // 在配置中的位置，Order 相同时按它排序
}

// Artifact 构建产物。
type Artifact struct {
	Path   string `yaml:"path"` // 相对配置目录；static、python、node、dotnet 为目录
	Verify Verify `yaml:"verify"`
	// Exclude 目录里不上传的（python、node、dotnet、static）：文件名或相对路径，可以用 * ?，如 tests、*.log。
	// python 默认不传 .venv、venv、__pycache__、.git……，node 默认不传 node_modules、.git……
	Exclude []string `yaml:"exclude"`
	// Files 附带文件（配置文件等）：和程序放在同一级目录，随版本走（回滚时一起回滚）。只用于 java / binary
	Files []*ExtraFile `yaml:"files"`
}

// Deps 服务器上装依赖的设置。
type Deps struct {
	Off      bool   `yaml:"off"`      // 不在服务器上装（依赖已经在上传的目录里）
	Registry string `yaml:"registry"` // pip 的 index-url（如 https://mirrors.aliyun.com/pypi/simple）/ npm 的 registry（如 https://registry.npmmirror.com）
	Python   string `yaml:"python"`   // 建 venv 用的 Python（如 python3.11、/usr/local/bin/python3.12），默认 python3
}

// ExtraFile 一个附带文件或目录。可以只写本机路径（放过去同名），或 {from, to, exec}。
type ExtraFile struct {
	From string `yaml:"from" json:"from"`           // 本机路径，相对配置目录，可用 ${变量}；目录 = 整个目录
	To   string `yaml:"to" json:"to"`               // 放到版本目录里的相对路径，不写 = from 的文件名
	Exec bool   `yaml:"exec" json:"exec,omitempty"` // 可执行（脚本）
}

// UnmarshalYAML 支持字符串或对象两种写法。
func (x *ExtraFile) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		x.From = n.Value
		return nil
	}
	if err := checkKeys(n, "from", "to", "exec"); err != nil {
		return err
	}
	type plain ExtraFile
	return n.Decode((*plain)(x))
}

// Hooks 部署时在服务器上执行的命令（手动回滚时不执行）。
type Hooks struct {
	// BeforeSwitch 新版本放好之后、停止旧版本之前（工作目录是新版本的目录），如数据库迁移；失败时不切换，线上照旧
	BeforeSwitch []*Hook `yaml:"beforeSwitch"`
	// AfterSwitch 新版本就绪之后（工作目录是 running/），如预热、刷新缓存；失败时回滚（optional 的只提示）
	AfterSwitch []*Hook `yaml:"afterSwitch"`
}

// Any 有没有钩子。
func (h *Hooks) Any() bool { return len(h.BeforeSwitch) > 0 || len(h.AfterSwitch) > 0 }

// Hook 一条命令（sh -c）。可以只写命令，或 {run, user, timeout, optional}。
type Hook struct {
	Run      string   `yaml:"run"`
	User     string   `yaml:"user"`     // 以谁的身份执行，默认应用的运行用户；可以写 root
	Timeout  Duration `yaml:"timeout"`  // 默认 5m
	Optional bool     `yaml:"optional"` // 失败只提示，不中止部署
}

// UnmarshalYAML 支持字符串或对象两种写法。
func (h *Hook) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		h.Run = n.Value
		return nil
	}
	if err := checkKeys(n, "run", "user", "timeout", "optional"); err != nil {
		return err
	}
	type plain Hook
	return n.Decode((*plain)(h))
}

// Verify 上传前对产物的检查。
type Verify struct {
	SpringBoot *SpringBootVerify `yaml:"springBoot"`
	Static     *StaticVerify     `yaml:"static"`
	MinSize    int64             `yaml:"minSize"`
}

// SpringBootVerify Spring Boot fat jar 检查。
type SpringBootVerify struct {
	RequireLibs  []string `yaml:"requireLibs"`  // BOOT-INF/lib 下必须存在的依赖（通配符，如 my-common-core-*.jar）
	NonEmptyLibs bool     `yaml:"nonEmptyLibs"` // requireLibs 匹配到的依赖里必须有 .class
}

// StaticVerify 静态目录检查。
type StaticVerify struct {
	Index string `yaml:"index"` // 必须存在的文件，默认 index.html
}

// Service systemd 服务（java / binary）。
type Service struct {
	Unit        string `yaml:"unit"` // 默认 <project>-<app>（小写），自动补 .service
	Description string `yaml:"description"`
	User        string `yaml:"user"`    // 默认目标的 runAs
	Workdir     string `yaml:"workdir"` // 默认 ${shared}
	Java        string `yaml:"java"`    // java 可执行文件，默认 /usr/bin/java
	// Runtime 由 Ferry 提供运行环境（自动下载 JDK、上传到服务器）；设置后不要再写 java
	Runtime      *Runtime          `yaml:"runtime"`
	JvmArgs      []string          `yaml:"jvmArgs"`
	Args         []string          `yaml:"args"`
	Exec         []string          `yaml:"exec"` // 完整命令行，设置后忽略 java/jvmArgs/args
	Env          map[string]string `yaml:"env"`
	EnvFile      StringList        `yaml:"envFile"` // 可多个；以 - 开头表示文件不存在也不报错
	ExecStartPre []string          `yaml:"execStartPre"`
	Conflicts    []string          `yaml:"conflicts"` // 启动前停掉的单元（如同一端口的旧服务）
	SharedDirs   []string          `yaml:"sharedDirs"`
	DiagLogs     []string          `yaml:"diagLogs"`
	// ArchiveLogs shared 下的日志目录，切换版本时随离开的版本归档；不写时为 sharedDirs 里的 logs（有的话），[] 表示不归档
	ArchiveLogs *[]string `yaml:"archiveLogs"`
	Health      Health    `yaml:"health"`
	UnitExtra   UnitExtra `yaml:"unitExtra"`
	// ZeroDowntime 不停机发布：新版本在另一个端口启动、就绪后再停旧的
	ZeroDowntime *ZeroDowntime `yaml:"zeroDowntime"`
}

// ZeroDowntime 不停机发布。两个端口轮流用；${port} 是这次启动用的端口，程序还会拿到环境变量 SERVER_PORT、PORT。
type ZeroDowntime struct {
	Ports []int `yaml:"ports"` // 两个端口，轮流用
	// Listen 直接对外的应用：Ferry 的 nginx 监听这个端口（8080 或 0.0.0.0:8080），转发到运行中的版本
	Listen string `yaml:"listen"`
	// Drain 新版本接上之后等多久再停旧版本（还在处理的请求做完、注册中心摘掉旧版本），默认 30s
	Drain *Duration `yaml:"drain"`
	// Nginx listen 用的 nginx 版本系列，如 1.30；不写 = 服务器上 Ferry 已装的（没有就装最新稳定版）
	Nginx string `yaml:"nginx"`
}

// DrainSec 停旧版本之前等的秒数。
func (z *ZeroDowntime) DrainSec() int {
	if z.Drain == nil {
		return 30
	}
	return int(z.Drain.Std() / time.Second)
}

// Runtime Ferry 提供的运行环境。
type Runtime struct {
	Java int `yaml:"java"` // JDK 大版本，如 17、21
	// Version 锁定完整版本（如 25.0.4.1+1）：只用这个版本，单元文件指向它自己的链接，不用整台服务器共用的 java-N；
	// 空 = 用服务器上 java-N 当前指向的（同一台服务器上用 java-N 的项目共用一个 JDK）
	Version string `yaml:"version"`
}

// DefaultFerryDir 服务器上 ferry 组件的默认目录。
const DefaultFerryDir = "/usr/local/lib/ferry"

// RuntimeJava Ferry 提供的 JDK 在服务器上的 java（指向该大版本当前安装的 JDK 的固定链接）。
func (t *Target) RuntimeJava(major int) string {
	return t.FerryDir + "/runtimes/java-" + itoa(major) + "/bin/java"
}

// RuntimeJavaVersion 锁定版本的 JDK 在服务器上的 java（这个版本自己的链接，见 proto.JavaPinLink）。
func (t *Target) RuntimeJavaVersion(version string) string {
	return t.FerryDir + "/runtimes/" + proto.JavaPinLink(version) + "/bin/java"
}

// UnitExtra 追加到单元文件各段的原样行。
type UnitExtra struct {
	Unit    []string `yaml:"unit"`
	Service []string `yaml:"service"`
	Install []string `yaml:"install"`
}

// Health 就绪判断。
type Health struct {
	TCP      string   `yaml:"tcp"` // host:port 或 port（= 127.0.0.1:port）
	HTTP     string   `yaml:"http"`
	Status   []int    `yaml:"status"`
	Contains string   `yaml:"contains"`
	Timeout  Duration `yaml:"timeout"` // 默认 180s
	Stable   Duration `yaml:"stable"`  // 就绪后还要稳定运行多久；无健康检查时默认 10s
}

// Static 静态站点。两种方式：
//   - 设置 port：由 Ferry 提供的 nginx 单独运行这个站点（systemd 服务，nginx 装在部署目录里，不碰服务器上已有的 nginx）
//   - 不设置 port：只放文件，由服务器上已有的 nginx 提供（用 link、reload 配合）
type Static struct {
	NginxInstance string   `yaml:"nginxInstance"` // 软件中预先安装的独立 nginx 实例。
	Domains       []string `yaml:"domains"`       // 同一实例按域名托管多个站点；空为默认站点。
	Link          string   `yaml:"link"`          // 可选：额外维护的链接（nginx root 指向它）
	Reload        string   `yaml:"reload"`        // 可选：切换后执行，如 nginx -t && nginx -s reload
	Health        Health   `yaml:"health"`

	// Port 监听端口：8081，或 127.0.0.1:8081 只让本机访问
	Port string `yaml:"port"`
	// Nginx 用哪个系列的 nginx，如 1.30；不写 = 服务器上 Ferry 已装的（没有就装最新稳定版）
	Nginx string `yaml:"nginx"`
	// SPA 前端路由（history 模式）：找不到的路径都返回 index.html。默认 true
	SPA *bool `yaml:"spa"`
	// Proxy 反向代理：路径前缀 → 后端地址，如 /prod-api/: http://127.0.0.1:8080/
	Proxy map[string]string `yaml:"proxy"`
	// MaxBody 请求体上限（上传文件经代理时），默认 100m
	MaxBody string `yaml:"maxBody"`
	// Server 原样加到 nginx server 块里的配置行
	Server []string `yaml:"server"`
	// HTTPS 自动申请、续期证书（Let's Encrypt）
	HTTPS *HTTPS `yaml:"https"`
}

// HTTPS 由服务器上的 Ferry 申请、每天检查续期的证书。
type HTTPS struct {
	Domains []string `yaml:"domains"` // 证书里的域名
	Email   string   `yaml:"email"`   // 证书服务的联系邮箱（到期提醒），可以不填
	Port    string   `yaml:"port"`    // https 端口，默认 443
	// DNS 用 DNS 验证：aliyun | dnspod | cloudflare（密钥在 Ferry 的设置里填）；不写 = HTTP 验证（static.port 要是 80）
	DNS string `yaml:"dns"`
	// Redirect http 访问跳到 https，默认 true
	Redirect *bool `yaml:"redirect"`
	// CA 证书服务：letsencrypt（默认）| letsencrypt-staging（测试用，证书浏览器不认）| ACME 目录地址
	CA string `yaml:"ca"`
}

// CAURL ACME 目录地址。
func (h *HTTPS) CAURL() string {
	switch h.CA {
	case "", "letsencrypt":
		return "https://acme-v02.api.letsencrypt.org/directory"
	case "letsencrypt-staging":
		return "https://acme-staging-v02.api.letsencrypt.org/directory"
	}
	return h.CA
}

// HTTPSPort https 监听的端口（默认 443）。
func (h *HTTPS) HTTPSPort() string {
	if h.Port == "" {
		return "443"
	}
	return h.Port
}

// IsRedirect http 是否跳到 https（默认是）。
func (h *HTTPS) IsRedirect() bool { return h.Redirect == nil || *h.Redirect }

// Served 由 Ferry 提供的 nginx 运行（设置了 port）。
func (s *Static) Served() bool { return s.Port != "" }

// IsSPA 是否按前端路由处理（默认是）。
func (s *Static) IsSPA() bool { return s.SPA == nil || *s.SPA }

// NginxLink 服务器上 nginx 的链接名：nginx-1.30，或不指定系列时的 nginx。
func (s *Static) NginxLink() string {
	if s.Nginx == "" {
		return "nginx"
	}
	return "nginx-" + s.Nginx
}

// RuntimeNginx Ferry 提供的 nginx 在服务器上的路径：放在部署根目录下（与其他项目、系统里的 nginx 互不影响）。
func (t *Target) RuntimeNginx(link string) string {
	return t.Root + "/runtimes/" + link + "/nginx"
}

// Duration 支持 "90s"、"2m" 或纯数字（秒）。
type Duration time.Duration

// UnmarshalYAML 解析时长。
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v := strings.TrimSpace(n.Value)
	if v == "" {
		*d = 0
		return nil
	}
	if sec, err := strconv.Atoi(v); err == nil {
		*d = Duration(time.Duration(sec) * time.Second)
		return nil
	}
	p, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("第 %d 行：无效的时长 %q", n.Line, v)
	}
	*d = Duration(p)
	return nil
}

// Std 转为 time.Duration。
func (d Duration) Std() time.Duration { return time.Duration(d) }

// StringList 支持单个字符串或字符串列表。
type StringList []string

// UnmarshalYAML 解析字符串或列表。
func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value == "" {
			*l = nil
		} else {
			*l = StringList{n.Value}
		}
		return nil
	}
	var v []string
	if err := n.Decode(&v); err != nil {
		return err
	}
	*l = v
	return nil
}

// checkKeys 自定义解码时检查未知字段（yaml.v3 的 KnownFields 不会传进自定义 UnmarshalYAML）。
func checkKeys(n *yaml.Node, allowed ...string) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("第 %d 行：应为对象", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if k == "<<" {
			continue
		}
		ok := false
		for _, a := range allowed {
			if a == k {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("第 %d 行：未知字段 %q（可用：%s）。%s", n.Content[i].Line, k, strings.Join(allowed, ", "), UnknownHint)
		}
	}
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
