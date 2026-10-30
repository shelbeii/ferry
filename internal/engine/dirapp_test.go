package engine

import (
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

func TestDirAppSpecs(t *testing.T) {
	f, err := config.LoadContent([]byte(`
version: 1
project: shop
targets:
  prod: {host: 10.0.0.1, runAs: shop, root: /opt/shop}
apps:
  - name: api
    type: python
    artifact: {path: backend}
    deps: {registry: https://mirrors.aliyun.com/pypi/simple}
    service:
      exec: ["${running}/.venv/bin/uvicorn", "main:app", "--port", "8000"]
      health: {tcp: 8000}
  - name: web
    type: node
    artifact: {path: web, exclude: [src]}
    service:
      exec: [node, "${running}/dist/main.js"]
  - name: crm
    type: dotnet
    artifact: {path: publish}
    service:
      exec: ["${running}/Crm.Web"]
      env: {ASPNETCORE_URLS: "http://127.0.0.1:5000"}
`), t.TempDir()+"/ferry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := f.Target("prod")
	api, err := Spec(f, tg, f.App("api"))
	if err != nil {
		t.Fatal(err)
	}
	if len(api.Executables) != 0 {
		t.Errorf(".venv 里的程序是服务器上装的，不用检查：%v", api.Executables)
	}
	if api.Deps == nil || api.Deps.Kind != "python" || api.Deps.Registry != "https://mirrors.aliyun.com/pypi/simple" || api.FileName != "" {
		t.Fatalf("python：%+v", api)
	}
	for _, want := range []string{"ExecStart=/opt/shop/apps/api/running/.venv/bin/uvicorn main:app --port 8000", `Environment="PYTHONUNBUFFERED=1"`} {
		if !strings.Contains(api.UnitText, want) {
			t.Errorf("缺少 %q：\n%s", want, api.UnitText)
		}
	}
	web, _ := Spec(f, tg, f.App("web"))
	if !strings.Contains(web.UnitText, "ExecStart=/usr/bin/env node /opt/shop/apps/web/running/dist/main.js") || !strings.Contains(web.UnitText, "NODE_ENV=production") || web.Deps == nil {
		t.Errorf("node：\n%s", web.UnitText)
	}
	crm, _ := Spec(f, tg, f.App("crm"))
	if len(crm.Executables) != 1 || crm.Executables[0] != "Crm.Web" || crm.Deps != nil {
		t.Errorf("dotnet：%+v", crm)
	}

	// 没写 exec、deps 用在别的类型上：报错
	_, err = config.LoadContent([]byte(`
version: 1
project: shop
targets:
  prod: {host: 10.0.0.1, runAs: shop, root: /opt/shop}
apps:
  - name: api
    type: python
    artifact: {path: backend}
  - name: job
    type: java
    artifact: {path: job.jar}
    deps: {off: true}
`), t.TempDir()+"/ferry.yaml")
	if err == nil || !strings.Contains(err.Error(), "要写 service.exec") || !strings.Contains(err.Error(), "deps 只用于") {
		t.Fatalf("%v", err)
	}
}
