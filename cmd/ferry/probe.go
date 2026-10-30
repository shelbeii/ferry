package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

// 新建项目向导的「测试连接」。项目还没创建，没有 ferry.yaml：先把向导里的服务器写进数据目录里一个临时的
// 「只登录」配置（和新建连接的格式一样），之后核对主机公钥、输入密码、检查服务器都用 -c 指向它。
//
//	ferry probe prepare（FERRY_PROBE_VALUE：项目目录和各台服务器）→ 临时配置的路径
//	ferry probe <目标> -c <临时配置>（FERRY_PROBE_VALUE：部署目录、运行用户、端口……）→ 检查结果
//	ferry probe <目标> -c <临时配置> --create-user <用户名> → 在服务器上创建运行用户

type probeTarget struct {
	Name         string `json:"name"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	User         string `json:"user"`
	Auth         string `json:"auth"`
	IdentityFile string `json:"identityFile"`
	// Connection 从已有的连接填入：沿用它固定的主机公钥，私钥的相对路径按连接文件所在目录换算
	Connection string `json:"connection"`
}

func probePath() string { return appdir.Path("new-project-probe.yaml") }

func probePrepareCmd(cio *cliIO, js *ui.JSON, fail func(error) int) int {
	var in struct {
		ProjectDir string        `json:"projectDir"`
		Targets    []probeTarget `json:"targets"`
	}
	if err := json.Unmarshal([]byte(cio.getenv("FERRY_PROBE_VALUE")), &in); err != nil {
		return fail(fmt.Errorf("FERRY_PROBE_VALUE 格式错误：%w", err))
	}
	if len(in.Targets) == 0 {
		return fail(errors.New("没有服务器"))
	}
	conns, err := readConnections()
	if err != nil {
		return fail(err)
	}
	cf := &connFile{Version: 1, Kind: config.KindConnections, Targets: map[string]*connTarget{}}
	// 返回给向导：固定的主机公钥（写进生成的 ferry.yaml）、换算成绝对路径的私钥
	type fixed struct {
		HostKey      string `json:"hostKey,omitempty"`
		IdentityFile string `json:"identityFile,omitempty"`
	}
	out := map[string]fixed{}
	for _, pt := range in.Targets {
		if pt.Name == "" || strings.TrimSpace(pt.Host) == "" {
			return fail(fmt.Errorf("服务器 %q 没有填地址", pt.Name))
		}
		t := &connTarget{Host: strings.TrimSpace(pt.Host), Port: pt.Port, User: strings.TrimSpace(pt.User), Auth: pt.Auth}
		base := in.ProjectDir
		if c := conns.Targets[pt.Connection]; c != nil {
			base = filepath.Dir(connectionsPath())
			if c.Host == t.Host && orPort(c.Port) == orPort(t.Port) && orDefault(c.User, "root") == orDefault(t.User, "root") {
				t.HostKey = c.HostKey
			}
		}
		if id := strings.TrimSpace(pt.IdentityFile); id != "" && t.Auth != "password" {
			if !filepath.IsAbs(id) && !strings.HasPrefix(id, "~") && base != "" {
				id = filepath.Join(base, id)
			}
			t.IdentityFile = filepath.ToSlash(id)
		}
		cf.Targets[pt.Name] = t
		out[pt.Name] = fixed{HostKey: t.HostKey, IdentityFile: t.IdentityFile}
	}
	data, err := yaml.Marshal(cf)
	if err != nil {
		return fail(err)
	}
	data = append([]byte("# Ferry 新建项目时「测试连接」用的临时配置，可以删除。\n"), data...)
	if _, err := config.LoadContent(data, probePath()); err != nil {
		return fail(err)
	}
	if err := appdir.WriteFileAtomic(probePath(), data, 0o600); err != nil {
		return fail(err)
	}
	return ok0(js, map[string]any{"path": filepath.ToSlash(probePath()), "targets": out})
}

func probeCmd(c *cli, cio *cliIO, js *ui.JSON, e *engine.Engine, fail func(error) int) int {
	if len(c.pos) == 0 {
		return fail(errors.New("用法：ferry probe <目标> -c <临时配置>"))
	}
	if e.File.Kind != config.KindConnections {
		return fail(errors.New("测试连接只用于新建项目（先用 ferry probe prepare）"))
	}
	target := c.pos[0]
	if u := c.flags["create-user"]; u != "" {
		if err := e.CreateRunUser(target, u); err != nil {
			return fail(err)
		}
		return ok0(js, map[string]any{"user": u})
	}
	var req engine.ProbeRequest
	if v := cio.getenv("FERRY_PROBE_VALUE"); v != "" {
		if err := json.Unmarshal([]byte(v), &req); err != nil {
			return fail(fmt.Errorf("FERRY_PROBE_VALUE 格式错误：%w", err))
		}
	}
	r, err := e.Probe(target, req)
	if err != nil {
		return fail(err)
	}
	if js != nil {
		return ok0(js, r)
	}
	for _, ch := range r.Checks {
		fmt.Fprintf(cio.stdout, "%-5s %s\n", ch.Level, ch.Text)
	}
	return 0
}

func orPort(p int) int {
	if p == 0 {
		return 22
	}
	return p
}
