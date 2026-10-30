// 生成端到端测试用的「JDK」压缩包：结构与 Temurin 的 Linux JDK 相同（release、bin/java、目录内的符号链接），
// bin/java 遇到 -version 报 17.0.99，其他情况启动测试服务。
package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
)

func main() {
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	top := "jdk-17.0.99+9/"
	add := func(name, body string, mode int64) {
		tw.WriteHeader(&tar.Header{Name: top + name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: top, Mode: 0o755, Typeflag: tar.TypeDir})
	add("release", "IMPLEMENTOR=\"Eclipse Adoptium\"\nJAVA_RUNTIME_VERSION=\"17.0.99+9\"\nJAVA_VERSION=\"17.0.99\"\nOS_ARCH=\"x86_64\"\nOS_NAME=\"Linux\"\n", 0o644)
	add("bin/java", "#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo 'openjdk version \"17.0.99\" 2026-01-01' >&2; exit 0; fi\nexec /usr/local/bin/demoserver \"$@\"\n", 0o755)
	add("lib/modules", "modules", 0o644)
	tw.WriteHeader(&tar.Header{Name: top + "legal/LICENSE", Linkname: "../release", Typeflag: tar.TypeSymlink, Mode: 0o777})
	tw.Close()
	gz.Close()
	f.Close()
}
