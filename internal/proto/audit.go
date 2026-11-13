package proto

import (
	"fmt"
	"strings"
	"time"
)

// AuditEntry 一条审计记录：谁、什么时候、在哪台服务器上做了什么改动、结果怎样。
// 服务器上记在 <ferryDir>/audit.jsonl（只有 root 能读写）并写一份到 syslog（logger -t ferry-audit），
// 这台电脑上也留一份（<数据目录>/audit.jsonl）。
type AuditEntry struct {
	Time    time.Time `json:"time"`
	Actor   string    `json:"actor"`             // 操作人：「张三（zhangsan@PC）」，和部署历史一样
	Project string    `json:"project,omitempty"` // 项目
	Target  string    `json:"target,omitempty"`  // 目标（服务器）
	Host    string    `json:"host,omitempty"`    // 服务器地址（本机那份里有）
	Session string    `json:"session,omitempty"` // Ferry 的会话：同一次打开的终端的开、关是同一个
	Kind    string    `json:"kind"`              // 做了什么，见 AuditKinds
	Object  string    `json:"object,omitempty"`  // 对象：应用、文件路径、软件实例、端口、数据库……
	Detail  string    `json:"detail,omitempty"`  // 细节：SQL（截到 2 KB）、大小、时长……（不记文件内容、查询结果、终端里敲的）
	Result  string    `json:"result"`            // ok | failed
	Error   string    `json:"error,omitempty"`   // 失败的原因
}

// AuditQuery 读审计记录的条件。
type AuditQuery struct {
	Since time.Time `json:"since,omitempty"`
	Kind  string    `json:"kind,omitempty"`  // 类型；以 - 结尾时按前缀（file- = 文件的各种操作）
	Actor string    `json:"actor,omitempty"` // 操作人里含这几个字
	Limit int       `json:"limit,omitempty"` // 最多几条（默认 200，最多 2000），新的在前
}

// AuditMaxDetail Detail 最长多少字节。
const AuditMaxDetail = 2048

// AuditKinds 审计记录的类型和中文名。
var AuditKinds = map[string]string{
	"deploy": "部署", "rollback": "回滚", "control": "启停应用", "runjob": "运行定时任务", "runtime": "安装 JDK",
	"terminal-open": "打开终端", "terminal-close": "关闭终端",
	"file-save": "保存文件", "file-rename": "改名", "file-remove": "删除文件", "file-upload": "上传文件", "file-mkdir": "新建文件夹",
	"db-exec": "执行 SQL", "redis-exec": "执行 Redis 命令",
	"software-install": "安装软件", "software-remove": "卸载软件", "software-upgrade": "升级软件", "software-restore": "恢复软件",
	"software-control": "启停软件", "software-config": "改软件配置",
	"port": "放行 / 关闭端口", "firewall-on": "打开防火墙", "firewall-off": "关闭防火墙",
	"monitor-on": "打开告警", "monitor-off": "关闭告警",
	"backup-on": "打开数据备份", "backup-off": "关闭数据备份", "backup-run": "现在备份", "backup-restore": "恢复备份",
	"backup-fetch": "取回异地备份", "backup-delete": "删除备份",
}

// AuditKindText 类型的中文名（不认识的原样）。
func AuditKindText(k string) string {
	if t := AuditKinds[k]; t != "" {
		return t
	}
	return k
}

// AuditOf 改动服务器的 agent 命令记哪种审计、对象、细节；查看的命令 ok 为 false。agent 做完记到服务器上，
// 本机按同样的规则记一份到这台电脑上。
func AuditOf(cmd string, q *Request) (kind, object, detail string, ok bool) {
	apps := func() string {
		var ns []string
		for _, s := range q.Apps {
			ns = append(ns, s.Name)
		}
		return strings.Join(ns, "、")
	}
	switch cmd {
	case "deploy":
		var ids []string
		for _, r := range q.Releases {
			ids = append(ids, r.App+" "+r.ID)
		}
		return "deploy", apps(), strings.Join(ids, "、"), true
	case "rollback":
		d := "上一个版本"
		if q.To != "" {
			d = q.To
		}
		return "rollback", apps(), "回滚到 " + d, true
	case "control":
		return "control", apps(), q.Action, true
	case "runjob":
		return "runjob", apps(), "", true
	case "runtime":
		if q.Runtime != nil {
			return "runtime", q.Runtime.ID, "", true
		}
		return "runtime", "", "", true
	case "swinstall", "swupgrade":
		k := map[string]string{"swinstall": "software-install", "swupgrade": "software-upgrade"}[cmd]
		if p := q.Install; p != nil {
			return k, p.Name, strings.TrimSpace(p.Title + " " + p.Version), true
		}
		return k, "", "", true
	case "swcontrol":
		return "software-control", q.Instance, q.Action, true
	case "swunit":
		if q.Action == "logs" {
			return "", "", "", false
		}
		o := q.SwUnit
		if q.SwContainer != "" {
			o = "容器 " + q.SwContainer
		}
		return "software-control", o, q.Action, true
	case "swremove":
		d := "保留数据"
		if q.Purge {
			d = "连数据一起删除"
		}
		return "software-remove", q.Instance, d, true
	case "swrestore":
		return "software-restore", q.Instance, "", true
	case "swconfig":
		c := q.SoftwareConfig
		if c == nil || (c.Operation != "save" && c.Operation != "restore") {
			return "", "", "", false
		}
		o := c.Instance
		if o == "" {
			o = c.Unit
			if o == "" {
				o = c.Container
			}
		}
		return "software-config", o, c.Operation + " " + c.FilePath, true
	case "portset":
		pr := q.PortProto
		if pr == "" {
			pr = "tcp"
		}
		return "port", fmt.Sprintf("%d/%s", q.Port, pr), q.Action, true
	case "fwenable":
		return "firewall-on", "", "放行 " + strings.Join(q.Allow, " "), true
	case "fwdisable":
		return "firewall-off", "", "", true
	case "monitor":
		if m := q.Monitor; m != nil && (m.Action == "on" || m.Action == "off") {
			return "monitor-" + m.Action, m.Project + "/" + m.Target, "", true
		}
	case "backup":
		if b := q.Backup; b != nil {
			switch b.Action {
			case "on", "off", "run", "restore", "fetch", "delete":
				return "backup-" + b.Action, b.Name, b.File, true
			}
		}
	}
	return "", "", "", false
}
