package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/ui"
)

// 桌面版「新建连接」保存的服务器：数据目录里的 connections.yaml（kind: connections，只有 targets）。
// 密码不在文件里：记住的密码在系统凭据库，没记住的只在桌面版这次运行的内存里。

type connFile struct {
	Version int                    `yaml:"version"`
	Kind    string                 `yaml:"kind"`
	Targets map[string]*connTarget `yaml:"targets"`
}

type connTarget struct {
	Title        string `yaml:"title,omitempty" json:"title"`
	Host         string `yaml:"host" json:"host"`
	Port         int    `yaml:"port,omitempty" json:"port"`
	User         string `yaml:"user,omitempty" json:"user"`
	Auth         string `yaml:"auth,omitempty" json:"auth"`
	IdentityFile string `yaml:"identityFile,omitempty" json:"identityFile"`
	HostKey      string `yaml:"hostKey,omitempty" json:"hostKey"`
}

type connEntry struct {
	Name string `json:"name"`
	connTarget
}

func connectionsPath() string { return appdir.Path("connections.yaml") }

func readConnections() (*connFile, error) {
	cf := &connFile{Version: 1, Kind: config.KindConnections, Targets: map[string]*connTarget{}}
	data, err := os.ReadFile(connectionsPath())
	if errors.Is(err, os.ErrNotExist) {
		return cf, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, cf); err != nil {
		return nil, fmt.Errorf("%s：%w", connectionsPath(), err)
	}
	if cf.Targets == nil {
		cf.Targets = map[string]*connTarget{}
	}
	return cf, nil
}

func writeConnections(cf *connFile) error {
	data, err := yaml.Marshal(cf)
	if err != nil {
		return err
	}
	data = append([]byte("# Ferry 桌面版「新建连接」保存的服务器。密码不在这里（在系统凭据库）。\n"), data...)
	// 先按项目配置的规则校验一遍（主机、端口、登录方式）
	if _, err := config.LoadContent(data, connectionsPath()); err != nil {
		return err
	}
	return appdir.WriteFileAtomic(connectionsPath(), data, 0o600)
}

var reConnName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// connName 新连接的名字（命令、凭据、主机公钥核对里用）：由用户和主机生成，重名时加序号。
func connName(cf *connFile, t *connTarget) string {
	base := strings.Trim(reConnName.ReplaceAllString(t.User+"-"+t.Host, "-"), "-._")
	if base == "" {
		base = "server"
	}
	name := base
	for i := 2; cf.Targets[name] != nil; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

func listConnections(cf *connFile) []connEntry {
	out := []connEntry{}
	for n, t := range cf.Targets {
		e := connEntry{Name: n, connTarget: *t}
		if e.Port == 0 {
			e.Port = 22
		}
		if e.User == "" {
			e.User = "root"
		}
		if e.Auth == "" {
			e.Auth = "key"
		}
		if e.Title == "" {
			e.Title = n
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}

// connectionCmd ferry connection list | save（内容在 FERRY_CONNECTION_VALUE）| remove <名称>
func connectionCmd(c *cli, cio *cliIO, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	sub := "list"
	if len(c.pos) > 0 {
		sub = c.pos[0]
	}
	cf, err := readConnections()
	if err != nil {
		return fail(err)
	}
	switch sub {
	case "list":
		list := listConnections(cf)
		if js != nil {
			return ok0(js, map[string]any{"path": connectionsPath(), "connections": list})
		}
		for _, e := range list {
			fmt.Fprintf(cio.stdout, "%-24s %s@%s:%d  %s\n", e.Name, e.User, e.Host, e.Port, e.Title)
		}
		return 0
	case "save":
		var in connEntry
		if err := json.Unmarshal([]byte(cio.getenv("FERRY_CONNECTION_VALUE")), &in); err != nil {
			return fail(fmt.Errorf("FERRY_CONNECTION_VALUE 格式错误：%w", err))
		}
		t := in.connTarget
		t.Host, t.User, t.Title = strings.TrimSpace(t.Host), strings.TrimSpace(t.User), strings.TrimSpace(t.Title)
		if t.Host == "" {
			return fail(errors.New("请填写主机"))
		}
		if t.Auth == "password" {
			t.IdentityFile = ""
		}
		name := in.Name
		if old := cf.Targets[name]; old != nil {
			// 主机、端口、用户没变时保留已固定的主机公钥
			if old.Host == t.Host && old.Port == t.Port && old.User == t.User {
				t.HostKey = old.HostKey
			}
		} else {
			name = connName(cf, &t)
		}
		if t.Title == "" {
			t.Title = fmt.Sprintf("%s@%s", orDefault(t.User, "root"), t.Host)
		}
		cf.Targets[name] = &t
		if err := writeConnections(cf); err != nil {
			return fail(err)
		}
		if con != nil {
			con.OK("已保存 %s", name)
		}
		return ok0(js, map[string]any{"name": name, "path": connectionsPath()})
	case "remove":
		if len(c.pos) < 2 {
			return fail(errors.New("用法：ferry connection remove <名称>"))
		}
		if cf.Targets[c.pos[1]] == nil {
			return fail(fmt.Errorf("没有连接 %s", c.pos[1]))
		}
		delete(cf.Targets, c.pos[1])
		if err := writeConnections(cf); err != nil {
			return fail(err)
		}
		return ok0(js, map[string]any{"removed": c.pos[1]})
	}
	return fail(errors.New("用法：ferry connection list | save | remove <名称>"))
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
