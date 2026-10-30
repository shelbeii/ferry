package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/config"
)

func write(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const bootPlugin = `<build><plugins><plugin><groupId>org.springframework.boot</groupId><artifactId>spring-boot-maven-plugin</artifactId></plugin></plugins></build>`

func fixture(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "My Shop")
	write(t, dir, "pom.xml", `<project><parent><artifactId>spring-boot-starter-parent</artifactId><version>3.2.5</version></parent><artifactId>shop</artifactId><version>1.2.0</version><packaging>pom</packaging>
<properties><revision>9</revision></properties><modules><module>shop-api</module><module>shop-admin</module><module>shop-common</module></modules></project>`)
	write(t, dir, "shop-api/pom.xml", `<project><parent><artifactId>shop</artifactId><version>1.2.0</version></parent><artifactId>shop-api</artifactId>`+bootPlugin+`</project>`)
	write(t, dir, "shop-api/src/main/resources/application.yml", "spring:\n  name: x\nserver:\n  address: 0.0.0.0\n  port: 8081\n")
	write(t, dir, "shop-admin/pom.xml", `<project><parent><version>1.2.0</version></parent><artifactId>shop-admin</artifactId>
<build><finalName>${project.artifactId}</finalName><plugins><plugin><artifactId>spring-boot-maven-plugin</artifactId></plugin></plugins></build></project>`)
	write(t, dir, "shop-admin/src/main/resources/application.properties", "server.port = 9090\n")
	write(t, dir, "shop-common/pom.xml", `<project><artifactId>shop-common</artifactId></project>`) // 不是 Spring Boot 应用
	write(t, dir, "web/package.json", `{"name":"@shop/web","scripts":{"build":"vite build"},"dependencies":{"vue":"3"},"devDependencies":{"vite":"5"}}`)
	write(t, dir, "web/pnpm-lock.yaml", "")
	write(t, dir, "web/vite.config.ts", "export default { build: { outDir: './public-dist' } }")
	write(t, dir, "desk/package.json", `{"name":"desk","scripts":{"build":"x"},"dependencies":{"@tauri-apps/api":"2"}}`)
	write(t, dir, "agent/go.mod", "module example.com/shop/agent\n")
	write(t, dir, "agent/cmd/shopd/main.go", "package main\nfunc main(){}\n")
	return dir
}

func TestDetect(t *testing.T) {
	dir := fixture(t)
	d, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Project != "my-shop" {
		t.Errorf("项目名：%q", d.Project)
	}
	got := map[string]Suggestion{}
	for _, a := range d.Apps {
		got[a.Name] = a
	}
	if len(d.Apps) != 4 {
		t.Fatalf("应识别 4 个应用：%+v", d.Apps)
	}
	api, admin, web, shopd := got["api"], got["admin"], got["web"], got["shopd"]
	if api.Artifact != "shop-api/target/shop-api-1.2.0.jar" || api.Port != 8081 || api.Module != "shop-api" || api.BuildGroup != "." ||
		!strings.Contains(api.GroupCommand, "${each:module}") || len(api.Notes) == 0 {
		t.Errorf("api：%+v", api)
	}
	if api.Java != 17 || admin.Java != 17 {
		t.Errorf("Spring Boot 3 需要 Java 17：%d %d", api.Java, admin.Java)
	}
	if admin.Artifact != "shop-admin/target/shop-admin.jar" || admin.Port != 9090 {
		t.Errorf("admin（finalName）：%+v", admin)
	}
	if web.Type != "static" || web.Artifact != "web/public-dist" || web.Command != "pnpm install --frozen-lockfile && pnpm run build" || web.Workdir != "web" {
		t.Errorf("web：%+v", web)
	}
	if shopd.Type != "binary" || shopd.Command != "go build -o build/shopd ./cmd/shopd" || shopd.Artifact != "agent/build/shopd" || shopd.Env["GOOS"] != "linux" {
		t.Errorf("go：%+v", shopd)
	}
}

// 生成的配置要能通过 ferry 自己的校验；共用的 Maven 构建、部署目录、单独的应用目录都要写进去。
func TestGenerateValid(t *testing.T) {
	dir := fixture(t)
	d, _ := Detect(dir)
	spec := Spec{ProjectDir: dir, Project: d.Project,
		Targets: []TargetSpec{
			{Name: "prod", Host: "10.0.0.5", User: "root", Root: "/data/shop", RunAs: "shop", HostKey: "SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"},
			{Name: "test", Host: "10.0.0.6", Port: 2222, User: "deploy", Sudo: true, Auth: "password", RunAs: "shop"},
		}}
	for _, a := range d.Apps {
		spec.Apps = append(spec.Apps, AppSpec{Name: a.Name, Type: a.Type, Workdir: a.Workdir, Command: a.Command, Env: a.Env,
			Artifact: a.Artifact, SharedBuild: a.BuildGroup != "", BuildGroup: a.BuildGroup, GroupCommand: a.GroupCommand, Module: a.Module,
			Port: a.Port, Health: map[bool]string{true: "tcp", false: "none"}[a.Port > 0], FileLogs: a.Type == "java"})
	}
	spec.Apps[0].Root = "/data2/shop"
	spec.Apps[1].RuntimeJava = 17 // admin 用 Ferry 提供的 JDK
	spec.Targets[1].FerryDir = "/opt/ferry-tools"
	spec.Apps[0].JvmArgs = []string{"-Xmx512m", "-Dfoo=a b"}
	for i := range spec.Apps {
		if spec.Apps[i].Type == "static" {
			spec.Apps[i].Link = "/usr/share/nginx/html/shop"
			spec.Apps[i].Reload = "nginx -t && nginx -s reload"
		}
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
	if f.Targets["prod"].Root != "/data/shop" || f.Targets["prod"].HostKey != "SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" || f.Targets["test"].Root != "/opt/my-shop" || !f.Targets["test"].Sudo || f.Targets["test"].Auth != "password" {
		t.Errorf("targets：%+v %+v", f.Targets["prod"], f.Targets["test"])
	}
	if b := f.Builds["maven"]; b == nil || !strings.Contains(b.Command, "${each:module}") {
		t.Errorf("应生成共用的 maven 构建：\n%s", text)
	}
	api := f.App("api")
	if api.Root != "/data2/shop" || api.Vars["module"] != "shop-api" || len(api.Service.JvmArgs) != 3 || api.Service.Health.TCP != "127.0.0.1:8081" {
		t.Errorf("api：%+v\n%s", api, text)
	}
	if adm := f.App("admin"); adm.Service.Runtime == nil || adm.Service.Runtime.Java != 17 || adm.Service.Java != "" {
		t.Errorf("admin 应使用 runtime：%+v", adm.Service)
	}
	if f.Targets["test"].FerryDir != "/opt/ferry-tools" || f.Targets["prod"].FerryDir != "/usr/local/lib/ferry" {
		t.Errorf("ferryDir：%q %q", f.Targets["test"].FerryDir, f.Targets["prod"].FerryDir)
	}
	if w := f.App("web"); w.Static.Link != "/usr/share/nginx/html/shop" || filepath.ToSlash(w.Artifact.Path) != "web/public-dist" {
		t.Errorf("web：%+v", w)
	}

	// 配置文件不放在项目目录里：路径换算成相对配置文件的
	other := filepath.Join(filepath.Dir(dir), "conf", "ferry.yaml")
	text2, _ := Generate(spec, other)
	if !strings.Contains(text2, "path: ../My Shop/shop-api/target/shop-api-1.2.0.jar") && !strings.Contains(text2, `path: '../My Shop/shop-api/target/shop-api-1.2.0.jar'`) {
		t.Errorf("路径应相对配置文件：\n%s", text2)
	}
	if _, err := config.LoadContent([]byte(text2), other); err != nil {
		t.Errorf("换了位置后校验失败：%v", err)
	}
}

func TestShortenNames(t *testing.T) {
	apps := []Suggestion{{Module: "ShopMall-auth"}, {Module: "mods/ShopMall-system"}, {Module: "ShopMall-gateway"}}
	shortenNames(apps)
	if apps[0].Name != "auth" || apps[1].Name != "system" || apps[2].Name != "gateway" {
		t.Fatalf("%+v", apps)
	}
}

func TestFrontendProxy(t *testing.T) {
	// 开发代理保留前缀：照搬
	dir := t.TempDir()
	write(t, dir, "vite.config.js", `export default { server: { proxy: { '/api': { target: 'http://localhost:8008', changeOrigin: true, configure(p) { p.on('x', () => {}) } } } } }`)
	if p, from := frontendProxy(dir); from != "vite.config.js" || len(p) != 1 || p["/api/"] != "http://127.0.0.1:8008" {
		t.Fatalf("%v %s", p, from)
	}
	// 若依式：开发时 /dev-api 去掉前缀转发，生产的前缀在 .env.production
	dir2 := t.TempDir()
	write(t, dir2, "vite.config.ts", "proxy: {\n  '/dev-api': {\n    target: 'http://localhost:8080',\n    changeOrigin: true,\n    rewrite: (p) => p.replace(/^\\/dev-api/, '')\n  }\n}")
	write(t, dir2, ".env.production", "VITE_APP_ENV = 'production'\nVITE_APP_BASE_API = '/prod-api'\n")
	if p, _ := frontendProxy(dir2); len(p) != 1 || p["/prod-api/"] != "http://127.0.0.1:8080/" {
		t.Fatalf("%v", p)
	}
	// 简写
	dir3 := t.TempDir()
	write(t, dir3, "vite.config.mjs", `proxy: { '/socket': 'ws://x', '/api/': 'http://10.0.0.2:9000/' }`)
	if p, _ := frontendProxy(dir3); len(p) != 1 || p["/api/"] != "http://10.0.0.2:9000" {
		t.Fatalf("%v", p)
	}
}

func TestGenerateServedStatic(t *testing.T) {
	dir := t.TempDir()
	spec := Spec{ProjectDir: dir, Project: "shop",
		Targets: []TargetSpec{{Name: "prod", Host: "10.0.0.5", User: "root", Root: "/data/shop", RunAs: "web"}},
		Apps: []AppSpec{{Name: "admin", Type: "static", Command: "npm run build", Artifact: "dist", Port: 8081,
			Proxy: map[string]string{"/api/": "http://127.0.0.1:8008", "/prod-api/": "http://127.0.0.1:8080/"}, NoSPA: true}}}
	path := filepath.Join(dir, "ferry.yaml")
	text, err := Generate(spec, path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := config.LoadContent([]byte(text), path)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	a := f.App("admin")
	if a.Static.Port != "8081" || a.Static.IsSPA() || a.Static.Proxy["/prod-api/"] != "http://127.0.0.1:8080/" || a.Service.Unit != "shop-admin.service" {
		t.Fatalf("%+v\n%s", a.Static, text)
	}
}

// Gradle 多模块、放在第二层目录里的工程、profile 里的端口、${PORT:8080} 写法、没找到端口的提醒
func TestDetectMore(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "settings.gradle", "rootProject.name = 'mall'\ninclude ':mall-common', ':mall-order'\ninclude(\":services:mall-user\")\n")
	write(t, dir, "build.gradle", "plugins { id 'org.springframework.boot' version '3.3.0' apply false }\nallprojects { version = '2.0.1' }\n")
	write(t, dir, "gradlew", "")
	write(t, dir, "mall-common/build.gradle", "plugins { id 'java-library' }\n")
	write(t, dir, "mall-order/build.gradle", "plugins { id 'org.springframework.boot' }\n")
	write(t, dir, "mall-order/src/main/resources/application.yml", "spring:\n  profiles:\n    active: ${ENV:test}\nserver:\n  port: 8000\n")
	write(t, dir, "mall-order/src/main/resources/application-test.yml", "server:\n  port: ${ORDER_PORT:8101}\n")
	write(t, dir, "services/mall-user/build.gradle.kts", "plugins { id(\"org.springframework.boot\") }\njava { toolchain { languageVersion.set(JavaLanguageVersion.of(21)) } }\n")
	write(t, dir, "services/mall-user/src/main/resources/application-prod.properties", "server.port=8201\n")
	write(t, dir, "frontend/admin/package.json", `{"name":"admin","scripts":{"build":"vite build"},"devDependencies":{"vite":"5"}}`)
	write(t, dir, "tools/job/pom.xml", `<project><artifactId>job</artifactId><version>1.0</version>`+bootPlugin+`</project>`)
	d, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Suggestion{}
	for _, a := range d.Apps {
		got[a.Name] = a
	}
	if len(d.Apps) != 4 {
		t.Fatalf("应识别 4 个应用：%+v", d.Apps)
	}
	order, user, admin, job := got["order"], got["user"], got["admin"], got["job"]
	if order.Command != "gradlew :mall-order:bootJar -x test" || order.Workdir != "." || order.Artifact != "mall-order/build/libs/mall-order-2.0.1.jar" ||
		order.Port != 8101 || order.Java != 17 || !strings.Contains(strings.Join(order.Notes, "\n"), "取自 application-test.yml") {
		t.Errorf("order：%+v", order)
	}
	if user.Command != "gradlew :services:mall-user:bootJar -x test" || user.Artifact != "services/mall-user/build/libs/mall-user-2.0.1.jar" ||
		user.Port != 8201 || user.Java != 21 || user.Module != "" {
		t.Errorf("user：%+v", user)
	}
	if admin.Workdir != "frontend/admin" || admin.Artifact != "frontend/admin/dist" {
		t.Errorf("第二层的前端：%+v", admin)
	}
	if job.Workdir != "tools/job" || job.Port != 0 || !strings.Contains(strings.Join(job.Notes, "\n"), "没找到 server.port") {
		t.Errorf("第二层的 Maven 工程：%+v", job)
	}
}
