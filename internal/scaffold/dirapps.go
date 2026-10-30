package scaffold

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Python、Node 服务端、.NET：上传目录，由 service.exec 启动。

// ---------------------------------------------------------------- Python

var (
	reFastAPI  = regexp.MustCompile(`(?m)^(\w+)\s*(?::\s*\w+\s*)?=\s*FastAPI\(`)
	reFlask    = regexp.MustCompile(`(?m)^(\w+)\s*=\s*Flask\(`)
	reDjangoMS = regexp.MustCompile(`DJANGO_SETTINGS_MODULE['"]\s*,\s*['"]([\w.]+)\.settings['"]`)
)

func isPythonProject(dir string) bool {
	for _, n := range []string{"requirements.txt", "pyproject.toml", "manage.py"} {
		if exists(filepath.Join(dir, n)) {
			return true
		}
	}
	return false
}

// pyModule 文件（相对项目目录）→ 模块名：app/main.py → app.main，app/__init__.py → app。
func pyModule(rel string) string {
	m := strings.TrimSuffix(filepath.ToSlash(rel), ".py")
	m = strings.TrimSuffix(m, "/__init__")
	m = strings.TrimPrefix(m, "src/")
	return strings.ReplaceAll(m, "/", ".")
}

func detectPython(base, rel string) (Suggestion, bool) {
	dir := filepath.Join(base, rel)
	reqs := strings.ToLower(readText(filepath.Join(dir, "requirements.txt")) + readText(filepath.Join(dir, "pyproject.toml")))
	need := func(pkg string) bool {
		return regexp.MustCompile(`(?m)(^|["'\s])` + regexp.QuoteMeta(pkg) + `\b`).MatchString(reqs)
	}
	venv := "${running}/.venv/bin/"
	s := Suggestion{Name: appName(filepath.Base(dir)), Type: "python", Workdir: rel, Artifact: join(rel, "."), Port: 8000}
	if rel == "." {
		s.Name = appName(filepath.Base(base))
	}
	found := false
	// Django
	if t := readText(filepath.Join(dir, "manage.py")); t != "" {
		if m := reDjangoMS.FindStringSubmatch(t); m != nil {
			s.Kind = "Python（Django）"
			s.Exec = []string{venv + "gunicorn", m[1] + ".wsgi:application", "--bind", "127.0.0.1:8000", "--workers", "2"}
			if !need("gunicorn") {
				s.Notes = append(s.Notes, "用 gunicorn 运行：requirements.txt 里加上 gunicorn")
			}
			s.Notes = append(s.Notes, "静态文件（collectstatic）、数据库迁移（migrate）可以写成部署钩子（hooks.beforeSwitch）")
			found = true
		}
	}
	// FastAPI / Flask：在常见的入口文件里找 app = FastAPI() / Flask(__name__)
	if !found {
		for _, f := range []string{"main.py", "app.py", "app/main.py", "app/__init__.py", "src/main.py", "api/main.py", "server.py", "wsgi.py", "run.py"} {
			t := readText(filepath.Join(dir, filepath.FromSlash(f)))
			if m := reFastAPI.FindStringSubmatch(t); m != nil {
				s.Kind = "Python（FastAPI）"
				s.Exec = []string{venv + "uvicorn", pyModule(f) + ":" + m[1], "--host", "127.0.0.1", "--port", "8000"}
				if !need("uvicorn") {
					s.Notes = append(s.Notes, "用 uvicorn 运行：requirements.txt 里加上 uvicorn")
				}
				found = true
				break
			}
			if m := reFlask.FindStringSubmatch(t); m != nil {
				s.Kind = "Python（Flask）"
				s.Exec = []string{venv + "gunicorn", "--bind", "127.0.0.1:8000", "--workers", "2", pyModule(f) + ":" + m[1]}
				if !need("gunicorn") {
					s.Notes = append(s.Notes, "用 gunicorn 运行：requirements.txt 里加上 gunicorn")
				}
				found = true
				break
			}
		}
	}
	if !found {
		main := ""
		for _, f := range []string{"main.py", "app.py", "server.py", "run.py"} {
			if exists(filepath.Join(dir, f)) {
				main = f
				break
			}
		}
		if main == "" {
			return Suggestion{}, false
		}
		s.Kind, s.Port = "Python", 0
		s.Exec = []string{venv + "python", "${running}/" + main}
		s.Notes = append(s.Notes, "没认出是哪种 Web 框架：确认启动命令，有端口就填上")
	}
	switch {
	case exists(filepath.Join(dir, "requirements.txt")):
	case exists(filepath.Join(dir, "uv.lock")):
		s.Command = "uv export --no-dev --no-hashes -o requirements.txt"
		s.Notes = append(s.Notes, "服务器上按 requirements.txt 装依赖：构建时用 uv 导出一份")
	case exists(filepath.Join(dir, "poetry.lock")):
		s.Command = "poetry export -f requirements.txt -o requirements.txt --without-hashes"
		s.Notes = append(s.Notes, "服务器上按 requirements.txt 装依赖：构建时用 poetry 导出一份")
	default:
		s.Notes = append(s.Notes, "没有 requirements.txt：服务器上不装依赖")
	}
	s.Notes = append(s.Notes, "依赖在服务器上装进 venv（要有 python3；国内服务器可以在 deps.registry 填 pip 镜像）")
	return s, true
}

// ---------------------------------------------------------------- Node 服务端

var (
	reListen     = regexp.MustCompile(`\.listen\(\s*(?:process\.env\.PORT\s*(?:\|\||\?\?)\s*)?(\d{2,5})`)
	reNextExport = regexp.MustCompile(`output\s*:\s*['"]export['"]`)
	reNodeStart  = regexp.MustCompile(`^node\s+(\S+\.(?:js|mjs|cjs))`)
)

// nodeServer package.json 是 Node 服务端（不是前端）时给出建议。
func nodeServer(dir, rel, name, install, run string, scripts map[string]string, main string, has func(string) bool) (Suggestion, bool) {
	s := Suggestion{Name: appName(name), Type: "node", Workdir: rel, Artifact: join(rel, "."), Port: 3000,
		Env: nil, ServiceEnv: map[string]string{}}
	build := ""
	if scripts["build"] != "" {
		build = install + " && " + run
	}
	port := func(files ...string) {
		for _, f := range files {
			if m := reListen.FindStringSubmatch(readText(filepath.Join(dir, filepath.FromSlash(f)))); m != nil {
				if p, _ := strconv.Atoi(m[1]); p > 0 {
					s.Port = p
				}
				return
			}
		}
	}
	switch {
	case has("@nestjs/core"):
		port("src/main.ts", "src/main.js")
		s.Kind, s.Command = "Node（NestJS）", build
		s.Exec = []string{"node", "${running}/dist/main.js"}
		s.Exclude = []string{"src", "test"}
	case has("next"):
		cfg := ""
		for _, n := range []string{"next.config.js", "next.config.mjs", "next.config.ts"} {
			cfg += readText(filepath.Join(dir, n))
		}
		if reNextExport.MatchString(cfg) {
			return Suggestion{}, false // 静态导出：按前端
		}
		s.Kind, s.Command = "Node（Next.js）", build
		s.Exec = []string{"node", "${running}/node_modules/next/dist/bin/next", "start", "--hostname", "127.0.0.1", "--port", strconv.Itoa(s.Port)}
	case has("express") || has("koa") || has("fastify") || has("@hapi/hapi") || has("hono"):
		entry := main
		if m := reNodeStart.FindStringSubmatch(scripts["start"]); m != nil {
			entry = m[1]
		}
		if entry == "" {
			for _, f := range []string{"server.js", "app.js", "index.js", "dist/index.js", "dist/main.js"} {
				if exists(filepath.Join(dir, filepath.FromSlash(f))) {
					entry = f
					break
				}
			}
		}
		if entry == "" {
			return Suggestion{}, false
		}
		entry = strings.TrimPrefix(filepath.ToSlash(entry), "./")
		port(entry, "src/index.ts", "src/server.ts", "src/app.ts", "src/main.ts")
		kind := "Express"
		for _, k := range []string{"koa", "fastify", "@hapi/hapi", "hono"} {
			if has(k) {
				kind = strings.TrimPrefix(k, "@hapi/")
				kind = strings.ToUpper(kind[:1]) + kind[1:]
			}
		}
		s.Kind, s.Command = "Node（"+kind+"）", build
		s.Exec = []string{"node", "${running}/" + entry}
	default:
		return Suggestion{}, false
	}
	s.ServiceEnv["PORT"] = strconv.Itoa(s.Port)
	s.Notes = append(s.Notes, "依赖在服务器上装（npm ci，要有 Node.js；国内服务器可以在 deps.registry 填 https://registry.npmmirror.com）")
	return s, true
}

// ---------------------------------------------------------------- .NET

type csproj struct {
	Sdk   string `xml:"Sdk,attr"`
	Props []struct {
		AssemblyName string `xml:"AssemblyName"`
		IsTest       string `xml:"IsTestProject"`
	} `xml:"PropertyGroup"`
	Refs []struct {
		Include string `xml:"Include,attr"`
	} `xml:"ItemGroup>PackageReference"`
}

func csprojIn(dir string) string {
	m, _ := filepath.Glob(filepath.Join(dir, "*.csproj"))
	if len(m) == 1 {
		return m[0]
	}
	return ""
}

func detectDotnet(base, rel string) (Suggestion, bool) {
	dir := filepath.Join(base, rel)
	p := csprojIn(dir)
	b, err := os.ReadFile(p)
	if err != nil {
		return Suggestion{}, false
	}
	var c csproj
	if xml.Unmarshal(b, &c) != nil {
		return Suggestion{}, false
	}
	name := strings.TrimSuffix(filepath.Base(p), ".csproj")
	for _, pg := range c.Props {
		if pg.IsTest == "true" {
			return Suggestion{}, false
		}
		if pg.AssemblyName != "" {
			name = pg.AssemblyName
		}
	}
	for _, r := range c.Refs {
		if r.Include == "Microsoft.NET.Test.Sdk" {
			return Suggestion{}, false
		}
	}
	s := Suggestion{Name: appName(name), Type: "dotnet", Workdir: rel, Artifact: join(rel, "build/publish"),
		Command: "dotnet publish -c Release -r linux-x64 --self-contained true -o build/publish",
		Exec:    []string{"${running}/" + name}, ServiceEnv: map[string]string{}}
	switch c.Sdk {
	case "Microsoft.NET.Sdk.Web":
		s.Kind, s.Port = ".NET（ASP.NET Core）", 5000
		s.ServiceEnv["ASPNETCORE_URLS"] = "http://127.0.0.1:5000"
	case "Microsoft.NET.Sdk.Worker":
		s.Kind = ".NET（后台服务）"
	default:
		return Suggestion{}, false
	}
	s.Notes = append(s.Notes, "自带 .NET 运行时发布（服务器上不用装 .NET）；服务器是 arm64 时把 -r linux-x64 改成 -r linux-arm64")
	return s, true
}
