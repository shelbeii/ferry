// 生成端到端测试用的「JDK」压缩包：结构与 Temurin 的 Linux JDK 相同（release、bin/java、目录内的符号链接），
// bin/java 遇到 -version 报指定版本，其他情况启动测试服务。
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"strings"
)

func main() {
	version, arch := "17.0.99+9", "x86_64"
	if len(os.Args) > 2 {
		version = os.Args[2]
	}
	if len(os.Args) > 3 {
		arch = os.Args[3]
	}
	base, _, _ := strings.Cut(version, "+")
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	top := "jdk-" + version + "/"
	add := func(name, body string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: top + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: top, Mode: 0o755, Typeflag: tar.TypeDir})
	add("release", fmt.Sprintf("IMPLEMENTOR=\"Eclipse Adoptium\"\nJAVA_RUNTIME_VERSION=%q\nJAVA_VERSION=%q\nOS_ARCH=%q\nOS_NAME=\"Linux\"\n", version, base, arch), 0o644)
	add("bin/java", "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo 'openjdk version \""+base+"\" 2026-01-01' >&2; exit 0; fi\n# 记下收到的参数（一行一个），检查 systemd 把启动命令拆成了什么\nprintf '%s\\n' \"$@\" >> /tmp/fakejava-args-$(id -un)\nexec /usr/local/bin/demoserver \"$@\"\n", 0o755)
	add("lib/modules", "modules", 0o644)
	tw.WriteHeader(&tar.Header{Name: top + "legal/LICENSE", Linkname: "../release", Typeflag: tar.TypeSymlink, Mode: 0o777})
	tw.Close()
	gz.Close()
	f.Close()
}
