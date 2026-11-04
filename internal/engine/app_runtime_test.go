package engine

import (
	"strings"
	"testing"
)

const runtimeConfig = `version: 1
project: demo
targets:
  a:
    host: 192.0.2.1
    runAs: app
    appRuntimes:
      api: {java: /opt/jdk21/bin/java}
      py: {python: /opt/python312/bin/python3}
      node: {node: /opt/node22/bin/node}
      net: {dotnet: /opt/dotnet8/dotnet}
      web: {nginxInstance: edge, port: 8090}
  b: {host: 192.0.2.2, runAs: app}
apps:
  - {name: api, type: java, artifact: {path: api.jar}, service: {runtime: {java: 17}}}
  - {name: py, type: python, artifact: {path: src}, service: {exec: ['${running}/.venv/bin/uvicorn', main:app]}}
  - {name: node, type: node, artifact: {path: dist}, service: {exec: [node, '${running}/main.js']}}
  - {name: net, type: dotnet, artifact: {path: publish}, service: {exec: [dotnet, '${running}/App.dll']}}
  - {name: web, type: static, artifact: {path: dist}, static: {port: 8081}}
`

func TestTargetRuntimeOverridesOnlySelectedTarget(t *testing.T) {
	f, err := loadYAML(t, runtimeConfig)
	if err != nil {
		t.Fatal(err)
	}
	java, err := Spec(f, f.Targets["a"], f.App("api"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := Spec(f, f.Targets["b"], f.App("api"))
	if err != nil {
		t.Fatal(err)
	}
	if java.Java != "/opt/jdk21/bin/java" || java.RuntimeJava != 0 || !strings.Contains(java.UnitText, "/opt/jdk21/bin/java") || other.RuntimeJava != 17 || f.App("api").Service.Runtime.Java != 17 {
		t.Fatalf("修改影响了其他目标或原配置：%+v %+v", java, other)
	}
	py, err := Spec(f, f.Targets["a"], f.App("py"))
	if err != nil {
		t.Fatal(err)
	}
	if py.Deps == nil || py.Deps.Python != "/opt/python312/bin/python3" || !strings.Contains(py.UnitText, ".venv/bin/uvicorn") {
		t.Fatalf("Python 必须用所选解释器重建 venv：%+v", py)
	}
	node, err := Spec(f, f.Targets["a"], f.App("node"))
	if err != nil {
		t.Fatal(err)
	}
	if node.Deps.Node != "/opt/node22/bin/node" || !strings.Contains(node.UnitText, "/opt/node22/bin/node") || !strings.Contains(node.UnitText, "/opt/node22/bin:") {
		t.Fatalf("Node 服务和依赖必须使用同一个版本：%+v", node)
	}
	net, err := Spec(f, f.Targets["a"], f.App("net"))
	if err != nil || !strings.Contains(net.UnitText, "/opt/dotnet8/dotnet") {
		t.Fatalf(".NET 主机没有切换：%+v %v", net, err)
	}
	web, err := Spec(f, f.Targets["a"], f.App("web"))
	if err != nil {
		t.Fatal(err)
	}
	if web.NginxInstance != "edge" || web.Unit != "" || web.SitePort != "8090" || !strings.Contains(web.Health.HTTP, ":8090/") {
		t.Fatalf("Nginx 绑定及健康检查应一起覆盖：%+v", web)
	}
}

func TestRuntimeOverrideCanConnectFilesOnlyFrontend(t *testing.T) {
	text := strings.Replace(runtimeConfig, "static: {port: 8081}", "static: {}", 1)
	f, err := loadYAML(t, text)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Spec(f, f.Targets["a"], f.App("web"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Health.HTTP != "http://127.0.0.1:8090/" || s.NginxInstance != "edge" {
		t.Fatalf("仅发布文件的站点应生成新健康检查：%+v", s)
	}
}
