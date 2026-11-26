package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/safe"
	"github.com/captain/ferry/internal/settings"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// approverFile 桌面版写端口和令牌的文件（测试时换掉）。
var approverFile = approve.DefaultFile

// waitCap 一次工具调用最多等多久：超过就先返回「还在进行」，用 task_status 接着查。
// 编程软件对一次调用通常有超时（常见 60 秒起），等确认、构建、上传都可能比这久。
var waitCap = 40 * time.Second

type deployIn struct {
	targetIn
	Apps      []string `json:"apps" jsonschema:"要部署的应用（必须写明，不会默认部署全部）"`
	SkipBuild bool     `json:"skipBuild,omitempty" jsonschema:"跳过本机构建，直接用现有的产物（确认产物已经是最新的才用）"`
	Reason    string   `json:"reason" jsonschema:"为什么要部署（显示在 Ferry 的确认框里）"`
	// 和 CLI / 部署对话框一致的可选项
	Full       bool            `json:"full,omitempty" jsonschema:"全量上传（不用增量；怀疑服务器上的文件不对时用）"`
	NoRollback bool            `json:"noRollback,omitempty" jsonschema:"失败时保留失败版本，不自动回滚（排查用）"`
	KeepPerms  bool            `json:"keepPerms,omitempty" jsonschema:"不自动修正权限（默认会把运行用户访问不了的目录、不安全的部署目录改好）"`
	Keep       json.RawMessage `json:"keep,omitempty" jsonschema:"这次保留服务器上的文件，代替 ferry.yaml 里的 artifact.keep，如 {\"api\":[{\"path\":\"config.js\"},{\"path\":\"uploads\",\"shared\":true}]}"`
	// 一次部署到几台
	Targets    []string `json:"targets,omitempty" jsonschema:"一次部署到几台服务器时写全（按切换的顺序；写了就不用 target）。构建一次，同时上传，再按切换方式切换"`
	Batch      int      `json:"batch,omitempty" jsonschema:"部署到几台时：一次同时切换几台；不填 = 用 ferry.yaml 的 rollout（默认 1）"`
	Canary     *int     `json:"canary,omitempty" jsonschema:"部署到几台时：先切换几台，成功后 Ferry 弹窗问用户要不要继续部署其余的；0 = 不暂停；不填 = 用 ferry.yaml 的 rollout"`
	ObserveSec int      `json:"observeSec,omitempty" jsonschema:"先切换的那几台观察几秒（这期间应用一直要正常）再问用户；最多 3600"`
	OnFailure  string   `json:"onFailure,omitempty" jsonschema:"部署到几台时有一台失败：stop 停下（后面的不部署）或 rollback（再把这次已经切换的回滚到部署前的版本）；不填 = 用 ferry.yaml 的 rollout"`
}

// rolloutOpts 请求里的切换方式。
func rolloutOpts(r approve.Request) engine.RolloutOptions {
	o := engine.RolloutOptions{Batch: r.Batch, Canary: r.Canary, OnFailure: r.OnFailure}
	if r.ObserveSec > 0 {
		d := time.Duration(r.ObserveSec) * time.Second
		o.Observe = &d
	}
	return o
}

// checkTargets 部署到多台：每台都要允许改动、有这些应用。
func checkTargets(st settings.Settings, f *config.File, r *approve.Request) error {
	if len(r.Targets) == 0 {
		return nil
	}
	if r.Action != "deploy" {
		return errors.New("只有部署能一次写几台服务器")
	}
	if err := engine.CheckRollout(rolloutOpts(*r)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, name := range r.Targets {
		t, err := f.Target(name)
		if err != nil {
			return err
		}
		if seen[t.Name] {
			return fmt.Errorf("服务器 %s 写了两次", t.Name)
		}
		seen[t.Name] = true
		r.Targets[i] = t.Name
		if err := ChangeAllowed(st, f, t); err != nil {
			return err
		}
		for _, a := range r.Apps {
			if f.App(a) == nil || !t.Allows(a) {
				return fmt.Errorf("%s 上没有应用 %s（有：%s）", t.Name, a, strings.Join(appsOn(f, t), "、"))
			}
		}
	}
	if r.Targets[0] != r.Target {
		return errors.New("targets 的第一台要和 target 一样")
	}
	return nil
}

type controlIn struct {
	targetIn
	Action string   `json:"action" jsonschema:"start、stop 或 restart"`
	Apps   []string `json:"apps" jsonschema:"要操作的应用（必须写明）"`
	Reason string   `json:"reason" jsonschema:"为什么要这样做（显示在 Ferry 的确认框里）"`
}

type rollbackIn struct {
	targetIn
	App    string `json:"app" jsonschema:"要回滚的应用"`
	To     string `json:"to,omitempty" jsonschema:"回滚到哪个版本（app_releases 里的 id）；不填 = 上一个版本"`
	Reason string `json:"reason" jsonschema:"为什么要回滚（显示在 Ferry 的确认框里）"`
}

type statusTaskIn struct {
	targetIn
	ID   string `json:"id" jsonschema:"改动工具返回的 id；软件安装也可查询服务器保留的任务"`
	Wait int    `json:"wait,omitempty" jsonschema:"最多等多少秒看它结束，默认 30，最多 40"`
}

type cancelTaskIn struct {
	ID string `json:"id" jsonschema:"要停止或取消的改动任务 id"`
}

var actionNames = map[string]string{"deploy": "部署", "start": "启动", "stop": "停止", "restart": "重启", "rollback": "回滚",
	"sw-start": "启动软件", "sw-stop": "停止软件", "sw-restart": "重启软件", "port-open": "放行端口", "port-close": "关闭端口",
	"fw-enable": "打开防火墙", "ssh-kick": "断开 SSH 连接", "backup-run": "现在备份", "create": "新建项目",
	"exec": "执行命令", "write-file": "写文件", "install": "安装软件", "tpl-save": "保存软件模板", "secret": "填写密码",
	"sw-upgrade": "升级软件", "sw-restore": "恢复软件", "sw-backup": "设置数据备份", "db-exec": "改数据库", "config-edit": "改项目配置",
	"run-job": "现在运行定时任务", "backup-fetch": "取回异地的备份", "upload-file": "上传文件"}

// reSoftware Ferry 装的软件的名称（安装时的 --name）。
var reSoftware = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// rePortSpec 防火墙放行 / 关闭的端口：8080/tcp、53/udp。
var rePortSpec = regexp.MustCompile(`^([1-9][0-9]{0,4})/(tcp|udp)$`)

// validPort 端口写法对不对（8080/tcp，1～65535）。
func validPort(s string) bool {
	m := rePortSpec.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	return n <= 65535
}

func (s *server) addChangeTools(m *mcp.Server) {
	change := func() *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}
	}
	const how = "需要用户在 Ferry 设置里允许编程软件请求改动；每次都要用户确认（默认在 Ferry 桌面版弹窗，也可在设置里开启在编程软件中确认）；生产服务器不允许。" +
		"最多等 40 秒：结束了直接返回结果，没结束返回 id 和当前进展，再用 task_status 查。"
	addTool(s, m, &mcp.Tool{Name: "deploy", Title: "部署",
		Description: "把项目里的应用部署到某台服务器：先在本机构建（skipBuild 时跳过），上传，逐个切换到新版本并等健康检查通过；失败自动回滚该应用。" +
			"一次部署到几台时用 targets 写全：构建一次、同时上传，再按切换方式切换（batch 一次几台、canary 先发几台后让用户确认、onFailure: rollback 有一台失败就把已切换的回滚，" +
			"不填用 ferry.yaml 的 rollout，用 config_get 看 rollout）。" +
			how + "构建失败时结果里有构建输出的最后几十行。",
		Annotations: change()},
		loggedReq(s, "deploy", func(ctx context.Context, req *mcp.CallToolRequest, in deployIn) (any, error) {
			if len(in.Apps) == 0 {
				return nil, errors.New("要用 apps 写明部署哪些应用")
			}
			r := approve.Request{Action: "deploy", Apps: in.Apps, SkipBuild: in.SkipBuild, Reason: in.Reason,
				Full: in.Full, NoRollback: in.NoRollback, KeepPerms: in.KeepPerms, Keep: in.Keep,
				Batch: in.Batch, Canary: in.Canary, ObserveSec: in.ObserveSec, OnFailure: in.OnFailure}
			if len(in.Targets) > 0 {
				if in.Target != "" && in.Target != in.Targets[0] {
					return nil, errors.New("写了 targets 就不用 target（或者让 target 是 targets 的第一台）")
				}
				in.Target = in.Targets[0]
				if len(in.Targets) > 1 {
					r.Targets = append([]string(nil), in.Targets...)
				}
			}
			if err := engine.CheckRollout(rolloutOpts(r)); err != nil {
				return nil, err
			}
			return s.change(ctx, req, in.targetIn, r)
		}))

	addTool(s, m, &mcp.Tool{Name: "app_control", Title: "启动 / 停止 / 重启应用",
		Description: "启动、停止或重启项目里的应用（等健康检查通过）。停止同时取消开机自启（服务器重启后不会自己起来），启动、重启后恢复。" +
			"定时任务（写了 job 的）：停止 = 关掉定时器（到点不再运行），启动 = 打开定时器（不会马上运行；要马上运行用 run_job）。" + how,
		Annotations: change()},
		loggedReq(s, "app_control", func(ctx context.Context, req *mcp.CallToolRequest, in controlIn) (any, error) {
			if in.Action != "start" && in.Action != "stop" && in.Action != "restart" {
				return nil, fmt.Errorf("action 只能是 start、stop、restart：%s", in.Action)
			}
			if len(in.Apps) == 0 {
				return nil, errors.New("要用 apps 写明操作哪些应用")
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: in.Action, Apps: in.Apps, Reason: in.Reason})
		}))

	addTool(s, m, &mcp.Tool{Name: "rollback", Title: "回滚",
		Description: "把项目里的某个应用切回服务器上保留的旧版本（先用 app_releases 看有哪些版本）。" + how,
		Annotations: change()},
		loggedReq(s, "rollback", func(ctx context.Context, req *mcp.CallToolRequest, in rollbackIn) (any, error) {
			if in.App == "" {
				return nil, errors.New("要指定 app（应用名）")
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: "rollback", Apps: []string{in.App}, To: in.To, Reason: in.Reason})
		}))

	addTool(s, m, &mcp.Tool{Name: "task_status", Title: "改动的进展",
		Description: "查改动工具返回的 id 和结果，最多等 wait 秒。查询 Ferry 重启前的软件安装时，带上 project 和 target，从服务器读同一个 id 的最新结果。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		loggedReq(s, "task_status", func(ctx context.Context, req *mcp.CallToolRequest, in statusTaskIn) (any, error) {
			s.mu.Lock()
			o := s.ops[in.ID]
			s.mu.Unlock()
			if in.Project != "" || in.Target != "" {
				snapshot, err := s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, target string) (any, error) {
					task, err := e.SoftwareTask(target, in.ID)
					if err != nil || task == nil {
						return nil, err
					}
					return task, nil
				})
				if snapshot != nil || (err != nil && o == nil) {
					return snapshot, err
				}
			}
			if o == nil {
				return nil, fmt.Errorf("没有这个 id：%s（ferry mcp 重启过的话，之前的记录就没了；可以用 deploy_history、app_status 看结果）", in.ID)
			}
			wait := time.Duration(in.Wait) * time.Second
			if in.Wait <= 0 {
				wait = 30 * time.Second
			}
			return o.wait(ctx, req, min(wait, waitCap))
		}))

	addTool(s, m, &mcp.Tool{Name: "task_cancel", Title: "停止 / 取消任务",
		Description: "停止正在执行中的改动任务（支持离线直接执行与 Ferry 桌面端任务），或取消一张尚未操作的待确认卡片。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}},
		loggedReq(s, "task_cancel", func(ctx context.Context, req *mcp.CallToolRequest, in cancelTaskIn) (any, error) {
			return s.cancelOp(ctx, strings.TrimSpace(in.ID))
		}))
}

func (s *server) cancelOp(ctx context.Context, id string) (any, error) {
	if id == "" {
		return nil, errors.New("要指定任务 id")
	}
	trackOp(ctx, id)
	s.mu.Lock()
	o := s.ops[id]
	var pendingCard *pendingAppCard
	var pendingCardID string
	for aid, c := range s.appCards {
		if c.op == o && !c.decided {
			pendingCard = c
			pendingCardID = aid
			c.decided = true
			c.rejected = true
			c.rejectMsg = "任务已取消，未执行任何改动"
			if c.timer != nil {
				c.timer.Stop()
			}
			// 保留短时间供卡片轮询终态，随后自动释放避免常驻内存
			c.expiresAt = time.Now().Add(2 * time.Minute)
			c.timer = time.AfterFunc(2*time.Minute, func() {
				s.mu.Lock()
				if cur, ok := s.appCards[pendingCardID]; ok && cur == c {
					delete(s.appCards, pendingCardID)
				}
				s.mu.Unlock()
			})
			break
		}
	}
	s.mu.Unlock()
	if o == nil {
		return nil, fmt.Errorf("没有这个 id：%s", id)
	}
	if pendingCard != nil {
		if pendingCard.desktopOnline {
			approve.Cancel(context.Background(), approverFile(), pendingCard.prepareToken, pendingCard.req.Session)
		}
		s.finishOp(o, pendingCard.req, approve.Decision{Approve: false, Note: "任务已取消，未执行任何改动"}, nil, nil)
		return map[string]any{"id": id, "state": "已取消", "title": opTitle(pendingCard.req)}, nil
	}
	o.mu.Lock()
	state := o.state
	cancelFn := o.cancelFn
	title := opTitle(o.req)
	o.mu.Unlock()
	if state == "done" {
		return map[string]any{"id": id, "state": "已结束", "title": title, "note": "任务已经结束了"}, nil
	}
	if cancelFn != nil {
		cancelFn()
		return map[string]any{"id": id, "state": "已发送停止请求", "title": title, "note": "正在停止，可用 task_status 查看最终状态"}, nil
	}
	return nil, fmt.Errorf("%s 当前处于等待状态，无法从此处直接取消（请在确认界面拒绝）", title)
}

// ---------------------------------------------------------------- 是否允许

// ChangeAllowed 这台服务器能不能经 MCP 改动：设置里允许了、在允许的目标里、不是生产服务器、没有设 aiChanges: false。
func ChangeAllowed(st settings.Settings, f *config.File, t *config.Target) error {
	key := f.Project + "/" + t.Name
	switch {
	case !st.MCP.AllowChanges:
		return errors.New("Ferry 没有允许编程软件改动服务器：用户可以在 Ferry「设置 → 命令行与 MCP」里打开「允许编程软件请求部署、启停、回滚等改动」，或者自己在 Ferry 里操作")
	case t.Production:
		return fmt.Errorf("%s 是生产服务器（production: true），不能通过编程软件改动：请用户在 Ferry 里操作", t.Name)
	case !t.AllowAIChanges():
		return fmt.Errorf("%s 在配置里设了 aiChanges: false，不能通过编程软件改动：请用户在 Ferry 里操作", t.Name)
	case len(st.MCP.Targets) > 0 && !slices.Contains(st.MCP.Targets, key):
		return fmt.Errorf("%s 不在 Ferry 设置里允许编程软件改动的目标里（允许的：%s）", key, strings.Join(st.MCP.Targets, "、"))
	}
	return nil
}

// PrepareRequest 收到请求、交给用户确认之前做完整校验并生成权威预览（防止绕过 ferry mcp 直接发请求、防止预览与执行脱节）；
// 新建项目的 ferry.yaml 在这里按 Spec 重新生成，确认框里显示的就是要写入的。
func PrepareRequest(r *approve.Request) error {
	return prepareRequestWithPool(r, nil, os.Getenv, false)
}

// PrepareRequestWithPool 同 PrepareRequest，允许桌面端（ferry serve）传入全局 SSH 连接池以复用连接、避免每次预检重建 SSH。
func PrepareRequestWithPool(r *approve.Request, pool *engine.Pool, getenv func(string) string) error {
	return prepareRequestWithPool(r, pool, getenv, false)
}

func prepareRequestWithPool(r *approve.Request, pool *engine.Pool, getenv func(string) string, reuseDetail bool) error {
	if _, ok := actionNames[r.Action]; !ok {
		return fmt.Errorf("不支持的操作：%s", r.Action)
	}
	if r.ClientConfirmed {
		return errors.New("此操作需要通过预检令牌或在 Ferry 里确认")
	}
	if r.Action == "db-exec" {
		return prepareDB(r, pool, getenv)
	}
	if r.Action == "config-edit" {
		return prepareConfigEdit(r)
	}
	if r.Action == "create" {
		content, err := generateProject(r.Spec, r.Config)
		if err != nil {
			return err
		}
		r.Content = content
		if len(r.Detail) == 0 {
			r.Detail = createDetail(r.Spec)
		}
		return nil
	}
	f, err := config.LoadWithLocal(r.Config)
	if err != nil {
		return err
	}
	t, err := f.Target(r.Target)
	if err != nil {
		return err
	}
	st, _ := settings.Load()
	if err := ChangeAllowed(st, f, t); err != nil {
		return err
	}
	r.Config, r.Project, r.Target, r.Host = f.Path, f.Project, t.Name, t.Host
	if getenv == nil {
		getenv = os.Getenv
	}
	e := &engine.Engine{File: f, Rep: quiet{}, Settings: st, AcceptNewHostKey: st.Connect.AcceptNewHostKey, Pool: pool, Ctx: context.Background(), Getenv: getenv, DisableAgentInstall: true}
	if _, ok := aiActionTool[r.Action]; ok {
		return prepareAI(e, r)
	}
	if r.Action == "secret" {
		if err := ai.CheckSecretFields(r.Fields); err != nil {
			return err
		}
		if len(r.Detail) == 0 {
			r.Detail, _ = aiPreview(e, t.Name, *r)
		}
		return nil
	}
	if strings.HasPrefix(r.Action, "sw-") {
		if !reSoftware.MatchString(r.Software) {
			return fmt.Errorf("软件名称不对：%q", r.Software)
		}
		r.Detail = preview(e, t.Name, *r)
		return nil
	}
	if strings.HasPrefix(r.Action, "port-") {
		if !validPort(r.Port) {
			return fmt.Errorf("端口不对：%q（写 8080/tcp 或 53/udp）", r.Port)
		}
		r.Detail = preview(e, t.Name, *r)
		return nil
	}
	switch r.Action {
	case "fw-enable":
		for _, p := range r.Allow {
			if !validPort(p) {
				return fmt.Errorf("端口不对：%q（写 8080/tcp 或 53/udp）", p)
			}
		}
		if !reuseDetail || len(r.Detail) == 0 {
			r.Detail = preview(e, t.Name, *r)
		}
		return nil
	case "backup-run":
		if r.Backup != "" && !reBackupName.MatchString(r.Backup) {
			return fmt.Errorf("备份的名字不对：%q", r.Backup)
		}
		if err := hasBackup(e, t.Name, r.Backup); err != nil {
			return err
		}
		r.Detail = preview(e, t.Name, *r)
		return nil
	case "ssh-kick":
		if len(r.Pids) == 0 {
			return errors.New("没有写明断开哪个连接")
		}
		for _, p := range r.Pids {
			if p <= 1 {
				return fmt.Errorf("PID 不对：%d", p)
			}
		}
		if !reuseDetail || len(r.Detail) == 0 {
			r.Detail = preview(e, t.Name, *r)
		}
		return nil
	}
	if len(r.Apps) == 0 {
		return errors.New("没有写明应用")
	}
	if err := checkTargets(st, f, r); err != nil {
		return err
	}
	if r.Action == "rollback" && len(r.Apps) != 1 {
		return errors.New("回滚一次只能一个应用")
	}
	for _, a := range r.Apps {
		if f.App(a) == nil || !t.Allows(a) {
			return fmt.Errorf("%s 上没有应用 %s", t.Name, a)
		}
	}
	r.Detail = preview(e, t.Name, *r)
	return nil
}

// prepareLocal 当 Ferry 桌面端未打开时，在 ferry mcp 内部复用连接池执行权威预检（复用本进程刚预检好的 fw-enable / ssh-kick 详情避免二次 SSH）。
func (s *server) prepareLocal(r *approve.Request) error {
	return prepareRequestWithPool(r, s.pool, s.o.Getenv, true)
}

// ---------------------------------------------------------------- 提请求、等结果

// change 检查是否允许，补上给人看的信息，按确认设置路由；最多等 waitCap，没结束就返回 id。
func (s *server) change(ctx context.Context, req *mcp.CallToolRequest, in targetIn, r approve.Request) (any, error) {
	if req != nil && req.Params != nil && req.Params.RequestState != "" {
		return s.resumeMRTR(ctx, req)
	}
	_, err := s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, target string) (any, error) {
		t, _ := e.File.Target(target)
		if err := ChangeAllowed(e.Settings, e.File, t); err != nil {
			return nil, err
		}
		for _, a := range r.Apps {
			if e.File.App(a) == nil || !t.Allows(a) {
				return nil, fmt.Errorf("%s 上没有应用 %s（有：%s）", t.Name, a, strings.Join(appsOn(e.File, t), "、"))
			}
		}
		r.ID = newID()
		r.Session, r.Client = s.session, clientName(req)
		r.Project, r.Config, r.Target, r.Host = e.File.Project, e.File.Path, t.Name, t.Host
		if len(r.Targets) > 0 {
			r.Targets[0] = t.Name
		}
		if err := checkTargets(e.Settings, e.File, &r); err != nil {
			return nil, err
		}
		r.Reason = strings.TrimSpace(r.Reason)
		// 仅填写密码（纯本地表单，不连 SSH）在发给桌面版前补预览；
		// 其余改动交给桌面端 PrepareRequest（复用 ferry serve 连接池）或离线 prepareLocal 一次性计算权威预览，避免双倍 SSH 预检。
		if r.Action == "secret" {
			r.Detail, _ = aiPreview(e, target, r)
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return s.ask(ctx, req, r)
}

// ask 按确认策略路由到 Ferry 桌面端或编程软件客户端确认。
func (s *server) ask(ctx context.Context, req *mcp.CallToolRequest, r approve.Request) (any, error) {
	return s.requestChange(ctx, req, r)
}

func appsOn(f *config.File, t *config.Target) []string {
	var out []string
	for _, a := range f.Ordered() {
		if t.Allows(a.Name) {
			out = append(out, a.Name)
		}
	}
	return out
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// preview 确认框里给人看的：部署的代码提交和构建命令，线上现在的版本和状态，回滚到哪个版本。读不到的写原因，不影响提请求。
func preview(e *engine.Engine, target string, r approve.Request) []string {
	var out []string
	if r.Action == "deploy" && len(r.Targets) > 1 {
		out = append(out, fmt.Sprintf("部署到 %d 台：%s（构建一次、同时上传，再切换）", len(r.Targets), strings.Join(r.Targets, "、")))
		how := e.RolloutText(rolloutOpts(r), len(r.Targets))
		if how == "" {
			how = "一台一台切换，有一台失败就停下"
		}
		out = append(out, "切换方式："+how)
		if e.RolloutCanary(rolloutOpts(r), len(r.Targets)) {
			out = append(out, "先部署的上线后，Ferry 会弹窗问你要不要继续部署其余的")
		}
	}
	if r.Action == "deploy" {
		git, steps, err := e.DeployPreview(target, r.Apps)
		out = append(out, git.Describe()...)
		if r.SkipBuild {
			out = append(out, "构建：跳过，用本机现有的产物")
		} else if err == nil {
			for _, st := range steps {
				out = append(out, fmt.Sprintf("构建 %s：%s", st.Name, st.Command))
			}
		}
		var keep map[string][]*config.KeepItem
		if len(r.Keep) > 0 {
			_ = json.Unmarshal(r.Keep, &keep)
		}
		out = append(out, engine.DeployOptionNotes(engine.DeployOptions{Full: r.Full, NoRollback: r.NoRollback, KeepPerms: r.KeepPerms, Keep: keep})...)
		// 部署钩子会在服务器上执行命令：确认前要让人看到
		if t, err := e.File.Target(target); err == nil {
			if apps, err := e.File.Select(t, r.Apps); err == nil {
				for _, a := range apps {
					s, err := engine.Spec(e.File, t, a)
					if err != nil || s.Hooks == nil {
						continue
					}
					for _, h := range s.Hooks.BeforeSwitch {
						out = append(out, fmt.Sprintf("%s 切换前在服务器上执行（以 %s）：%s", a.Name, h.User, h.Run))
					}
					for _, h := range s.Hooks.AfterSwitch {
						out = append(out, fmt.Sprintf("%s 切换后在服务器上执行（以 %s）：%s", a.Name, h.User, h.Run))
					}
				}
			}
			var before []string
			for _, b := range e.File.Backups {
				if b.BeforeDeploy && b.AppliesTo(t.Name) {
					before = append(before, b.Name)
				}
			}
			if len(before) > 0 {
				out = append(out, "部署前先备份（服务器上打开了数据备份时）："+strings.Join(before, "、"))
			}
		}
	}
	ctx, cancel := context.WithTimeout(e.Ctx, 20*time.Second)
	defer cancel()
	e2 := *e
	e2.Ctx = ctx
	if r.Action == "rollback" {
		rs, err := e2.Releases(target, r.Apps)
		if err != nil || len(rs) == 0 {
			return append(out, "线上版本：读取失败（"+errOr(err, "没有记录")+"）")
		}
		cur, prev := "", ""
		for _, x := range rs[0].Releases {
			if x.Current {
				cur = x.ID
			} else if prev == "" && !x.Failed && cur != "" {
				prev = x.ID
			}
		}
		to := r.To
		if to == "" {
			to = orDash(prev) + "（上一个版本）"
		}
		return append(out, fmt.Sprintf("版本：%s → %s", orDash(cur), to))
	}
	if strings.HasPrefix(r.Action, "port-") {
		rep, err := e2.Ports(target)
		if err != nil {
			return append(out, "端口：读取失败（"+err.Error()+"）")
		}
		fw := rep.Firewall
		switch {
		case !fw.Active:
			out = append(out, "防火墙：没开（执行时会报错）")
		case !fw.Manage:
			out = append(out, "防火墙："+fw.Kind+"，Ferry 只能查看（"+fw.Why+"）")
		default:
			out = append(out, "防火墙："+strings.TrimSpace(fw.Kind+" "+fw.Zone))
		}
		for _, p := range rep.Ports {
			if fmt.Sprintf("%d/%s", p.Port, p.Proto) != r.Port {
				continue
			}
			who := "没有程序在用"
			if len(p.Listen) > 0 {
				who = strings.TrimSpace(p.Process+" "+p.Unit) + " 在监听 " + strings.Join(p.Listen, " ")
			}
			state := map[string]string{"open": "放行", "blocked": "拦着", "partial": "只放行部分来源", "": "看不出来"}[p.Firewall]
			if p.Rule != "" {
				state += "（" + p.Rule + "）"
			}
			if p.SSH {
				who += "；是 SSH 端口"
			}
			return append(out, r.Port+"："+who+"；防火墙现在"+state)
		}
		return append(out, r.Port+"：没有程序在用，防火墙也没有放行")
	}
	if r.Action == "fw-enable" {
		rep, err := e2.Ports(target)
		if err != nil {
			return append(out, "端口：读取失败（"+err.Error()+"）")
		}
		return append(out, formatFWEnablePreview(rep, r)...)
	}
	if r.Action == "backup-run" {
		st, err := e2.Backup(target, "status", engine.BackupOptions{})
		if err != nil {
			return append(out, "数据备份：读取失败（"+err.Error()+"）")
		}
		for _, it := range st.Items {
			if r.Backup != "" && it.Name != r.Backup {
				continue
			}
			line := fmt.Sprintf("备份 %s（%s）：已有 %d 份", it.Name, backupKind(it.Kind), it.Count)
			if it.Last != nil {
				res := "成功"
				if !it.Last.OK {
					res = "失败"
				}
				line += "，上次 " + it.Last.Time.Local().Format("01-02 15:04") + " " + res
			}
			out = append(out, line)
		}
		if st.Dir != "" {
			out = append(out, "备份文件放在服务器上的 "+st.Dir)
		}
		return out
	}
	if r.Action == "ssh-kick" {
		rep, err := e2.SSHSessions(target)
		if err != nil {
			return append(out, "SSH 连接：读取失败（"+err.Error()+"）")
		}
		return append(out, formatSSHKickPreview(rep, r)...)
	}
	if strings.HasPrefix(r.Action, "sw-") {
		rep, err := e2.Software(target)
		if err != nil {
			return append(out, "软件状态：读取失败（"+err.Error()+"）")
		}
		for _, sw := range rep.Items {
			if sw.Instance == r.Software {
				state := map[string]string{"running": "运行中", "stopped": "已停止"}[sw.State]
				return append(out, fmt.Sprintf("%s：%s %s，%s", r.Software, sw.Name, sw.Version, orDash(state)))
			}
		}
		return append(out, "服务器上没找到 Ferry 装的 "+r.Software+"（执行时会报错）")
	}
	st, err := e2.Status(target, r.Apps)
	if err != nil {
		return append(out, "线上状态：读取失败（"+err.Error()+"）")
	}
	for _, a := range st {
		state := map[string]string{"active": "运行中", "inactive": "已停止", "failed": "失败"}[a.Active]
		if state == "" {
			state = orDash(a.Active)
		}
		ver := "未部署"
		if a.Current != nil {
			ver = a.Current.ID
		}
		out = append(out, fmt.Sprintf("线上 %s：%s，版本 %s", a.Name, state, ver))
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// backupKind 备份的种类给人看的名字。
func backupKind(k string) string {
	if n, ok := map[string]string{"mysql": "MySQL", "postgres": "PostgreSQL", "paths": "目录", "command": "自己写的命令"}[k]; ok {
		return n
	}
	return k
}

func errOr(err error, s string) string {
	if err != nil {
		return err.Error()
	}
	return s
}

// op 一个交给桌面版或本地直接执行的改动：等确认、执行中、结束。ferry mcp 退出时就没了（桌面版那边照样执行完）。
type op struct {
	req     approve.Request
	started time.Time

	mu             sync.Mutex
	state          string // waiting | running | done
	waitState      string // waiting 态展示文案（空 = 默认「等用户在 Ferry 里确认」）
	waiters        int    // 当前正在阻塞等待结果的 task_status 调用数
	modelDelivered bool   // 终态结果已通过 task_status / 原工具返回给模型
	finishedAt     time.Time
	decision       *approve.Decision
	outcome        *approve.Outcome
	err            error
	progress       []string
	notifiers      map[int]func(string) // 并发等待的 task_status 各自注册进度回调，互不覆盖
	nextNotify     int
	cancelFn       func() // 停止正在执行的任务（本地取消或向桌面端发取消）
	doneOnce       sync.Once
	done           chan struct{}
}

func (o *op) closeDone() {
	o.doneOnce.Do(func() { close(o.done) })
}

func (o *op) isModelListeningLocked() bool {
	return o.waiters > 0 || o.modelDelivered
}

const (
	opRetainTTL = 30 * time.Minute // 已完成的 op 保留多久供 task_status 回查，超时后自动淘汰防内存增长
	opMaxDone   = 200              // 已完成的 op 最多保留条数
)

// sweepOpsLocked 淘汰已完成且不再被等待的旧 op（仅在持有 s.mu 时调用）。
func (s *server) sweepOpsLocked(now time.Time) {
	type doneItem struct {
		id string
		at time.Time
	}
	var finished []doneItem
	for id, o := range s.ops {
		o.mu.Lock()
		isDone := o.state == "done" && o.waiters == 0
		at := o.finishedAt
		if at.IsZero() {
			at = o.started
		}
		o.mu.Unlock()
		if !isDone {
			continue
		}
		if now.Sub(at) > opRetainTTL {
			delete(s.ops, id)
			continue
		}
		finished = append(finished, doneItem{id: id, at: at})
	}
	if len(finished) <= opMaxDone {
		return
	}
	slices.SortFunc(finished, func(a, b doneItem) int {
		if a.at.Before(b.at) {
			return -1
		}
		if a.at.After(b.at) {
			return 1
		}
		return 0
	})
	for _, x := range finished[:len(finished)-opMaxDone] {
		delete(s.ops, x.id)
	}
}

func (s *server) submit(r approve.Request) *op {
	now := time.Now()
	o := &op{req: r, started: now, state: "waiting", done: make(chan struct{})}
	o.cancelFn = func() {
		_ = approve.CancelRun(context.Background(), approverFile(), r.ID, r.Session)
	}
	s.mu.Lock()
	s.sweepOpsLocked(now)
	s.ops[r.ID] = o
	s.mu.Unlock()
	safe.Go("等待桌面版确认", func() {
		// 不跟着这次工具调用的 ctx：调用先返回了，请求还在桌面版那边等着
		d, out, err := approve.Ask(context.Background(), approverFile(), r,
			func(d approve.Decision) {
				o.mu.Lock()
				o.decision = &d
				if d.Approve {
					o.state = "running"
				}
				o.mu.Unlock()
			},
			o.addProgress)
		s.finishOp(o, r, d, out, err)
	})
	return o
}

// opTitle 给编程软件看的一句话，如「部署 api（test）」。
func opTitle(r approve.Request) string {
	switch {
	case r.Action == "create":
		return "新建项目 " + r.Project + "（" + r.Config + "）"
	case r.Action == "config-edit":
		var in engine.ConfigEditInput
		json.Unmarshal(r.Input, &in)
		if in.Scope == "local" {
			return "改本机设置 " + r.Project
		}
		return "改项目配置 " + r.Project + "（" + r.Config + "）"
	case strings.HasPrefix(r.Action, "sw-"):
		name := r.Software
		if r.Tool != "" { // 升级、恢复、设置备份：名称在给助手工具的参数里
			var in struct{ Name string }
			json.Unmarshal(r.Input, &in)
			name = in.Name
		}
		return actionNames[r.Action] + " " + name + "（" + r.Target + "）"
	case strings.HasPrefix(r.Action, "port-"):
		return actionNames[r.Action] + " " + r.Port + "（" + r.Target + "）"
	case r.Action == "fw-enable" || r.Action == "exec" || r.Action == "install" || r.Action == "tpl-save":
		return actionNames[r.Action] + "（" + r.Target + "）"
	case r.Action == "write-file":
		var in struct{ Path string }
		json.Unmarshal(r.Input, &in)
		return actionNames[r.Action] + " " + in.Path + "（" + r.Target + "）"
	case r.Action == "upload-file":
		var in struct{ Locals []string }
		json.Unmarshal(r.Input, &in)
		return fmt.Sprintf("%s %d 个文件（%s）", actionNames[r.Action], len(in.Locals), r.Target)
	case r.Action == "db-exec":
		unit := "语句"
		if r.DBKind == "redis" {
			unit = "命令"
		}
		return fmt.Sprintf("%s %s（%d 条%s）", actionNames[r.Action], r.DBName, len(r.Stmts), unit)
	case r.Action == "secret":
		labels := make([]string, len(r.Fields))
		for i, f := range r.Fields {
			labels[i] = f.Label
		}
		return actionNames[r.Action] + "：" + strings.Join(labels, "、") + "（" + r.Target + "）"
	case r.Action == "run-job":
		var in struct{ App string }
		json.Unmarshal(r.Input, &in)
		return actionNames[r.Action] + " " + in.App + "（" + r.Target + "）"
	case r.Action == "backup-fetch":
		var in struct{ File string }
		json.Unmarshal(r.Input, &in)
		return actionNames[r.Action] + " " + in.File + "（" + r.Target + "）"
	case r.Action == "backup-run":
		what := r.Backup
		if what == "" {
			what = "全部"
		}
		return actionNames[r.Action] + " " + what + "（" + r.Target + "）"
	case r.Action == "ssh-kick":
		pids := make([]string, len(r.Pids))
		for i, p := range r.Pids {
			pids[i] = strconv.Itoa(p)
		}
		return actionNames[r.Action] + " PID " + strings.Join(pids, "、") + "（" + r.Target + "）"
	}
	where := r.Target
	if len(r.Targets) > 1 {
		where = strings.Join(r.Targets, "、")
	}
	return actionNames[r.Action] + " " + strings.Join(r.Apps, "、") + "（" + where + "）"
}

func (o *op) addProgress(line string) {
	o.mu.Lock()
	o.progress = append(o.progress, line)
	if len(o.progress) > 40 {
		o.progress = o.progress[len(o.progress)-40:]
	}
	var fns []func(string)
	for _, fn := range o.notifiers {
		fns = append(fns, fn)
	}
	o.mu.Unlock()
	for _, fn := range fns {
		fn(line)
	}
}

// wait 最多等 d，返回现在的情况；拒绝、失败作为错误返回（编程软件那边标成失败）。
func (o *op) wait(ctx context.Context, req *mcp.CallToolRequest, d time.Duration) (any, error) {
	o.mu.Lock()
	o.waiters++
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.waiters--
		o.mu.Unlock()
	}()
	if req != nil && req.Session != nil && req.Params != nil {
		if tok := req.Params.GetProgressToken(); tok != nil {
			n := 0
			o.mu.Lock()
			if o.notifiers == nil {
				o.notifiers = map[int]func(string){}
			}
			nid := o.nextNotify
			o.nextNotify++
			o.notifiers[nid] = func(line string) {
				n++
				req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Message: line, Progress: float64(n)})
			}
			o.mu.Unlock()
			defer func() {
				o.mu.Lock()
				delete(o.notifiers, nid)
				o.mu.Unlock()
			}()
		}
	}
	select {
	case <-o.done:
	case <-time.After(d):
	case <-ctx.Done():
	}
	return o.view()
}

func (o *op) view() (any, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	title := opTitle(o.req)
	v := map[string]any{"id": o.req.ID, "title": title, "elapsed": time.Since(o.started).Round(time.Second).String()}
	switch o.state {
	case "waiting":
		if o.waitState != "" {
			v["state"] = o.waitState
			v["next"] = "用户还没点同意或拒绝（最多等 5 分钟）。请不要提前结束回复，继续调用 task_status(id=\"" + o.req.ID + "\", wait=40) 等待结果"
		} else {
			v["state"] = "等用户在 Ferry 里确认"
			v["next"] = "还没确认（最多等 5 分钟）。过一会儿用 task_status 查这个 id"
		}
		return v, nil
	case "running":
		v["state"] = "已同意，正在执行"
		v["progress"] = slices.Clone(o.progress)
		v["next"] = "还没结束。请继续用 task_status(id=\"" + o.req.ID + "\", wait=40) 等待执行完成"
		return v, nil
	}
	o.modelDelivered = true
	if o.err != nil {
		if errors.Is(o.err, approve.ErrNoDesktop) {
			return nil, o.err
		}
		return nil, fmt.Errorf("%s：%v", title, o.err)
	}
	if o.decision == nil || !o.decision.Approve {
		note := "你在 Ferry 里拒绝了"
		if o.decision != nil && o.decision.Note != "" {
			note = o.decision.Note
		}
		return nil, fmt.Errorf("没有执行%s：%s。不要换个办法做同样的事，先问用户怎么办", title, note)
	}
	out := o.outcome
	if out == nil {
		return nil, fmt.Errorf("%s：结果未知", title)
	}
	if !out.OK {
		return nil, fmt.Errorf("%s 失败：%s\n\n过程的最后几行：\n%s", title, out.Error, strings.Join(out.Log, "\n"))
	}
	v["state"] = "完成"
	if o.decision.Note != "" {
		v["approval"] = o.decision.Note
	}
	var data any
	if json.Unmarshal(out.Data, &data) == nil {
		v["result"] = data
	}
	if len(out.Log) > 0 {
		v["log"] = out.Log
	}
	return v, nil
}

func formatFWEnablePreview(rep *proto.PortReport, r approve.Request) []string {
	var out []string
	fw := rep.Firewall
	switch {
	case fw.Active:
		out = append(out, "防火墙："+fw.Kind+" 已经在运行（执行时会报错）")
	case !fw.Enable:
		out = append(out, "防火墙：没有 Ferry 能打开的防火墙（执行时会报错）")
	default:
		out = append(out, "防火墙：打开 "+fw.Kind+"；SSH 端口一定放行，打开后连不上 SSH 时 3 分钟后自动关掉")
	}
	allow := "（只有 SSH 端口）"
	if len(r.Allow) > 0 {
		allow = strings.Join(r.Allow, "、")
	}
	out = append(out, "放行："+allow)
	if r.RestartDocker {
		out = append(out, "打开后重启 Docker（所有容器会重启一次）")
	}
	var lost []string
	for _, p := range rep.Ports {
		spec := fmt.Sprintf("%d/%s", p.Port, p.Proto)
		if len(p.Listen) == 0 || p.SSH || p.Docker || p.Internal || slices.Contains(r.Allow, spec) || localOnly(p.Listen) {
			continue
		}
		lost = append(lost, spec+"（"+orDash(strings.TrimSpace(p.Process+" "+p.Unit))+"）")
	}
	if len(lost) > 0 {
		out = append(out, "打开后外部连不上："+strings.Join(lost, "、"))
	}
	return out
}

func formatSSHKickPreview(rep *engine.SSHReport, r approve.Request) []string {
	out := make([]string, 0, len(r.Pids))
	for _, pid := range r.Pids {
		out = append(out, sshLine(rep, pid))
	}
	return out
}

// localOnly 只监听本机（127.0.0.1、[::1]）。
func localOnly(listen []string) bool {
	for _, h := range listen {
		if !strings.HasPrefix(h, "127.") && h != "[::1]" {
			return false
		}
	}
	return len(listen) > 0
}

// sshLine 确认框里一个要断开的连接：谁、从哪里、在干什么。
func sshLine(rep *engine.SSHReport, pid int) string {
	for _, x := range rep.Sessions {
		if x.PID != pid {
			continue
		}
		kind := map[string]string{"terminal": "终端", "sftp": "传文件", "ferry": "Ferry", "exec": "执行命令", "idle": "空闲"}[x.Kind]
		line := fmt.Sprintf("PID %d：%s，来自 %s，%s", pid, x.User, orDash(x.From), kind)
		if len(x.Cmds) > 0 {
			line += "，在运行 " + strings.Join(x.Cmds, "；")
		}
		if x.Since > 0 && rep.Now > x.Since {
			line += "，连了 " + span(rep.Now-x.Since)
		}
		return line
	}
	return fmt.Sprintf("PID %d：服务器上已经没有这个连接", pid)
}

// span 秒数 → 3 分钟、2 小时、5 天。
func span(sec int64) string {
	switch {
	case sec < 60:
		return "不到 1 分钟"
	case sec < 3600:
		return fmt.Sprintf("%d 分钟", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%d 小时", sec/3600)
	}
	return fmt.Sprintf("%d 天", sec/86400)
}
