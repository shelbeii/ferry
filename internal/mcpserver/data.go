package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/captain/ferry/internal/approve"
	"github.com/captain/ferry/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 部署会带上线的改动、服务器告警、数据备份（只读）；现在备份一次（要确认）。
// 打开 / 关闭告警和数据备份、恢复、删掉备份只能由用户在 Ferry 里做。

type changesIn struct {
	targetIn
	Apps []string `json:"apps,omitempty" jsonschema:"只看这些应用；不填 = 这台服务器上的全部应用"`
}

type monitorIn struct {
	targetIn
	Check bool `json:"check,omitempty" jsonschema:"现在在服务器上检查一次（应用、磁盘、证书、防火墙、端口、数据备份），返回发现的问题；不发告警。不填 = 只看告警开没开、上次检查发现的问题"`
}

type backupIn struct {
	targetIn
	Files bool   `json:"files,omitempty" jsonschema:"列出服务器上的备份文件（文件名、大小、时间）"`
	Name  string `json:"name,omitempty" jsonschema:"files 时只列这一项（backups 里的 name）"`
}

type backupRunIn struct {
	targetIn
	Name   string `json:"name,omitempty" jsonschema:"备份哪一项（backup_status 结果里 items 的 name）；不填 = 这台服务器上的全部"`
	Reason string `json:"reason" jsonschema:"为什么要备份（显示在 Ferry 的确认框里）"`
}

// reBackupName 配置里 backups 的名字。
var reBackupName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func (s *server) addDataTools(m *mcp.Server) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}

	addTool(s, m, &mcp.Tool{Name: "deploy_changes", Title: "部署会带上线的改动",
		Description: "部署前看这次会带上线什么（不改动服务器）：线上运行的版本和本地代码之间的提交、改了的文件，" +
			"生成的 systemd 单元和线上比变了的行，附带文件新增、改了、去掉的。线上有本地没有的提交（别人部署过更新的代码）时 warn 为 true，部署前先提醒用户。",
		Annotations: readOnly},
		logged(s, "deploy_changes", func(ctx context.Context, in changesIn) (any, error) {
			return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.Changes(t, in.Apps)
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "monitor_status", Title: "服务器告警",
		Description: "服务器告警（服务器上的 Ferry 定时检查应用、磁盘、HTTPS 证书、防火墙、对外开放的端口、数据备份，有问题发到群里）：开没开、多久检查一次、发到哪、上次检查发现的问题。" +
			"check 为 true 时现在检查一次，返回发现的问题（不发告警，没打开告警也能查）。打开、关闭告警要用户在 Ferry 的「服务器告警」里操作。",
		Annotations: readOnly},
		logged(s, "monitor_status", func(ctx context.Context, in monitorIn) (any, error) {
			action := "status"
			if in.Check {
				action = "check"
			}
			return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.Monitor(t, action, engine.MonitorOptions{})
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "backup_status", Title: "数据备份",
		Description: "某台服务器上的数据备份（配置里的 backups：MySQL、PostgreSQL、目录、自己写的备份命令）：开没开、各项多久备份一次、下次什么时候、上次的结果（失败时有原因）、有几份、占多少；" +
			"files 为 true 时列出备份文件。只读。现在备份一次用 backup_run；打开、关闭、恢复、删掉备份要用户在 Ferry 的「数据备份」里操作。",
		Annotations: readOnly},
		logged(s, "backup_status", func(ctx context.Context, in backupIn) (any, error) {
			action := "status"
			if in.Files {
				action = "list"
			}
			if in.Name != "" && !reBackupName.MatchString(in.Name) {
				return nil, fmt.Errorf("备份的名字不对：%q（用 backup_status 结果里 items 的 name）", in.Name)
			}
			return s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				return e.Backup(t, action, engine.BackupOptions{Name: in.Name})
			})
		}))

	addTool(s, m, &mcp.Tool{Name: "backup_run", Title: "现在备份",
		Description: "在服务器上现在备份一次配置里的 backups（一项或全部；没打开数据备份也能用），备份文件留在服务器上，按 keep 只保留最近几份。" +
			"改数据库、改数据目录之类的操作前先备份。" +
			"和 deploy 一样：需要用户在 Ferry 设置里允许编程软件请求改动，每次在 Ferry 桌面版里确认，同意后由桌面版执行；生产服务器不允许。最多等 40 秒，没结束用 task_status 查。",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}},
		loggedReq(s, "backup_run", func(ctx context.Context, req *mcp.CallToolRequest, in backupRunIn) (any, error) {
			if in.Name != "" && !reBackupName.MatchString(in.Name) {
				return nil, fmt.Errorf("备份的名字不对：%q（用 backup_status 结果里 items 的 name）", in.Name)
			}
			_, err := s.withEngine(ctx, in.Project, in.Target, func(e *engine.Engine, t string) (any, error) {
				if err := allowedOn(e, t); err != nil {
					return nil, err
				}
				return nil, hasBackup(e, t, in.Name)
			})
			if err != nil {
				return nil, err
			}
			return s.change(ctx, req, in.targetIn, approve.Request{Action: "backup-run", Backup: in.Name, Reason: in.Reason})
		}))
}

// hasBackup 配置里这台服务器上有没有要备份的（name 为空 = 有任何一项）。
func hasBackup(e *engine.Engine, target, name string) error {
	var names []string
	for _, b := range e.File.Backups {
		if b.AppliesTo(target) {
			if b.Name == name {
				return nil
			}
			names = append(names, b.Name)
		}
	}
	switch {
	case len(names) == 0:
		return fmt.Errorf("项目配置里没有在 %s 上的 backups：要用户先在 ferry.yaml 里写上要备份什么", target)
	case name == "":
		return nil
	}
	return fmt.Errorf("%s 上没有备份 %s（有：%s）", target, name, strings.Join(names, "、"))
}
