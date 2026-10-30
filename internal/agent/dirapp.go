package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// python、node、dotnet：上传的是目录，解压进版本目录；依赖在服务器上装（python 的 venv、node 的 node_modules），
// 放在 apps/<app>/deps/<种类>-<依赖文件的哈希>/，版本目录里的 .venv、node_modules 链接过去：依赖文件没变的部署直接复用。

const (
	dirDeps = "deps"
	// servicePath systemd 给服务的 PATH（ExecStart 里的 node、python3 在这里找）
	servicePath   = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	depsTimeout   = 30 * time.Minute
	depsMaxLines  = 80
	depsReadyMark = ".ferry-ok"
)

func (a *Agent) depsDir(app string) string { return filepath.Join(a.appDir(app), dirDeps) }

// placeDirApp 解压上传的目录，标上可执行，装依赖。
func (a *Agent) placeDirApp(s *proto.AppSpec, src, release string, gid int) error {
	if err := extractTarGz(src, release); err != nil {
		return fmt.Errorf("解压失败：%v", err)
	}
	if s.Deps != nil {
		if err := a.installDeps(s, release, gid); err != nil {
			return err
		}
	}
	for _, x := range s.Executables {
		p := filepath.Join(release, filepath.FromSlash(x))
		if !within(release, p) {
			return fmt.Errorf("可执行文件不在版本目录里：%s", x)
		}
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("上传的目录里没有启动程序 %s", x)
		}
		os.Chmod(p, 0o755)
	}
	return nil
}

// installDeps 装依赖（或复用上次装好的），把版本目录里的 .venv / node_modules 链接过去。
func (a *Agent) installDeps(s *proto.AppSpec, release string, gid int) error {
	d := s.Deps
	var key []byte
	var link, cmd, label string
	env := append(os.Environ(), "PATH="+servicePath, "HOME=/root")
	switch d.Kind {
	case proto.TypePython:
		req, err := os.ReadFile(filepath.Join(release, "requirements.txt"))
		if err != nil {
			a.info(proto.EvInfo, s.Name, "没有 requirements.txt：不装依赖")
			return nil
		}
		if exists(filepath.Join(release, ".venv")) {
			return nil
		}
		py := d.Python
		if py == "" {
			py = "python3"
		}
		ver, err := a.Sys.Shell("PATH="+servicePath+" "+shellQuote(py)+" -c 'import sys; print(sys.version)'", 30*time.Second)
		if err != nil {
			return fmt.Errorf("服务器上没有 %s（建 venv 用）：装上 Python 3，或在 deps.python 里写用哪个", py)
		}
		key = append(append(req, '\n'), []byte(py+"\n"+ver+d.Registry)...)
		link, label = ".venv", "Python 依赖"
		pip := "{venv}/bin/pip install --disable-pip-version-check --no-input"
		if d.Registry != "" {
			pip += " --index-url " + shellQuote(d.Registry)
		}
		cmd = shellQuote(py) + " -m venv {venv} && " + pip + " -r requirements.txt"
		env = append(env, "PIP_ROOT_USER_ACTION=ignore")
	case proto.TypeNode:
		pj, err := os.ReadFile(filepath.Join(release, "package.json"))
		if err != nil || exists(filepath.Join(release, "node_modules")) {
			return nil // 依赖已经在上传的目录里（如 Next.js standalone）
		}
		var pkg map[string]json.RawMessage
		if json.Unmarshal(pj, &pkg) != nil {
			return fmt.Errorf("package.json 不是有效的 JSON")
		}
		if deps := strings.TrimSpace(string(pkg["dependencies"])); deps == "" || deps == "{}" || deps == "null" {
			return nil
		}
		for _, k := range []string{"dependencies", "optionalDependencies", "overrides", "resolutions", "pnpm"} {
			key = append(append(key, k...), pkg[k]...)
		}
		mgr := "npm install --no-audit --no-fund --no-package-lock"
		for _, l := range []struct{ file, cmd string }{
			{"package-lock.json", "npm ci --no-audit --no-fund"},
			{"npm-shrinkwrap.json", "npm ci --no-audit --no-fund"},
			{"pnpm-lock.yaml", "pnpm install --frozen-lockfile --prod"},
			{"yarn.lock", "yarn install --frozen-lockfile --production --non-interactive"},
		} {
			if b, err := os.ReadFile(filepath.Join(release, l.file)); err == nil {
				key, mgr = append(key, b...), l.cmd
				break
			}
		}
		tool := strings.Fields(mgr)[0]
		ver, err := a.Sys.Shell("PATH="+servicePath+" node -v && PATH="+servicePath+" "+tool+" -v", 30*time.Second)
		if err != nil {
			return fmt.Errorf("服务器上没有 node 或 %s（装依赖用）：装上 Node.js（%s）", tool, strings.TrimSpace(ver))
		}
		key = append(key, []byte(ver+d.Registry)...)
		link, label, cmd = "node_modules", "Node 依赖", mgr
		env = append(env, "NODE_ENV=production", "npm_config_update_notifier=false")
		if d.Registry != "" {
			env = append(env, "npm_config_registry="+d.Registry)
		}
	default:
		return nil
	}
	sum := sha256.Sum256(key)
	dir := filepath.Join(a.depsDir(s.Name), d.Kind+"-"+hex.EncodeToString(sum[:8]))
	target := dir // 链接指向的
	if d.Kind == proto.TypeNode {
		target = filepath.Join(dir, "node_modules")
	}
	if !exists(filepath.Join(dir, depsReadyMark)) {
		os.RemoveAll(dir)
		if err := os.MkdirAll(a.depsDir(s.Name), 0o750); err != nil {
			return err
		}
		a.Sys.Lchown(a.depsDir(s.Name), 0, gid)
		a.info(proto.EvInfo, s.Name, "依赖有变化，在服务器上装%s…", label)
		run := strings.ReplaceAll(cmd, "{venv}", shellQuote(dir))
		var tail []string
		n := 0
		start := a.Sys.Now()
		err := a.Sys.RunHook(HookExec{Cmd: run, Dir: release, Env: env, Timeout: depsTimeout}, func(l string) {
			if n++; n <= depsMaxLines {
				a.info(proto.EvInfo, s.Name, "  %s", l)
			}
			if tail = append(tail, l); len(tail) > 15 {
				tail = tail[1:]
			}
		})
		if err != nil {
			os.RemoveAll(dir)
			os.RemoveAll(filepath.Join(release, "node_modules"))
			msg := fmt.Sprintf("装%s失败（%v）", label, err)
			if n > depsMaxLines {
				msg += "：\n" + strings.Join(tail, "\n")
			}
			return fmt.Errorf("%s。看看 registry（镜像源）能不能连上，或在 deps.registry 里换一个", msg)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if d.Kind == proto.TypeNode {
			if err := os.Rename(filepath.Join(release, "node_modules"), target); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(dir, depsReadyMark), nil, 0o644); err != nil {
			return err
		}
		a.info(proto.EvInfo, s.Name, "已装好%s（%.0f 秒）", label, a.Sys.Now().Sub(start).Seconds())
	} else {
		a.info(proto.EvInfo, s.Name, "%s没有变化：用上次装好的", label)
	}
	if err := os.Symlink(target, filepath.Join(release, link)); err != nil {
		return err
	}
	return nil
}

// pruneDeps 去掉没有版本在用的依赖目录。
func (a *Agent) pruneDeps(app string) {
	ents, err := os.ReadDir(a.depsDir(app))
	if err != nil {
		return
	}
	used := map[string]bool{}
	for _, id := range a.listReleases(app) {
		for _, l := range []string{".venv", "node_modules"} {
			if t, err := os.Readlink(filepath.Join(a.releaseDir(app, id), l)); err == nil {
				used[filepath.Clean(t)] = true
				used[filepath.Dir(filepath.Clean(t))] = true
			}
		}
	}
	for _, e := range ents {
		p := filepath.Join(a.depsDir(app), e.Name())
		if !used[p] {
			os.RemoveAll(p)
		}
	}
}

// execCommand 单元文件 ExecStart 里用 /usr/bin/env 找的命令（node、python3、dotnet）；绝对路径的为空。
func execCommand(unitText string) string {
	for _, l := range strings.Split(unitText, "\n") {
		if v, ok := strings.CutPrefix(l, "ExecStart=/usr/bin/env "); ok {
			if f := strings.Fields(v); len(f) > 0 {
				return strings.Trim(f[0], `"`)
			}
		}
	}
	return ""
}

// checkDirRuntimes 预检：python、node、dotnet 启动、装依赖要用的命令服务器上有没有。
func (a *Agent) checkDirRuntimes(p *proto.Preflight) {
	has := func(c string) bool {
		_, err := a.Sys.Shell("PATH="+servicePath+" command -v "+shellQuote(c), 10*time.Second)
		return err == nil
	}
	hint := map[string]string{"node": "Node.js", "npm": "Node.js（带 npm）", "python3": "Python 3", "dotnet": ".NET 运行时（或用自带运行时的方式发布：dotnet publish --self-contained）"}
	for _, s := range a.req.Apps {
		if !proto.DirService(s.Type) {
			continue
		}
		if c := execCommand(s.UnitText); c != "" && !has(c) {
			p.Problems = append(p.Problems, fmt.Sprintf("%s 用 %s 启动，服务器上没有（systemd 的 PATH 里找不到）：装上 %s，或在 service.exec 里写完整路径", s.Name, c, or(hint[c], c)))
		}
		if s.Deps == nil {
			continue
		}
		switch s.Deps.Kind {
		case proto.TypePython:
			py := or(s.Deps.Python, "python3")
			if _, err := a.Sys.Shell("PATH="+servicePath+" "+shellQuote(py)+" -c 'import venv, ensurepip'", 20*time.Second); err != nil {
				p.Problems = append(p.Problems, fmt.Sprintf("%s 要在服务器上建 venv 装依赖，但 %s 不能用（没装 Python 3，或 Debian / Ubuntu 上缺 python3-venv）", s.Name, py))
			}
		case proto.TypeNode:
			if !has("npm") {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s 要在服务器上装依赖，服务器上没有 npm（用 yarn、pnpm 的话装上它们）", s.Name))
			}
		}
	}
}
