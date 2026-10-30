package scaffold

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

func TestDetectDirApps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mono")
	write(t, dir, "api/requirements.txt", "fastapi==0.110\nuvicorn[standard]\n")
	write(t, dir, "api/app/main.py", "from fastapi import FastAPI\n\napp = FastAPI()\n")
	write(t, dir, "admin/manage.py", "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'shopadmin.settings')\n")
	write(t, dir, "admin/requirements.txt", "Django==5.0\n")
	write(t, dir, "flaskapp/pyproject.toml", "[project]\ndependencies = [\"flask\", \"gunicorn\"]\n")
	write(t, dir, "flaskapp/uv.lock", "")
	write(t, dir, "flaskapp/wsgi.py", "from flask import Flask\napplication = Flask(__name__)\n")
	write(t, dir, "nest/package.json", `{"name":"nest-api","scripts":{"build":"nest build"},"dependencies":{"@nestjs/core":"10"}}`)
	write(t, dir, "nest/package-lock.json", "{}")
	write(t, dir, "nest/src/main.ts", "await app.listen(process.env.PORT ?? 3100);")
	write(t, dir, "site/package.json", `{"name":"site","scripts":{"build":"next build"},"dependencies":{"next":"14","react":"18"}}`)
	write(t, dir, "exp/package.json", `{"name":"exp","main":"server.js","dependencies":{"express":"4"}}`)
	write(t, dir, "exp/server.js", "app.listen(4000)")
	write(t, dir, "crm/Crm.Web.csproj", `<Project Sdk="Microsoft.NET.Sdk.Web"><PropertyGroup><TargetFramework>net8.0</TargetFramework></PropertyGroup></Project>`)
	write(t, dir, "crm.tests/Crm.Tests.csproj", `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup><PackageReference Include="Microsoft.NET.Test.Sdk" /></ItemGroup></Project>`)
	d, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Suggestion{}
	for _, a := range d.Apps {
		got[a.Name] = a
	}
	if len(d.Apps) != 7 {
		t.Fatalf("应识别 7 个应用：%+v", d.Apps)
	}
	if a := got["api"]; a.Type != "python" || a.Kind != "Python（FastAPI）" || strings.Join(a.Exec, " ") != "${running}/.venv/bin/uvicorn app.main:app --host 127.0.0.1 --port 8000" || a.Artifact != "api" {
		t.Errorf("fastapi：%+v", a)
	}
	if a := got["admin"]; a.Kind != "Python（Django）" || a.Exec[1] != "shopadmin.wsgi:application" || !strings.Contains(strings.Join(a.Notes, " "), "gunicorn") {
		t.Errorf("django：%+v", a)
	}
	if a := got["flaskapp"]; a.Kind != "Python（Flask）" || a.Exec[len(a.Exec)-1] != "wsgi:application" || !strings.HasPrefix(a.Command, "uv export") {
		t.Errorf("flask：%+v", a)
	}
	if a := got["nest-api"]; a.Type != "node" || a.Port != 3100 || a.Exec[1] != "${running}/dist/main.js" || a.Command != "npm ci && npm run build" || a.ServiceEnv["PORT"] != "3100" {
		t.Errorf("nest：%+v", a)
	}
	if a := got["site"]; a.Type != "node" || a.Kind != "Node（Next.js）" {
		t.Errorf("next：%+v", a)
	}
	if a := got["exp"]; a.Type != "node" || a.Port != 4000 || a.Exec[1] != "${running}/server.js" || a.Command != "" {
		t.Errorf("express：%+v", a)
	}
	if a := got["Crm.Web"]; a.Type != "dotnet" || a.Exec[0] != "${running}/Crm.Web" || a.ServiceEnv["ASPNETCORE_URLS"] != "http://127.0.0.1:5000" || a.Artifact != "crm/build/publish" {
		t.Errorf("dotnet：%+v", a)
	}

	// 生成的配置能通过校验
	spec := Spec{ProjectDir: dir, Project: d.Project, Targets: []TargetSpec{{Name: "prod", Host: "10.0.0.5", User: "root", RunAs: "shop"}}}
	for _, a := range d.Apps {
		spec.Apps = append(spec.Apps, AppSpec{Name: a.Name, Type: a.Type, Workdir: a.Workdir, Command: a.Command, Artifact: a.Artifact,
			Port: a.Port, Health: map[bool]string{true: "tcp", false: "none"}[a.Port > 0], Exec: a.Exec, ServiceEnv: a.ServiceEnv, Exclude: a.Exclude,
			Registry: map[bool]string{true: "https://registry.npmmirror.com"}[a.Type == "node"]})
	}
	path := filepath.Join(dir, "ferry.yaml")
	text, err := Generate(spec, path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := config.LoadContent([]byte(text), path)
	if err != nil {
		t.Fatalf("生成的配置校验失败：%v\n%s", err, text)
	}
	if n := f.App("nest-api"); n.Deps.Registry != "https://registry.npmmirror.com" || len(n.Artifact.Exclude) != 2 || n.Service.Env["PORT"] != "3100" {
		t.Errorf("nest：%+v\n%s", n, text)
	}
}
