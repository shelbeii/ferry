package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/remote"
)

const aiSudoList = "files_list_sudo"

type aiSudoFilesKey struct{}

type aiListArgs struct {
	Path  string `json:"path"`
	Limit int    `json:"limit"`
}

func aiDirectoryResult(dir, home string, entries []remote.FileEntry, limit int) string {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	total := len(entries)
	if total > limit {
		entries = entries[:limit]
	}
	return toJSON(struct {
		Path      string             `json:"path"`
		Home      string             `json:"home,omitempty"`
		Total     int                `json:"total"`
		Truncated bool               `json:"truncated,omitempty"`
		Entries   []remote.FileEntry `json:"entries"`
	}{dir, home, total, total > limit, entries})
}

func aiSudoListArgs(in json.RawMessage) (aiListArgs, error) {
	var a aiListArgs
	if err := parseArgs(in, &a); err != nil {
		return a, err
	}
	if !path.IsAbs(a.Path) || strings.ContainsAny(a.Path, "\x00\r\n") {
		return a, fmt.Errorf("要指定目录的绝对路径")
	}
	return a, nil
}

func aiSudoListTool() ai.Tool {
	return ai.Tool{
		Name: aiSudoList,
		Description: "普通列目录因登录用户权限不足时，在用户逐次确认后用服务器配置的 sudo 列出指定目录。" +
			"结果会发给当前模型。只列目录条目和属性，不读文件内容，不修改权限。日志正文优先用 app_logs。",
		Schema: schema(map[string]any{
			"path":  str("普通列目录失败的绝对目录路径"),
			"limit": map[string]any{"type": "integer", "description": "最多列多少项，默认 500，最多 2000"},
		}, "path"),
		Approve: func(_ context.Context, in json.RawMessage) *ai.Approval {
			a, err := aiSudoListArgs(in)
			if err != nil {
				return nil
			}
			return &ai.Approval{Kind: "sudo 列目录", Title: "用 sudo 查看目录", AsRoot: true, State: toJSON(a), Once: "本次只授权查看这个目录",
				Detail: fmt.Sprintf("目录 %q（如果是符号链接，会查看它指向的目录）\n条目名称和属性将发送给当前模型。不会读取文件内容或修改权限。", a.Path)}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			a, err := aiSudoListArgs(in)
			if err != nil {
				return "", err
			}
			files, _ := ctx.Value(aiSudoFilesKey{}).(*remote.FileClient)
			if files == nil || ai.ApprovedState(ctx) != toJSON(a) {
				return "", fmt.Errorf("这项提权查看尚未获得当前用户确认")
			}
			dir, entries, err := files.ListDir(a.Path)
			if err != nil {
				return "", err
			}
			return aiDirectoryResult(dir, "", entries, a.Limit), nil
		},
	}
}

// aiFileTools 文件、服务器信息、应用详情、历史日志、保留扫描这些查看工具，以及上传（改动）。
// 编程软件经 MCP 用的是同名工具；参数名两边保持一致。
func (e *Engine) aiFileTools(t *config.Target, change bool) []ai.Tool {
	tools := []ai.Tool{{
		Name: "files_list",
		Description: "列出服务器上某个目录（默认登录用户的主目录）：名字、是不是目录、大小、权限、属主、修改时间；" +
			"以 SSH 登录用户查看。权限不足时，排查应用日志优先用 app_logs；确实要列受限目录时，可用 files_list_sudo 申请单次确认（如该工具可用）。" +
			"默认最多 500 项，超过时 total 是实际项数、truncated 为 true。",
		Schema: schema(map[string]any{
			"path":  str("服务器上的目录；不填 = 登录用户的主目录"),
			"limit": map[string]any{"type": "integer", "description": "最多列多少项，默认 500，最多 2000"},
		}),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a aiListArgs
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			l, err := e.ListDir(t.Name, a.Path)
			if err != nil {
				var denied *fileAccessError
				if errors.As(err, &denied) {
					return "", fmt.Errorf("%w。此目标已配置 sudo。需要查看这个目录时，请开启「允许读取并发送排查明细」并确认本次提权查看", err)
				}
				return "", err
			}
			return aiDirectoryResult(l.Path, l.Home, l.Entries, a.Limit), nil
		},
	}, {
		Name:        "files_preview",
		Description: "只读预览服务器上的文本文件（最多 1 MiB，超出时 truncated 为 true；二进制文件 unsupported 说明原因）。",
		Schema:      schema(map[string]any{"path": str("文件的绝对路径")}, "path"),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if a.Path == "" {
				return "", fmt.Errorf("要给出 path")
			}
			d, err := e.PreviewFile(t.Name, a.Path)
			if err != nil {
				return "", err
			}
			return toJSON(d), nil
		},
	}, {
		Name:        "files_size",
		Description: "服务器上文件或目录的逻辑大小（目录不展开内部链接，统计最多 2 分钟）。",
		Schema:      schema(map[string]any{"path": str("服务器上的绝对路径")}, "path"),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			r, err := e.FileSize(t.Name, a.Path)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name:        "files_access",
		Description: "按实际执行身份检查服务器上某个路径能不能读、写、进入（不能时说明属主和权限）。",
		Schema:      schema(map[string]any{"path": str("服务器上的绝对路径")}, "path"),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			r, err := e.FileAccess(t.Name, a.Path)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name: "files_download",
		Description: "把服务器上的文件下载到用户这台电脑的 Ferry 数据目录下的 downloads/<时间>/ 里（不会覆盖任何东西），返回本机的路径；" +
			"只支持单个文件、每个不超过 200 MiB（目录请用 Ferry 桌面版下载）。下载下来的文件用户和编程软件都能直接打开。",
		Schema: schema(map[string]any{"paths": strList("服务器上的文件或目录（绝对路径）")}, "paths"),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Paths []string `json:"paths"`
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			if len(a.Paths) == 0 {
				return "", fmt.Errorf("要给出 paths")
			}
			dir := filepath.Join(appdir.Dir(), "downloads", time.Now().Format("20060102-150405"))
			for i := 2; ; i++ { // 同一秒内下载两次：换个目录，不覆盖
				if _, err := os.Stat(dir); os.IsNotExist(err) {
					break
				}
				dir = filepath.Join(appdir.Dir(), "downloads", fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), i))
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
			for _, p := range a.Paths {
				st, err := e.FileSize(t.Name, p)
				if err != nil {
					return "", err
				}
				if st.Dir {
					return "", fmt.Errorf("%s 是目录：目录请用 Ferry 桌面版下载（一次拉整个目录容易把本机磁盘占满）", p)
				}
				if st.Bytes > 200<<20 {
					return "", fmt.Errorf("%s 有 %s，超过 200 MiB：请用 Ferry 桌面版下载", p, humanBytes(st.Bytes))
				}
			}
			if err := e.Download(t.Name, a.Paths, dir, false); err != nil {
				return "", err
			}
			return toJSON(map[string]any{"dir": dir, "paths": a.Paths}), nil
		},
	}, {
		Name:        "server_info",
		Description: "服务器系统信息：系统版本、内核、架构、运行时间，CPU 型号与使用率，内存和 Swap，各挂载点的容量与 inode，网卡地址。",
		Schema:      schema(map[string]any{}),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			r, err := e.ServerInfo(t.Name)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name:        "app_info",
		Description: "应用的实际情况：按什么运行环境（JDK 路径与会话）、运行的进程、systemd 服务的启动命令与用户、工作目录、部署目录与当前版本位置。",
		Schema:      schema(map[string]any{"app": str("应用名")}, "app"),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				App string `json:"app"`
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			r, err := e.AppInfo(t.Name, a.App)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name:        "app_log_runs",
		Description: "应用归档的各次运行日志：每次切换版本时归档，返回版本、起止时间和目录。要读某次运行的日志，用 app_logs 的 release/run。",
		Schema:      schema(map[string]any{"app": str("应用名")}, "app"),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				App string `json:"app"`
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			r, err := e.LogRuns(t.Name, a.App)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}, {
		Name: "keep_scan",
		Description: "部署时保留的文件（artifact.keep）在服务器上的情况：每项在包里的、服务器上的是否存在、是否不同；" +
			"给了 dir 还会列出服务器上运行中的版本里这个目录下的条目（用来挑选要保留的路径）。",
		Schema: schema(map[string]any{
			"app":  str("应用名"),
			"dir":  str("要列出的目录（相对应用目录，\".\" = 最上层）；不填 = 只回报配置里的那些"),
			"keep": map[string]any{"type": "array", "description": "用这份代替 ferry.yaml 里的 artifact.keep，如 [{\"path\":\"config.js\"},{\"path\":\"uploads\",\"shared\":true}]", "items": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "shared": map[string]any{"type": "boolean"}}}},
		}, "app"),
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				App  string             `json:"app"`
				Dir  string             `json:"dir"`
				Keep []*config.KeepItem `json:"keep"`
			}
			if err := parseArgs(in, &a); err != nil {
				return "", err
			}
			r, err := e.KeepScan(t.Name, a.App, a.Keep, a.Dir)
			if err != nil {
				return "", err
			}
			return toJSON(r), nil
		},
	}}
	if change {
		tools = append(tools, ai.Tool{
			Name: "files_upload",
			Description: "把用户这台电脑上的文件或目录上传到服务器上的目录（同名的默认不覆盖，overwrite 时才覆盖）。上传会记进审计；" +
				"生产服务器和不允许 AI 改动的服务器不能用。",
			Schema: schema(map[string]any{
				"locals":    strList("本机的文件或目录（绝对路径）"),
				"dir":       str("服务器上的目标目录（绝对路径）"),
				"overwrite": map[string]any{"type": "boolean", "description": "同名文件已存在时覆盖（默认不覆盖）"},
			}, "locals", "dir"),
			Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
				var a struct {
					Locals    []string `json:"locals"`
					Dir       string   `json:"dir"`
					Overwrite bool     `json:"overwrite"`
				}
				if parseArgs(in, &a) != nil || len(a.Locals) == 0 || a.Dir == "" {
					return nil
				}
				var d []string
				total := int64(0)
				for _, p := range a.Locals {
					st, err := os.Stat(p)
					if err != nil {
						return nil
					}
					d = append(d, fmt.Sprintf("%s（%s）", p, humanBytes(st.Size())))
					total += st.Size()
				}
				d = append(d, fmt.Sprintf("上传到服务器 %s 的 %s，共 %s", t.Name, a.Dir, humanBytes(total)))
				if a.Overwrite {
					d = append(d, "同名文件/目录已存在时会覆盖")
				}
				return &ai.Approval{Kind: "上传文件", Title: fmt.Sprintf("上传 %d 个文件到 %s", len(a.Locals), t.Name), Detail: strings.Join(d, "\n"), AsRoot: true,
					Danger: map[bool]string{true: "会覆盖服务器上的同名文件（确认框里显示了来源和去向）", false: ""}[a.Overwrite]}
			},
			Run: func(ctx context.Context, in json.RawMessage) (string, error) {
				var a struct {
					Locals    []string `json:"locals"`
					Dir       string   `json:"dir"`
					Overwrite bool     `json:"overwrite"`
				}
				if err := parseArgs(in, &a); err != nil {
					return "", err
				}
				if err := e.Upload(t.Name, a.Locals, a.Dir, a.Overwrite); err != nil {
					return "", err
				}
				return fmt.Sprintf("已上传 %d 个文件到 %s", len(a.Locals), a.Dir), nil
			},
		})
	}
	return tools
}
