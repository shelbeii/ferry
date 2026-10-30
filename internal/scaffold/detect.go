// Package scaffold 新建项目：识别项目目录里的后端 / 前端应用，按向导填写的内容生成 ferry.yaml。
package scaffold

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Suggestion 识别出的一个应用（路径都相对项目目录，用 /）。
type Suggestion struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"` // 给人看的说明，如「Spring Boot（Maven 多模块）」
	Type     string            `json:"type"` // java / binary / static / python / node / dotnet
	Workdir  string            `json:"workdir"`
	Command  string            `json:"command"`
	Env      map[string]string `json:"env,omitempty"`
	Artifact string            `json:"artifact"`
	Port     int               `json:"port,omitempty"`
	Java     int               `json:"java,omitempty"` // 需要的 Java 版本（从 pom 推断；0 = 不知道）
	// Proxy 前端的接口转发（从开发服务器的代理配置推断）：路径前缀 → 后端地址
	Proxy map[string]string `json:"proxy,omitempty"`
	// 同一个 Maven 工程的多个模块：可以共用一条构建（-pl ${each:module}）
	Module       string   `json:"module,omitempty"`
	BuildGroup   string   `json:"buildGroup,omitempty"`   // Maven 工程根目录
	GroupCommand string   `json:"groupCommand,omitempty"` // 共用构建的命令
	Notes        []string `json:"notes,omitempty"`
	// ConfigFiles 工程目录里看着像程序要读的配置文件、配置目录（相对项目目录）：向导里给附带文件做候选，默认不加
	ConfigFiles []string `json:"configFiles,omitempty"`
	// python、node、dotnet：启动命令、服务的环境变量、不上传的
	Exec       []string          `json:"exec,omitempty"`
	ServiceEnv map[string]string `json:"serviceEnv,omitempty"`
	Exclude    []string          `json:"exclude,omitempty"`
	// AI 是 AI 识别新加或改过的（向导里提示用户核对），Reason 是它的判断依据
	AI     bool   `json:"ai,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Detected 识别结果。
type Detected struct {
	Dir     string       `json:"dir"`
	Project string       `json:"project"` // 建议的项目名
	Git     bool         `json:"git"`     // 是 git 仓库（版本列表能显示提交与分支）
	Config  string       `json:"config"`  // 目录里已有的 ferry.yaml（有则可以直接添加）
	Apps    []Suggestion `json:"apps"`
}

var reProject = regexp.MustCompile(`[^a-z0-9._-]+`)

// ProjectName 目录名 → 合法的项目名（小写字母、数字、. _ -）。
func ProjectName(dir string) string {
	n := reProject.ReplaceAllString(strings.ToLower(filepath.Base(dir)), "-")
	n = strings.Trim(n, "-._")
	if n == "" {
		return "myapp"
	}
	return n
}

var reAppName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func appName(s string) string {
	n := strings.Trim(reAppName.ReplaceAllString(s, "-"), "-._")
	if n == "" {
		return "app"
	}
	return n
}

// skipDir 不往里找的目录。
func skipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "target", "dist", "build", "out", "vendor", "bin", "obj", "logs", "tmp":
		return true
	}
	return false
}

// hasProject 目录本身是一个工程（不用再往下找）。
func hasProject(dir string) bool {
	for _, n := range []string{"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "package.json", "go.mod"} {
		if exists(filepath.Join(dir, n)) {
			return true
		}
	}
	return isPythonProject(dir) || csprojIn(dir) != ""
}

// Detect 识别 dir（及其下两层子目录）里的应用。
func Detect(dir string) (*Detected, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, &os.PathError{Op: "识别", Path: abs, Err: os.ErrNotExist}
	}
	d := &Detected{Dir: filepath.ToSlash(abs), Project: ProjectName(abs), Apps: []Suggestion{}}
	if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
		d.Git = true
	}
	for _, n := range []string{"ferry.yaml", "ferry.yml"} {
		if _, err := os.Stat(filepath.Join(abs, n)); err == nil {
			d.Config = filepath.ToSlash(filepath.Join(abs, n))
			break
		}
	}
	// 根目录和第一层子目录；第一层里不是工程的目录（如 services/、web/）再往下找一层
	dirs := []string{"."}
	ents, _ := os.ReadDir(abs)
	for _, e := range ents {
		if !e.IsDir() || skipDir(e.Name()) {
			continue
		}
		dirs = append(dirs, e.Name())
		if hasProject(filepath.Join(abs, e.Name())) {
			continue
		}
		sub, _ := os.ReadDir(filepath.Join(abs, e.Name()))
		for _, s := range sub {
			if s.IsDir() && !skipDir(s.Name()) {
				dirs = append(dirs, e.Name()+"/"+s.Name())
			}
		}
	}
	mavenRoots := map[string]bool{}
	gradleRoots := map[string]bool{} // Gradle 多模块工程：模块已作为应用识别
	inside := func(roots map[string]bool, rel string) bool {
		for r := range roots {
			if r == "." || strings.HasPrefix(rel, r+"/") {
				return rel != r
			}
		}
		return false
	}
	for _, rel := range dirs {
		full := filepath.Join(abs, filepath.FromSlash(rel))
		// Maven：子目录属于上层 Maven 工程（已作为模块识别）时跳过
		if exists(filepath.Join(full, "pom.xml")) && !inside(mavenRoots, rel) {
			mavenRoots[rel] = true
			d.Apps = append(d.Apps, detectMaven(abs, rel)...)
			continue
		}
		if (exists(filepath.Join(full, "build.gradle")) || exists(filepath.Join(full, "build.gradle.kts"))) && !inside(gradleRoots, rel) {
			apps, multi := detectGradle(abs, rel)
			if multi {
				gradleRoots[rel] = true
			}
			d.Apps = append(d.Apps, apps...)
			continue
		}
		if csprojIn(full) != "" {
			if s, ok := detectDotnet(abs, rel); ok {
				d.Apps = append(d.Apps, s)
			}
			continue
		}
		if isPythonProject(full) {
			if s, ok := detectPython(abs, rel); ok {
				d.Apps = append(d.Apps, s)
				continue
			}
		}
		if exists(filepath.Join(full, "package.json")) {
			if s, ok := detectNode(abs, rel); ok {
				d.Apps = append(d.Apps, s)
			}
			continue
		}
		if exists(filepath.Join(full, "go.mod")) {
			if s, ok := detectGo(abs, rel); ok {
				d.Apps = append(d.Apps, s)
			}
		}
	}
	for i := range d.Apps {
		if a := &d.Apps[i]; a.Type != "static" {
			a.ConfigFiles = configCandidates(abs, a)
		}
		if a := &d.Apps[i]; a.Type == "static" || a.Type == "python" || a.Type == "node" {
			a.ConfigFiles = nil // 上传的是整个目录：配置文件已经在里面
		}
		if a := &d.Apps[i]; a.Type == "java" && a.Port == 0 {
			a.Notes = append(a.Notes, "配置文件里没找到 server.port（可能在 Nacos 等配置中心里，或者不是 Web 应用）：有端口就填上；不填则按「进程持续运行 10 秒」判断是否启动成功")
		}
	}
	// 名字去重
	seen := map[string]int{}
	for i := range d.Apps {
		n := strings.ToLower(d.Apps[i].Name)
		seen[n]++
		if seen[n] > 1 {
			d.Apps[i].Name += "-" + strconv.Itoa(seen[n])
		}
	}
	return d, nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// 构建、开发工具用的文件：不是程序运行时读的配置
var toolFiles = map[string]bool{
	"package.json": true, "package-lock.json": true, "composer.json": true, "jsconfig.json": true, "lerna.json": true,
	"nx.json": true, "turbo.json": true, "renovate.json": true, "vercel.json": true, "pnpm-lock.yaml": true,
	"pnpm-workspace.yaml": true, "ferry.yaml": true, "ferry.yml": true, "mkdocs.yml": true, "codecov.yml": true,
	"appveyor.yml": true, "gradle.properties": true, "sonar-project.properties": true, "lombok.config": true,
	"cargo.toml": true, "rust-toolchain.toml": true, "rustfmt.toml": true, "clippy.toml": true, "pyproject.toml": true, "deno.json": true,
}

var configExts = map[string]bool{".yml": true, ".yaml": true, ".toml": true, ".ini": true, ".conf": true, ".properties": true, ".json": true, ".env": true}

// configCandidates 工程目录（构建目录，以及产物 target/、build/ 之前的模块目录）里看着像配置的文件，和 config、conf、configs 目录。
func configCandidates(root string, s *Suggestion) []string {
	dirs := []string{s.Workdir}
	a := path.Clean(s.Artifact)
	for _, seg := range []string{"/target/", "/build/"} {
		if i := strings.Index("/"+a, seg); i > 0 {
			dirs = append(dirs, a[:i-1])
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		if d == "" {
			d = "."
		}
		ents, _ := os.ReadDir(filepath.Join(root, filepath.FromSlash(d)))
		for _, e := range ents {
			n := e.Name()
			lower := strings.ToLower(n)
			ok := false
			switch {
			case e.IsDir():
				ok = lower == "config" || lower == "conf" || lower == "configs"
			case e.Type().IsRegular():
				ext := path.Ext(lower)
				ok = (configExts[ext] || lower == ".env") && !toolFiles[lower] &&
					!strings.HasPrefix(lower, "tsconfig") && !strings.HasPrefix(lower, "docker-compose") && !strings.HasPrefix(lower, "compose.") &&
					!strings.HasPrefix(lower, ".eslintrc") && !strings.HasPrefix(lower, ".prettierrc") && !strings.HasPrefix(lower, ".golangci") &&
					!strings.HasPrefix(lower, ".goreleaser") && !strings.HasPrefix(lower, ".gitlab-ci") && !strings.HasPrefix(lower, ".travis")
			}
			if p := join(d, n); ok && !seen[p] && len(out) < 20 {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func join(rel, p string) string {
	if rel == "." || rel == "" {
		return p
	}
	return path.Join(rel, p)
}

// ---------------------------------------------------------------- Maven

type pom struct {
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Packaging  string `xml:"packaging"`
	Parent     struct {
		ArtifactID string `xml:"artifactId"`
		Version    string `xml:"version"`
	} `xml:"parent"`
	Modules    []string `xml:"modules>module"`
	Properties struct {
		Entries []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	} `xml:"properties"`
	Build struct {
		FinalName string `xml:"finalName"`
		Plugins   []struct {
			ArtifactID string `xml:"artifactId"`
		} `xml:"plugins>plugin"`
	} `xml:"build"`
}

func readPom(p string) (*pom, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var x pom
	if err := xml.Unmarshal(b, &x); err != nil {
		return nil, err
	}
	return &x, nil
}

var reProp = regexp.MustCompile(`\$\{([^}]+)\}`)

// detectMaven Maven 工程（rel 为工程根目录）：单模块或多模块里带 spring-boot-maven-plugin 的模块。
func detectMaven(base, rel string) []Suggestion {
	root, err := readPom(filepath.Join(base, rel, "pom.xml"))
	if err != nil {
		return nil
	}
	props := map[string]string{}
	addProps := func(p *pom) {
		for _, e := range p.Properties.Entries {
			if _, ok := props[e.XMLName.Local]; !ok {
				props[e.XMLName.Local] = strings.TrimSpace(e.Value)
			}
		}
	}
	addProps(root)
	tool := "mvn"
	if exists(filepath.Join(base, rel, "mvnw")) || exists(filepath.Join(base, rel, "mvnw.cmd")) {
		tool = "mvnw"
	}
	var out []Suggestion
	var walk func(modRel string, p *pom, depth int)
	walk = func(modRel string, p *pom, depth int) {
		addProps(p)
		if len(p.Modules) > 0 && depth < 4 {
			for _, m := range p.Modules {
				m = strings.TrimSpace(m)
				child, err := readPom(filepath.Join(base, rel, filepath.FromSlash(modRel), filepath.FromSlash(m), "pom.xml"))
				if err == nil {
					walk(path.Join(modRel, m), child, depth+1)
				}
			}
			return
		}
		if !hasPlugin(p, "spring-boot-maven-plugin") || p.Packaging == "pom" || p.Packaging == "war" {
			return
		}
		version := p.Version
		if version == "" {
			version = p.Parent.Version
		}
		resolve := func(s string) string {
			return reProp.ReplaceAllStringFunc(s, func(m string) string {
				k := m[2 : len(m)-1]
				switch k {
				case "project.artifactId", "artifactId", "pom.artifactId":
					return p.ArtifactID
				case "project.version", "version", "pom.version":
					return version
				}
				if v, ok := props[k]; ok {
					return v
				}
				return m
			})
		}
		version = resolve(version)
		name := p.Build.FinalName
		if name == "" {
			name = p.ArtifactID + "-" + version
		}
		name = resolve(name)
		modDir := join(rel, modRel)
		s := Suggestion{Name: appName(p.ArtifactID), Type: "java", Workdir: rel, Artifact: join(modDir, "target/"+name+".jar")}
		setSpringPort(&s, filepath.Join(base, filepath.FromSlash(modDir)))
		if modRel == "" || modRel == "." {
			s.Kind = "Spring Boot（Maven）"
			s.Command = tool + " clean package -DskipTests"
		} else {
			s.Kind = "Spring Boot（Maven 多模块）"
			s.Module = modRel
			s.BuildGroup = rel
			s.Command = tool + " -pl " + modRel + " -am clean package -DskipTests"
			s.GroupCommand = tool + " -pl ${each:module} -am clean package -DskipTests"
		}
		if strings.Contains(name, version) && version != "" {
			s.Notes = append(s.Notes, "jar 名里带版本号 "+version+"：升级版本后要同步修改产物路径（或在 pom 里设置 finalName）")
		}
		if strings.Contains(name, "${") {
			s.Notes = append(s.Notes, "jar 名里有无法确定的变量，请确认产物路径")
		}
		out = append(out, s)
	}
	walk("", root, 0)
	shortenNames(out)
	if j := requiredJava(root, props); j > 0 {
		for i := range out {
			out[i].Java = j
			out[i].Notes = append(out[i].Notes, javaNote(j))
		}
	}
	return out
}

// requiredJava 从 pom 推断需要的 Java 版本：java.version / maven.compiler.release / target，Spring Boot 3 至少 17。
func requiredJava(root *pom, props map[string]string) int {
	j := 0
	for _, k := range []string{"java.version", "maven.compiler.release", "maven.compiler.target", "maven.compiler.source"} {
		if v := props[k]; v != "" {
			v = strings.TrimPrefix(v, "1.")
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				j = n
				break
			}
		}
	}
	boot := ""
	if root.Parent.ArtifactID == "spring-boot-starter-parent" {
		boot = root.Parent.Version
	} else if v := props["spring-boot.version"]; v != "" {
		boot = v
	}
	if strings.HasPrefix(boot, "3.") || strings.HasPrefix(boot, "4.") {
		j = max(j, 17)
	}
	return j
}

// shortenNames 多模块时用模块目录名去掉共同前缀做应用名（ShopMall-system → system）。
func shortenNames(apps []Suggestion) {
	if len(apps) < 2 {
		return
	}
	bases := make([]string, len(apps))
	for i, a := range apps {
		bases[i] = path.Base(a.Module)
	}
	prefix := bases[0]
	for _, b := range bases[1:] {
		for !strings.HasPrefix(b, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	// 只在 - 或 _ 处截断
	cut := strings.LastIndexAny(prefix, "-_")
	if cut < 0 {
		return
	}
	prefix = prefix[:cut+1]
	names := map[string]bool{}
	for _, b := range bases {
		rest := strings.TrimPrefix(b, prefix)
		if rest == "" || names[strings.ToLower(rest)] {
			return
		}
		names[strings.ToLower(rest)] = true
	}
	for i := range apps {
		apps[i].Name = appName(strings.TrimPrefix(bases[i], prefix))
	}
}

func hasPlugin(p *pom, id string) bool {
	for _, pl := range p.Build.Plugins {
		if strings.TrimSpace(pl.ArtifactID) == id {
			return true
		}
	}
	return false
}

// javaNote 需要的 Java 版本的提醒。
func javaNote(j int) string {
	return fmt.Sprintf("需要 Java %d 或更高：用「由 Ferry 提供」的 JDK 时选 %d 及以上；用服务器上的 java 时要指向 JDK %d+（很多服务器的 /usr/bin/java 是 Java 8）", j, j, j)
}

var (
	// server.port=8080、server.port: ${PORT:8080}
	rePropPort = regexp.MustCompile(`(?m)^\s*server\.port\s*[=:]\s*["']?(?:\$\{[^:}]+:)?(\d+)`)
	reYmlPort  = regexp.MustCompile(`(?m)^server:\s*\n(?:[ \t]+.*\n)*?[ \t]+port:\s*["']?(?:\$\{[^:}]+:)?(\d+)`)
	// spring.profiles.active（yml 的嵌套写法或 properties）；${PROFILE:dev} 取默认值
	reActiveProfile = regexp.MustCompile(`(?m)(?:^\s*spring\.profiles\.active\s*[=:]|^[ \t]+profiles:\s*\n[ \t]+active:)\s*["']?(?:\$\{[^:}]+:)?([A-Za-z0-9_-]+)`)
)

func readText(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// springPort 服务器上运行时的端口：application-prod.* 优先，其次 spring.profiles.active 指定的 profile，
// 最后 application.* / bootstrap.*。返回端口和取自哪个文件。
func springPort(moduleDir string) (int, string) {
	res := filepath.Join(moduleDir, "src", "main", "resources")
	portIn := func(names ...string) (int, string) {
		for _, n := range names {
			text := readText(filepath.Join(res, n))
			pats := []*regexp.Regexp{rePropPort} // yml 里也可以写 server.port: 8080
			if !strings.HasSuffix(n, ".properties") {
				pats = append(pats, reYmlPort)
			}
			for _, re := range pats {
				if m := re.FindStringSubmatch(text); m != nil {
					if v, err := strconv.Atoi(m[1]); err == nil {
						return v, n
					}
				}
			}
		}
		return 0, ""
	}
	base := []string{"application.yml", "application.yaml", "application.properties", "bootstrap.yml", "bootstrap.yaml", "bootstrap.properties"}
	profiles := []string{"prod"}
	for _, n := range base {
		if m := reActiveProfile.FindStringSubmatch(readText(filepath.Join(res, n))); m != nil {
			if m[1] != "prod" {
				profiles = append(profiles, m[1])
			}
			break
		}
	}
	for _, pf := range profiles {
		if p, n := portIn("application-"+pf+".yml", "application-"+pf+".yaml", "application-"+pf+".properties"); p > 0 {
			return p, n
		}
	}
	return portIn(base...)
}

// setSpringPort 填端口；取自 profile 的配置文件时说明一下。
func setSpringPort(s *Suggestion, moduleDir string) {
	p, from := springPort(moduleDir)
	s.Port = p
	if p > 0 && strings.HasPrefix(from, "application-") {
		s.Notes = append(s.Notes, fmt.Sprintf("端口 %d 取自 %s", p, from))
	}
}

// ---------------------------------------------------------------- Gradle

var (
	// 行首的 version '1.0' / version = '1.0'，或 allprojects { version = '1.0' } 里的赋值；插件的 id '…' version '…' 不算
	reGradleVersion = regexp.MustCompile(`(?m)(?:^\s*version\s*=?|\bversion\s*=)\s*["']([^"']+)["']`)
	reGradlePropVer = regexp.MustCompile(`(?m)^\s*version\s*=\s*(\S+)`)
	reRootName      = regexp.MustCompile(`rootProject\.name\s*=\s*["']([^"']+)["']`)
	reInclude       = regexp.MustCompile(`(?m)^\s*include\b(.*)$`)
	reQuoted        = regexp.MustCompile(`["']([^"']+)["']`)
	// sourceCompatibility = '17' / JavaVersion.VERSION_17 / JavaVersion.VERSION_1_8，toolchain 的 JavaLanguageVersion.of(17)
	reGradleJava = regexp.MustCompile(`(?:sourceCompatibility|targetCompatibility)\s*=\s*(?:JavaVersion\.VERSION_(?:1_)?|["']?(?:1\.)?)(\d+)|JavaLanguageVersion\.of\(\s*(\d+)`)
	// plugins { id 'org.springframework.boot' version '3.2.5' }、id("org.springframework.boot") version "3.2.5"
	reGradleBoot = regexp.MustCompile(`org\.springframework\.boot["']\)?\s+version\s+["'](\d+)\.`)
)

// gradleText 目录里的 build.gradle / build.gradle.kts。
func gradleText(dir string) string {
	return readText(filepath.Join(dir, "build.gradle")) + readText(filepath.Join(dir, "build.gradle.kts"))
}

// gradleSettings settings.gradle(.kts) 里的 rootProject.name 和 include 的模块目录（:a:b → a/b）。
func gradleSettings(dir string) (name string, mods []string) {
	text := readText(filepath.Join(dir, "settings.gradle")) + readText(filepath.Join(dir, "settings.gradle.kts"))
	if m := reRootName.FindStringSubmatch(text); m != nil {
		name = m[1]
	}
	for _, line := range reInclude.FindAllStringSubmatch(text, -1) {
		for _, q := range reQuoted.FindAllStringSubmatch(line[1], -1) {
			if m := strings.Trim(strings.ReplaceAll(strings.TrimPrefix(q[1], ":"), ":", "/"), "/"); m != "" {
				mods = append(mods, m)
			}
		}
	}
	return name, mods
}

// gradleJava 需要的 Java 版本：sourceCompatibility / toolchain；Spring Boot 3 及以上至少 17。
func gradleJava(texts ...string) int {
	j := 0
	for _, t := range texts {
		if m := reGradleJava.FindStringSubmatch(t); m != nil && j == 0 {
			j, _ = strconv.Atoi(m[1] + m[2])
		}
		if m := reGradleBoot.FindStringSubmatch(t); m != nil {
			if v, _ := strconv.Atoi(m[1]); v >= 3 {
				j = max(j, 17)
			}
		}
	}
	return j
}

// detectGradle Gradle 工程：单个 Spring Boot 工程，或 settings.gradle 里 include 的模块中带 Spring Boot 插件的。
// multi 表示是多模块工程（它下面的模块目录不用再单独识别）。
func detectGradle(base, rel string) (apps []Suggestion, multi bool) {
	dir := filepath.Join(base, filepath.FromSlash(rel))
	root := gradleText(dir)
	rootName, mods := gradleSettings(dir)
	tool := "gradle"
	if exists(filepath.Join(dir, "gradlew")) || exists(filepath.Join(dir, "gradlew.bat")) {
		tool = "gradlew"
	}
	version := ""
	if m := reGradleVersion.FindStringSubmatch(root); m != nil {
		version = m[1]
	} else if m := reGradlePropVer.FindStringSubmatch(readText(filepath.Join(dir, "gradle.properties"))); m != nil {
		version = m[1]
	}
	jarOf := func(name, text string) string {
		v := version
		if m := reGradleVersion.FindStringSubmatch(text); m != nil {
			v = m[1]
		}
		if v == "" || v == "unspecified" {
			return name
		}
		return name + "-" + v
	}
	note := "请确认 build/libs 下生成的 jar 名与产物路径一致"
	if len(mods) == 0 {
		if !strings.Contains(root, "org.springframework.boot") {
			return nil, false
		}
		name := filepath.Base(dir)
		if rootName != "" {
			name = rootName
		}
		s := Suggestion{
			Name: appName(name), Kind: "Spring Boot（Gradle）", Type: "java", Workdir: rel,
			Command: tool + " bootJar -x test", Artifact: join(rel, "build/libs/"+jarOf(name, root)+".jar"), Notes: []string{note},
		}
		setSpringPort(&s, dir)
		if j := gradleJava(root); j > 0 {
			s.Java = j
			s.Notes = append(s.Notes, javaNote(j))
		}
		return []Suggestion{s}, false
	}
	for _, m := range mods {
		modDir := filepath.Join(dir, filepath.FromSlash(m))
		text := gradleText(modDir)
		if !strings.Contains(text, "org.springframework.boot") {
			continue // 公共库等不是 Spring Boot 应用的模块
		}
		name := path.Base(m)
		s := Suggestion{
			Name: appName(name), Kind: "Spring Boot（Gradle 多模块）", Type: "java", Workdir: rel, Module: m,
			Command:  tool + " :" + strings.ReplaceAll(m, "/", ":") + ":bootJar -x test",
			Artifact: join(rel, m+"/build/libs/"+jarOf(name, text)+".jar"), Notes: []string{note},
		}
		setSpringPort(&s, modDir)
		if j := gradleJava(text, root); j > 0 {
			s.Java = j
			s.Notes = append(s.Notes, javaNote(j))
		}
		apps = append(apps, s)
	}
	shortenNames(apps)
	for i := range apps {
		apps[i].Module = "" // 只用来缩短应用名：Gradle 模块不走共用构建
	}
	return apps, true
}

// ---------------------------------------------------------------- 前端

var reOutDir = regexp.MustCompile(`outDir\s*:\s*['"]([^'"]+)['"]`)

func detectNode(base, rel string) (Suggestion, bool) {
	dir := filepath.Join(base, rel)
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return Suggestion{}, false
	}
	var pkg struct {
		Name            string            `json:"name"`
		Main            string            `json:"main"`
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return Suggestion{}, false
	}
	has := func(dep string) bool {
		_, a := pkg.Dependencies[dep]
		_, c := pkg.DevDependencies[dep]
		return a || c
	}
	if has("@tauri-apps/api") || has("electron") {
		return Suggestion{}, false // 桌面应用，不是网站
	}
	install, run := "npm ci", "npm run build"
	switch {
	case exists(filepath.Join(dir, "pnpm-lock.yaml")):
		install, run = "pnpm install --frozen-lockfile", "pnpm run build"
	case exists(filepath.Join(dir, "yarn.lock")):
		install, run = "yarn install --frozen-lockfile", "yarn build"
	case !exists(filepath.Join(dir, "package-lock.json")):
		install = "npm install"
	}
	// Node 服务端（NestJS、Next.js、Express……）
	srvName := pkg.Name
	if i := strings.LastIndex(srvName, "/"); i >= 0 {
		srvName = srvName[i+1:]
	}
	if srvName == "" {
		srvName = filepath.Base(dir)
	}
	if s, ok := nodeServer(dir, rel, srvName, install, run, pkg.Scripts, pkg.Main, has); ok {
		return s, true
	}
	if pkg.Scripts["build"] == "" {
		return Suggestion{}, false
	}
	out, kind := "dist", "前端（"+frontendKind(has)+"）"
	switch {
	case has("react-scripts"):
		out = "build"
	case has("@angular/core"):
		out = "dist"
		if b, err := os.ReadFile(filepath.Join(dir, "angular.json")); err == nil {
			if m := regexp.MustCompile(`"outputPath"\s*:\s*"([^"]+)"`).FindSubmatch(b); m != nil {
				out = string(m[1])
			}
		}
	case has("next"):
		out = "out"
	}
	for _, n := range []string{"vite.config.ts", "vite.config.js", "vite.config.mjs", "vue.config.js"} {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			if m := reOutDir.FindSubmatch(b); m != nil {
				out = strings.TrimPrefix(string(m[1]), "./")
			}
		}
	}
	name := pkg.Name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		name = filepath.Base(dir)
	}
	s := Suggestion{
		Name: appName(name), Kind: kind, Type: "static", Workdir: rel,
		Command: install + " && " + run, Artifact: join(rel, out),
	}
	if has("next") {
		s.Notes = append(s.Notes, "Next.js 需要静态导出（output: 'export'）才能作为静态站点部署")
	}
	if p, from := frontendProxy(dir); len(p) > 0 {
		s.Proxy = p
		s.Notes = append(s.Notes, "接口转发是从 "+from+" 的开发代理推断的，请确认服务器上后端的地址和端口")
	}
	return s, true
}

var (
	// '/api': { target: 'http://localhost:8008', ... }  或  '/api': 'http://localhost:8008'
	reDevProxy = regexp.MustCompile(`['"](/[A-Za-z0-9._~/-]*)['"]\s*:\s*(?:\{[^}]*?target\s*:\s*)?['"](https?://[^'"\s]+)['"]`)
	// .env.production 里的接口前缀：VITE_APP_BASE_API = '/prod-api'
	reBaseAPI = regexp.MustCompile(`(?m)^\s*\w*(?:BASE_API|API_BASE|API_PREFIX)\w*\s*=\s*['"]?(/[A-Za-z0-9._~/-]+)`)
)

// frontendProxy 从 vite / vue-cli 的开发代理推断生产环境的接口转发。
//   - 开发代理去掉了前缀（rewrite）、且 .env.production 里有接口前缀（如 /prod-api）：前缀 → 后端/（去掉前缀转发）
//   - 否则照搬开发代理：/api/ → 后端（保留前缀）
func frontendProxy(dir string) (map[string]string, string) {
	for _, n := range []string{"vite.config.ts", "vite.config.js", "vite.config.mjs", "vue.config.js"} {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		text := string(b)
		ms := reDevProxy.FindAllStringSubmatchIndex(text, -1)
		if len(ms) == 0 {
			continue
		}
		prodBase := ""
		for _, env := range []string{".env.production", ".env.prod", ".env"} {
			if eb, err := os.ReadFile(filepath.Join(dir, env)); err == nil {
				if m := reBaseAPI.FindSubmatch(eb); m != nil {
					prodBase = string(m[1])
					break
				}
			}
		}
		out := map[string]string{}
		for _, m := range ms {
			path, target := text[m[2]:m[3]], text[m[4]:m[5]]
			target = strings.Replace(target, "//localhost", "//127.0.0.1", 1)
			end := m[1] + 400 // 这一项后面的 rewrite / pathRewrite
			if end > len(text) {
				end = len(text)
			}
			rest := text[m[1]:end]
			if next := reDevProxy.FindStringIndex(rest); next != nil {
				rest = rest[:next[0]]
			}
			strip := strings.Contains(rest, "rewrite") || strings.Contains(rest, "pathRewrite")
			if strip && prodBase != "" {
				out[strings.TrimSuffix(prodBase, "/")+"/"] = strings.TrimSuffix(target, "/") + "/"
				continue
			}
			key := strings.TrimSuffix(path, "/") + "/"
			if strip {
				out[key] = strings.TrimSuffix(target, "/") + "/"
			} else {
				out[key] = strings.TrimSuffix(target, "/")
			}
		}
		return out, n
	}
	return nil, ""
}

func frontendKind(has func(string) bool) string {
	switch {
	case has("vue"):
		return "Vue"
	case has("react"):
		return "React"
	case has("@angular/core"):
		return "Angular"
	case has("svelte"):
		return "Svelte"
	}
	return "Node"
}

// ---------------------------------------------------------------- Go

func detectGo(base, rel string) (Suggestion, bool) {
	dir := filepath.Join(base, rel)
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return Suggestion{}, false
	}
	m := regexp.MustCompile(`(?m)^module\s+(\S+)`).FindSubmatch(b)
	if m == nil {
		return Suggestion{}, false
	}
	// 找 main 包：根目录或 cmd/<名字>
	pkg, name := "", path.Base(string(m[1]))
	if isMainPkg(dir) {
		pkg = "."
	} else if ents, err := os.ReadDir(filepath.Join(dir, "cmd")); err == nil {
		var cands []string
		for _, e := range ents {
			if e.IsDir() && isMainPkg(filepath.Join(dir, "cmd", e.Name())) {
				cands = append(cands, e.Name())
			}
		}
		sort.Strings(cands)
		if len(cands) > 0 {
			pkg, name = "./cmd/"+cands[0], cands[0]
		}
	}
	if pkg == "" {
		return Suggestion{}, false
	}
	name = appName(name)
	return Suggestion{
		Name: name, Kind: "Go 程序", Type: "binary", Workdir: rel,
		Command: "go build -o build/" + name + " " + pkg, Artifact: join(rel, "build/"+name),
		Env:   map[string]string{"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"},
		Notes: []string{"按 linux/amd64 交叉编译；服务器是 arm64 时把构建环境的 GOARCH 改成 arm64"},
	}, true
}

func isMainPkg(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && regexp.MustCompile(`(?m)^package main\b`).Match(b) {
				return true
			}
		}
	}
	return false
}
