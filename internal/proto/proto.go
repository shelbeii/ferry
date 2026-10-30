// Package proto 定义 ferry（本机引擎）与 ferry-agent（服务器端助手）之间的协议。
//
// 引擎通过 SSH 执行 `ferry-agent <命令>`，把 Request 以 JSON 写入 stdin；
// agent 向 stdout 逐行输出 JSON：若干 {"t":"ev"} 事件，最后一行 {"t":"result"}。
// 进程退出码与 Result.RC 一致。
package proto

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// Version 协议版本。引擎与 agent 版本不一致时 agent 拒绝执行（agent 按内容寻址上传，版本总是匹配的）。
const Version = 17

// 结果码（同时是 agent 的进程退出码）。
const (
	CodeOK       = 0 // 成功
	CodeFailed   = 1 // 有应用失败（已按要求回滚）
	CodeBadInput = 2 // 请求无效
	CodeVerify   = 3 // 上传文件校验未通过，未改动任何应用
	CodeLocked   = 4 // 另一个部署/回滚/启停正在进行
)

// 应用类型。
const (
	TypeJava   = "java"   // Spring Boot 等可执行 JAR，systemd 管理
	TypeBinary = "binary" // 原生可执行文件（Go、GraalVM Native……），systemd 管理
	TypeStatic = "static" // 静态站点（前端 dist），切换 current 链接后可选执行 reload
	// 下面三种上传的是目录（源码或构建好的目录），由 service.exec 启动，systemd 管理
	TypePython = "python" // Python（FastAPI / Flask / Django……，uvicorn、gunicorn 启动）：依赖在服务器上装进 venv
	TypeNode   = "node"   // Node 服务端（Next.js、NestJS、Express……）：依赖在服务器上装（npm ci）
	TypeDotnet = "dotnet" // .NET（ASP.NET Core 等，dotnet publish 的目录）
)

// DirService 上传目录、作为服务运行的类型（python、node、dotnet）。
func DirService(t string) bool { return t == TypePython || t == TypeNode || t == TypeDotnet }

// Deps 服务器上装依赖（python：venv + pip；node：npm / yarn / pnpm）。依赖文件没变就用上次装好的。
type Deps struct {
	Kind     string `json:"kind"`               // python | node
	Registry string `json:"registry,omitempty"` // pip 的 index-url / npm 的 registry
	Python   string `json:"python,omitempty"`   // 建 venv 用的 Python，默认 python3
}

// Request 一次 agent 调用的输入。不同命令只用到其中一部分字段。
type Request struct {
	Proto   int    `json:"proto"`
	Root    string `json:"root"`    // 服务器上的部署根目录
	Project string `json:"project"` // 项目名
	Target  string `json:"target"`  // 目标名（只用于记录）
	Actor   string `json:"actor"`   // 操作人（本机 用户@主机）

	Apps []AppSpec `json:"apps"` // 本次涉及的应用（已按顺序排列）

	// deploy
	Stage      string        `json:"stage,omitempty"`    // 上传目录
	Releases   []ReleaseSpec `json:"releases,omitempty"` // 每个应用一项
	NoRollback bool          `json:"noRollback,omitempty"`

	// control
	Action string `json:"action,omitempty"` // start | stop | restart

	// rollback
	To string `json:"to,omitempty"` // 目标版本 ID；空 = 当前版本之前最近一个没失败的版本

	// libcache
	Seed bool `json:"seed,omitempty"` // 从各 Java 应用的当前版本抽取依赖到缓存
	// Sites prepare 时同时返回各静态站点当前版本的文件清单（静态站点只传变化的文件）
	Sites bool `json:"sites,omitempty"`

	// history
	Limit int `json:"limit,omitempty"`

	// logread：读归档的运行日志（LogRun 为空 = 当前运行）
	LogRelease string `json:"logRelease,omitempty"`
	LogRun     string `json:"logRun,omitempty"`
	LogFile    string `json:"logFile,omitempty"` // 相对 shared 的路径；可以有通配符（取最近修改的）；loggrep 为空 = 这次运行的全部文件
	Lines      int    `json:"lines,omitempty"`
	// Around logread：看第 Around 行附近（搜索结果点进来）；0 = 看末尾
	Around int `json:"around,omitempty"`
	// logtail：看服务输出（journald）还是日志文件；LogDiag 为 diagLogs 的第一个（LogFile 为空时看它）；
	// LogFollow 之后要持续输出（返回从哪里接着看）
	LogJournal bool   `json:"logJournal,omitempty"`
	LogDiag    string `json:"logDiag,omitempty"`
	LogFollow  bool   `json:"logFollow,omitempty"`
	// loggrep：关键字、区分大小写、每条结果带前后几行
	Grep        string `json:"grep,omitempty"`
	GrepCase    bool   `json:"grepCase,omitempty"`
	GrepContext int    `json:"grepContext,omitempty"`

	// FerryDir 服务器上 ferry 组件的目录（agent、运行环境）
	FerryDir string `json:"ferryDir,omitempty"`
	// runtime：安装上传目录里的 JDK
	Runtime *RuntimeSpec `json:"runtime,omitempty"`

	// preflight：自动修正权限。"plan" 列出将要做的修改（Fixable），"apply" 修改后重新检查（Fixed）
	FixPerms string `json:"fixPerms,omitempty"`

	// 锁等待：0 = 拿不到立即失败
	LockWaitSec int `json:"lockWaitSec,omitempty"`
	// Detail releases：同时返回运行中的版本的单元文件（部署前比较）
	Detail bool `json:"detail,omitempty"`
	// Monitor monitor：服务器监控的设置
	Monitor *MonitorSpec `json:"monitor,omitempty"`
	// 数据备份：backup 命令
	Backup *BackupSpec `json:"backup,omitempty"`

	// 软件：swinstall 的内容；swcontrol / swremove / swlogs 的实例名（Action、Lines 同上）
	Install        *SoftwarePlan          `json:"install,omitempty"`
	Instance       string                 `json:"instance,omitempty"`
	SoftwareConfig *SoftwareConfigRequest `json:"softwareConfig,omitempty"`
	Purge          bool                   `json:"purge,omitempty"`   // swremove：连安装目录（数据）一起删除
	LogMain        bool                   `json:"logMain,omitempty"` // swlogs：看软件自己的日志文件（否则看 journald）
	// swcheck：安装前检查（上传之前发现问题，不用白传）
	SwCheck *SwCheckSpec `json:"swCheck,omitempty"`
	// swunit：不是 Ferry 装的软件，按系统服务或 Docker 容器启停、看日志（Action 为 start / stop / restart / logs）
	SwUnit      string `json:"swUnit,omitempty"`
	SwContainer string `json:"swContainer,omitempty"`

	// portset：防火墙放行 / 关闭一个端口（Action 为 open / close）
	Port      int    `json:"port,omitempty"`
	PortProto string `json:"portProto,omitempty"` // tcp（默认）| udp
	// KeepPorts 不许关闭的端口（Ferry 连这台服务器用的 SSH 端口）；sshd 监听的端口 agent 自己找
	KeepPorts []int `json:"keepPorts,omitempty"`
	// fwenable：打开防火墙前先放行这些端口（8080/tcp……；SSH 端口 agent 自己加上）；RestartDocker 打开（fwdisable：关闭）后重启 Docker
	Allow         []string `json:"allow,omitempty"`
	RestartDocker bool     `json:"restartDocker,omitempty"`
}

// AppSpec 应用在某个目标上的完整描述。所有模板变量都已由引擎展开。
type AppSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	FileName string `json:"fileName,omitempty"` // java/binary：版本目录里的文件名
	// Deps python、node：部署时在服务器上装依赖；nil = 不装
	Deps *Deps `json:"deps,omitempty"`
	// Executables 上传的目录里要能执行的文件（相对版本目录，如 .NET 自带运行时发布的程序）
	Executables []string `json:"executables,omitempty"`
	// Root 应用自己的部署根目录（应用目录为 <Root>/apps/<Name>）；空 = 请求的 Root
	Root string `json:"root,omitempty"`
	// Java java 应用用的 java（预检时查它的版本）；用 Ferry 提供的 JDK 时为空
	Java string `json:"java,omitempty"`
	// RuntimeJava 用 Ferry 提供的 JDK 的大版本（java 在 <FerryDir>/runtimes/java-<N>/bin/java）
	RuntimeJava int `json:"runtimeJava,omitempty"`
	// RuntimeVersion 锁定的 JDK 版本（如 25.0.4.1+1）：java 在 <FerryDir>/runtimes/<JavaPinLink(版本)>/bin/java，
	// 不用 java-N（同一台服务器上别的项目升级 JDK 影响不到它）
	RuntimeVersion string `json:"runtimeVersion,omitempty"`

	Unit     string   `json:"unit,omitempty"`     // systemd 单元名（含 .service）；static 为空
	UnitText string   `json:"unitText,omitempty"` // 渲染好的单元文件内容
	User     string   `json:"user,omitempty"`     // 运行用户：版本目录、shared 目录归它所有
	Conflict []string `json:"conflicts,omitempty"`

	SharedDirs []string `json:"sharedDirs,omitempty"` // shared 下要创建的子目录（属主为 User）
	// ArchiveLogs shared 下的日志目录：切换版本时随离开的版本归档，新的运行从空目录开始
	ArchiveLogs []string `json:"archiveLogs,omitempty"`
	DiagLogs    []string `json:"diagLogs,omitempty"` // 启动失败时输出尾部的日志文件（绝对路径）

	Health Health `json:"health"`

	// static
	Link   string `json:"link,omitempty"`   // 额外维护的一个指向 current 的链接（如 nginx root）
	Reload string `json:"reload,omitempty"` // 切换后执行的命令（sh -c），失败则切回
	// NginxConf 由 Ferry 的 nginx 运行的静态站点（Unit 不为空）：放进版本目录的 nginx.conf
	NginxConf string `json:"nginxConf,omitempty"`
	// RuntimeNginx 用到的 Ferry 提供的 nginx（部署根目录 runtimes 下的链接名：nginx 或 nginx-1.30）
	RuntimeNginx  string   `json:"runtimeNginx,omitempty"`
	NginxInstance string   `json:"nginxInstance,omitempty"`
	SiteNames     []string `json:"siteNames,omitempty"`
	SitePort      string   `json:"sitePort,omitempty"` // static.port：部署前检查端口有没有被别人占着

	KeepReleases int `json:"keepReleases"`

	// Hooks 部署时在服务器上执行的命令（手动回滚时不执行）
	Hooks *Hooks `json:"hooks,omitempty"`
	// HookEnv / HookEnvFiles 钩子的环境变量：和服务一样的 env、envFile（以 - 开头表示文件不存在也不报错）
	HookEnv      map[string]string `json:"hookEnv,omitempty"`
	HookEnvFiles []string          `json:"hookEnvFiles,omitempty"`

	// ZD 不停机发布（java / binary）：两个槽位轮流用。设置了时 Unit、UnitText、Health 是槽位 a 的
	ZD *ZeroDowntime `json:"zd,omitempty"`
	// TLS 由 Ferry 的 nginx 运行的站点：自动申请、续期 HTTPS 证书
	TLS *TLSSpec `json:"tls,omitempty"`
}

// TLSSpec 自动申请 HTTPS 证书（ACME，默认 Let's Encrypt）。证书放在 .ferry/apps/<app>/tls/，nginx 配置里写这个位置；
// 服务器上的 ferry-acme.timer 每天检查一次，快到期时续期并重新加载 nginx。
type TLSSpec struct {
	Domains []string `json:"domains"`
	Email   string   `json:"email,omitempty"`
	CA      string   `json:"ca"` // ACME 目录地址
	// DNS 用 DNS 验证（aliyun | dnspod | cloudflare）；空 = HTTP 验证（站点的 http 端口要是 80，挑战文件放在 Webroot）
	DNS string `json:"dns,omitempty"`
	// DNSCred DNS 服务商的 API 密钥（本机有就带上，存到服务器上供续期用；不带时用服务器上已经存的）
	DNSCred map[string]string `json:"dnsCred,omitempty"`
	Webroot string            `json:"webroot,omitempty"` // HTTP 验证的文件放在这里（nginx 的 /.well-known/acme-challenge/ 指向它）
}

// MonitorSpec 服务器监控（monitor 命令）：服务器上的定时器每隔几分钟检查一次，有问题时发通知。
// 一台服务器上可以有几个项目各自开着（各自的通知），按「项目 / 目标」区分。
type MonitorSpec struct {
	Action      string           `json:"action"` // on | off | status | check（只检查、不发通知）| test（发一条测试告警）
	Project     string           `json:"project"`
	Target      string           `json:"target"`
	Host        string           `json:"host,omitempty"`
	IntervalMin int              `json:"intervalMin,omitempty"` // 默认 5
	DiskPct     int              `json:"diskPct,omitempty"`     // 磁盘用到百分之几告警，默认 90
	CertDays    int              `json:"certDays,omitempty"`    // HTTPS 证书还剩几天告警，默认 14
	Channels    []MonitorChannel `json:"channels,omitempty"`
}

// MonitorChannel 告警发到哪（地址、加签密钥存在服务器上，只有 root 能读）。
type MonitorChannel struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	URL  string `json:"url,omitempty"`
	Sign string `json:"sign,omitempty"`
}

// MonitorStatus monitor 的结果：开没开、多久检查一次、上次检查的时间、现在有哪些问题。
type MonitorStatus struct {
	On          bool             `json:"on"`
	IntervalMin int              `json:"intervalMin,omitempty"`
	DiskPct     int              `json:"diskPct,omitempty"`
	CertDays    int              `json:"certDays,omitempty"`
	Channels    []string         `json:"channels,omitempty"` // 发到哪（只有名字）
	LastRun     time.Time        `json:"lastRun,omitzero"`
	Problems    []MonitorProblem `json:"problems"`
	// Sent test：发出去的通知（名字 → 错误；空 = 成功）
	Sent map[string]string `json:"sent,omitempty"`
}

// MonitorProblem 一个问题。
type MonitorProblem struct {
	Key   string    `json:"key"`   // unit:<单元>、disk:<挂载点>、cert:<应用>、firewall……
	Level string    `json:"level"` // bad | warn
	Text  string    `json:"text"`
	Since time.Time `json:"since,omitzero"`
}

// BackupSpec 数据备份（backup 命令）。
//
//	status  查看：开没开、各项上次备份的结果、有几份
//	on      打开：记下各项、装上定时器（之后按时备份；部署前要备份的在部署前备份）
//	off     关闭：去掉定时器，已有的备份文件留着
//	before  部署前：开着的话同步各项，执行 beforeDeploy 的；没开什么也不做
//	run     现在备份（Name 空 = 全部）；关着也能用
//	list    列出备份文件（Name 空 = 全部）
//	restore 用 File 恢复 Name（先自动备份一份现在的）
//	delete  删掉 Name 的备份文件 File
type BackupSpec struct {
	Action  string       `json:"action"`
	Project string       `json:"project"`
	Target  string       `json:"target"`
	Host    string       `json:"host,omitempty"`
	Items   []BackupItem `json:"items,omitempty"`
	Name    string       `json:"name,omitempty"`
	File    string       `json:"file,omitempty"`
	// Force restore：恢复前备份现在的数据失败了也照样恢复
	Force bool `json:"force,omitempty"`
}

// BackupItem 一项备份（配置里的 backups，变量已展开）。Kind：mysql | postgres | paths | command。
type BackupItem struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Database     string   `json:"database,omitempty"`
	Host         string   `json:"host,omitempty"`
	Port         int      `json:"port,omitempty"`
	User         string   `json:"user,omitempty"`
	Container    string   `json:"container,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	Command      string   `json:"command,omitempty"`
	Restore      string   `json:"restore,omitempty"`
	EnvFiles     []string `json:"envFiles,omitempty"`
	OnCalendar   string   `json:"onCalendar"`
	Keep         int      `json:"keep"`
	BeforeDeploy bool     `json:"beforeDeploy,omitempty"`
}

// BackupStatus status / on / off / run 的结果。
type BackupStatus struct {
	On    bool               `json:"on"`
	Dir   string             `json:"dir"` // 备份文件在服务器上的目录
	Items []BackupItemStatus `json:"items"`
	Files []BackupFile       `json:"files,omitempty"` // list
}

// BackupItemStatus 一项的情况。Changed：服务器上记的和现在的配置不一样（重新打开或部署一次就同步了）。
type BackupItemStatus struct {
	BackupItem
	Next    string     `json:"next,omitempty"` // 下次定时备份（systemd 的时间）
	Last    *BackupRun `json:"last,omitempty"`
	LastOK  *BackupRun `json:"lastOk,omitempty"`
	Count   int        `json:"count"`
	Bytes   int64      `json:"bytes"`
	Changed bool       `json:"changed,omitempty"`
	Removed bool       `json:"removed,omitempty"` // 配置里已经没有这一项了（服务器上还开着）
}

// BackupRun 一次备份的结果。Reason：定时 | 部署前 | 手动 | 恢复前。
type BackupRun struct {
	Time   time.Time `json:"time"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
	File   string    `json:"file,omitempty"`
	Size   int64     `json:"size,omitempty"`
	Sec    float64   `json:"sec"`
	Reason string    `json:"reason"`
}

// BackupFile 服务器上的一个备份文件。
type BackupFile struct {
	Item string    `json:"item"`
	Name string    `json:"name"`
	Path string    `json:"path"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

// DefaultCA Let's Encrypt 的 ACME 目录。
const DefaultCA = "https://acme-v02.api.letsencrypt.org/directory"

// ZeroDowntime 不停机发布：新版本在空着的槽位（另一个端口）启动、就绪后再停旧的。
// 槽位的程序目录 apps/<app>/slots/<a|b> 是指向版本目录的链接，running 也是指向运行中的版本的链接。
type ZeroDowntime struct {
	Slots [2]Slot `json:"slots"`
	// DrainSec 新版本就绪、转发切过去之后，等多久再停旧版本（让还在处理的请求做完、注册中心摘掉旧版本）
	DrainSec int `json:"drainSec"`
	// 前面的 Ferry nginx（配置了 listen 时）：单元名、单元文件、nginx 配置（转发到的端口写成 {{FERRY_PORT}}）
	ProxyUnit     string `json:"proxyUnit,omitempty"`
	ProxyUnitText string `json:"proxyUnitText,omitempty"`
	ProxyConf     string `json:"proxyConf,omitempty"`
}

// Slot 一个槽位。
type Slot struct {
	Name     string `json:"name"` // a | b
	Port     int    `json:"port"`
	Unit     string `json:"unit"`
	UnitText string `json:"unitText"`
	Health   Health `json:"health"`
}

// ProxyPort nginx 配置里转发到的端口的占位。
const ProxyPort = "{{FERRY_PORT}}"

// Hooks 部署时在服务器上执行的命令。
//   - BeforeSwitch：新版本放好之后、停止旧版本之前，工作目录是新版本的目录；失败时不切换，线上照旧
//   - AfterSwitch：新版本就绪之后，工作目录是 running/；失败时回滚（Optional 的只提示）
type Hooks struct {
	BeforeSwitch []Hook `json:"beforeSwitch,omitempty"`
	AfterSwitch  []Hook `json:"afterSwitch,omitempty"`
}

// Hook 一条命令（sh -c），以 User 的身份执行。
type Hook struct {
	Run        string `json:"run"`
	User       string `json:"user"`
	TimeoutSec int    `json:"timeoutSec"`
	Optional   bool   `json:"optional,omitempty"` // 失败只提示，不中止部署
}

// Health 就绪判断。TCP、HTTP 都为空时：进程持续运行 StableSec 秒即就绪。
type Health struct {
	Host       string `json:"host,omitempty"` // HTTP 虚拟主机的 Host。
	TCP        string `json:"tcp,omitempty"`  // host:port
	HTTP       string `json:"http,omitempty"` // 完整 URL
	Status     []int  `json:"status,omitempty"`
	Contains   string `json:"contains,omitempty"`
	TimeoutSec int    `json:"timeoutSec"`
	StableSec  int    `json:"stableSec,omitempty"` // 健康检查通过后还要再稳定运行的秒数
}

// ReleaseSpec 一个应用本次要发布的版本。
type ReleaseSpec struct {
	App     string `json:"app"`
	ID      string `json:"id"`
	File    string `json:"file"`              // 上传目录中的最终文件名（java/binary 为程序，static 为 tar.gz）
	Plan    string `json:"plan,omitempty"`    // 增量上传：拼装计划文件
	Pack    string `json:"pack,omitempty"`    // 增量上传：原样字节文件
	Archive bool   `json:"archive,omitempty"` // static：File 是 tar.gz，需要解开
	// Site static：站点全部文件的清单（JSON 的 []SiteFile，上传目录中的文件名），存进版本记录，下次部署据此只传变化的文件
	Site       string `json:"site,omitempty"`
	SiteSHA256 string `json:"siteSha256,omitempty"`
	// SiteDelta File 里只有变化的文件：其余的从当前版本复制，按 Site 逐个核对
	SiteDelta bool   `json:"siteDelta,omitempty"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	// Files 附带文件（配置文件等）打成的 tar.gz（上传目录中的文件名）：解到版本目录里，和程序放在一起
	Files       string      `json:"files,omitempty"`
	FilesSHA256 string      `json:"filesSha256,omitempty"`
	FilesSize   int64       `json:"filesSize,omitempty"`
	Meta        ReleaseMeta `json:"meta"`
}

// ReleaseMeta 写进版本目录的 release.json。
type ReleaseMeta struct {
	ID         string    `json:"id"`
	App        string    `json:"app"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	Commit     string    `json:"commit,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Dirty      bool      `json:"dirty,omitempty"`
	BuiltAt    time.Time `json:"builtAt,omitempty"`
	DeployedAt time.Time `json:"deployedAt,omitempty"`
	DeployedBy string    `json:"deployedBy,omitempty"`
	Source     string    `json:"source,omitempty"` // 本机产物路径
	// Files 附带文件放到版本目录里的位置（文件或目录）
	Files []string `json:"files,omitempty"`
	// FileSums 附带文件里每个文件的 sha256（相对版本目录的路径 → sha256）：下次部署时比较哪些文件改了
	FileSums map[string]string `json:"fileSums,omitempty"`
}

// reservedFileNames 版本目录里归 Ferry 用的名字（运行日志、站点文件、以前放在版本目录里的 Ferry 文件）：附带文件不能用。
var reservedFileNames = []string{"logs", "www", ".ferry", "release.json", "unit.service", "nginx.conf", ".failed", ".libs-seeded", ".logruns"}

// CheckExtraFile 检查附带文件放到的位置 to：版本目录里的相对路径（用 /），不能越界，
// 不能占用程序文件名 program（空 = 不查）和 Ferry 用的名字。
func CheckExtraFile(to, program string) error {
	if to == "" {
		return fmt.Errorf("放到的位置不能为空")
	}
	if strings.Contains(to, `\`) || strings.ContainsFunc(to, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("放到的位置 %q 里有不能用的字符（路径用 /）", to)
	}
	if path.IsAbs(to) {
		return fmt.Errorf("放到的位置 %q 要写相对路径（相对程序所在的目录）", to)
	}
	if c := path.Clean(to); c != to || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("放到的位置 %q 无效：不能有 .、..、多余的 /", to)
	}
	top, _, _ := strings.Cut(to, "/")
	if program != "" && top == program {
		return fmt.Errorf("放到的位置 %q 和程序文件同名", to)
	}
	for _, r := range reservedFileNames {
		if top == r {
			if r == "logs" {
				return fmt.Errorf("放到的位置 %q 不能用：logs 是程序运行日志的目录", to)
			}
			return fmt.Errorf("放到的位置 %q 不能用：%s 是 Ferry 自己用的名字", to, r)
		}
	}
	return nil
}

// Event agent 输出的过程事件。
type Event struct {
	T     string `json:"t"` // "ev"
	Time  string `json:"time,omitempty"`
	Level string `json:"level"` // info | warn | error
	Code  string `json:"code"`  // 见下方常量
	App   string `json:"app,omitempty"`
	Msg   string `json:"msg"`
}

// 事件码。
const (
	EvInfo     = "INFO"
	EvWarn     = "WARN"
	EvLock     = "LOCK"
	EvCache    = "CACHE"
	EvAssemble = "ASSEMBLE"
	EvVerify   = "VERIFY"
	EvRelease  = "RELEASE"
	EvStop     = "STOP"
	EvSwitch   = "SWITCH"
	EvUnit     = "UNIT"
	EvStart    = "START"
	EvWait     = "WAIT"
	EvReady    = "READY"
	EvCrash    = "CRASH"
	EvFail     = "FAIL"
	EvDiag     = "DIAG"
	EvRollback = "ROLLBACK"
	EvAbort    = "ABORT"
	EvPrune    = "PRUNE"
	EvReload   = "RELOAD"
	EvLogs     = "LOGS"
	EvRuntime  = "RUNTIME"
	EvSoftware = "SOFTWARE"
	EvFirewall = "FIREWALL"
	EvHook     = "HOOK"
	EvCert     = "CERT"
)

// Result agent 的最后一行输出。
type Result struct {
	T    string          `json:"t"` // "result"
	RC   int             `json:"rc"`
	Msg  string          `json:"msg,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ---------------------------------------------------------------- 各命令的结果数据

// Preflight 服务器环境。
type Preflight struct {
	OS             string            `json:"os"`   // /etc/os-release PRETTY_NAME
	Arch           string            `json:"arch"` // amd64 / arm64（agent 编译架构）
	Kernel         string            `json:"kernel"`
	Systemd        int               `json:"systemd"` // 主版本号，0 = 没有 systemd
	SELinux        string            `json:"selinux,omitempty"`
	UID            int               `json:"uid"`
	Users          map[string]bool   `json:"users"`         // 运行用户是否存在
	RootFreeMB     int64             `json:"rootFreeMB"`    // 部署根目录所在分区剩余空间
	RootOwner      string            `json:"rootOwner"`     // 部署根目录属主
	Commands       map[string]string `json:"commands"`      // 需要的命令 → 路径（缺失为空）
	ExistingUnits  map[string]string `json:"existingUnits"` // 单元 → managed（ferry 管理）/ foreign（其他来源）/ 空
	Problems       []string          `json:"problems"`      // 阻止部署的问题
	Warnings       []string          `json:"warnings"`      // 提示
	AgentVersion   string            `json:"agentVersion"`
	ProtoVersion   int               `json:"protoVersion"`
	DeployRootUsed bool              `json:"deployRootUsed"`      // 部署根目录已存在
	AppRoots       []RootInfo        `json:"appRoots,omitempty"`  // 应用单独设置的部署根目录
	Fixable        []string          `json:"fixable,omitempty"`   // FixPerms=plan：部署时会自动修正的权限
	Fixed          []string          `json:"fixed,omitempty"`     // FixPerms=apply：已经修正的权限
	Javas          map[string]Java   `json:"javas,omitempty"`     // 各应用 service.java 的版本
	Runtimes       map[int]Runtime   `json:"runtimes,omitempty"`  // 服务器上 Ferry 提供的 JDK：大版本 → 当前安装的
	JDKs           []Runtime         `json:"jdks,omitempty"`      // 服务器上 Ferry 装过的全部 JDK（锁定版本时按 ID 找）
	JavaPins       map[string]string `json:"javaPins,omitempty"`  // 锁定版本的链接 → 指向的 JDK（ID）
	JavaFound      []Java            `json:"javaFound,omitempty"` // 服务器上找到的其他 JDK
	// Nginx 部署根目录下 Ferry 提供的 nginx：链接名（nginx、nginx-1.30）→ 当前指向的
	Nginx map[string]Runtime `json:"nginx,omitempty"`
}

// RuntimeSpec 要安装的运行环境：JDK（上传目录里的 tar.gz，装到 FerryDir）或 nginx（单个程序，装到部署根目录）。
type RuntimeSpec struct {
	Kind string `json:"kind,omitempty"` // 空 = JDK；nginx
	// nginx：链接名（nginx、nginx-1.30）与完整版本
	Link    string `json:"link,omitempty"`
	Version string `json:"version,omitempty"`
	ID      string `json:"id"`   // 如 jdk-17.0.13_11-x64
	Java    int    `json:"java"` // 大版本
	// Dir JDK 装到这个目录下的 <ID> 文件夹（装软件时用户指定的）；空 = <FerryDir>/runtimes 下，java-N 指向它
	Dir string `json:"dir,omitempty"`
	// Pin 锁定版本：在 <FerryDir>/runtimes 下建这个名字的链接指向 <ID>（JavaPinLink），不动 java-N；
	// 服务器上已有 <ID> 时不用上传（File 为空）
	Pin    string `json:"pin,omitempty"`
	File   string `json:"file"` // 上传目录里的文件名
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Runtime 服务器上已安装的一个 JDK。
type Runtime struct {
	ID      string `json:"id"`
	Java    int    `json:"java"`
	Version string `json:"version,omitempty"`
}

// Java 服务器上的一个 java。
type Java struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"` // java -version 里的版本，如 1.8.0_412、17.0.9
	Major   int    `json:"major"`             // 8、17、21；0 = 不知道
	Error   string `json:"error,omitempty"`
}

// RootInfo 应用单独设置的部署根目录。
type RootInfo struct {
	Path   string   `json:"path"`
	Apps   []string `json:"apps"`
	FreeMB int64    `json:"freeMB"` // 所在分区剩余空间，-1 = 未知
}

// Prepared prepare 的结果：本次的上传目录与服务器已缓存的依赖。
type Prepared struct {
	Stage  string   `json:"stage"`
	Libs   []string `json:"libs"`   // 已缓存依赖的 sha256
	Seeded int      `json:"seeded"` // 本次从各应用当前版本抽取的新依赖数
	// Sites 各静态站点当前版本的文件（请求了 Sites 时）；没有当前版本、文件太多时没有这个应用
	Sites map[string][]SiteFile `json:"sites,omitempty"`
}

// SiteFile 静态站点里的一个文件（路径相对站点目录，用 /）。
type SiteFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// AppStatus 应用状态。
type AppStatus struct {
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	Unit     string       `json:"unit,omitempty"`
	Active   string       `json:"active,omitempty"` // systemd ActiveState
	Sub      string       `json:"sub,omitempty"`
	PID      int          `json:"pid,omitempty"`
	Since    string       `json:"since,omitempty"`
	Healthy  *bool        `json:"healthy,omitempty"`
	Current  *ReleaseMeta `json:"current,omitempty"`
	Releases int          `json:"releases"`
	Problem  string       `json:"problem,omitempty"`
	// Listen 主端口：服务正在监听的业务地址，如 127.0.0.1:9201、*:8080（静态站点为 nginx 的监听地址）
	Listen []string `json:"listen,omitempty"`
	// Extra 附加端口：服务还在监听的其他地址（Sentinel、远程调试、JMX……）及其用途
	Extra []ExtraPort `json:"extra,omitempty"`
	// Port 配置里健康检查用的端口（服务没运行时显示）
	Port string `json:"port,omitempty"`
	// Sites 静态站点：nginx 里 root / alias 指向它的 server（Listen 为这些 server 的监听地址）
	Sites []Site `json:"sites,omitempty"`
	// Serving 静态站点没找到 nginx 站点时的原因
	Serving string `json:"serving,omitempty"`
	// Busy 现在有人在这台服务器上部署、回滚、启停（这个项目的部署目录）
	Busy *LockHolder `json:"busy,omitempty"`
}

// LockHolder 正拿着部署锁的操作：谁、在做什么。
type LockHolder struct {
	Actor  string    `json:"actor"`
	Action string    `json:"action"` // 部署 | 回滚 | 启动 | 停止 | 重启
	Apps   []string  `json:"apps,omitempty"`
	Since  time.Time `json:"since"`
	PID    int       `json:"pid"`
}

// ExtraPort 一个附加端口。
type ExtraPort struct {
	Addr string `json:"addr"`          // 如 *:8721
	Use  string `json:"use,omitempty"` // 用途（识别不出时为空）
	// Risky 这类端口不宜对外开放（没有认证，或能控制进程）
	Risky bool `json:"risky,omitempty"`
}

// Site nginx 里提供某个静态站点的一个 server 块。
type Site struct {
	Listen   []string `json:"listen"`             // *:80、127.0.0.1:8080
	SSL      bool     `json:"ssl,omitempty"`      // listen 带 ssl
	Names    []string `json:"names,omitempty"`    // server_name
	Location string   `json:"location,omitempty"` // root / alias 在 location 里时的路径
	File     string   `json:"file,omitempty"`     // 所在配置文件
}

// ReleaseInfo 一个历史版本。
type ReleaseInfo struct {
	ReleaseMeta
	Current bool `json:"current"`
	Failed  bool `json:"failed"`
	LogRuns int  `json:"logRuns,omitempty"` // 归档了几次运行的日志
}

// LogRun 某个版本一次运行的日志：当前运行（在 shared 里），或切换版本时归档的。
type LogRun struct {
	Release string    `json:"release"`
	ID      string    `json:"id,omitempty"` // 归档名（归档时间 20060102-150405）；当前运行为空
	Start   time.Time `json:"start,omitzero"`
	End     time.Time `json:"end,omitzero"`
	Current bool      `json:"current,omitempty"`
	Files   []LogFile `json:"files"`
	Size    int64     `json:"size"` // 日志总大小（以前压缩的归档为压缩后的大小）
	// Dir 归档的日志在服务器上的目录（里面的路径与 shared 下一样）；以前压缩的归档为空
	Dir string `json:"dir,omitempty"`
}

// LogFile 运行日志里的一个文件（路径相对 shared）。
type LogFile struct {
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime,omitzero"` // 最后修改时间（0.12.10 以前的归档没有）
}

// LogText logread 的结果：文件末尾若干行。
type LogText struct {
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`       // 前面还有更早的行
	File      string   `json:"file,omitempty"`  // 实际读的文件（写了通配符时是匹配到的那个）
	First     int      `json:"first,omitempty"` // 看某一行附近时：第一行的行号（从 1 数）
}

// LogTail logtail 的结果：选中的那次运行和最后几行，一次来回就能显示；持续输出时从哪里接着看。
type LogTail struct {
	Run  LogRun  `json:"run"`
	Text LogText `json:"text"`
	// Follow 持续输出时 ferry 接着运行的命令（从读到的地方之后接着看，不重复也不漏）；空 = 不用接着看
	Follow string `json:"follow,omitempty"`
	// Path 日志文件在服务器上的完整路径
	Path string `json:"path,omitempty"`
	// Stream 这里没有读（diagLogs 不在应用目录下、要持续看的文件有通配符或还不存在）：由 ferry 直接 tail Path
	Stream bool `json:"stream,omitempty"`
}

// LogSearch loggrep 的结果。
type LogSearch struct {
	Matches   []LogMatch `json:"matches"`
	Files     int        `json:"files"`     // 搜了几个文件
	Truncated bool       `json:"truncated"` // 结果太多，只返回了前面的
}

// LogMatch 一处搜索结果。
type LogMatch struct {
	File   string   `json:"file"` // 相对 shared；服务输出为空
	Line   int      `json:"line"` // 行号（从 1 数）；服务输出为 0
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// AppReleases 某个应用的全部版本（新 → 旧）。
type AppReleases struct {
	App      string        `json:"app"`
	Releases []ReleaseInfo `json:"releases"`
	Legacy   bool          `json:"legacy"` // 有接管前的旧单元文件，可回到接管前
	// Unit 运行中的版本保存的单元文件（请求里 Detail 时才有）：部署前比较配置改了什么
	Unit string `json:"unit,omitempty"`
}

// DeployOutcome 部署结果。
type DeployOutcome struct {
	Apps []AppOutcome `json:"apps"`
}

// AppOutcome 单个应用的部署结果。
type AppOutcome struct {
	App     string `json:"app"`
	Release string `json:"release,omitempty"`
	Result  string `json:"result"` // OK | FAILED | ROLLED_BACK | ROLLBACK_FAILED | SKIPPED
	Msg     string `json:"msg,omitempty"`
	// ReadySec 从启动到就绪用了几秒；PrevReadySec 上一次成功部署时用了几秒（0 = 没有记录）
	ReadySec     int `json:"readySec,omitempty"`
	PrevReadySec int `json:"prevReadySec,omitempty"`
	// ErrorLines 启动时服务输出里有几行错误（error、exception）；PrevErrorLines 上一次成功部署时的（-1 = 没有记录）
	ErrorLines     int `json:"errorLines"`
	PrevErrorLines int `json:"prevErrorLines"`
}

// HistoryEntry 服务器上的一条部署历史（.ferry/history.jsonl）。
type HistoryEntry struct {
	Time    time.Time `json:"time"`
	Action  string    `json:"action"` // deploy | rollback | start | stop | restart
	App     string    `json:"app"`
	Release string    `json:"release,omitempty"`
	Result  string    `json:"result"`
	Commit  string    `json:"commit,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
	Actor   string    `json:"actor,omitempty"`
	Msg     string    `json:"msg,omitempty"`
	// ReadySec 部署成功时，新版本从启动到就绪用了几秒（下次部署时比较）
	ReadySec int `json:"readySec,omitempty"`
	// ErrorLines 部署成功时，启动期间服务输出里的错误行数（nil = 没有记录）
	ErrorLines *int `json:"errorLines,omitempty"`
}

// SoftwareReport software 的结果：服务器上已有的软件（只读查看，不改动服务器）。
type SoftwareReport struct {
	Items []Software `json:"items"`
	// Docker 服务器上的 Docker：空 = 没装；ok = 已读取容器列表；其他为读取不了的原因
	Docker string   `json:"docker,omitempty"`
	Notes  []string `json:"notes,omitempty"` // 结果可能不全的原因等

	// 以下给安装软件用
	Arch     string          `json:"arch,omitempty"`     // amd64 / arm64
	OS       *OSInfo         `json:"os,omitempty"`       // /etc/os-release
	Runtimes map[int]Runtime `json:"runtimes,omitempty"` // 服务器上 Ferry 提供的 JDK
	FerryDir string          `json:"ferryDir,omitempty"` // ferry 组件的目录（公共的 JDK 在它下面的 runtimes）
	Disks    []Disk          `json:"disks,omitempty"`    // 常用目录所在分区的剩余空间
	Ports    []int           `json:"ports,omitempty"`    // 已被占用的 TCP 端口
}

// OSInfo 服务器的系统（/etc/os-release）。
type OSInfo struct {
	ID      string   `json:"id"`             // rocky、ubuntu……
	Like    []string `json:"like,omitempty"` // ID_LIKE
	Version string   `json:"version,omitempty"`
	Pretty  string   `json:"pretty,omitempty"`
}

// Disk 一个目录所在分区的剩余空间。
type Disk struct {
	Path   string `json:"path"`
	FreeMB int64  `json:"freeMB"`
}

// SoftwarePlan 按模板渲染好的安装计划（swinstall）：agent 照着做，不需要认识具体软件。
type SoftwarePlan struct {
	Template string `json:"template"` // 模板 ID
	Title    string `json:"title"`    // 软件的显示名（Nacos、MySQL……）
	Name     string `json:"name"`     // 实例名：单元为 ferry-<名称>.service
	Version  string `json:"version,omitempty"`
	Dir      string `json:"dir"`  // 安装目录（程序、配置、数据、日志都在里面）
	User     string `json:"user"` // 运行用户（没有就创建）
	// Reach 运行用户还要能进的目录（装在别处的 JDK）
	Reach []string `json:"reach,omitempty"`
	// FixParents 运行用户进不了安装目录（或 Reach）的上级目录时，给它加上进入的权限（用户已同意）
	FixParents bool `json:"fixParents,omitempty"`

	Package *PlanPackage `json:"package,omitempty"` // 上传目录里的安装包
	System  *PlanSystem  `json:"system,omitempty"`  // 用服务器的软件源装的包
	// Proxy 下载经这台电脑：装包、执行步骤时设成 http_proxy / https_proxy（服务器上只监听 127.0.0.1 的端口，
	// 经 SSH 转到这台电脑）；空 = 用服务器自己的网络。NoProxy 是服务器自己连得上的地址，不经代理
	Proxy   string   `json:"proxy,omitempty"`
	NoProxy []string `json:"noProxy,omitempty"`

	Files []PlanFile  `json:"files,omitempty"`
	Props []PlanProps `json:"props,omitempty"`
	Steps []PlanStep  `json:"steps,omitempty"`

	Exec    string            `json:"exec"`
	Workdir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"` // 写进只有 root 能读的文件
	// ReadyPort 启动后等这个端口能连上
	ReadyPort    int                  `json:"readyPort"`
	ReadyTimeout int                  `json:"readyTimeout,omitempty"`
	Ports        []int                `json:"ports"`             // 要占用的端口（安装前检查有没有被占用）
	PortMap      map[string]int       `json:"portMap,omitempty"` // 端口参数（记下来给用户看）
	Logs         []string             `json:"logs,omitempty"`    // 软件自己的日志文件
	Notes        []string             `json:"notes,omitempty"`   // 安装完告诉用户的话
	Configs      []SoftwareConfigFile `json:"configs,omitempty"`
	Hosting      *NginxHosting        `json:"hosting,omitempty"`
}

// PlanPackage 上传的安装包怎么放。
type PlanPackage struct {
	File   string `json:"file"` // 上传目录里的文件名
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Format string `json:"format"`          // tar.gz | tar.xz | zip | jar | binary
	Strip  int    `json:"strip,omitempty"` // 解压时去掉几层目录
	Into   string `json:"into,omitempty"`  // 安装目录下的相对路径（jar / binary 为文件名）
	Check  string `json:"check,omitempty"` // 解开后必须有的文件（相对 Into）
	// Cached 上次安装失败时留在服务器上的同一个安装包（swcheck 报告有），不用再上传
	Cached bool `json:"cached,omitempty"`
}

// SwCheckSpec swcheck 要检查的：安装目录、运行用户、装在别处的 JDK、服务器上有没有留着的安装包。不填的不查。
type SwCheckSpec struct {
	Dir        string `json:"dir,omitempty"`
	User       string `json:"user,omitempty"`
	Java       int    `json:"java,omitempty"`    // 要的 JDK 大版本
	JavaDir    string `json:"javaDir,omitempty"` // 放 JDK 的目录（JDK 装在它下面以版本命名的文件夹里；空 = Ferry 的公共目录）
	PackageSHA string `json:"packageSha,omitempty"`
}

// SwCheck swcheck 的结果。
type SwCheck struct {
	// Blocked 运行用户进不了的上级目录（从上往下）
	Blocked []BlockedDir `json:"blocked,omitempty"`
	// DirBusy 安装目录已存在且不是空的
	DirBusy bool `json:"dirBusy,omitempty"`
	// JavaHome JavaDir 里已有的 Ferry 装的这个大版本的 JDK（直接用）；没有为空
	JavaHome string `json:"javaHome,omitempty"`
	// Cached 服务器上留着这个安装包（上次安装失败时）
	Cached bool `json:"cached,omitempty"`
}

// BlockedDir 运行用户进不去的目录。
type BlockedDir struct {
	Path  string `json:"path"`
	Owner string `json:"owner,omitempty"`
	Mode  string `json:"mode,omitempty"` // drwx------
}

// PlanSystem 用服务器的软件源安装。
type PlanSystem struct {
	DNF     []string `json:"dnf,omitempty"`
	APT     []string `json:"apt,omitempty"`
	Disable []string `json:"disable,omitempty"` // 装包时系统自带、被自动启动的服务：停掉并禁用（原来就在运行的不动）
	Require string   `json:"require,omitempty"`
}

// PlanFile 要写的文件。
type PlanFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"` // 默认 0640
	At      string `json:"at,omitempty"`   // install（默认）| ready
}

// PlanProps 修改配置文件里的项。
type PlanProps struct {
	File string            `json:"file"`
	Sep  string            `json:"sep,omitempty"` // 默认 =
	Set  map[string]string `json:"set"`
}

// PlanStep 在服务器上执行的一步（root）。
type PlanStep struct {
	At      string    `json:"at,omitempty"` // prepare（默认）| init | ready
	Run     string    `json:"run,omitempty"`
	HTTP    *PlanHTTP `json:"http,omitempty"`
	Timeout int       `json:"timeout,omitempty"`
	Error   string    `json:"error,omitempty"`
	Soft    string    `json:"soft,omitempty"`
}

// PlanHTTP 就绪后调用的接口。
type PlanHTTP struct {
	Post  string            `json:"post"`
	Form  map[string]string `json:"form,omitempty"`
	Retry int               `json:"retry,omitempty"`
}

// SoftwareInstance Ferry 安装的一个软件（<ferryDir>/software/<名称>.json）。
type SoftwareInstance struct {
	Name        string               `json:"name"`
	Kind        string               `json:"kind"`  // 模板 ID
	Title       string               `json:"title"` // 显示名
	Version     string               `json:"version,omitempty"`
	Dir         string               `json:"dir"`
	Unit        string               `json:"unit"`
	User        string               `json:"user"`
	Ports       map[string]int       `json:"ports,omitempty"`
	ReadyPort   int                  `json:"readyPort"`
	Logs        []string             `json:"logs,omitempty"`
	Packages    []string             `json:"packages,omitempty"` // 从软件源装的包（卸载时不删，告诉用户）
	InstalledAt time.Time            `json:"installedAt"`
	InstalledBy string               `json:"installedBy,omitempty"`
	Notes       []string             `json:"notes,omitempty"`
	Configs     []SoftwareConfigFile `json:"configs,omitempty"`
	Hosting     *NginxHosting        `json:"hosting,omitempty"`
}

// Software 服务器上找到的一个软件（一个运行中的实例，或装了没运行的服务 / 容器）。
type Software struct {
	Kind    string `json:"kind,omitempty"` // mysql、redis、nacos……；认不出的容器为空
	Name    string `json:"name"`           // 显示名：MySQL、Redis；认不出的容器为镜像名
	Version string `json:"version,omitempty"`
	State   string `json:"state"` // running | stopped
	// Via 运行方式：systemd（系统服务）| docker（容器）| process（直接启动的进程）
	Via       string `json:"via"`
	Unit      string `json:"unit,omitempty"`      // systemd 单元
	Container string `json:"container,omitempty"` // 容器名
	Image     string `json:"image,omitempty"`     // 容器的镜像
	Status    string `json:"status,omitempty"`    // 容器状态原文（docker ps 的 STATUS，如 Exited (0) 3 days ago）
	// Manager 装它的工具：1Panel、宝塔面板、Ferry、Docker Compose（项目名）……；认不出为空
	Manager string `json:"manager,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Since   string `json:"since,omitempty"` // 启动时间（RFC3339）
	// Listen 主机上能访问的监听地址：进程监听的，或容器映射到主机的端口
	Listen []string `json:"listen,omitempty"`
	// Inner 容器里监听、没有映射到主机的端口（外部访问不到）
	Inner []string `json:"inner,omitempty"`
	Path  string   `json:"path,omitempty"` // 程序（Java 软件为 jar 或安装目录）；容器里的为容器内路径
	// Instance Ferry 安装的软件的实例名（可以启停、看日志、卸载）；其他来源的为空
	Instance string        `json:"instance,omitempty"`
	Hosting  *NginxHosting `json:"hosting,omitempty"`
	Dir      string        `json:"dir,omitempty"` // Ferry 安装的：安装目录
}

// PortReport ports 的结果：服务器上的端口与防火墙（portset 改完也返回它）。
type PortReport struct {
	Firewall Firewall   `json:"firewall"`
	Ports    []PortInfo `json:"ports"`
	// DockerGuard 有 Docker 映射的端口时：Ferry 能不能在 Docker 的 DOCKER-USER 规则链里拦外部访问
	DockerGuard *DockerGuard `json:"dockerGuard,omitempty"`
	Notes       []string     `json:"notes,omitempty"` // 结果可能不全的原因等
}

// DockerGuard 拦 Docker 映射端口的外部访问（不经过 firewalld / ufw）。
type DockerGuard struct {
	OK    bool   `json:"ok"`
	Iface string `json:"iface,omitempty"` // 拦从这张网卡（默认路由的网卡）进来的连接
	Why   string `json:"why,omitempty"`   // 拦不了的原因
}

// Firewall 服务器的防火墙。
type Firewall struct {
	// Kind firewalld | ufw | iptables | nftables；空 = 没找到防火墙
	Kind   string `json:"kind,omitempty"`
	Active bool   `json:"active"`
	// Manage Ferry 能不能放行 / 关闭端口（firewalld、ufw 在运行，而且不是默认放行全部）
	Manage bool   `json:"manage"`
	Zone   string `json:"zone,omitempty"` // firewalld：查看、改动的区域（默认路由那张网卡的区域）
	Why    string `json:"why,omitempty"`  // 不能改的原因
	// Rules 放行的规则（给人看）：ssh、8080/tcp、9000-9100/tcp、3306/tcp 只允许 10.0.0.0/8……
	Rules []string `json:"rules,omitempty"`
	// Enable 装了没开、Ferry 能打开（firewalld、ufw）
	Enable bool `json:"enable,omitempty"`
	// Disable 在运行、Ferry 能关掉（firewalld、ufw）
	Disable bool `json:"disable,omitempty"`
	// Docker 服务器上的 Docker 正在运行（打开、关闭 firewalld 后要重启它，容器的端口映射才正常）
	Docker bool `json:"docker,omitempty"`
}

// PortInfo 一个端口：有程序在监听的，或防火墙放行了的。
type PortInfo struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"` // tcp | udp
	// Listen 监听的地址（*、127.0.0.1、[::1]……）；没有程序在监听时为空
	Listen    []string `json:"listen,omitempty"`
	PID       int      `json:"pid,omitempty"`
	Process   string   `json:"process,omitempty"`   // 程序名
	Unit      string   `json:"unit,omitempty"`      // 所在的 systemd 单元
	Ferry     bool     `json:"ferry,omitempty"`     // Unit 是 Ferry 生成的（部署的应用、装的软件）
	Docker    bool     `json:"docker,omitempty"`    // Docker 映射到主机的端口（防火墙规则管不住）
	Container string   `json:"container,omitempty"` // Docker 映射的端口：容器名
	SSH       bool     `json:"ssh,omitempty"`       // SSH 端口：不能关闭
	// Iface 监听的是具体地址时，地址所在的网卡（virbr0、eth1……）
	Iface string `json:"iface,omitempty"`
	// Internal 只在内部的虚拟网卡上监听（libvirt、Docker 的网桥……）：外部连不到
	Internal bool `json:"internal,omitempty"`
	// Docker 映射的端口
	CtrPort int  `json:"ctrPort,omitempty"` // 映射到容器里的端口
	Guard   bool `json:"guard,omitempty"`   // Ferry 在 DOCKER-USER 里拦着外部访问
	// Manager 容器是谁建的：1Panel | compose；空 = 直接 docker run 或认不出
	Manager        string `json:"manager,omitempty"`
	ComposeFile    string `json:"composeFile,omitempty"`    // compose：配置文件
	ComposeDir     string `json:"composeDir,omitempty"`     // compose：工作目录
	ComposeService string `json:"composeService,omitempty"` // compose：服务名
	// Firewall 防火墙对它：open 放行 | blocked 拦截 | partial 只放行部分来源；空 = 没有防火墙或看不出来
	Firewall string `json:"firewall,omitempty"`
	Rule     string `json:"rule,omitempty"` // 起作用的规则：8080/tcp、服务 http（80/tcp 443/tcp）、范围 8000-8100/tcp……
	Temp     bool   `json:"temp,omitempty"` // 放行只在这次开机有效（没写进永久配置）
}

// evLabels 事件代码给人看的名称（代码本身留在协议与 --json 输出里，程序按代码判断）。
var evLabels = map[string]string{
	EvInfo:     "信息",
	EvWarn:     "注意",
	EvLock:     "加锁",
	EvCache:    "缓存",
	EvAssemble: "组装",
	EvVerify:   "校验",
	EvRelease:  "新版本",
	EvStop:     "停止",
	EvSwitch:   "切换",
	EvUnit:     "单元文件",
	EvStart:    "启动",
	EvWait:     "等待就绪",
	EvReady:    "已就绪",
	EvCrash:    "崩溃",
	EvFail:     "失败",
	EvDiag:     "诊断",
	EvRollback: "回滚",
	EvAbort:    "中止",
	EvPrune:    "清理",
	EvReload:   "重新加载",
	EvLogs:     "日志",
	EvRuntime:  "运行环境",
	EvSoftware: "软件",
	EvFirewall: "防火墙",
	EvHook:     "部署命令",
	EvCert:     "证书",
}

// EvLabel 事件代码的中文名称；不认识的代码原样返回。
func EvLabel(code string) string {
	if l, ok := evLabels[code]; ok {
		return l
	}
	return code
}

// JavaPinLink 锁定 JDK 版本时服务器上的链接名：java-<版本>，+ 换成 _，去掉 -LTS 之类的后缀（25.0.4.1+1 → java-25.0.4.1_1）。
func JavaPinLink(version string) string {
	v, _, _ := strings.Cut(version, "-")
	return "java-" + strings.ReplaceAll(v, "+", "_")
}

// RepoProbe 服务器上软件包管理器用的软件源（agent repoprobe）：服务器自己连不连得上。
type RepoProbe struct {
	Manager string     `json:"manager"` // dnf / yum / apt-get；空 = 没有
	Hosts   []RepoHost `json:"hosts"`
}

// RepoHost 一个软件源地址。
type RepoHost struct {
	Scheme string `json:"scheme"` // http / https / ftp
	Host   string `json:"host"`   // 主机名或 IP
	Port   int    `json:"port"`
	OK     bool   `json:"ok"`
	Err    string `json:"err,omitempty"`
}
