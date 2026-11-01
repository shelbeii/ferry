package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/captain/ferry/internal/ai"
	"github.com/captain/ferry/internal/config"
)

// 服务器助手（和编程软件经 MCP）用的：升级、恢复 Ferry 装的软件，给 Ferry 装的数据库设置数据备份。都要用户确认。

// reSoftwareName Ferry 装的软件的实例名。
var reSoftwareName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

type softwareLifeArgs struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Major    bool   `json:"major"`
	Database string `json:"database"`
}

func parseLifeArgs(in json.RawMessage) (softwareLifeArgs, error) {
	var a softwareLifeArgs
	if err := json.Unmarshal(in, &a); err != nil {
		return a, fmt.Errorf("参数格式不对：%v", err)
	}
	if !reSoftwareName.MatchString(a.Name) {
		return a, fmt.Errorf("要给出 name：Ferry 装的软件的实例名（list_software 里的 instance）")
	}
	return a, nil
}

func (e *Engine) aiSoftwareLife(t *config.Target) []ai.Tool {
	return []ai.Tool{{
		Name: "software_upgrade",
		Description: "把 Ferry 用安装包装的软件（list_software 里 upgradable 为 true 的）升级到新版本：换掉程序，配置、数据带到新版本，" +
			"新版本放不上、起不来时自动换回旧版本。version 不填 = 最新；没有下载地址的软件先让用户在 Ferry 里导入新版本的安装包，用 package 指定。" +
			"跨大版本升级（如 2.x → 3.x）可能不兼容：先告诉用户风险、建议先备份，用户同意后再填 major: true。数据库升级前先 backup_run 备份。从软件源装的软件不能在这里升级。",
		Schema: schema(map[string]any{
			"name":    str("Ferry 装的软件的实例名（list_software 里的 instance）"),
			"version": str("要升到的版本；不填 = 最新"),
			"package": str("用本机安装包库里的这个文件（software_templates 的 library 里的 file）"),
			"major":   map[string]any{"type": "boolean", "description": "跨大版本升级：只有告诉用户风险、用户同意后才填 true"},
		}, "name"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			a, err := parseLifeArgs(in)
			if err != nil {
				return nil // 参数不对：执行时报错交回模型
			}
			to := orDefault(a.Version, "最新版本")
			d := fmt.Sprintf("把 %s 升级到 %s：停下来换掉程序，配置、数据带到新版本；新版本起不来时换回现在的版本\n", a.Name, to)
			if a.Package != "" {
				d += "安装包：" + a.Package + "\n"
			}
			ap := &ai.Approval{Kind: "软件：升级", Title: "升级软件 " + a.Name + " 到 " + to, Detail: d, AsRoot: true}
			if a.Major {
				ap.Danger = "跨大版本升级：配置、数据可能不兼容"
			}
			return ap
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			a, err := parseLifeArgs(in)
			if err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) {
				return e.SoftwareUpgrade(t.Name, a.Name, UpgradeOptions{Version: a.Version, PackageFile: a.Package, Major: a.Major})
			})
			var me *MajorUpgradeError
			if errors.As(err, &me) {
				return "", fmt.Errorf("%s 从 %s 升到 %s 是跨大版本升级：配置、数据可能不兼容。先把风险告诉用户、建议先备份，用户同意后再用 major: true 升级", me.Name, me.From, me.To)
			}
			if err != nil {
				return "", err
			}
			return toJSON(r) + "\n已升级。", nil
		},
	}, {
		Name:        "software_restore",
		Description: "恢复卸载时保留了数据的 Ferry 软件（list_software 结果里 kept 列出的）：照原样建回系统服务、启动，密码还是原来的。",
		Schema:      schema(map[string]any{"name": str("卸载时保留了数据的软件的实例名（list_software 结果里 kept 的 name）")}, "name"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			a, err := parseLifeArgs(in)
			if err != nil {
				return nil
			}
			return &ai.Approval{Kind: "软件：恢复", Title: "恢复软件 " + a.Name, Detail: "照原样建回系统服务 ferry-" + a.Name + " 并启动（数据是卸载时保留下来的）\n", AsRoot: true}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			a, err := parseLifeArgs(in)
			if err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) { return e.SoftwareRestore(t.Name, a.Name) })
			if err != nil {
				return "", err
			}
			return toJSON(r) + "\n已恢复并启动。", nil
		},
	}, {
		Name: "software_backup",
		Description: "给 Ferry 装的 MySQL、MariaDB、PostgreSQL 设置数据备份：在服务器上写好只有 root 能读的备份密码文件，在项目的 ferry.yaml 的 backups 里加上一项。" +
			"加好后要用户在 Ferry 的「数据备份」里打开才按时备份（你不能替用户打开）；现在就要备份一份用 backup_run。",
		Schema: schema(map[string]any{
			"name":     str("Ferry 装的数据库的实例名（list_software 里的 instance）"),
			"database": str("备份哪个库；不填 = 全部库"),
		}, "name"),
		Approve: func(ctx context.Context, in json.RawMessage) *ai.Approval {
			a, err := parseLifeArgs(in)
			if err != nil {
				return nil
			}
			db := orDefault(a.Database, "全部库")
			d := fmt.Sprintf("在服务器上写只有 root 能读的备份密码文件（%s 的安装目录下 ferry-backup.env）\n在 %s 的 backups 里加上一项：备份 %s 的 %s\n",
				a.Name, orDefault(e.File.Path, "ferry.yaml"), a.Name, db)
			return &ai.Approval{Kind: "软件：设置备份", Title: "给 " + a.Name + " 设置数据备份", Detail: d, AsRoot: true}
		},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			a, err := parseLifeArgs(in)
			if err != nil {
				return "", err
			}
			r, err := withProgress(e, ctx, func() (any, error) { return e.SoftwareBackup(t.Name, a.Name, a.Database) })
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("已在配置的 backups 里加上 %s。要用户在 Ferry 的「数据备份」里打开后才按时备份；现在就要备份一份可以用 backup_run（backup: %s）。", r, r), nil
		},
	}}
}
