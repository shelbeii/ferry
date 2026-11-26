package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/safe"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	projectsResourceURI     = "ferry://projects"
	projectConfigTemplate   = "ferry://projects/{project}/config"
	projectTargetStatusTmpl = "ferry://projects/{project}/{target}/status"
)

// addResources 注册除 MCP App 卡片之外的只读 MCP 资源与参数化资源模板（支持客户端 @ 引用项目、配置与服务器状态）。
func (s *server) addResources(m *mcp.Server) {
	m.AddResource(&mcp.Resource{
		URI:         projectsResourceURI,
		Name:        "Ferry 项目与服务器列表",
		Title:       "Ferry 项目列表",
		Description: "Ferry 当前管理的全部项目、部署目标（服务器）、应用及本机缺失项（只读 JSON）",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		text := redactJSON(s.projects())
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      projectsResourceURI,
				MIMEType: "application/json",
				Text:     ai.Clip(text, resultLimit),
			}},
		}, nil
	})

	m.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: projectConfigTemplate,
		Name:        "项目配置文件 ferry.yaml",
		Title:       "项目配置",
		Description: "只读读取指定项目的 ferry.yaml 内容（自动脱敏，不含本机凭据）",
		MIMEType:    "text/yaml",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := ""
		if req != nil && req.Params != nil {
			uri = req.Params.URI
		}
		rawProject, ok := parseProjectConfigURI(uri)
		if !ok {
			return nil, fmt.Errorf("无效的资源地址：%s（应为 %s）", uri, projectConfigTemplate)
		}
		p, err := s.configProject(rawProject)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(p.Config)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      uri,
				MIMEType: "text/yaml",
				Text:     ai.Clip(ai.Redact(string(b)), resultLimit),
			}},
		}, nil
	})

	m.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: projectTargetStatusTmpl,
		Name:        "服务器应用实时状态",
		Title:       "目标状态快照",
		Description: "只读读取指定项目在某台服务器上的实时应用运行状态（JSON，自动脱敏）",
		MIMEType:    "application/json",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := ""
		if req != nil && req.Params != nil {
			uri = req.Params.URI
		}
		project, target, ok := parseTargetStatusURI(uri)
		if !ok {
			return nil, fmt.Errorf("无效的资源地址：%s（应为 %s）", uri, projectTargetStatusTmpl)
		}
		res, err := s.withEngine(ctx, project, target, func(e *engine.Engine, t string) (any, error) {
			return e.Status(t, nil)
		})
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      uri,
				MIMEType: "application/json",
				Text:     ai.Clip(redactJSON(res), resultLimit),
			}},
		}, nil
	})
}

func parseProjectConfigURI(uri string) (string, bool) {
	const prefix = "ferry://projects/"
	const suffix = "/config"
	if !strings.HasPrefix(uri, prefix) || !strings.HasSuffix(uri, suffix) {
		return "", false
	}
	mid := uri[len(prefix) : len(uri)-len(suffix)]
	if mid == "" || strings.Contains(mid, "/") {
		return "", false
	}
	project, err := url.PathUnescape(mid)
	if err != nil || strings.TrimSpace(project) == "" {
		return "", false
	}
	return project, true
}

func parseTargetStatusURI(uri string) (project, target string, ok bool) {
	const prefix = "ferry://projects/"
	const suffix = "/status"
	if !strings.HasPrefix(uri, prefix) || !strings.HasSuffix(uri, suffix) {
		return "", "", false
	}
	mid := uri[len(prefix) : len(uri)-len(suffix)]
	parts := strings.Split(mid, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	p, err1 := url.PathUnescape(parts[0])
	t, err2 := url.PathUnescape(parts[1])
	if err1 != nil || err2 != nil || strings.TrimSpace(p) == "" || strings.TrimSpace(t) == "" {
		return "", "", false
	}
	return p, t, true
}

// uiMetaMiddleware 在「桌面端弹窗确认」模式下剥离 tools/list 返回里的 _meta.ui 元数据，
// 避免支持 MCP Apps 的客户端在走桌面确认时仍渲染空白或通用 JSON 卡片。
// 不改变对外注册的工具清单（工具数量与名称保持不变）。
func (s *server) uiMetaMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if req != nil {
			if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
				s.ensureHello(ss)
			}
		}
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/list" {
			return res, err
		}
		lr, ok := res.(*mcp.ListToolsResult)
		if !ok || lr == nil || len(lr.Tools) == 0 {
			return res, err
		}
		st, _ := loadSettingsCached()
		if st.MCP.ConfirmInClient() {
			return res, err
		}
		cloned := make([]*mcp.Tool, len(lr.Tools))
		changed := false
		for i, t := range lr.Tools {
			if t == nil || t.Meta == nil {
				cloned[i] = t
				continue
			}
			_, hasUI := t.Meta["ui"]
			_, hasLegacy := t.Meta["ui/resourceUri"]
			// 保留 app-only 工具（mcp_app_decide / mcp_app_status）的 visibility: ["app"]，只剥离改动工具的 resourceUri
			if (!hasUI && !hasLegacy) || t.Name == "mcp_app_decide" || t.Name == "mcp_app_status" || t.Name == "mcp_app_cancel" {
				cloned[i] = t
				continue
			}
			changed = true
			cp := *t
			meta := make(mcp.Meta, len(t.Meta))
			for k, v := range t.Meta {
				if k == "ui" || k == "ui/resourceUri" {
					continue
				}
				meta[k] = v
			}
			if len(meta) == 0 {
				cp.Meta = nil
			} else {
				cp.Meta = meta
			}
			cloned[i] = &cp
		}
		if !changed {
			return res, err
		}
		out := *lr
		out.Tools = cloned
		return &out, nil
	}
}

func sanitizeClientInfo(p *mcp.InitializeParams) string {
	if p == nil || p.ClientInfo == nil {
		return ""
	}
	raw := strings.TrimSpace(p.ClientInfo.Name + " " + p.ClientInfo.Version)
	var b strings.Builder
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	clean := strings.Join(strings.Fields(b.String()), " ")
	if rs := []rune(clean); len(rs) > 80 {
		return string(rs[:80])
	}
	return clean
}

func sessionCapabilities(p *mcp.InitializeParams) (protoVer string, caps []string) {
	if p == nil {
		return "", nil
	}
	protoVer = p.ProtocolVersion
	if p.Capabilities == nil {
		return protoVer, nil
	}
	if p.Capabilities.Extensions != nil {
		if _, ok := p.Capabilities.Extensions[mcpAppExtension]; ok {
			caps = append(caps, "mcp-app")
		}
	} else if p.Capabilities.Experimental != nil {
		if _, ok := p.Capabilities.Experimental[mcpAppExtension]; ok {
			caps = append(caps, "mcp-app")
		}
	}
	if p.Capabilities.Elicitation != nil && (p.Capabilities.Elicitation.Form != nil || p.Capabilities.Elicitation.URL == nil) {
		caps = append(caps, "elicitation")
	}
	if protoVer >= "2026-07-28" {
		caps = append(caps, "mrtr")
	}
	if p.Capabilities.RootsV2 != nil || p.Capabilities.Roots.ListChanged {
		caps = append(caps, "roots")
	}
	return protoVer, caps
}

func (s *server) reportHello(ss *mcp.ServerSession) {
	if ss == nil {
		return
	}
	p := ss.InitializeParams()
	if p == nil {
		return
	}
	client := sanitizeClientInfo(p)
	protoVer, caps := sessionCapabilities(p)
	st, _ := loadSettingsCached()
	channel := "desktop"
	if st.MCP.ConfirmInClient() {
		switch {
		case slices.Contains(caps, "mcp-app"):
			channel = "mcp-app"
		case slices.Contains(caps, "elicitation"):
			channel = "elicitation"
		case st.MCP.FallbackTwoStep():
			channel = "two-step"
		}
	}
	dir := s.o.Dir
	s.mu.Lock()
	if len(s.roots) > 0 {
		dir = s.roots[0]
	}
	s.mu.Unlock()
	pid := os.Getpid()
	safe.Go("上报 MCP 会话能力", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		approve.Hello(ctx, approverFile(), s.session, pid, dir, client, protoVer, channel, caps)
	})
}

func (s *server) ensureHello(ss *mcp.ServerSession) {
	if ss == nil || ss.InitializeParams() == nil {
		return
	}
	s.mu.Lock()
	if s.helloSent {
		s.mu.Unlock()
		return
	}
	s.helloSent = true
	s.mu.Unlock()
	s.reportHello(ss)
}

func (s *server) onInitialized(ctx context.Context, req *mcp.InitializedRequest) {
	if req == nil || req.Session == nil {
		return
	}
	s.ensureHello(req.Session)
	p := req.Session.InitializeParams()
	if p != nil && p.Capabilities != nil && (p.Capabilities.RootsV2 != nil || p.Capabilities.Roots.ListChanged) {
		safe.Go("拉取 MCP Roots", func() {
			s.refreshRoots(req.Session)
		})
	}
}

func (s *server) onRootsChanged(ctx context.Context, req *mcp.RootsListChangedRequest) {
	if req == nil || req.Session == nil {
		return
	}
	safe.Go("刷新 MCP Roots", func() {
		s.refreshRoots(req.Session)
	})
}

func (s *server) refreshRoots(ss *mcp.ServerSession) {
	if ss == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := ss.ListRoots(ctx, nil)
	if err != nil || res == nil {
		return
	}
	var dirs []string
	for _, r := range res.Roots {
		if r == nil || r.URI == "" {
			continue
		}
		if dir := fileURIToPath(r.URI); dir != "" {
			dirs = append(dirs, dir)
		}
	}
	s.mu.Lock()
	s.roots = dirs
	s.mu.Unlock()
}

// fileURIToPath 把 MCP Roots 的 file:// URI 转成本机目录路径（兼容 Windows 的 file:///D:/... 与 Unix 的 file:///...）。
func fileURIToPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return ""
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
			return filepath.FromSlash(`\\` + u.Host + p)
		}
		if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
	}
	if p == "" {
		return ""
	}
	return filepath.Clean(filepath.FromSlash(p))
}

// currentDirConfig 优先按客户端经 MCP Roots 上报的当前工作区目录查找 ferry.yaml，找不到再回退启动目录 s.o.Dir。
func (s *server) currentDirConfig() string {
	s.mu.Lock()
	roots := append([]string(nil), s.roots...)
	s.mu.Unlock()
	for _, dir := range roots {
		if found, err := config.Find(dir); err == nil && found != "" {
			return found
		}
	}
	if s.o.Dir != "" {
		if found, err := config.Find(s.o.Dir); err == nil && found != "" {
			return found
		}
	}
	return ""
}

// handleCompletion 为 MCP Prompt 参数（project / target / app / connection）提供自动补全。
func (s *server) handleCompletion(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	if req == nil || req.Params == nil {
		return &mcp.CompleteResult{}, nil
	}
	argName := req.Params.Argument.Name
	prefix := strings.ToLower(strings.TrimSpace(req.Params.Argument.Value))
	ctxArgs := map[string]string{}
	if req.Params.Context != nil && req.Params.Context.Arguments != nil {
		ctxArgs = req.Params.Context.Arguments
	}

	var candidates []string
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		if prefix != "" && !strings.Contains(strings.ToLower(v), prefix) {
			return
		}
		seen[v] = true
		candidates = append(candidates, v)
	}

	switch argName {
	case "project":
		for _, p := range s.projects() {
			if p.Name != connectionsName {
				add(p.Name)
				if p.Project != "" && p.Project != p.Name {
					add(p.Project)
				}
			}
		}
	case "target":
		ctxProject := ctxArgs["project"]
		if u, err := url.PathUnescape(ctxProject); err == nil {
			ctxProject = u
		}
		if p, err := s.find(ctxProject); err == nil {
			for _, t := range p.Targets {
				add(t.Name)
			}
		} else {
			for _, pr := range s.projects() {
				for _, t := range pr.Targets {
					add(t.Name)
				}
			}
		}
	case "app":
		ctxProject := ctxArgs["project"]
		if u, err := url.PathUnescape(ctxProject); err == nil {
			ctxProject = u
		}
		if p, err := s.find(ctxProject); err == nil {
			for _, a := range p.Apps {
				add(a.Name)
			}
		} else {
			for _, pr := range s.projects() {
				for _, a := range pr.Apps {
					add(a.Name)
				}
			}
		}
	case "connection":
		if list, _, err := engine.AIDBConns(); err == nil {
			for _, c := range list {
				add(c.Name)
			}
		}
	}

	hasMore := false
	if len(candidates) > 50 {
		candidates = candidates[:50]
		hasMore = true
	}
	if candidates == nil {
		candidates = []string{}
	}
	// 确保可以序列化为标准 CompleteResult
	_ = json.Valid
	return &mcp.CompleteResult{
		Completion: mcp.CompletionResultDetails{
			Values:  candidates,
			Total:   len(candidates),
			HasMore: hasMore,
		},
	}, nil
}
