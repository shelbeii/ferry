package engine

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/captain/ferry/internal/config"
)

// 换一台服务器部署时，最常见的问题是配置里的服务器路径（JDK、脚本、密钥文件……）在新服务器上不一样。
// 预检报的路径来自哪个变量、该在哪里给这台服务器单独设置，在这里补上说明。

// pathVar 一个值是服务器绝对路径的变量。
type pathVar struct {
	name, value string
	where       string // app / target（ferry.yaml 里这台服务器的）/ mine（本机设置里这台服务器的）/ project / local（本机设置里所有服务器共用的）
}

// pathVars 应用在目标上用到的、值是绝对路径的变量（应用、服务器、项目里写的 vars，本机设置改过的也算）。
func pathVars(f *config.File, t *config.Target, a *config.App) []pathVar {
	sc := remoteScope(f, t, a)
	seen := map[string]bool{}
	var out []pathVar
	for _, l := range []struct {
		vars  map[string]string
		where string
	}{{a.Vars, "app"}, {t.Vars, "target"}, {f.Vars, "project"}} {
		names := make([]string, 0, len(l.vars))
		for k := range l.vars {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if seen[k] {
				continue
			}
			seen[k] = true
			v, err := sc.Expand("${" + k + "}")
			if err != nil || !strings.HasPrefix(v, "/") || len(v) < 2 {
				continue
			}
			where := l.where
			if where == "project" && slices.Contains(f.LocalVars, k) {
				where = "local"
			}
			if where == "target" && slices.Contains(t.LocalVars, k) {
				where = "mine"
			}
			out = append(out, pathVar{name: k, value: strings.TrimRight(v, "/"), where: where})
		}
	}
	return out
}

var reAbsPath = regexp.MustCompile(`/[^\s，。、；：（）()"'“”]+`)

// varHint 说明 msg 里第一个来自变量的路径：哪个变量、在哪里给这台服务器单独设置。找不到时返回空。
func varHint(msg string, vars []pathVar, t *config.Target, a *config.App) string {
	for _, p := range reAbsPath.FindAllString(msg, -1) {
		var best *pathVar
		for i := range vars {
			v := &vars[i]
			if (p == v.value || strings.HasPrefix(p, v.value+"/")) && (best == nil || len(v.value) > len(best.value)) {
				best = v
			}
		}
		if best == nil {
			continue
		}
		set := fmt.Sprintf("在顶部选中 %s，打开「本机设置」，在变量里把 %s 改成这台服务器上的路径，只对这台生效", t.Name, best.name)
		switch best.where {
		case "app":
			return fmt.Sprintf("这个路径来自应用 %s 的变量 ${%s}（%s）。只是这台服务器不一样的话，在 ferry.yaml 里改成引用一个项目变量，再%s", a.Name, best.name, best.value, set)
		case "mine":
			return fmt.Sprintf("这个路径来自本机设置里 %s 的变量 ${%s}（%s）：在顶部选中 %s，打开「本机设置」改它", t.Name, best.name, best.value, t.Name)
		case "target":
			return fmt.Sprintf("这个路径来自 ferry.yaml 里服务器 %s 的变量 ${%s}（%s）：改 ferry.yaml，或%s", t.Name, best.name, best.value, set)
		case "local":
			return fmt.Sprintf("这个路径来自变量 ${%s}（%s），它的值在本机设置的「所有服务器共用」里，对这个项目的每台服务器都生效。"+
				"只是这台服务器不一样的话，%s（对话框里有「改成只对 %s」）", best.name, best.value, set, t.Name)
		}
		return fmt.Sprintf("这个路径来自变量 ${%s}（%s，ferry.yaml 里通用的值）。这台服务器上的位置不一样的话，%s，其他服务器不受影响", best.name, best.value, set)
	}
	return ""
}

// withVarHints 预检的问题后面补上变量的说明。问题的开头是应用名（agent 的写法），据此找应用。
func withVarHints(f *config.File, t *config.Target, items []*item, problems []string) []string {
	vars := map[string][]pathVar{}
	out := make([]string, len(problems))
	for i, pr := range problems {
		out[i] = pr
		for _, it := range items {
			if !strings.HasPrefix(pr, it.app.Name+" ") {
				continue
			}
			if _, ok := vars[it.app.Name]; !ok {
				vars[it.app.Name] = pathVars(f, t, it.app)
			}
			if h := varHint(pr, vars[it.app.Name], t, it.app); h != "" {
				out[i] = pr + "\n" + h
			}
			break
		}
	}
	return out
}

// localPathVarsNote 本机设置「所有服务器共用」里改了服务器路径变量、项目又有好几台服务器时的提醒：
// 给一台新服务器改路径时容易把别的服务器也改了（应该改在那台服务器自己的本机设置里）。没有要提醒的时返回空。
func localPathVarsNote(f *config.File, deploying []string) string {
	if len(f.Targets) < 2 || len(f.LocalVars) == 0 {
		return ""
	}
	var names, others []string
	for _, k := range f.LocalVars {
		if strings.HasPrefix(f.Vars[k], "/") {
			names = append(names, k)
		}
	}
	for tn, t := range f.Targets {
		if slices.Contains(deploying, tn) {
			continue
		}
		for _, k := range names {
			if _, own := t.Vars[k]; !own { // 服务器自己有这个变量的（ferry.yaml 或本机设置里），不受影响
				others = append(others, tn)
				break
			}
		}
	}
	if len(others) == 0 {
		return ""
	}
	sort.Strings(others)
	return fmt.Sprintf("本机设置的「所有服务器共用」里改了变量 %s（服务器上的路径），这对 %s 也生效。只是某台服务器的路径不一样的话，"+
		"打开本机设置点「改成只对 <这台服务器>」，改到那台服务器自己的变量里",
		strings.Join(names, "、"), strings.Join(others, "、"))
}
