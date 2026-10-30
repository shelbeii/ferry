package mcpserver

import (
	"cmp"
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
	ID   string `json:"id" jsonschema:"deploy、app_control、rollback 返回的 id"`
	Wait int    `json:"wait,omitempty" jsonschema:"最多等多少秒看它结束，默认 30，最多 40"`
}

var actionNames = map[string]string{"deploy": "部署", "start": "启动", "stop": "停止", "restart": "重启", "rollback": "回滚",
	"sw-start": "启动软件", "sw-stop": "停止软件", "sw-restart": "重启软件", "port-open": "放行端口", "port-close": "关闭端口",
	"fw-enable": "打开防火墙", "ssh-kick": "断开 SSH 连接", "backup-run": "现在备份", "create": "新建项目",
	"exec": "执行命令", "write-file": "写文件", "install": "安装软件", "tpl-save": "保存软件模板", "secret": "填写密码"}

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
	const how = "需要用户在 Ferry 设置里允许编程软件请求改动，每次都在 Ferry 桌面版里确认，同意后由桌面版执行（任务面板里能看到）；生产服务器不允许。" +
		"最多等 40 秒：结束了直接返回结果，没结束返回 id 和当前进展，再用 task_status 查。"
	addTool(s, m, &mcp.Tool{Name: "deploy", Title: "部署",
		Description: "把项目里的应用部署到某台服务器：先在本机构建（skipBuild 时跳过），上传，逐个切换到新版本并等健康检查通过；失败自动回滚该应用。" +
			how + "构建失败时结果里有构建输出的最后几十行。",
		Annotations: change()},
		loggedReq(s, "deploy", func(ctx context.Context, req *mcp.CallToolRequest, in deployIn) (any, error) {
			if len(in.Apps) == 0 {
				return nil, errors.New("要用 apps 写明部署哪些应用")
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: "deploy", Apps: in.Apps, SkipBuild: in.SkipBuild, Reason: in.Reason})
		}))

	addTool(s, m, &mcp.Tool{Name: "app_control", Title: "启动 / 停止 / 重启应用",
		Description: "启动、停止或重启项目里的应用（等健康检查通过）。" + how,
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
		Description: "查 deploy、app_control、rollback 的进展和结果（它们没在 40 秒内结束时返回的 id）。最多等 wait 秒看它结束。",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		loggedReq(s, "task_status", func(ctx context.Context, req *mcp.CallToolRequest, in statusTaskIn) (any, error) {
			s.mu.Lock()
			o := s.ops[in.ID]
			s.mu.Unlock()
			if o == nil {
				return nil, fmt.Errorf("没有这个 id：%s（ferry mcp 重启过的话，之前的记录就没了；可以用 deploy_history、app_status 看结果）", in.ID)
			}
			wait := time.Duration(in.Wait) * time.Second
			if in.Wait <= 0 {
				wait = 30 * time.Second
			}
			return o.wait(ctx, req, min(wait, waitCap))
		}))
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

// PrepareRequest 桌面版收到请求、交给用户之前再检查一遍（防止有人绕过 ferry mcp 直接发请求）；
// 新建项目的 ferry.yaml 在这里按 Spec 重新生成，确认框里显示的就是要写入的。
func PrepareRequest(r *approve.Request) error {
	if _, ok := actionNames[r.Action]; !ok {
		return fmt.Errorf("不支持的操作：%s", r.Action)
	}
	if r.ClientConfirmed && (r.Action == "create" || r.Action == "secret") {
		return errors.New("新建项目、填写密码只能在 Ferry 里确认")
	}
	if r.Action == "create" {
		content, err := generateProject(r.Spec, r.Config)
		if err != nil {
			return err
		}
		r.Content = content
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
	if r.ClientConfirmed && st.MCP.ConfirmIn != "client" {
		return errors.New("Ferry 设置里没有选在编程软件里确认改动：要在 Ferry 里确认")
	}
	if _, ok := aiActionTool[r.Action]; ok {
		e := &engine.Engine{File: f, Rep: quiet{}, Settings: st, AcceptNewHostKey: st.Connect.AcceptNewHostKey, Ctx: context.Background(), Getenv: os.Getenv}
		return prepareAI(e, r)
	}
	if r.Action == "secret" {
		return ai.CheckSecretFields(r.Fields)
	}
	if strings.HasPrefix(r.Action, "sw-") {
		if !reSoftware.MatchString(r.Software) {
			return fmt.Errorf("软件名称不对：%q", r.Software)
		}
		return nil
	}
	if strings.HasPrefix(r.Action, "port-") {
		if !validPort(r.Port) {
			return fmt.Errorf("端口不对：%q（写 8080/tcp 或 53/udp）", r.Port)
		}
		return nil
	}
	switch r.Action {
	case "fw-enable":
		for _, p := range r.Allow {
			if !validPort(p) {
				return fmt.Errorf("端口不对：%q（写 8080/tcp 或 53/udp）", p)
			}
		}
		return nil
	case "backup-run":
		if r.Backup != "" && !reBackupName.MatchString(r.Backup) {
			return fmt.Errorf("备份的名字不对：%q", r.Backup)
		}
		return hasBackup(&engine.Engine{File: f}, t.Name, r.Backup)
	case "ssh-kick":
		if len(r.Pids) == 0 {
			return errors.New("没有写明断开哪个连接")
		}
		for _, p := range r.Pids {
			if p <= 1 {
				return fmt.Errorf("PID 不对：%d", p)
			}
		}
		return nil
	}
	if len(r.Apps) == 0 {
		return errors.New("没有写明应用")
	}
	if r.Action == "rollback" && len(r.Apps) != 1 {
		return errors.New("回滚一次只能一个应用")
	}
	for _, a := range r.Apps {
		if f.App(a) == nil || !t.Allows(a) {
			return fmt.Errorf("%s 上没有应用 %s", t.Name, a)
		}
	}
	return nil
}

// ---------------------------------------------------------------- 提请求、等结果

// change 检查是否允许，补上给人看的信息，交给桌面版；最多等 waitCap，没结束就返回 id。
func (s *server) change(ctx context.Context, req *mcp.CallToolRequest, in targetIn, r approve.Request) (any, error) {
	if req != nil && req.Params != nil && req.Params.RequestState != "" {
		return s.confirmed(ctx, req)
	}
	inClient := false
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
		r.Reason = strings.TrimSpace(r.Reason)
		if r.Tool != "" || r.Action == "secret" {
			r.Detail, r.Danger = aiPreview(e, target, r)
		} else {
			r.Detail = preview(e, target, r)
		}
		inClient = e.Settings.MCP.ConfirmIn == "client"
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	// 设置里选了在编程软件里确认、编程软件也支持时，先让编程软件问用户（用户在编程软件的界面里回答，不经过模型），
	// 回答后编程软件带着回答再调一次（见上面）；密码始终在 Ferry 里填
	if inClient && r.Action != "secret" && canElicit(req) {
		state := newID() + newID()
		s.mu.Lock()
		s.confirms[state] = r
		s.mu.Unlock()
		return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{confirmKey: confirmForm(r)}, RequestState: state}, nil
	}
	return s.submit(r).wait(ctx, req, waitCap)
}

// confirmKey 交给编程软件的确认表单的标识。
const confirmKey = "ferry-confirm"

// confirmed 编程软件带着用户的回答再调一次：按第一次准备好的请求执行（同意时）。
func (s *server) confirmed(ctx context.Context, req *mcp.CallToolRequest) (any, error) {
	p := req.Params
	s.mu.Lock()
	r, ok := s.confirms[p.RequestState]
	delete(s.confirms, p.RequestState)
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("这次确认已经失效（ferry mcp 重启过，或者已经回答过）：要做的话重新调用")
	}
	yes := false
	if res, ok := p.InputResponses[confirmKey].(*mcp.ElicitResult); ok && res.Action == "accept" {
		yes, _ = res.Content["approve"].(bool)
	}
	if !yes {
		return nil, fmt.Errorf("没有执行%s：用户在 %s 里拒绝了。不要换个办法做同样的事，先问用户怎么办", opTitle(r), orDash(r.Client))
	}
	r.ClientConfirmed = true
	return s.submit(r).wait(ctx, req, waitCap)
}

// canElicit 编程软件支不支持在它的界面里弹出表单（MCP 的 elicitation）。
func canElicit(req *mcp.CallToolRequest) bool {
	if req == nil || req.Session == nil {
		return false
	}
	p := req.Session.InitializeParams()
	if p == nil || p.Capabilities == nil || p.Capabilities.Elicitation == nil {
		return false
	}
	el := p.Capabilities.Elicitation
	return el.Form != nil || el.URL == nil // 两个都没写时按支持表单算
}

// confirmForm 在编程软件里给用户看的确认表单：要做什么、在哪台服务器、详情、理由，一个「同意执行」。
func confirmForm(r approve.Request) *mcp.ElicitParams {
	var b strings.Builder
	fmt.Fprintf(&b, "Ferry：%s 请求%s\n服务器：%s（%s）\n", orDash(r.Client), opTitle(r), r.Target, r.Host)
	if r.Danger != "" {
		fmt.Fprintf(&b, "危险：%s\n", r.Danger)
	}
	for _, d := range r.Detail {
		b.WriteString(d + "\n")
	}
	if r.Reason != "" {
		fmt.Fprintf(&b, "理由：%s\n", r.Reason)
	}
	b.WriteString("同意后由 Ferry 桌面版执行（任务面板里能看到）。看不明白要做什么时请拒绝。")
	return &mcp.ElicitParams{Mode: "form", Message: ai.Clip(b.String(), 8000), RequestedSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"approve": map[string]any{"type": "boolean", "title": "同意执行", "default": false},
		},
		"required": []string{"approve"},
	}}
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
	if r.Action == "deploy" {
		git, steps, err := e.DeployPreview(target, r.Apps)
		switch {
		case git.Commit != "":
			line := fmt.Sprintf("代码：%s 分支，提交 %s", orDash(git.Branch), git.Short())
			if git.Dirty {
				line += fmt.Sprintf("，有 %d 个文件改了没提交", len(git.Files))
			}
			out = append(out, line)
		default:
			out = append(out, "代码：不在 git 仓库里，看不到提交")
		}
		if r.SkipBuild {
			out = append(out, "构建：跳过，用本机现有的产物")
		} else if err == nil {
			for _, st := range steps {
				out = append(out, fmt.Sprintf("构建 %s：%s", st.Name, st.Command))
			}
		}
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
		for _, pid := range r.Pids {
			out = append(out, sshLine(rep, pid))
		}
		return out
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

// op 一个交给桌面版的改动：等确认、执行中、结束。ferry mcp 退出时就没了（桌面版那边照样执行完）。
type op struct {
	req     approve.Request
	started time.Time

	mu       sync.Mutex
	state    string // waiting | running | done
	decision *approve.Decision
	outcome  *approve.Outcome
	err      error
	progress []string
	notify   func(string) // 有工具调用在等时：把进展转成 MCP 的进度通知
	done     chan struct{}
}

func (s *server) submit(r approve.Request) *op {
	o := &op{req: r, started: time.Now(), state: "waiting", done: make(chan struct{})}
	s.mu.Lock()
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
		o.mu.Lock()
		o.state, o.outcome, o.err = "done", out, err
		if o.decision == nil && err == nil {
			o.decision = &d
		}
		o.mu.Unlock()
		close(o.done)
		args, _ := json.Marshal(map[string]any{"project": r.Project, "target": r.Target, "apps": r.Apps, "name": cmp.Or(r.Software, r.Backup), "port": r.Port, "action": actionNames[r.Action]})
		entry := callEntry{Time: time.Now(), Client: r.Client, Tool: r.Action + "（结果）", Op: r.ID, Args: args, Ms: time.Since(o.started).Milliseconds()}
		switch {
		case err != nil:
			entry.Error = errText(err)
		case !d.Approve:
			entry.Decision, entry.Error = "拒绝", d.Note
		default:
			entry.Decision = "同意"
			if d.Note != "" {
				entry.Decision = d.Note
			}
			entry.OK = out != nil && out.OK
			if out != nil && !out.OK {
				entry.Error = errText(errors.New(out.Error))
			}
		}
		writeLog(entry)
	})
	return o
}

// opTitle 给编程软件看的一句话，如「部署 api（test）」。
func opTitle(r approve.Request) string {
	switch {
	case r.Action == "create":
		return "新建项目 " + r.Project + "（" + r.Config + "）"
	case strings.HasPrefix(r.Action, "sw-"):
		return actionNames[r.Action] + " " + r.Software + "（" + r.Target + "）"
	case strings.HasPrefix(r.Action, "port-"):
		return actionNames[r.Action] + " " + r.Port + "（" + r.Target + "）"
	case r.Action == "fw-enable" || r.Action == "exec" || r.Action == "install" || r.Action == "tpl-save":
		return actionNames[r.Action] + "（" + r.Target + "）"
	case r.Action == "write-file":
		var in struct{ Path string }
		json.Unmarshal(r.Input, &in)
		return actionNames[r.Action] + " " + in.Path + "（" + r.Target + "）"
	case r.Action == "secret":
		labels := make([]string, len(r.Fields))
		for i, f := range r.Fields {
			labels[i] = f.Label
		}
		return actionNames[r.Action] + "：" + strings.Join(labels, "、") + "（" + r.Target + "）"
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
	return actionNames[r.Action] + " " + strings.Join(r.Apps, "、") + "（" + r.Target + "）"
}

func (o *op) addProgress(line string) {
	o.mu.Lock()
	o.progress = append(o.progress, line)
	if len(o.progress) > 40 {
		o.progress = o.progress[len(o.progress)-40:]
	}
	notify := o.notify
	o.mu.Unlock()
	if notify != nil {
		notify(line)
	}
}

// wait 最多等 d，返回现在的情况；拒绝、失败作为错误返回（编程软件那边标成失败）。
func (o *op) wait(ctx context.Context, req *mcp.CallToolRequest, d time.Duration) (any, error) {
	if req != nil && req.Session != nil && req.Params != nil {
		if tok := req.Params.GetProgressToken(); tok != nil {
			n := 0
			o.mu.Lock()
			o.notify = func(line string) {
				n++
				req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Message: line, Progress: float64(n)})
			}
			o.mu.Unlock()
			defer func() { o.mu.Lock(); o.notify = nil; o.mu.Unlock() }()
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
		v["state"] = "等用户在 Ferry 里确认"
		v["next"] = "还没确认（最多等 5 分钟）。过一会儿用 task_status 查这个 id"
		return v, nil
	case "running":
		v["state"] = "已同意，Ferry 正在执行"
		v["progress"] = slices.Clone(o.progress)
		v["next"] = "还没结束。过一会儿用 task_status 查这个 id"
		return v, nil
	}
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
