package agent

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

// 预检：java 应用用的 java 是什么版本，以及服务器上还有哪些 JDK（版本不够时告诉用户改用哪个）。

var reJavaVersion = regexp.MustCompile(`version "([^"]+)"`)

// parseJavaMajor 1.8.0_412 → 8；17.0.9 → 17；21 → 21。
func parseJavaMajor(v string) int {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == '+' })
	if len(parts) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(parts[0])
	if n == 1 && len(parts) > 1 {
		n, _ = strconv.Atoi(parts[1])
	}
	return n
}

func (a *Agent) javaInfo(p string) proto.Java {
	j := proto.Java{Path: p}
	out, err := a.Sys.JavaVersion(p)
	if m := reJavaVersion.FindStringSubmatch(out); m != nil {
		j.Version, j.Major = m[1], parseJavaMajor(m[1])
		return j
	}
	if err != nil {
		j.Error = err.Error()
	} else {
		j.Error = "读不出版本：" + strings.TrimSpace(out)
	}
	return j
}

// 常见的 JDK 安装位置。
var javaGlobs = []string{
	"/usr/lib/jvm/*/bin/java", "/usr/java/*/bin/java", "/usr/local/*/bin/java", "/usr/local/java/*/bin/java",
	"/opt/*/bin/java", "/opt/*/*/bin/java", "/home/*/runtime/*/bin/java", "/home/*/*/bin/java", "/root/*/bin/java", "/usr/bin/java",
}

var reJavaDir = regexp.MustCompile(`(?i)(?:jdk|jre|java|openjdk|temurin|zulu|corretto)[-_]?(?:1\.)?(\d{1,2})(?:\D|$)`)

// findJavas 服务器上的 JDK（按版本从新到旧）；skip 里的已经查过。
func (a *Agent) findJavas(skip map[string]bool) []proto.Java {
	seen := map[string]bool{}
	for p := range skip {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			seen[r] = true
		}
	}
	var out []proto.Java
	runs := 0
	for _, g := range javaGlobs {
		ms, _ := filepath.Glob(g)
		for _, p := range ms {
			real, err := filepath.EvalSymlinks(p)
			if err != nil || seen[real] || skip[p] {
				continue
			}
			seen[real] = true
			j := proto.Java{Path: p}
			// 目录名能看出版本就不启动 JVM；看不出时最多实际运行几个
			if m := reJavaDir.FindStringSubmatch(real); m != nil {
				j.Major, _ = strconv.Atoi(m[1])
			} else if runs < 6 {
				runs++
				j = a.javaInfo(p)
			}
			if j.Major > 0 {
				out = append(out, j)
			}
			if len(out) >= 12 {
				break
			}
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Major > out[k].Major })
	return out
}

// checkJavas 预检里调用：各 java 应用的 java 版本 + 服务器上的其他 JDK。
func (a *Agent) checkJavas(p *proto.Preflight) {
	paths := map[string]bool{}
	for _, s := range a.req.Apps {
		if s.Type == proto.TypeJava && s.Java != "" {
			paths[s.Java] = true
		}
	}
	if len(paths) == 0 {
		return
	}
	p.Javas = map[string]proto.Java{}
	for jp := range paths {
		p.Javas[jp] = a.javaInfo(jp)
	}
	p.JavaFound = a.findJavas(paths)
}
