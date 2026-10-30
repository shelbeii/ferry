package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/scaffold"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type commandIn struct {
	targetIn
	Command string `json:"command" jsonschema:"要执行的只读命令，如 df -h、free -m、cat /etc/nginx/nginx.conf、ss -lntp、systemctl status xxx"`
}

type buildLogIn struct {
	Project string `json:"project,omitempty" jsonschema:"项目名或 ferry.yaml 的路径；不填 = 当前目录的项目，或只有一个项目时用它"`
	File    string `json:"file,omitempty" jsonschema:"哪个日志（返回的 files 里的 name）；不填 = 最近一次"`
	Lines   int    `json:"lines,omitempty" jsonschema:"最后多少行，默认 200，最多 2000"`
}

type softwareIn struct {
	targetIn
	Action string `json:"action" jsonschema:"start、stop 或 restart"`
	Name   string `json:"name" jsonschema:"Ferry 装的软件的实例名（server_software 结果里的 instance）"`
	Reason string `json:"reason" jsonschema:"为什么要这样做（显示在 Ferry 的确认框里）"`
}

type portIn struct {
	targetIn
	Action string `json:"action" jsonschema:"open（放行）或 close（关闭）"`
	Port   string `json:"port" jsonschema:"端口，如 8080/tcp、53/udp（不写协议按 tcp）"`
	Reason string `json:"reason" jsonschema:"为什么要这样做（显示在 Ferry 的确认框里）"`
}

type detectIn struct {
	Dir string `json:"dir" jsonschema:"项目目录（代码所在的目录）"`
}

// createSpec 新建项目的内容（字段和 Ferry 新建项目向导一样，转成 scaffold.Spec）。
type createSpec struct {
	ProjectDir string         `json:"projectDir" jsonschema:"项目目录（代码所在的目录）"`
	Project    string         `json:"project" jsonschema:"项目名：小写字母、数字、. _ -（detect_project 给的建议名）"`
	Targets    []createTarget `json:"targets" jsonschema:"部署到哪些服务器（地址、用户等由用户提供，不要编）"`
	Apps       []createApp    `json:"apps" jsonschema:"应用（照 detect_project 的结果填）"`
}

type createTarget struct {
	Name         string `json:"name" jsonschema:"目标名，如 test、prod"`
	Host         string `json:"host" jsonschema:"服务器地址"`
	Port         int    `json:"port,omitempty" jsonschema:"SSH 端口，默认 22"`
	User         string `json:"user,omitempty" jsonschema:"登录用户，默认 root"`
	Sudo         bool   `json:"sudo,omitempty" jsonschema:"登录用户不是 root 时用 sudo"`
	Auth         string `json:"auth,omitempty" jsonschema:"key（默认，私钥或 ssh-agent）或 password"`
	IdentityFile string `json:"identityFile,omitempty" jsonschema:"私钥文件"`
	Root         string `json:"root,omitempty" jsonschema:"部署目录，默认 /opt/<项目名>"`
	RunAs        string `json:"runAs,omitempty" jsonschema:"服务的运行用户"`
	Arch         string `json:"arch,omitempty" jsonschema:"amd64 或 arm64，默认连接时识别"`
}

type createApp struct {
	Name         string            `json:"name"`
	Type         string            `json:"type" jsonschema:"java、binary、static、python、node（Node 服务端，不是前端）或 dotnet"`
	Workdir      string            `json:"workdir,omitempty" jsonschema:"构建的目录（相对项目目录）"`
	Command      string            `json:"command,omitempty" jsonschema:"构建命令；空 = 不构建"`
	Env          map[string]string `json:"env,omitempty"`
	Artifact     string            `json:"artifact,omitempty" jsonschema:"产物（相对项目目录）：jar、程序、前端的输出目录；python、node 是要上传的目录，dotnet 是发布的输出目录"`
	Files        []createFile      `json:"files,omitempty" jsonschema:"java、binary：附带文件（程序要读的配置文件、证书目录等），部署时和程序放在同一级目录，随版本走"`
	SharedBuild  bool              `json:"sharedBuild,omitempty"`
	BuildGroup   string            `json:"buildGroup,omitempty"`
	GroupCommand string            `json:"groupCommand,omitempty"`
	Module       string            `json:"module,omitempty"`
	Java         string            `json:"java,omitempty" jsonschema:"服务器上 java 的路径（可选）"`
	RuntimeJava  int               `json:"runtimeJava,omitempty" jsonschema:"由 Ferry 提供的 JDK 大版本，如 17"`
	JvmArgs      []string          `json:"jvmArgs,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Port         int               `json:"port,omitempty" jsonschema:"监听端口（健康检查、静态站点用）"`
	Health       string            `json:"health,omitempty" jsonschema:"tcp、http 或 none"`
	HealthURL    string            `json:"healthUrl,omitempty"`
	FileLogs     bool              `json:"fileLogs,omitempty"`
	Proxy        map[string]string `json:"proxy,omitempty" jsonschema:"static：路径前缀 → 后端地址"`
	NoSPA        bool              `json:"noSpa,omitempty"`
	Exec         []string          `json:"exec,omitempty" jsonschema:"python、node、dotnet：启动命令，每项一个参数（相对上传的目录），如 [\"python\", \"-m\", \"uvicorn\", \"main:app\", \"--port\", \"8000\"]、[\"node\", \"server.js\"]、[\"./MyApp\"]"`
	ServiceEnv   map[string]string `json:"serviceEnv,omitempty" jsonschema:"python、node、dotnet：服务运行时的环境变量（如 PORT、ASPNETCORE_URLS）"`
	Exclude      []string          `json:"exclude,omitempty" jsonschema:"python、node：不上传的文件（.venv、node_modules、.git 等默认就不传）"`
	Registry     string            `json:"registry,omitempty" jsonschema:"python、node：服务器上装依赖用的镜像源（pip 的 index-url 或 npm registry；国内服务器建议填）"`
}

type createFile struct {
	From string `json:"from" jsonschema:"本机路径（相对项目目录）：文件或目录"`
	To   string `json:"to,omitempty" jsonschema:"放到服务器上叫什么（相对程序所在的目录，如 config/application.yml）；不填 = 同名"`
	Exec bool   `json:"exec,omitempty" jsonschema:"可执行（脚本）"`
}

type createIn struct {
	Spec   createSpec `json:"spec" jsonschema:"项目的内容"`
	Config string     `json:"config,omitempty" jsonschema:"ferry.yaml 放在哪里；不填 = 项目目录下的 ferry.yaml。已有文件不会覆盖"`
	Reason string     `json:"reason" jsonschema:"为什么要新建（显示在 Ferry 的确认框里）"`
}

var reBuildLog = regexp.MustCompile(`^build-[0-9]{8}-[0-9]{6}-[A-Za-z0-9._-]+\.log$`)

func (s *server) addMoreTools(m *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}

	addTool(s, m, &mcp.Tool{Name: "run_command", Title: "执行只读命令",
		Description: "在服务器上执行一条只读的 shell 命令（看磁盘、内存、进程、端口、配置文件、systemctl status、journalctl、docker ps 等），返回输出和退出码。" +
			"只放行确定不改东西的命令（和 Ferry 的 AI 助手只读模式同一套规则），改东西的、交互式的、一直不结束的会被拒绝；访问本机和内网以外的地址也不行。" +
			"最多运行 60 秒，输出太长保留开头和结尾。「新建连接」的服务器也能用。",
		Annotations: readOnly},
		logged(s, "run_command", func(ctx context.Context, in commandIn) (any, error) {
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.RunReadOnly(t, in.Command)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "build_log", Title: "构建日志",
		Description: "本机最近的构建日志（部署时在本机执行构建命令的完整输出）。构建失败时用它看完整的编译错误。返回最近几次的列表和其中一个的最后 lines 行。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		logged(s, "build_log", func(ctx context.Context, in buildLogIn) (any, error) {
			f, err := s.resolve(in.Project)
			if err != nil {
				return nil, err
			}
			if f.Kind == config.KindConnections {
				return nil, errors.New("「新建连接」的服务器没有构建")
			}
			if in.File != "" && !reBuildLog.MatchString(in.File) {
				return nil, fmt.Errorf("日志名不对：%s（用 files 里的 name）", in.File)
			}
			e := &engine.Engine{File: f, Rep: quiet{}, Ctx: ctx}
			files := e.BuildLogs(10)
			if len(files) == 0 {
				return "（这个项目还没有构建日志：部署时在本机构建才会有）", nil
			}
			pick := files[0]
			if in.File != "" {
				found := false
				for _, x := range e.BuildLogs(200) {
					if x.Name == in.File {
						pick, found = x, true
					}
				}
				if !found {
					return nil, fmt.Errorf("没有这个日志：%s", in.File)
				}
			}
			lines := in.Lines
			if lines <= 0 {
				lines = 200
			}
			text, err := tailFile(pick.Path, min(lines, 2000))
			if err != nil {
				return nil, err
			}
			return map[string]any{"file": pick.Name, "time": pick.Time, "files": files, "content": text}, nil
		}))

	addTool(s, m, &mcp.Tool{Name: "software_control", Title: "启停 Ferry 装的软件",
		Description: "启动、停止或重启 Ferry 在服务器上装的软件（MySQL、Redis、Nacos 等；先用 server_software 看 instance）。" +
			"和 deploy 一样：需要用户在 Ferry 设置里允许编程软件请求改动，每次在 Ferry 桌面版里确认，同意后由桌面版执行；生产服务器不允许。最多等 40 秒，没结束用 task_status 查。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}},
		loggedReq(s, "software_control", func(ctx context.Context, req *mcp.CallToolRequest, in softwareIn) (any, error) {
			if in.Action != "start" && in.Action != "stop" && in.Action != "restart" {
				return nil, fmt.Errorf("action 只能是 start、stop、restart：%s", in.Action)
			}
			if !reSoftware.MatchString(in.Name) {
				return nil, fmt.Errorf("软件名称不对：%q（用 server_software 结果里的 instance）", in.Name)
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: "sw-" + in.Action, Software: in.Name, Reason: in.Reason})
		}))

	addTool(s, m, &mcp.Tool{Name: "port_control", Title: "放行 / 关闭端口",
		Description: "用服务器的防火墙（firewalld / ufw）放行或关闭一个端口（运行时和永久配置一起改；先用 server_ports 看现状）。" +
			"SSH 端口不能关；Docker 映射的端口不经过防火墙，关闭时在 Docker 的 DOCKER-USER 规则链里拦外部访问（本机、其他容器照常能连）；端口范围、限制来源的规则 Ferry 不改。" +
			"和 deploy 一样：需要用户在 Ferry 设置里允许编程软件请求改动，每次在 Ferry 桌面版里确认，同意后由桌面版执行；生产服务器不允许。最多等 40 秒，没结束用 task_status 查。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}},
		loggedReq(s, "port_control", func(ctx context.Context, req *mcp.CallToolRequest, in portIn) (any, error) {
			if in.Action != "open" && in.Action != "close" {
				return nil, fmt.Errorf("action 只能是 open、close：%s", in.Action)
			}
			p := strings.TrimSpace(in.Port)
			if !strings.Contains(p, "/") {
				p += "/tcp"
			}
			if !validPort(p) {
				return nil, fmt.Errorf("端口不对：%q（写 8080/tcp 或 53/udp）", in.Port)
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: "port-" + in.Action, Port: p, Reason: in.Reason})
		}))

	addTool(s, m, &mcp.Tool{Name: "detect_project", Title: "识别项目",
		Description: "识别一个目录里的后端 / 前端应用（Maven、Gradle、Go、Node 前端、Python 的 FastAPI / Flask / Django、Node 服务端的 NestJS / Next.js / Express、ASP.NET Core……），" +
			"给出建议的项目名、各应用的构建命令、产物、启动命令、端口。新建项目前先用它。只读，不改任何东西。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		logged(s, "detect_project", func(ctx context.Context, in detectIn) (any, error) {
			if strings.TrimSpace(in.Dir) == "" {
				return nil, errors.New("要指定 dir（项目目录）")
			}
			return scaffold.Detect(in.Dir)
		}))

	addTool(s, m, &mcp.Tool{Name: "create_project", Title: "新建项目",
		Description: "在 Ferry 里新建项目：按 spec 生成 ferry.yaml（已有文件不会覆盖），加进 Ferry 的项目列表。不连服务器、不部署。" +
			"spec 照 detect_project 的结果填应用；服务器要用户提供地址、登录用户、部署目录、运行用户，不要自己编。" +
			"会在 Ferry 桌面版里弹出确认，显示要写入的 ferry.yaml，用户同意后才创建。新服务器第一次连接前，用户还要在 Ferry 里核对主机公钥。" +
			"最多等 40 秒，没结束用 task_status 查。",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptr(false)}},
		loggedReq(s, "create_project", func(ctx context.Context, req *mcp.CallToolRequest, in createIn) (any, error) {
			return s.create(ctx, req, in)
		}))
}

// tailFile 文件的最后 n 行。
func tailFile(path string, n int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) > n {
		lines = append([]string{fmt.Sprintf("…（前面还有 %d 行）", len(lines)-n)}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n"), nil
}

// ---------------------------------------------------------------- 新建项目

// generateProject 按向导的内容生成 ferry.yaml，校验通过才行；目标位置已有文件时报错（不会覆盖）。
func generateProject(spec json.RawMessage, path string) (string, error) {
	var sp scaffold.Spec
	if err := json.Unmarshal(spec, &sp); err != nil {
		return "", fmt.Errorf("项目内容格式不对：%v", err)
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("ferry.yaml 要用完整路径")
	}
	if fileExists(path) {
		return "", fmt.Errorf("%s 已经有了，不会覆盖：可以在 Ferry 里直接添加它", path)
	}
	if fi, err := os.Stat(sp.ProjectDir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("项目目录不存在：%s", sp.ProjectDir)
	}
	content, err := scaffold.Generate(sp, path)
	if err != nil {
		return "", err
	}
	if _, err := config.LoadContent([]byte(content), path); err != nil {
		return "", fmt.Errorf("生成的配置有问题：%v", err)
	}
	return content, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func (s *server) create(ctx context.Context, req *mcp.CallToolRequest, in createIn) (any, error) {
	var sp scaffold.Spec // 字段名和向导一样：转一下
	b, _ := json.Marshal(in.Spec)
	if err := json.Unmarshal(b, &sp); err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(sp.ProjectDir); err == nil && sp.ProjectDir != "" {
		sp.ProjectDir = abs
	}
	path := in.Config
	if path == "" {
		path = filepath.Join(sp.ProjectDir, config.DefaultFileName)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	raw, _ := json.Marshal(sp)
	if _, err := generateProject(raw, path); err != nil {
		return nil, err
	}
	var detail []string
	for _, t := range sp.Targets {
		user := t.User
		if user == "" {
			user = "root"
		}
		detail = append(detail, fmt.Sprintf("服务器 %s：%s@%s，部署目录 %s，运行用户 %s", t.Name, user, t.Host, orDash(t.Root), orDash(t.RunAs)))
	}
	for _, a := range sp.Apps {
		line := fmt.Sprintf("应用 %s（%s）：构建 %s，产物 %s", a.Name, a.Type, orDash(a.Command), orDash(a.Artifact))
		if len(a.Exec) > 0 {
			line += "，启动 " + strings.Join(a.Exec, " ")
		}
		detail = append(detail, line)
	}
	o := s.submit(approve.Request{ID: newID(), Session: s.session, Client: clientName(req), Action: "create",
		Project: sp.Project, Config: path, Spec: raw, Detail: detail, Reason: strings.TrimSpace(in.Reason)})
	return o.wait(ctx, req, waitCap)
}

// ---------------------------------------------------------------- 提示（编程软件里显示成可以直接选的命令）

func (s *server) addPrompts(m *mcp.Server) {
	arg := func(name, desc string, required bool) *mcp.PromptArgument {
		return &mcp.PromptArgument{Name: name, Description: desc, Required: required}
	}
	text := func(desc, body string) *mcp.GetPromptResult {
		return &mcp.GetPromptResult{Description: desc, Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: body}}}}
	}
	where := func(a map[string]string) string {
		w := "项目 " + orDash(a["project"])
		if a["target"] != "" {
			w += "、服务器 " + a["target"]
		}
		return w
	}
	m.AddPrompt(&mcp.Prompt{Name: "diagnose", Title: "排查应用故障",
		Description: "用 Ferry 查看服务器上某个应用的状态、日志，找出问题原因",
		Arguments:   []*mcp.PromptArgument{arg("app", "应用名", true), arg("project", "项目名（不填 = 当前目录的项目）", false), arg("target", "服务器（目标名）", false), arg("symptom", "现象，如「接口 500」「启动不了」", false)}},
		func(ctx context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			a := r.Params.Arguments
			return text("排查应用故障", fmt.Sprintf(`用 Ferry 的工具排查 %s 上的应用 %s。现象：%s。
步骤：
1. list_projects 确认项目、服务器和应用名（没指定服务器时问我用哪台，生产服务器只看不改）。
2. app_status 看是否在运行、端口、健康检查、当前版本。
3. app_logs 看最近的日志；用 keyword 搜 ERROR、Exception、Caused by，找到第一处出错。需要时看 source=file 的日志文件。
4. 需要看系统情况时用 run_command 执行只读命令（磁盘、内存、端口、配置文件）。
5. 对照代码找原因，给出结论和依据（哪条日志、哪个命令的输出）和修改建议。不要自己部署或重启，要做的话先问我。`,
				where(a), a["app"], orDash(a["symptom"]))), nil
		})
	m.AddPrompt(&mcp.Prompt{Name: "check_after_deploy", Title: "部署后检查",
		Description: "部署完检查应用是否正常：状态、健康检查、启动日志里的错误",
		Arguments:   []*mcp.PromptArgument{arg("app", "应用名（不填 = 这台服务器上的全部）", false), arg("project", "项目名", false), arg("target", "服务器（目标名）", false)}},
		func(ctx context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			a := r.Params.Arguments
			return text("部署后检查", fmt.Sprintf(`检查 %s 上刚部署的应用 %s 是否正常：
1. deploy_history 看最近一次部署的结果和版本。
2. app_status 看是否在运行、健康检查是否通过、监听端口对不对。
3. app_logs 看这次启动以来的日志，用 keyword 搜 ERROR、Exception、WARN，列出值得注意的地方。
4. 有问题时说明原因和建议；需要回滚时先问我，不要自己回滚。`, where(a), orDash(a["app"]))), nil
		})
}
