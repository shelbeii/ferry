package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 文件、服务器信息、应用详情、历史日志、保留扫描（只读）；上传文件（要用户在 Ferry 里确认）。

type filesListIn struct {
	targetIn
	Path  string `json:"path,omitempty" jsonschema:"服务器上的目录；不填 = 登录用户的主目录"`
	Limit int    `json:"limit,omitempty" jsonschema:"最多列多少项，默认 500，最多 2000"`
}

type filePathIn struct {
	targetIn
	Path string `json:"path" jsonschema:"服务器上的绝对路径"`
}

type filesDownloadIn struct {
	targetIn
	Paths []string `json:"paths" jsonschema:"服务器上的文件或目录（绝对路径）"`
}

type appToolIn struct {
	targetIn
	App string `json:"app" jsonschema:"应用名"`
}

type keepItemIn struct {
	Path   string `json:"path" jsonschema:"相对应用目录的路径，如 config.js、uploads"`
	Shared bool   `json:"shared,omitempty" jsonschema:"放在 data/ 下共用（上传目录等）：只有一份，回滚时不动"`
}

type keepScanIn struct {
	targetIn
	App  string       `json:"app" jsonschema:"应用名"`
	Dir  string       `json:"dir,omitempty" jsonschema:"要列出的目录（相对应用目录，\".\" = 最上层）；不填 = 只回报配置里的那些"`
	Keep []keepItemIn `json:"keep,omitempty" jsonschema:"用这份代替 ferry.yaml 里的 artifact.keep"`
}

type filesUploadIn struct {
	targetIn
	Locals    []string `json:"locals" jsonschema:"本机的文件或目录（绝对路径）"`
	Dir       string   `json:"dir" jsonschema:"服务器上的目标目录（绝对路径）"`
	Overwrite bool     `json:"overwrite,omitempty" jsonschema:"同名文件已存在时覆盖（默认不覆盖）"`
	Reason    string   `json:"reason" jsonschema:"为什么要上传（显示在确认框里）"`
}

func (s *server) addFileTools(m *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	change := func() *mcp.ToolAnnotations {
		return &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}
	}
	const how = "需要用户在 Ferry 设置里允许编程软件请求改动；每次都要用户确认（在 Ferry 桌面版里，或者用户设置了在编程软件里确认时在这里），不能授权自动同意；" +
		"同意后由 Ferry 桌面版执行，生产服务器、「新建连接」的服务器不允许。最多等 40 秒，没结束返回 id，再用 task_status 查。"

	addTool(s, m, &mcp.Tool{Name: "files_list", Title: "列目录",
		Description: "列出服务器上某个目录（默认登录用户的主目录）：名字、是不是目录、大小、权限、属主、修改时间；默认最多 500 项，" +
			"超过时 total 是实际项数、truncated 为 true。文件内容用 files_preview。",
		Annotations: readOnly},
		logged(s, "files_list", func(ctx context.Context, in filesListIn) (any, error) {
			if in.Limit <= 0 || in.Limit > 2000 {
				in.Limit = 500
			}
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				l, err := e.ListDir(t, in.Path)
				if err != nil {
					return nil, err
				}
				out := struct {
					Path      string `json:"path"`
					Home      string `json:"home"`
					Total     int    `json:"total"`
					Truncated bool   `json:"truncated,omitempty"`
					Entries   any    `json:"entries"`
				}{Path: l.Path, Home: l.Home, Total: len(l.Entries), Entries: l.Entries}
				if len(l.Entries) > in.Limit {
					out.Entries, out.Truncated = l.Entries[:in.Limit], true
				}
				return out, nil
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "files_preview", Title: "预览文件",
		Description: "只读预览服务器上的文本文件（最多 1 MiB，超出时 truncated 为 true；二进制文件 unsupported 说明原因）。改文件用 write_file。",
		Annotations: readOnly},
		logged(s, "files_preview", func(ctx context.Context, in filePathIn) (any, error) {
			if in.Path == "" {
				return nil, errors.New("path 是空的")
			}
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.PreviewFile(t, in.Path)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "files_size", Title: "统计大小",
		Description: "服务器上文件或目录的逻辑大小（目录不展开内部链接，最多统计 2 分钟）。",
		Annotations: readOnly},
		logged(s, "files_size", func(ctx context.Context, in filePathIn) (any, error) {
			if in.Path == "" {
				return nil, errors.New("path 是空的")
			}
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.FileSize(t, in.Path)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "files_access", Title: "检查文件权限",
		Description: "按实际执行身份检查服务器上某个路径能不能读、写、进入（不能时说明属主和权限）。",
		Annotations: readOnly},
		logged(s, "files_access", func(ctx context.Context, in filePathIn) (any, error) {
			if in.Path == "" {
				return nil, errors.New("path 是空的")
			}
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.FileAccess(t, in.Path)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "files_download", Title: "下载文件到本机",
		Description: "把服务器上的文件下载到运行 Ferry 的这台电脑的 Ferry 数据目录下的 downloads/<时间>/ 里（不会覆盖任何东西），" +
			"返回本机路径；只支持单个文件、每个不超过 200 MiB（目录和更大的文件请在 Ferry 桌面版里下载，下载下来的文件可以直接打开看）。",
		Annotations: readOnly},
		logged(s, "files_download", func(ctx context.Context, in filesDownloadIn) (any, error) {
			if len(in.Paths) == 0 || len(in.Paths) > 50 {
				return nil, errors.New("paths 写 1～50 个")
			}
			for _, p := range in.Paths {
				if p == "" || !path.IsAbs(p) {
					return nil, fmt.Errorf("要下载服务器上的绝对路径：%q", p)
				}
			}
			dir := filepath.Join(appdir.Dir(), "downloads", time.Now().Format("20060102-150405"))
			for i := 2; ; i++ {
				if _, err := os.Stat(dir); os.IsNotExist(err) {
					break
				}
				dir = filepath.Join(appdir.Dir(), "downloads", fmt.Sprintf("%s-%d", time.Now().Format("20060102-150405"), i))
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				for _, p := range in.Paths {
					st, err := e.FileSize(t, p)
					if err != nil {
						return nil, err
					}
					if st.Dir {
						return nil, fmt.Errorf("%s 是目录：目录请用 Ferry 桌面版下载", p)
					}
					if st.Bytes > 200<<20 {
						return nil, fmt.Errorf("%s 超过 200 MiB：请用 Ferry 桌面版下载", p)
					}
				}
				if err := e.Download(t, in.Paths, dir, false); err != nil {
					return nil, err
				}
				return map[string]any{"dir": dir, "paths": in.Paths}, nil
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "server_info", Title: "服务器信息",
		Description: "服务器系统信息：系统版本、内核、架构、运行时间，CPU 型号与使用率，内存和 Swap，各挂载点的容量与 inode，网卡地址。",
		Annotations: readOnly},
		logged(s, "server_info", func(ctx context.Context, in targetIn) (any, error) {
			return s.withEngineAny(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.ServerInfo(t)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "app_info", Title: "应用详情",
		Description: "应用的实际情况：按什么运行环境（JDK 路径与版本）、运行的进程、systemd 服务的启动命令与用户、工作目录、部署目录与当前版本位置。",
		Annotations: readOnly},
		logged(s, "app_info", func(ctx context.Context, in appToolIn) (any, error) {
			if in.App == "" {
				return nil, errors.New("要指定 app（应用名）")
			}
			return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.AppInfo(t, in.App)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "app_log_runs", Title: "应用历史运行日志",
		Description: "应用归档的各次运行日志（每次切换版本时归档）：版本、起止时间和目录。要读某次运行的日志，用 app_logs 的 release/run。",
		Annotations: readOnly},
		logged(s, "app_log_runs", func(ctx context.Context, in appToolIn) (any, error) {
			if in.App == "" {
				return nil, errors.New("要指定 app（应用名）")
			}
			return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.LogRuns(t, in.App)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "keep_scan", Title: "保留文件扫描",
		Description: "部署时保留的文件（artifact.keep）在服务器上的情况：每项在包里的、服务器上的是否存在、是否不同；" +
			"给了 dir 还会列出服务器上运行中的版本里这个目录下的条目（用来挑选要保留的路径）。",
		Annotations: readOnly},
		logged(s, "keep_scan", func(ctx context.Context, in keepScanIn) (any, error) {
			if in.App == "" {
				return nil, errors.New("要指定 app（应用名）")
			}
			var keep []*config.KeepItem
			for _, k := range in.Keep {
				keep = append(keep, &config.KeepItem{Path: k.Path, Shared: k.Shared})
			}
			return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.KeepScan(t, in.App, keep, in.Dir)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "files_upload", Title: "上传文件到服务器",
		Description: "把运行 Ferry 的这台电脑上的文件或目录上传到服务器上的目录（同名默认不覆盖，overwrite 时才覆盖）。" +
			"上传会记进审计；生产服务器和不允许 AI 改动的服务器不能用。" + how,
		Annotations: change()},
		loggedReq(s, "files_upload", func(ctx context.Context, req *mcp.CallToolRequest, in filesUploadIn) (any, error) {
			if len(in.Locals) == 0 || len(in.Locals) > 100 {
				return nil, errors.New("locals 写 1～100 个")
			}
			if in.Dir == "" || !path.IsAbs(in.Dir) {
				return nil, errors.New("dir 要写服务器上的绝对路径")
			}
			return s.aiChange(ctx, req, in.targetIn, "upload-file", map[string]any{"locals": in.Locals, "dir": in.Dir, "overwrite": in.Overwrite}, in.Reason)
		}))
}
