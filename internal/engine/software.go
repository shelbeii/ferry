package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/artifact"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
	"github.com/captain/ferry/internal/secret"
	"github.com/captain/ferry/internal/swpkg"
	"github.com/captain/ferry/internal/swtpl"
)

// SoftwareOptions 按模板安装软件的选项；不填的用默认值。
type SoftwareOptions struct {
	Template string // 模板 ID
	// TemplateYAML 直接给的模板（自定义安装）；不为空时不看 Template
	TemplateYAML []byte
	Variant      int    // 第几种装法（0 起）；-1 = 按服务器自动选
	Name         string // 默认同模板 ID（已有时加 -2、-3……）
	Version      string // 默认最新
	Dir          string // 安装目录；不填时为 <In>/<名称>-<版本>
	In           string // 不填 Dir 时放在这个目录下（默认 /opt/ferry-software）
	User         string
	Java         int // 默认模板要求的
	// JavaDir JDK 的安装目录；空 = Ferry 的公共目录（<ferryDir>/runtimes，多个软件、应用共用）
	JavaDir string
	// FixParents 运行用户进不了安装目录的上级目录时，给它加上进入的权限（用户已同意）
	FixParents bool
	// AllowChanges 从软件源装包会顺带升级、删除已装的包时也继续（用户已同意）
	AllowChanges bool
	Params       map[string]string // 模板参数；password 不填时随机生成
	PackageFile  string            // 用本机库里的这个安装包（没有下载地址的装法、自定义安装）
	// Network 服务器上要下载东西（从软件源装包、模板步骤里的下载）时走谁的网络：
	// local = 经这台电脑；server = 服务器自己的；空或 auto = 先看服务器连不连得上软件源，连不上的经这台电脑
	Network string
}

// SoftwareResult 安装结果。
type SoftwareResult struct {
	proto.SoftwareInstance
	Target   string `json:"target"`
	Password string `json:"password,omitempty"`
	// Saved 密码已存进系统凭据库（以后在「密码」里能再看到）
	Saved bool `json:"saved"`
	// Public 其他机器能不能连：chosen = 选了「允许其他机器访问」；always = 模板没有这个选项（监听所有地址）；空 = 只有本机能连
	Public string `json:"public,omitempty"`
	// OpenPorts 其他机器要连时防火墙要放行的端口（含附加端口）；Public 为空时没有
	OpenPorts []int `json:"openPorts,omitempty"`
	OpenUDP   []int `json:"openUDP,omitempty"`
}

// SoftwareSources 下载源（设置 → 运行环境）。
func (e *Engine) SoftwareSources() swpkg.Sources {
	return swpkg.Sources{GitHubMirror: e.Settings.Runtime.GitHubMirror, MySQL: e.Settings.Runtime.MySQLSource}
}

// Templates 全部模板（内置 + 自己的）。
func (e *Engine) Templates() ([]*swtpl.Template, []error) {
	return swtpl.All(e.Settings.TemplateDir())
}

var (
	reMemory = regexp.MustCompile(`^[1-9][0-9]{1,5}[mg]$`)
	// 账号：数据库、控制台都能用，写进 SQL、配置也不用转义
	reAccount = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{2,31}$`)
	rePass    = regexp.MustCompile(`^[A-Za-z0-9!@#%^*()_+=.,:~-]{8,64}$`)
)

// SoftwareInstall 按模板在服务器上安装软件：选出适合这台服务器的装法，准备安装包（本机库里没有就下载）、需要时装 JDK、上传，
// 渲染成安装计划交给 agent。
func (e *Engine) SoftwareInstall(target string, o SoftwareOptions) (*SoftwareResult, error) {
	var tpl *swtpl.Template
	var err error
	if len(o.TemplateYAML) > 0 {
		tpl, err = swtpl.Parse(o.TemplateYAML)
	} else {
		tpl, err = swtpl.Find(e.Settings.TemplateDir(), o.Template)
	}
	if err != nil {
		return nil, err
	}
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	s, err := e.connect(t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	e.Rep.Step("查看 %s 上已有的软件与端口", t.Name)
	var rep proto.SoftwareReport
	if err := s.callData("software", &proto.Request{}, &rep); err != nil {
		return nil, err
	}
	osInfo := swtpl.OS{}
	if rep.OS != nil {
		osInfo = swtpl.OS{ID: rep.OS.ID, Like: rep.OS.Like, Version: rep.OS.Version, Pretty: rep.OS.Pretty}
	}
	vi, err := tpl.Pick(osInfo, s.arch, o.Variant)
	if err != nil {
		return nil, err
	}
	v := tpl.Variants[vi]
	e.Rep.Info("%s：%s", tpl.Name, v.Label)

	// 名称、目录、用户
	taken := map[string]bool{}
	for _, it := range rep.Items {
		if it.Instance != "" {
			taken[it.Instance] = true
		}
	}
	name := o.Name
	if name == "" {
		name = tpl.ID
		for i := 2; taken[name]; i++ {
			name = tpl.ID + "-" + strconv.Itoa(i)
		}
	} else if taken[name] {
		return nil, fmt.Errorf("%s 上已经有 Ferry 安装的 %s：换一个名称", t.Name, name)
	}

	// 版本：先定下来（目录名要用），安装包等检查过目录再下载
	var pkg *swpkg.Meta
	lib := e.Settings.SoftwareDir()
	version := o.Version
	if p := v.Package; p != nil {
		switch {
		case o.PackageFile != "":
			if pkg = swpkg.FindFile(lib, o.PackageFile); pkg == nil {
				return nil, fmt.Errorf("本机库里没有 %s：先导入", o.PackageFile)
			}
			if version == "" {
				version = pkg.Version
			}
		case p.URL == "":
			if pkg = swpkg.Find(lib, tpl.ID, version); pkg == nil {
				return nil, fmt.Errorf("%s 没有下载地址：先在「导入安装包」里导入，再选它", tpl.Name)
			}
			version = pkg.Version
		case version == "":
			latest, err := swpkg.Latest(e.ctx(), e.SoftwareSources(), p)
			switch {
			case err == nil:
				version = latest
			case swpkg.Find(lib, tpl.ID, "") != nil:
				version = swpkg.Find(lib, tpl.ID, "").Version
				e.Rep.Warn("%v；使用本机已有的 %s", err, version)
			case p.Fallback != "":
				version = p.Fallback
				e.Rep.Warn("%v；使用 %s", err, version)
			default:
				return nil, err
			}
		}
	}
	// 安装目录：没指定时放在 o.In（默认 /opt/ferry-software）下，文件夹名是「名称-版本」，一看就知道装的哪个版本
	dir := o.Dir
	if dir == "" {
		in := o.In
		if in == "" {
			in = "/opt/ferry-software"
		}
		dir = path.Join(in, SoftwareFolder(name, version))
	}
	user := o.User
	if user == "" {
		user = v.User
	}

	// 参数
	vars := swtpl.Vars{"name": name, "dir": dir, "user": user, "host": t.Host}
	var ports, udpPorts []int
	portMap := map[string]int{}
	password, account := "", ""
	known := map[string]bool{}
	for _, p := range tpl.Params {
		known[p.Key] = true
		val, given := o.Params[p.Key]
		if !given {
			val = p.Default
		}
		label := p.Label
		if label == "" {
			label = p.Key
		}
		switch p.Type {
		case "port":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("%s 要写 1～65535 的端口：%q", label, val)
			}
			// 没指定端口、默认的又被占着：往后找一个空闲的（附加端口一起空闲）
			if !given && !portFree(p, n, ports, udpPorts, rep) {
				for q := n + 1; q <= n+1000 && q <= 65535; q++ {
					if portFree(p, q, ports, udpPorts, rep) {
						e.Rep.Info("%s 默认的 %d 已被占用：改用 %d", label, n, q)
						n, val = q, strconv.Itoa(q)
						break
					}
				}
			}
			portMap[p.Key] = n
			for _, off := range append([]int{0}, p.Offsets...) {
				q := n + off
				what := label
				if off != 0 {
					what = fmt.Sprintf("%s +%d", label, off)
				}
				for _, c := range []struct {
					on         bool
					pr         string
					list, used *[]int
				}{{p.TCP(), "tcp", &ports, &rep.Ports}, {p.UDP(), "udp", &udpPorts, &rep.UDPPorts}} {
					if !c.on {
						continue
					}
					pr := ""
					if c.pr == "udp" {
						pr = "UDP "
					}
					if slices.Contains(*c.list, q) {
						return nil, fmt.Errorf("%s端口 %d 重复", pr, q)
					}
					if slices.Contains(*c.used, q) {
						by := "已被占用"
						if who := rep.PortUsers[c.pr+"/"+strconv.Itoa(q)]; who != "" {
							by = "已被 " + who + "占用（它停着时也算）"
						}
						return nil, fmt.Errorf("%s 上%s端口 %d（%s）%s：换一个", t.Name, pr, q, what, by)
					}
					*c.list = append(*c.list, q)
				}
			}
		case "bool":
			val = strconv.FormatBool(swtpl.Truthy(val))
		case "memory":
			val = strings.ToLower(val)
			if !reMemory.MatchString(val) {
				return nil, fmt.Errorf("%s 要写成 512m、2g 这样：%q", label, val)
			}
		case "number":
			if _, err := strconv.Atoi(val); err != nil {
				return nil, fmt.Errorf("%s 要写数字：%q", label, val)
			}
		case "account":
			if !reAccount.MatchString(val) {
				return nil, fmt.Errorf("%s 要 3～32 位，字母开头，只能用字母、数字、下划线：%q", label, val)
			}
			if account == "" {
				account = val
			}
		case "select":
			if !slices.Contains(p.Options, val) {
				return nil, fmt.Errorf("%s 只能选 %s：%q", label, strings.Join(p.Options, "、"), val)
			}
		case "password":
			if val == "" {
				val = GeneratePassword()
			}
			if !rePass.MatchString(val) {
				return nil, fmt.Errorf("%s 要 8～64 位，只能用字母、数字和 !@#%%^*()_+=.,:~-", label)
			}
			if p.Key == "password" {
				password = val
			}
		default:
			if strings.ContainsAny(val, "\n\r") {
				return nil, fmt.Errorf("%s 不能换行", label)
			}
		}
		vars[p.Key] = val
	}
	for k := range o.Params {
		if !known[k] {
			return nil, fmt.Errorf("%s 没有参数 %s", tpl.Name, k)
		}
	}

	// 下载、上传之前先检查目录：有问题在这里就停下，服务器上不做任何改动
	java := v.Java
	if o.Java > 0 {
		java = o.Java
	}
	javaDir := ""
	if java > 0 && o.JavaDir != "" {
		javaDir = path.Clean(o.JavaDir)
		if javaDir == dir || strings.HasPrefix(javaDir, dir+"/") || strings.HasPrefix(dir, javaDir+"/") {
			return nil, fmt.Errorf("JDK 的安装目录不能和软件的安装目录 %s 重叠：换一个", dir)
		}
	}
	var chk proto.SwCheck
	if err := s.callData("swcheck", &proto.Request{SwCheck: &proto.SwCheckSpec{Dir: dir, User: user, Java: java, JavaDir: javaDir}}, &chk); err != nil {
		return nil, err
	}
	if chk.DirBusy {
		return nil, fmt.Errorf("%s 上的安装目录 %s 已存在且不是空的：换一个目录", t.Name, dir)
	}
	if err := diskRoom(t.Name, dir, chk, 0); err != nil {
		return nil, err
	}
	for _, p := range tpl.Params {
		if mem := vars[p.Key]; p.Type == "memory" && rep.MemAvailMB > 0 && memoryMB(mem) > rep.MemAvailMB {
			e.Rep.Warn("内存设成了 %s，%s 现在可用的内存只有 %d MB：其他程序占着的内存不释放的话，%s 可能起不来或被系统杀掉", mem, t.Name, rep.MemAvailMB, tpl.Name)
		}
	}
	fix := false
	if len(chk.Blocked) > 0 {
		if !o.FixParents && !e.Yes {
			return nil, &BlockedDirsError{Target: t.Name, User: user, Dirs: chk.Blocked}
		}
		fix = true
		e.Rep.Info("运行用户 %s 进不了 %s：安装时给它加上进入的权限", user, blockedPaths(chk.Blocked))
	}

	// 安装包：本机库里没有就下载
	if p := v.Package; p != nil {
		if pkg == nil {
			if pkg = swpkg.Find(lib, tpl.ID, version); pkg == nil {
				e.Rep.Step("下载 %s %s（%s）", tpl.Name, version, orUnknown(p.Size))
				pkg, err = swpkg.Download(e.ctx(), e.SoftwareSources(), tpl.ID, p, version, lib, func(done, total int64) { e.Rep.Progress(tpl.Name, done, total) })
				if err != nil {
					return nil, fmt.Errorf("下载 %s %s 失败：%w（可以在设置 → 运行环境里设置 GitHub 加速，或在浏览器里下载后导入）", tpl.Name, version, err)
				}
				e.Rep.Info("已下载 %s（%s），存放在 %s", pkg.File, humanBytes(pkg.Size), lib)
			} else {
				e.Rep.Info("使用本机已有的 %s（%s）", pkg.File, humanBytes(pkg.Size))
			}
		}
		if err := diskRoom(t.Name, dir, chk, pkg.Size); err != nil {
			return nil, err
		}
		if swtpl.FormatOf(p.Format, pkg.File) == "binary" {
			if err := checkProgramArch(filepath.Join(lib, pkg.File), s.arch); err != nil {
				return nil, err
			}
		}
	}
	vars["version"] = version

	// Java 软件用 Ferry 提供的 JDK：装在公共目录，或用户指定的目录下以版本命名的文件夹里（已有就直接用）
	vars["java"] = ""
	javaHome := ""
	switch {
	case java > 0 && javaDir != "":
		javaHome = chk.JavaHome
		if javaHome != "" {
			e.Rep.Info("使用已有的 JDK %d：%s", java, javaHome)
		} else {
			r, err := e.installJDK(s, java, javaDir)
			if err != nil {
				return nil, err
			}
			javaHome = javaDir + "/" + r.ID
		}
		vars["java"] = javaHome + "/bin/java"
	case java > 0:
		if _, ok := rep.Runtimes[java]; !ok {
			if _, err := e.installJDK(s, java, ""); err != nil {
				return nil, err
			}
		}
		vars["java"] = s.ferryDir + "/runtimes/java-" + strconv.Itoa(java) + "/bin/java"
	}

	// 上次安装失败时服务器上留着同一个安装包：不用再上传
	cached := false
	if pkg != nil {
		var c proto.SwCheck
		if err := s.callData("swcheck", &proto.Request{SwCheck: &proto.SwCheckSpec{PackageSHA: pkg.SHA256}}, &c); err == nil {
			cached = c.Cached
		}
	}

	stage := ""
	if pkg != nil && !cached {
		var prep proto.Prepared
		if err := s.callData("prepare", &proto.Request{}, &prep); err != nil {
			return nil, err
		}
		stage = prep.Stage
	}
	vars["package"] = ""
	if pkg != nil {
		// 模板里的 ${package}：安装包在服务器上的位置
		vars["package"] = remote.Join(stage, pkg.File)
		if cached {
			vars["package"] = s.ferryDir + "/software/.cache/" + pkg.SHA256
		}
	}
	plan, err := renderPlan(tpl, &v, vars, pkg)
	if err != nil {
		return nil, err
	}
	plan.Name, plan.Dir, plan.User, plan.Version, plan.Account = name, dir, user, version, account
	plan.Variant, plan.Logrotate = vi, v.Logrotate
	plan.Ports, plan.UDPPorts, plan.PortMap = ports, udpPorts, portMap
	plan.FixParents = fix
	if plan.System != nil {
		plan.System.AllowChanges = o.AllowChanges
	}
	if javaHome != "" {
		plan.Reach = []string{javaHome}
	}
	if cached {
		plan.Package.Cached = true
		e.Rep.Info("服务器上留着上次上传的 %s，不用再上传", pkg.File)
	} else if pkg != nil {
		e.Rep.Step("上传 %s 到 %s", pkg.File, t.Name)
		item := remote.UploadItem{Local: filepath.Join(lib, pkg.File), Remote: remote.Join(stage, pkg.File), Mode: 0o640, Size: pkg.Size}
		if err := e.uploadItems(s, []remote.UploadItem{item}, pkg.Size, pkg.Size); err != nil {
			return nil, fmt.Errorf("上传安装包失败：%w", err)
		}
	}
	proxy, err := e.softwareNetwork(s, plan, o.Network)
	if err != nil {
		return nil, err
	}
	if proxy != nil {
		defer e.closeNetProxy(proxy)
	}
	e.Rep.Step("在 %s 上安装 %s", t.Name, strings.TrimSpace(tpl.Name+" "+version))
	var inst proto.SoftwareInstance
	// 密码在动服务器之前就存进凭据库：连接断开、本机没拿到结果时，服务器上照样会装完，密码不会丢
	pwKey := secret.SoftwareAccount(name, t.Host, t.Port)
	saved := false
	if password != "" {
		if err := secret.Set(pwKey, password); err != nil {
			e.Rep.Warn("密码没能存进系统凭据库（%v）：装好后请自己记下来", err)
		} else {
			saved = true
		}
	}
	forget := func() {
		if saved {
			secret.Delete(pwKey)
		}
	}
	r, err := s.call("swinstall", &proto.Request{Stage: stage, Install: plan})
	if err != nil {
		if saved {
			return nil, fmt.Errorf("%w。稍后在「软件」里刷新：出现 %s 就是装好了，密码已经先存进本机凭据库，点「密码」能看到", err, name)
		}
		return nil, err
	}
	switch r.RC {
	case proto.CodeOK:
		if err := json.Unmarshal(r.Data, &inst); err != nil {
			return nil, err
		}
	case proto.CodeConfirm:
		forget()
		var c proto.SwChanges
		json.Unmarshal(r.Data, &c)
		return nil, &PkgChangesError{Target: t.Name, Packages: c.Packages, Changes: c.Changes, Msg: r.Msg}
	default:
		forget()
		return nil, &AgentError{Cmd: "swinstall", RC: r.RC, Msg: r.Msg}
	}
	res := &SoftwareResult{SoftwareInstance: inst, Target: t.Name, Password: password, Public: publicMode(tpl, vars)}
	if res.Public != "" {
		res.OpenPorts, res.OpenUDP = ports, udpPorts
	}
	res.Saved = saved
	if password != "" && slices.Contains(inst.Unset, "password") {
		// 设置密码的步骤没成功：这个密码用不了，不给用户、不保存（notes 里有怎么补设）
		forget()
		res.Password, res.Saved = "", false
		e.Rep.Warn("%s 的密码没有设置成功：按安装结果里的说明自己设置", name)
	}
	return res, nil
}

// portFree 端口参数 p 用 n 时，它要占的端口（含附加端口、按 TCP / UDP）都没被占用，也没和前面的端口参数重复。
func portFree(p swtpl.Param, n int, tcp, udp []int, rep proto.SoftwareReport) bool {
	for _, off := range append([]int{0}, p.Offsets...) {
		q := n + off
		if q < 1 || q > 65535 ||
			p.TCP() && (slices.Contains(tcp, q) || slices.Contains(rep.Ports, q)) ||
			p.UDP() && (slices.Contains(udp, q) || slices.Contains(rep.UDPPorts, q)) {
			return false
		}
	}
	return true
}

// diskRoom 安装目录所在分区够不够：只读、inode 用完、空间不够时在下载、上传之前就停下。
// size 为安装包大小（还不知道时为 0）：上传、解压、软件自己的数据大约要三倍，再留 300 MB。
func diskRoom(target, dir string, chk proto.SwCheck, size int64) error {
	if chk.ReadOnly {
		return fmt.Errorf("%s 上安装目录 %s 所在的分区是只读的：换一个安装位置", target, dir)
	}
	if chk.Inodes >= 0 && chk.Inodes < 5000 {
		return fmt.Errorf("%s 上安装目录 %s 所在的分区文件个数（inode）快用完了，只剩 %d：先清理小文件，或换一个安装位置", target, dir, chk.Inodes)
	}
	need := size*3/(1<<20) + 300
	if chk.FreeMB >= 0 && chk.FreeMB < need {
		return fmt.Errorf("%s 上安装目录 %s 所在的分区只剩 %d MB，至少要 %d MB：先清理，或换一个空间大的安装位置", target, dir, chk.FreeMB, need)
	}
	if size > 0 && chk.StageFreeMB >= 0 && chk.StageFreeMB < size/(1<<20)+100 {
		return fmt.Errorf("%s 上放上传文件的分区（Ferry 组件目录所在）只剩 %d MB，放不下 %s 的安装包：先清理", target, chk.StageFreeMB, humanBytes(size))
	}
	return nil
}

// memoryMB 512m、2g 这样的内存写法换成 MB；写错的为 0。
func memoryMB(s string) int64 {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 2 {
		return 0
	}
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil {
		return 0
	}
	switch s[len(s)-1] {
	case 'm':
		return n
	case 'g':
		return n * 1024
	}
	return 0
}

// publicMode 装好后其他机器能不能连（见 SoftwareResult.Public）：模板有 bool 参数 public 时看它的值，没有时算作总是能连。
func publicMode(tpl *swtpl.Template, vars swtpl.Vars) string {
	for _, p := range tpl.Params {
		if p.Key == "public" && p.Type == "bool" {
			if swtpl.Truthy(vars["public"]) {
				return "chosen"
			}
			return ""
		}
	}
	return "always"
}

var reFolderUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SoftwareFolder 软件的文件夹名：名称-版本（如 nacos-3.2.4）；没有版本（如从软件源装）时只用名称。
func SoftwareFolder(name, version string) string {
	v := strings.Trim(reFolderUnsafe.ReplaceAllString(version, "_"), "._-")
	if v == "" || strings.HasSuffix(name, "-"+v) {
		return name
	}
	return name + "-" + v
}

// PkgChangesError 从软件源装包会顺带升级、删除已装的包（桌面版据此询问要不要继续）；服务器上还没有改动。
type PkgChangesError struct {
	Target            string
	Packages, Changes []string
	Msg               string
}

func (e *PkgChangesError) Error() string {
	return e.Msg + "。确认没问题的话加上 --allow-changes 重新安装"
}

// JSONDetail --json 输出的附加信息。
func (e *PkgChangesError) JSONDetail() map[string]any {
	return map[string]any{"kind": "pkg-changes", "target": e.Target, "packages": e.Packages, "changes": e.Changes}
}

// BlockedDirsError 运行用户进不了安装目录的上级目录（桌面版据此询问要不要加上权限再装）。
type BlockedDirsError struct {
	Target, User string
	Dirs         []proto.BlockedDir
}

func (e *BlockedDirsError) Error() string {
	var parts []string
	for _, d := range e.Dirs {
		s := d.Path
		if d.Owner != "" {
			s += fmt.Sprintf("（属于 %s，%s）", d.Owner, d.Mode)
		}
		parts = append(parts, s)
	}
	return fmt.Sprintf("运行用户 %s 进不了 %s 上的 %s：上级目录不允许其他用户进入。还没有上传，服务器上没有做任何改动；"+
		"加上 --fix-parents 重新安装会给它加上进入的权限（只能进入，看不到里面有哪些文件），或换一个安装目录",
		e.User, e.Target, strings.Join(parts, "、"))
}

// JSONDetail --json 输出的附加信息。
func (e *BlockedDirsError) JSONDetail() map[string]any {
	dirs := make([]string, 0, len(e.Dirs))
	for _, d := range e.Dirs {
		dirs = append(dirs, d.Path)
	}
	return map[string]any{"kind": "blocked-dirs", "target": e.Target, "user": e.User, "dirs": dirs}
}

func blockedPaths(list []proto.BlockedDir) string {
	parts := make([]string, 0, len(list))
	for _, d := range list {
		parts = append(parts, d.Path)
	}
	return strings.Join(parts, "、")
}

// checkProgramArch 安装单个程序文件前，看它能不能在这台服务器上运行（CPU 架构不对 systemd 只会报 203/EXEC）。
// 脚本（#! 开头）不管架构。
func checkProgramArch(p, serverArch string) error {
	name := filepath.Base(p)
	hint := fmt.Sprintf("请换成 Linux %s 版的程序，重新导入后再装", serverArch)
	arch, err := artifact.ELFArch(p)
	if err != nil {
		head := swpkg.FileHead(p, 4)
		if strings.HasPrefix(head, "#!") {
			return nil
		}
		if head == "\x7fELF" {
			return fmt.Errorf("%v，服务器是 %s：%s", err, serverArch, hint)
		}
		return fmt.Errorf("%s 不是 Linux 程序（可能是 Windows 或 macOS 版）：%s", name, hint)
	}
	if arch != serverArch {
		return fmt.Errorf("%s 是 %s 版的程序，服务器是 %s，运行不了：%s", name, arch, serverArch, hint)
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "大小未知"
	}
	return s
}

// renderPlan 把装法里的 ${…} 换成值，得到给 agent 的安装计划（if 条件不满足的文件、步骤去掉）。
func renderPlan(tpl *swtpl.Template, v *swtpl.Variant, vars swtpl.Vars, pkg *swpkg.Meta) (*proto.SoftwarePlan, error) {
	var firstErr error
	x := func(s string) string {
		out, err := vars.Expand(s)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return out
	}
	pl := &proto.SoftwarePlan{Template: tpl.ID, Title: tpl.Name, Exec: strings.TrimSpace(x(v.Service.Exec)), Workdir: x(v.Service.Workdir)}
	for _, c := range v.Configs {
		pl.Configs = append(pl.Configs, proto.SoftwareConfigFile{Label: c.Label, Path: x(c.Path), Format: c.Format, Apply: c.Apply})
	}
	pl.SELinux = v.SELinux
	if h := v.Hosting; h != nil {
		pl.Hosting = &proto.NginxHosting{Binary: x(h.Binary), Config: x(h.Config), Prefix: x(h.Prefix), Sites: x(h.Sites)}
	}
	if p := v.Package; p != nil && pkg != nil {
		pl.Package = &proto.PlanPackage{File: pkg.File, SHA256: pkg.SHA256, Size: pkg.Size, Format: swtpl.FormatOf(p.Format, pkg.File),
			Strip: p.Strip, Into: x(p.Into), Check: x(p.Check)}
		if pl.Package.Format == "jar" {
			pl.Package.Check = "" // jar 里的类在本机导入、下载时已检查
		}
	}
	if sy := v.System; sy != nil {
		pl.System = &proto.PlanSystem{DNF: sy.DNF, APT: sy.APT, Disable: sy.Disable, Require: x(sy.Require)}
	}
	for _, f := range v.Files {
		if !vars.Cond(f.If) {
			continue
		}
		mode := uint32(0o640)
		if f.Mode != "" {
			m, err := strconv.ParseUint(f.Mode, 8, 32)
			if err != nil {
				return nil, fmt.Errorf("文件 %s 的 mode 要写成 0640 这样：%q", f.Path, f.Mode)
			}
			mode = uint32(m)
		}
		pl.Files = append(pl.Files, proto.PlanFile{Path: x(f.Path), Content: x(f.Content), Mode: mode, At: f.At})
	}
	for _, p := range v.Props {
		set := map[string]string{}
		for k, val := range p.Set {
			set[k] = x(val)
		}
		pl.Props = append(pl.Props, proto.PlanProps{File: x(p.File), Sep: p.Sep, Set: set})
	}
	for _, st := range v.Steps {
		if !vars.Cond(st.If) {
			continue
		}
		ps := proto.PlanStep{At: st.At, Run: x(st.Run), Timeout: st.Timeout, Error: st.Error, Soft: st.Soft, Sets: st.Sets}
		if h := st.HTTP; h != nil {
			form := map[string]string{}
			for k, val := range h.Form {
				form[k] = x(val)
			}
			ps.HTTP = &proto.PlanHTTP{Post: x(h.Post), Form: form, Retry: h.Retry}
		}
		pl.Steps = append(pl.Steps, ps)
	}
	if len(v.Service.Env) > 0 {
		pl.Env = map[string]string{}
		for k, val := range v.Service.Env {
			pl.Env[k] = x(val)
		}
	}
	port, err := strconv.Atoi(x(v.Service.Ready.Port))
	if err != nil {
		return nil, fmt.Errorf("模板 %s：service.ready.port 不是端口：%q", tpl.ID, v.Service.Ready.Port)
	}
	pl.ReadyPort, pl.ReadyTimeout = port, v.Service.Ready.Timeout
	for _, l := range v.Logs {
		if l = x(l); l != "" {
			pl.Logs = append(pl.Logs, l)
		}
	}
	for _, n := range v.Notes {
		pl.Notes = append(pl.Notes, x(n))
	}
	return pl, firstErr
}

// SoftwareLatest 模板的最新版本（只有要上传安装包的装法有）。
func (e *Engine) SoftwareLatest(id string, variant int) (string, error) {
	tpl, err := swtpl.Find(e.Settings.TemplateDir(), id)
	if err != nil {
		return "", err
	}
	for i, v := range tpl.Variants {
		if (variant < 0 || i == variant) && v.Package != nil && v.Package.URL != "" {
			return swpkg.Latest(e.ctx(), e.SoftwareSources(), v.Package)
		}
	}
	return "", errors.New("这个模板不用下载安装包")
}

// GeneratePassword 随机密码。
func GeneratePassword() string { return swtpl.RandomString(16) }

// SoftwareControl 启动 / 停止 / 重启 Ferry 安装的软件。
func (e *Engine) SoftwareControl(target, name, action string) error {
	return e.withTarget(target, func(s *session) error {
		return s.callData("swcontrol", &proto.Request{Instance: name, Action: action}, nil)
	})
}

// SoftwareRemove 卸载 Ferry 安装的软件；purge 时连安装目录（数据）一起删除。
func (e *Engine) SoftwareRemove(target, name string, purge bool) error {
	return e.withTarget(target, func(s *session) error {
		if s.t.NeedConfirm() && !e.Yes {
			what := "（保留安装目录和数据）"
			if purge {
				what = "，并删除安装目录和里面的全部数据"
			}
			if !e.Rep.Confirm(fmt.Sprintf("卸载 %s 上的 %s%s？", s.t.Name, name, what)) {
				return ErrCanceled
			}
		}
		if err := s.callData("swremove", &proto.Request{Instance: name, Purge: purge}, nil); err != nil {
			return err
		}
		// 保留数据时密码也留着：恢复后照样能用
		if purge {
			secret.Delete(secret.SoftwareAccount(name, s.t.Host, s.t.Port))
		}
		return nil
	})
}

// SoftwareLogs Ferry 安装的软件的日志：journald，或软件自己的日志文件（main）。返回内容与来源。
func (e *Engine) SoftwareLogs(target, name string, lines int, main bool) (*proto.LogText, string, error) {
	var out proto.LogText
	var from string
	err := e.withTarget(target, func(s *session) error {
		r, err := s.call("swlogs", &proto.Request{Instance: name, Lines: lines, LogMain: main})
		if err != nil {
			return err
		}
		if r.RC != proto.CodeOK {
			return &AgentError{Cmd: "swlogs", RC: r.RC, Msg: r.Msg}
		}
		from = r.Msg
		if len(r.Data) == 0 {
			return nil
		}
		return json.Unmarshal(r.Data, &out)
	})
	return &out, from, err
}

// SoftwareUnit 不是 Ferry 装的软件：按系统服务（unit）或 Docker 容器（container）启动 / 停止 / 重启 / 看日志（logs）。
// 看日志时返回内容与来源。
func (e *Engine) SoftwareUnit(target, unit, container, action string, lines int) (*proto.LogText, string, error) {
	var out proto.LogText
	var from string
	err := e.withTarget(target, func(s *session) error {
		if action != "logs" && s.t.NeedConfirm() && !e.Yes {
			what := unit
			if what == "" {
				what = "容器 " + container
			}
			label := map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}[action]
			if action != "start" && !e.Rep.Confirm(fmt.Sprintf("%s %s 上的 %s？", label, s.t.Name, what)) {
				return ErrCanceled
			}
		}
		r, err := s.call("swunit", &proto.Request{SwUnit: unit, SwContainer: container, Action: action, Lines: lines})
		if err != nil {
			return err
		}
		if r.RC != proto.CodeOK {
			return &AgentError{Cmd: "swunit", RC: r.RC, Msg: r.Msg}
		}
		from = r.Msg
		if len(r.Data) == 0 {
			return nil
		}
		return json.Unmarshal(r.Data, &out)
	})
	return &out, from, err
}

// SoftwarePassword 安装时保存的密码。
func (e *Engine) SoftwarePassword(target, name string) (string, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return "", err
	}
	v, ok, err := secret.Get(secret.SoftwareAccount(name, t.Host, t.Port))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("本机没有保存 %s 上 %s 的密码（可能是在别的电脑上安装的，或这个软件没有密码）", t.Name, name)
	}
	return v, nil
}

// withTarget 连接目标后执行（不需要应用）。
func (e *Engine) withTarget(target string, fn func(*session) error) error {
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	s, err := e.connect(t)
	if err != nil {
		return err
	}
	defer s.close()
	return fn(s)
}

// softwareNetwork 安装时服务器要下载东西的话，决定走谁的网络；要经这台电脑时开好代理、写进安装计划。
// 返回的代理用完要 closeNetProxy；不经这台电脑时为 nil。
func (e *Engine) softwareNetwork(s *session, plan *proto.SoftwarePlan, mode string) (*netProxy, error) {
	steps := false
	for _, st := range plan.Steps {
		steps = steps || st.Run != ""
	}
	switch mode {
	case "", "auto":
		mode = "auto"
	case "local", "server":
	default:
		return nil, fmt.Errorf("下载方式只能是 auto、local、server（当前 %q）", mode)
	}
	if mode == "server" || (plan.System == nil && !(mode == "local" && steps)) {
		return nil, nil
	}
	var direct []string
	if mode == "auto" {
		var rp proto.RepoProbe
		if err := s.callData("repoprobe", &proto.Request{}, &rp); err != nil {
			return nil, err
		}
		var bad []string
		for _, h := range rp.Hosts {
			if h.OK {
				direct = append(direct, h.Host)
			} else {
				bad = append(bad, fmt.Sprintf("%s（%s）", h.Host, h.Err))
			}
		}
		if len(rp.Hosts) > 0 && len(bad) == 0 {
			e.Rep.Info("服务器自己连得上软件源（%s），用服务器的网络下载", strings.Join(direct, "、"))
			return nil, nil
		}
		if len(bad) > 0 {
			e.Rep.Info("服务器连不上软件源 %s：这些经这台电脑下载", strings.Join(bad, "、"))
		} else {
			e.Rep.Info("服务器上没找到软件源的地址：下载经这台电脑")
		}
	} else {
		e.Rep.Info("下载经这台电脑（服务器的软件包管理器、安装步骤通过 SSH 用这台电脑的网络）")
	}
	p, err := startNetProxy(s.c)
	if err != nil {
		if mode == "auto" {
			e.Rep.Warn("%v；改用服务器自己的网络", err)
			return nil, nil
		}
		return nil, err
	}
	plan.Proxy, plan.NoProxy = p.URL, direct
	return p, nil
}

// closeNetProxy 关掉代理，说明经这台电脑下载了多少、这台电脑也连不上哪些地址。
func (e *Engine) closeNetProxy(p *netProxy) {
	p.Close()
	bytes, reqs, failed := p.Summary()
	if reqs > 0 {
		e.Rep.Info("经这台电脑下载了 %s（%d 次请求）", humanBytes(bytes), reqs)
	}
	if len(failed) > 0 {
		e.Rep.Warn("这台电脑也连不上：%s", strings.Join(failed, "、"))
	}
}
