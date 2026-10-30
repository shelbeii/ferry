package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/ui"
)

var localUsage = errors.New(`用法：
  ferry local [check]                       本机设置和这台电脑缺什么（check 只列缺的）
  ferry local set <目标> --key <私钥>         这台电脑上用这个私钥登录
  ferry local set <目标> --password          这台电脑上用密码登录
  ferry local set <目标> --default-key       这台电脑上用 ~/.ssh 下的默认私钥、ssh-agent 登录
  ferry local unset <目标>                    去掉目标的本机设置，按 ferry.yaml
  ferry local var <变量> [<值>] [--target <目标>]
                                            变量在这台电脑上的值：给了 --target 只对那台服务器（JDK、脚本的位置），
                                            不给是所有服务器共用（源码目录）；不给值 = 去掉，按 ferry.yaml
  ferry local save                          从 FERRY_LOCAL_VALUE 读整份本机设置（JSON）保存
本机设置只属于这台电脑，存在数据目录里，不写进 ferry.yaml。ferry.yaml 写通用的配置，
各台服务器自己的登录方式、路径写在本机设置里那台服务器下面。`)

// localTarget ferry local 里一个目标的情况。
type localTarget struct {
	Name         string              `json:"name"`
	Auth         string              `json:"auth"`         // 这台电脑上实际的登录方式
	IdentityFile string              `json:"identityFile"` // 这台电脑上实际用的私钥（原样）
	KeyPath      string              `json:"keyPath"`      // 私钥在这台电脑上的位置
	KeyMissing   bool                `json:"keyMissing"`
	SharedAuth   string              `json:"sharedAuth"` // ferry.yaml 里的
	SharedKey    string              `json:"sharedIdentityFile"`
	Local        *config.LocalTarget `json:"local"` // 本机设置的登录方式；nil = 按 ferry.yaml
	// Vars 这台服务器的变量：shared 是不算这台服务器的本机设置时的值，local 是本机设置里只对它的值
	Vars []localVar `json:"vars"`
}

type localVar struct {
	Name   string  `json:"name"`
	Shared string  `json:"shared"`
	Local  *string `json:"local"`
	// LocalUse 用在这台电脑上（源码目录等），所有服务器共用（只在项目变量里）
	LocalUse bool `json:"localUse,omitempty"`
}

type localReport struct {
	File     string                  `json:"file"`
	Targets  []localTarget           `json:"targets"`
	Vars     []localVar              `json:"vars"`
	Problems []config.MachineProblem `json:"problems"`
}

// localCmd ferry local。f 是套用了本机设置的配置。
func localCmd(c *cli, cio *cliIO, e *engine.Engine, js *ui.JSON, con *ui.Console, fail func(error) int) int {
	f := e.File
	sub := ""
	if len(c.pos) > 0 {
		sub = c.pos[0]
	}
	changed := func() int {
		cio.invalidate()
		if js != nil {
			return ok0(js, map[string]bool{"ok": true})
		}
		con.OK("已保存到这台电脑的本机设置（ferry.yaml 没改）")
		return 0
	}
	switch sub {
	case "", "check":
		rep, err := localShow(f)
		if err != nil {
			return fail(err)
		}
		if js != nil {
			return ok0(js, rep)
		}
		if sub == "" {
			printLocal(con, rep)
		}
		if len(rep.Problems) == 0 {
			con.OK("这台电脑上不缺什么")
			return 0
		}
		for _, p := range rep.Problems {
			con.Warn("%s", p.Msg)
		}
		if sub == "check" {
			return 1 // 缺东西：脚本里好判断
		}
		return 0

	case "set":
		if len(c.pos) < 2 {
			return fail(localUsage)
		}
		var lt *config.LocalTarget
		switch {
		case c.flags["key"] != "":
			p, err := filepath.Abs(f.LocalPath(c.flags["key"]))
			if err != nil {
				return fail(err)
			}
			lt = &config.LocalTarget{Auth: "key", IdentityFile: p}
		case c.has("password"):
			lt = &config.LocalTarget{Auth: "password"}
		case c.has("default-key"):
			lt = &config.LocalTarget{Auth: "key"}
		default:
			return fail(localUsage)
		}
		if err := e.SetLocalTarget(c.pos[1], lt); err != nil {
			return fail(err)
		}
		return changed()

	case "unset":
		if len(c.pos) < 2 {
			return fail(localUsage)
		}
		if err := e.SetLocalTarget(c.pos[1], nil); err != nil {
			return fail(err)
		}
		return changed()

	case "var":
		if len(c.pos) < 2 {
			return fail(localUsage)
		}
		l, err := config.ReadLocal(f.Project, f.Path)
		if err != nil {
			return fail(err)
		}
		if l.Vars == nil {
			l.Vars = map[string]string{}
		}
		vars := l.Vars
		if tn := c.flags["target"]; tn != "" {
			if l.Targets == nil {
				l.Targets = map[string]*config.LocalTarget{}
			}
			if l.Targets[tn] == nil {
				l.Targets[tn] = &config.LocalTarget{}
			}
			if l.Targets[tn].Vars == nil {
				l.Targets[tn].Vars = map[string]string{}
			}
			vars = l.Targets[tn].Vars
		}
		if len(c.pos) >= 3 {
			vars[c.pos[1]] = c.pos[2]
		} else {
			delete(vars, c.pos[1])
		}
		if err := saveLocal(f, l); err != nil {
			return fail(err)
		}
		return changed()

	case "save":
		var l config.Local
		if err := json.Unmarshal([]byte(cio.getenv("FERRY_LOCAL_VALUE")), &l); err != nil {
			return fail(fmt.Errorf("本机设置不是有效的 JSON：%w", err))
		}
		if err := saveLocal(f, &l); err != nil {
			return fail(err)
		}
		return changed()
	}
	return fail(localUsage)
}

// saveLocal 按 ferry.yaml 本身（不套用本机设置）检查后保存。
func saveLocal(f *config.File, l *config.Local) error {
	shared, err := config.Load(f.Path)
	if err != nil {
		return err
	}
	for k, t := range l.Targets {
		if t != nil && t.IdentityFile != "" {
			if t.IdentityFile, err = filepath.Abs(shared.LocalPath(t.IdentityFile)); err != nil {
				return err
			}
			l.Targets[k] = t
		}
	}
	if err := config.CheckLocal(shared, l); err != nil {
		return err
	}
	return config.WriteLocal(shared, l)
}

func localShow(f *config.File) (*localReport, error) {
	shared, err := config.Load(f.Path)
	if err != nil {
		return nil, err
	}
	l, err := config.ReadLocal(f.Project, f.Path)
	if err != nil {
		return nil, err
	}
	// 列表都给 []（ferry.yaml 里可能没有项目变量），界面直接遍历
	rep := &localReport{File: config.LocalFile(f.Project, f.Path), Problems: f.MachineProblems(), Targets: []localTarget{}, Vars: []localVar{}}
	if rep.Problems == nil {
		rep.Problems = []config.MachineProblem{}
	}
	// 不算各台服务器本机设置时的项目变量：ferry.yaml 的值，套上本机设置里所有服务器共用的
	common := map[string]string{}
	for k, v := range shared.Vars {
		common[k] = v
		if lv, ok := l.Vars[k]; ok {
			common[k] = lv
		}
	}
	for _, n := range f.TargetNames() {
		t, st := f.Targets[n], shared.Targets[n]
		lt := localTarget{Name: n, Auth: t.Auth, IdentityFile: t.IdentityFile, SharedAuth: st.Auth, SharedKey: st.IdentityFile, Local: t.Local, Vars: []localVar{}}
		var mine map[string]string
		if l.Targets[n] != nil {
			mine = l.Targets[n].Vars
		}
		for _, k := range varNames(common, st.Vars) {
			v := localVar{Name: k, Shared: common[k]}
			if sv, ok := st.Vars[k]; ok {
				v.Shared = sv
			}
			if lv, ok := mine[k]; ok {
				v.Local = &lv
			}
			lt.Vars = append(lt.Vars, v)
		}
		if t.IdentityFile != "" {
			lt.KeyPath = f.LocalPath(t.IdentityFile)
			lt.KeyMissing = f.KeyMissing(t) != ""
		}
		rep.Targets = append(rep.Targets, lt)
	}
	use := shared.LocalUseVars()
	for _, k := range varNames(shared.Vars) {
		v := localVar{Name: k, Shared: shared.Vars[k], LocalUse: use[k]}
		if lv, ok := l.Vars[k]; ok {
			v.Local = &lv
		}
		rep.Vars = append(rep.Vars, v)
	}
	return rep, nil
}

// varNames 几组变量的名字（去重、排序）。
func varNames(maps ...map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func printLocal(con *ui.Console, rep *localReport) {
	fmt.Println("本机设置：" + rep.File)
	for _, t := range rep.Targets {
		how, note := "密码登录", "按 ferry.yaml"
		if t.Auth == "key" {
			how = "默认私钥 / ssh-agent"
			if t.IdentityFile != "" {
				how = "私钥 " + t.KeyPath
			}
		}
		if t.Local != nil {
			note = "本机设置"
		}
		if t.KeyMissing {
			note = "这台电脑上没有，" + note
		}
		fmt.Printf("  %-16s %s（%s）\n", t.Name, how, note)
		for _, v := range t.Vars {
			if v.Local != nil {
				fmt.Printf("    ${%s} = %s（本机设置，只对 %s；不设置时是 %s）\n", v.Name, *v.Local, t.Name, v.Shared)
			}
		}
	}
	for _, v := range rep.Vars {
		if v.Local != nil {
			fmt.Printf("  ${%s} = %s（本机设置，所有服务器共用；ferry.yaml 里是 %s）\n", v.Name, *v.Local, v.Shared)
		}
	}
}
