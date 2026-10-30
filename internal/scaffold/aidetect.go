package scaffold

// AI 识别：规则认不出（或认得不全）的项目，让 AI 模型看项目目录里的文件，给出应用列表（和 Detect 的结果格式一样）。
// 模型只能用这里给的只读工具，而且只能看项目目录里的东西：不执行命令、不改文件；.env、私钥、证书这类文件不读，
// 交给模型的内容还会由 ai 包遮住像密码、密钥的部分。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/captain/ferry/internal/ai"
)

// AI 识别的上限：一次读文件最多多少字节、一共读多少、列目录最多几项、按名字找最多几个
const (
	aiReadMax   = 24 << 10
	aiReadTotal = 400 << 10
	aiListMax   = 400
	aiFindMax   = 200
)

// AIOptions AI 识别的设置。
type AIOptions struct {
	Provider ai.Provider
	// Emit 识别过程（模型的说明、在看哪个文件、用量），交给界面显示
	Emit func(ai.Event)
	// Hint 用户补充的说明（如「后端在 server 目录，用 make 构建」），可以为空
	Hint string
}

// AIDetected AI 识别的结果：应用列表和模型的总结。
type AIDetected struct {
	Detected
	Summary string   `json:"summary"`
	Usage   ai.Usage `json:"usage"`
	Model   string   `json:"model,omitempty"`
}

// AIDetect 先按规则识别，再让模型在规则结果的基础上检查、修正、补充。
func AIDetect(ctx context.Context, dir string, o AIOptions) (*AIDetected, error) {
	base, err := Detect(dir)
	if err != nil {
		return nil, err
	}
	root := filepath.FromSlash(base.Dir)
	fs := &aiFS{root: root}
	var sub *aiSubmit
	tools := []ai.Tool{fs.listTool(), fs.readTool(), fs.findTool(), submitTool(root, &sub)}

	out := &AIDetected{Detected: *base}
	emit := func(ev ai.Event) {
		if ev.Type == "usage" && ev.Usage != nil {
			out.Usage.Add(ev.Usage)
			out.Model = ev.Model
		}
		if o.Emit != nil {
			o.Emit(ev)
		}
	}
	conv := &ai.Conversation{ID: ai.NewID()}
	if err := ai.Run(ctx, o.Provider, aiDetectSystem, conv, aiDetectPrompt(fs, base, o.Hint), tools, emit); err != nil {
		return out, err
	}
	if sub == nil {
		last := ""
		for i := len(conv.Messages) - 1; i >= 0; i-- {
			if m := conv.Messages[i]; m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
				last = strings.TrimSpace(m.Text)
				break
			}
		}
		if last == "" {
			last = "没有说明原因"
		}
		return out, fmt.Errorf("AI 没有给出应用列表：%s", ai.Clip(last, 600))
	}
	out.Summary = sub.Summary
	out.Apps = mergeAIApps(root, base.Apps, sub.Apps)
	return out, nil
}

const aiDetectSystem = `你在帮用户把一个项目部署到 Linux 服务器（systemd）上，任务是看懂项目目录，找出里面要部署的应用，填好每个应用怎么构建、产物在哪、怎么运行。
你只能用给你的工具看项目目录里的文件（只读），不能执行命令。用中文写说明。

部署工具 Ferry 支持这几种应用（type）：
- java：Spring Boot 等可执行 jar。artifact 是构建出的 jar 文件；java 填需要的 JDK 大版本（如 17）。
- binary：原生程序（Go、Rust、C/C++……）。artifact 是 Linux 可执行文件；要交叉编译时把环境变量写在 env（如 Go 的 GOOS=linux、GOARCH=amd64、CGO_ENABLED=0）。
- static：前端静态站点。artifact 是构建出的目录（里面有 index.html）；开发服务器配置里有接口代理时写在 proxy（路径前缀 → 后端地址）。
- python、node（Node 服务端）、dotnet：上传整个目录。artifact 是要上传的目录（python、node 一般就是工程目录；dotnet 是 publish 输出目录）；
  exec 是启动命令，一项一个参数，第一项是程序（绝对路径，或 node、dotnet 这类命令），程序所在目录在服务器上写作 ${running}
  （如 ["${running}/.venv/bin/uvicorn", "main:app", "--host", "127.0.0.1", "--port", "8000"]、["node", "${running}/dist/main.js"]）；
  serviceEnv 是运行时的环境变量；exclude 是目录里不用上传的（文件名或相对路径）。

每个应用要填：
- name：应用名（字母、数字、. _ -）
- type：上面几种之一
- kind：一句话说明是什么（如「Spring Boot（Maven）」「Vue 3 + Vite」「Rust（Cargo）」）
- workdir：在哪个目录执行构建命令（相对项目目录，根目录写 .）
- command：构建命令（在用户这台电脑上执行，可能是 Windows，也可能是 macOS / Linux）；不需要构建时留空
- artifact：产物路径（相对项目目录，不是相对 workdir）
- port：程序监听的端口（从配置文件里找；找不到就不填）
- reason：你是根据哪些文件、哪些内容判断的（一两句话）
- notes：用户需要确认或注意的地方（可以为空）

做法：
1. 先看给你的目录结构和规则识别的结果；规则认对了的应用保持原样交回，认错的改正，漏掉的补上，不是独立应用的（工具库、测试工程）不要列。
2. 只读判断需要的文件：构建文件（pom.xml、build.gradle、package.json、Cargo.toml、go.mod、Makefile、*.csproj、pyproject.toml……）、
   Dockerfile、构建脚本、启动脚本、能看出端口的配置文件。不要读 .env、私钥、证书这类文件；不要逐个看源代码。
3. 读文件有总量限制，够判断就停。拿不准的写进 notes，不要编造路径或端口。
4. 最后调用一次 submit_apps 交回全部应用，然后用一两句话总结，不再调用别的工具。项目里没有能部署的应用时交回空列表，并在 summary 里说明原因。`

// aiDetectPrompt 用户的话：项目目录、两层目录结构、规则识别的结果、用户补充的说明。
func aiDetectPrompt(fs *aiFS, base *Detected, hint string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "项目目录：%s\n\n目录结构（两层）：\n", filepath.Base(fs.root))
	tree, _ := fs.list(".", 2)
	b.WriteString(tree)
	b.WriteString("\n规则识别的结果：")
	if len(base.Apps) == 0 {
		b.WriteString("没有识别出应用。\n")
	} else {
		type brief struct {
			Name     string   `json:"name"`
			Type     string   `json:"type"`
			Kind     string   `json:"kind"`
			Workdir  string   `json:"workdir"`
			Command  string   `json:"command"`
			Artifact string   `json:"artifact"`
			Port     int      `json:"port,omitempty"`
			Exec     []string `json:"exec,omitempty"`
		}
		var bs []brief
		for _, a := range base.Apps {
			bs = append(bs, brief{a.Name, a.Type, a.Kind, a.Workdir, a.Command, a.Artifact, a.Port, a.Exec})
		}
		j, _ := json.MarshalIndent(bs, "", "  ")
		b.WriteString("\n")
		b.Write(j)
		b.WriteString("\n")
	}
	if h := strings.TrimSpace(hint); h != "" {
		fmt.Fprintf(&b, "\n用户补充的说明：%s\n", h)
	}
	b.WriteString("\n请找出要部署的应用，最后用 submit_apps 交回。")
	return b.String()
}

// ---------------------------------------------------------------- 只读工具

// aiFS 只能看项目目录里的东西。
type aiFS struct {
	root string
	read int // 已经读了多少字节
}

// resolve 相对项目目录的路径 → 绝对路径；不能跑到项目目录外面（含符号链接）。
func (f *aiFS) resolve(rel string) (string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" {
		rel = "."
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", errors.New("只能用相对项目目录的路径")
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("只能看项目目录里的东西")
	}
	full := filepath.Join(f.root, filepath.FromSlash(clean))
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("没有 %s", clean)
	}
	rootReal, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		return "", err
	}
	if r, err := filepath.Rel(rootReal, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", errors.New("只能看项目目录里的东西（这是指向外面的链接）")
	}
	return full, nil
}

// sensitive 不给模型看的文件：环境变量文件、私钥、证书、凭据。
func sensitive(name string) bool {
	n := strings.ToLower(name)
	if n == ".env" || (strings.HasPrefix(n, ".env.") && !strings.HasSuffix(n, ".example") && !strings.HasSuffix(n, ".sample") && !strings.HasSuffix(n, ".template")) {
		return true
	}
	if strings.HasPrefix(n, "id_rsa") || strings.HasPrefix(n, "id_ed25519") || strings.HasPrefix(n, "id_ecdsa") || strings.HasPrefix(n, "id_dsa") {
		return true
	}
	switch n {
	case ".npmrc", ".pypirc", ".netrc", ".git-credentials", ".htpasswd", "credentials", "credentials.json", "secrets.json", "secrets.yaml", "secrets.yml":
		return true
	}
	switch path.Ext(n) {
	case ".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".crt", ".cer", ".der", ".ppk", ".kdbx", ".gpg", ".asc":
		return true
	}
	return false
}

// list 目录里的东西：目录带 /，文件带大小；node_modules 这类目录只列名字不展开。
func (f *aiFS) list(rel string, depth int) (string, error) {
	full, err := f.resolve(rel)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s 不是目录", rel)
	}
	var b strings.Builder
	n := 0
	var walk func(dir, prefix string, d int)
	walk = func(dir, prefix string, d int) {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if n >= aiListMax {
				return
			}
			n++
			name := e.Name()
			switch {
			case e.IsDir() && (skipDir(name) || d >= depth):
				note := ""
				if skipDir(name) {
					note = "（没有展开）"
				}
				fmt.Fprintf(&b, "%s%s/%s\n", prefix, name, note)
			case e.IsDir():
				fmt.Fprintf(&b, "%s%s/\n", prefix, name)
				walk(filepath.Join(dir, name), prefix+"  ", d+1)
			default:
				size := ""
				if fi, err := e.Info(); err == nil {
					size = fmt.Sprintf("  %s", humanSize(fi.Size()))
				}
				fmt.Fprintf(&b, "%s%s%s\n", prefix, name, size)
			}
		}
	}
	walk(full, "", 1)
	if n >= aiListMax {
		fmt.Fprintf(&b, "……（只列了前 %d 项，用更深的路径接着看）\n", aiListMax)
	}
	if b.Len() == 0 {
		return "（空目录）\n", nil
	}
	return b.String(), nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (f *aiFS) listTool() ai.Tool {
	return ai.Tool{
		Name:        "list_dir",
		Description: "列出项目目录里某个目录下的文件和子目录（相对项目目录的路径，根目录写 .）。depth 是展开几层，默认 1，最多 3。",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"path":  map[string]any{"type": "string", "description": "相对项目目录的路径"},
			"depth": map[string]any{"type": "integer", "minimum": 1, "maximum": 3},
		}, "required": []string{"path"}},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Path  string `json:"path"`
				Depth int    `json:"depth"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return "", err
			}
			if a.Depth < 1 {
				a.Depth = 1
			}
			if a.Depth > 3 {
				a.Depth = 3
			}
			ai.Progress(ctx, "查看目录 "+displayPath(a.Path))
			return f.list(a.Path, a.Depth)
		},
	}
}

func displayPath(p string) string {
	if strings.TrimSpace(p) == "" {
		return "."
	}
	return p
}

func (f *aiFS) readTool() ai.Tool {
	return ai.Tool{
		Name: "read_file",
		Description: fmt.Sprintf("读项目目录里的一个文本文件（相对项目目录的路径）。一次最多 %d KB，长的文件用 offset（从第几个字节开始）接着读；"+
			"一共能读 %d KB。.env、私钥、证书这类文件不能读。", aiReadMax>>10, aiReadTotal>>10),
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "相对项目目录的路径"},
			"offset": map[string]any{"type": "integer", "minimum": 0},
		}, "required": []string{"path"}},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Path   string `json:"path"`
				Offset int64  `json:"offset"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return "", err
			}
			ai.Progress(ctx, "读文件 "+a.Path)
			return f.readFile(a.Path, a.Offset)
		},
	}
}

func (f *aiFS) readFile(rel string, offset int64) (string, error) {
	if sensitive(path.Base(strings.ReplaceAll(rel, "\\", "/"))) {
		return "", errors.New("这个文件可能有密码、密钥，不能读")
	}
	full, err := f.resolve(rel)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("没有 %s", rel)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s 是目录，用 list_dir", rel)
	}
	left := aiReadTotal - f.read
	if left <= 0 {
		return "", errors.New("读文件的总量用完了：根据已经看到的内容交回结果，拿不准的写进 notes")
	}
	fh, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	if offset > 0 {
		if _, err := fh.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
	}
	n := min(aiReadMax, left)
	buf := make([]byte, n)
	got, err := io.ReadFull(fh, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	buf = buf[:got]
	if strings.ContainsRune(string(buf[:min(len(buf), 8<<10)]), 0) {
		return "", errors.New("这是二进制文件，不能读")
	}
	// 截断处不要切开一个字
	for len(buf) > 0 && !utf8.Valid(buf) && int64(got) == int64(n) {
		buf = buf[:len(buf)-1]
	}
	f.read += len(buf)
	end := offset + int64(len(buf))
	text := string(buf)
	if end < fi.Size() {
		text += fmt.Sprintf("\n……（文件共 %d 字节，这次读到第 %d 字节；要接着读用 offset: %d）", fi.Size(), end, end)
	}
	return text, nil
}

func (f *aiFS) findTool() ai.Tool {
	return ai.Tool{
		Name:        "find_files",
		Description: "按文件名在项目目录里找文件，可以用通配符 * ?（如 *.csproj、Makefile、Dockerfile*、application*.yml）。不进 node_modules、target、dist 这类目录。",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "文件名（可以用 * ?）"},
		}, "required": []string{"pattern"}},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var a struct {
				Pattern string `json:"pattern"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return "", err
			}
			ai.Progress(ctx, "找文件 "+a.Pattern)
			return f.find(a.Pattern)
		},
	}
}

func (f *aiFS) find(pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || strings.ContainsAny(pattern, "/\\") {
		return "", errors.New("只写文件名（可以用 * ?），不要带目录")
	}
	if _, err := path.Match(pattern, ""); err != nil {
		return "", fmt.Errorf("通配符写法不对：%v", err)
	}
	lower := strings.ToLower(pattern)
	var found []string
	var walk func(dir, rel string, d int)
	walk = func(dir, rel string, d int) {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if len(found) >= aiFindMax {
				return
			}
			p := join(rel, e.Name())
			if e.IsDir() {
				if !skipDir(e.Name()) && d < 6 {
					walk(filepath.Join(dir, e.Name()), p, d+1)
				}
				continue
			}
			if ok, _ := path.Match(lower, strings.ToLower(e.Name())); ok {
				found = append(found, p)
			}
		}
	}
	walk(f.root, ".", 1)
	sort.Strings(found)
	if len(found) == 0 {
		return "没有找到", nil
	}
	s := strings.Join(found, "\n")
	if len(found) >= aiFindMax {
		s += fmt.Sprintf("\n……（只列了前 %d 个）", aiFindMax)
	}
	return s, nil
}

// ---------------------------------------------------------------- 交回结果

// aiApp 模型交回的一个应用。
type aiApp struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Kind       string            `json:"kind"`
	Workdir    string            `json:"workdir"`
	Command    string            `json:"command"`
	Env        map[string]string `json:"env"`
	Artifact   string            `json:"artifact"`
	Port       int               `json:"port"`
	Java       int               `json:"java"`
	Proxy      map[string]string `json:"proxy"`
	Exec       []string          `json:"exec"`
	ServiceEnv map[string]string `json:"serviceEnv"`
	Exclude    []string          `json:"exclude"`
	Reason     string            `json:"reason"`
	Notes      []string          `json:"notes"`
}

type aiSubmit struct {
	Apps    []aiApp `json:"apps"`
	Summary string  `json:"summary"`
}

var appTypes = map[string]bool{"java": true, "binary": true, "static": true, "python": true, "node": true, "dotnet": true}

func submitTool(root string, out **aiSubmit) ai.Tool {
	str := map[string]any{"type": "string"}
	strList := map[string]any{"type": "array", "items": str}
	strMap := map[string]any{"type": "object", "additionalProperties": str}
	return ai.Tool{
		Name:        "submit_apps",
		Description: "交回识别出的全部应用（规则认对了的也要交回）。交回后用一两句话总结，不再调用别的工具。",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"apps": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"name": str, "type": map[string]any{"type": "string", "enum": []string{"java", "binary", "static", "python", "node", "dotnet"}},
				"kind": str, "workdir": str, "command": str, "env": strMap, "artifact": str,
				"port": map[string]any{"type": "integer"}, "java": map[string]any{"type": "integer"},
				"proxy": strMap, "exec": strList, "serviceEnv": strMap, "exclude": strList,
				"reason": str, "notes": strList,
			}, "required": []string{"name", "type", "kind", "workdir", "artifact", "reason"}}},
			"summary": map[string]any{"type": "string", "description": "一两句话总结：找到了什么、有什么要用户确认的"},
		}, "required": []string{"apps", "summary"}},
		Run: func(ctx context.Context, in json.RawMessage) (string, error) {
			var s aiSubmit
			if err := json.Unmarshal(in, &s); err != nil {
				return "", fmt.Errorf("格式不对：%v", err)
			}
			if probs := checkAIApps(root, s.Apps); len(probs) > 0 {
				return "", errors.New("有问题，改好后重新调用 submit_apps：\n- " + strings.Join(probs, "\n- "))
			}
			*out = &s
			return fmt.Sprintf("收到了 %d 个应用。不用再调用工具，用一两句话总结就行。", len(s.Apps)), nil
		},
	}
}

// relClean 相对项目目录的路径：统一成 / 分隔、去掉 ./；跑到外面或是绝对路径时返回空。
func relClean(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return ""
	}
	if path.IsAbs(p) || filepath.VolumeName(p) != "" {
		return ""
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return ""
	}
	return c
}

// checkAIApps 检查模型交回的应用，返回要它改的问题。
func checkAIApps(root string, apps []aiApp) []string {
	var probs []string
	names := map[string]bool{}
	for i, a := range apps {
		at := fmt.Sprintf("第 %d 个应用（%s）", i+1, a.Name)
		if !appTypes[a.Type] {
			probs = append(probs, at+"：type 只能是 java、binary、static、python、node、dotnet")
		}
		n := strings.ToLower(appName(a.Name))
		if names[n] {
			probs = append(probs, at+"：应用名重复")
		}
		names[n] = true
		wd := relClean(a.Workdir)
		if wd == "" {
			probs = append(probs, at+"：workdir 要写相对项目目录的路径（根目录写 .）")
		} else if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(wd))); err != nil || !fi.IsDir() {
			probs = append(probs, at+"：workdir "+wd+" 不是项目里的目录")
		}
		if relClean(a.Artifact) == "" {
			probs = append(probs, at+"：artifact 要写相对项目目录的路径")
		}
		if (a.Type == "python" || a.Type == "node" || a.Type == "dotnet") && len(a.Exec) == 0 {
			probs = append(probs, at+"：python、node、dotnet 要写 exec（启动命令）")
		}
		if a.Port < 0 || a.Port > 65535 {
			probs = append(probs, at+"：port 不对")
		}
	}
	return probs
}

// mergeAIApps 模型交回的应用 → 向导用的格式。和规则识别的结果一样的保持原样（带着规则算出的共用构建等），
// 改过的、新加的标上 AI 和判断依据。
func mergeAIApps(root string, rules []Suggestion, apps []aiApp) []Suggestion {
	out := []Suggestion{}
	used := map[string]bool{}
	for _, a := range apps {
		s := Suggestion{
			Name: appName(a.Name), Kind: strings.TrimSpace(a.Kind), Type: a.Type,
			Workdir: relClean(a.Workdir), Command: strings.TrimSpace(a.Command), Env: a.Env,
			Artifact: relClean(a.Artifact), Port: a.Port, Java: a.Java, Proxy: a.Proxy,
			Exec: a.Exec, ServiceEnv: a.ServiceEnv, Exclude: a.Exclude,
		}
		if same := sameRule(rules, s); same != nil {
			out = append(out, *same)
			used[same.Name] = true
			continue
		}
		s.AI = true
		s.Reason = strings.TrimSpace(a.Reason)
		if s.Kind == "" {
			s.Kind = s.Type
		}
		for _, n := range a.Notes {
			if n = strings.TrimSpace(n); n != "" {
				s.Notes = append(s.Notes, n)
			}
		}
		// 规则认出过同一个目录的应用：带上它算出的共用构建、附带文件候选
		for _, r := range rules {
			if r.Workdir == s.Workdir && r.Artifact == s.Artifact {
				s.Module, s.BuildGroup, s.GroupCommand = r.Module, r.BuildGroup, r.GroupCommand
				if s.BuildGroup != "" && s.Command != r.Command {
					s.Module, s.BuildGroup, s.GroupCommand = "", "", ""
				}
			}
		}
		if s.Type == "java" || s.Type == "binary" {
			s.ConfigFiles = configCandidates(root, &s)
		}
		out = append(out, s)
	}
	// 应用名不重复（不区分大小写）
	seen := map[string]int{}
	for i := range out {
		n := strings.ToLower(out[i].Name)
		seen[n]++
		if seen[n] > 1 {
			out[i].Name = fmt.Sprintf("%s-%d", out[i].Name, seen[n])
		}
	}
	return out
}

// sameRule 规则识别里和 s 一样的应用（类型、目录、构建命令、产物都一样）。
func sameRule(rules []Suggestion, s Suggestion) *Suggestion {
	for i, r := range rules {
		if r.Type == s.Type && r.Workdir == s.Workdir && r.Command == s.Command && r.Artifact == s.Artifact &&
			(s.Port == 0 || s.Port == r.Port) && strings.EqualFold(r.Name, s.Name) {
			return &rules[i]
		}
	}
	return nil
}
