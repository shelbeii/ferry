// 生成端到端测试用的软件安装包：文件名、结构与官方的一样，程序换成测试服务（由假 java 启动）。
//
//	mkpkg sentinel <目录>          sentinel-dashboard-1.8.10.jar
//	mkpkg nacos <目录> [版本]      nacos-server-<版本>.tar.gz（版本 0.0.1：缺 conf，安装时出错，测试撤销）
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
)

func main() {
	kind, dir := os.Args[1], os.Args[2]
	switch kind {
	case "sentinel":
		f, err := os.Create(filepath.Join(dir, "sentinel-dashboard-1.8.10.jar"))
		if err != nil {
			panic(err)
		}
		z := zip.NewWriter(f)
		w, _ := z.Create("BOOT-INF/classes/com/alibaba/csp/sentinel/dashboard/DashboardApplication.class")
		w.Write([]byte("fake"))
		z.Close()
		f.Close()
	case "nacos":
		ver := "3.2.4"
		if len(os.Args) > 3 {
			ver = os.Args[3]
		}
		f, err := os.Create(filepath.Join(dir, "nacos-server-"+ver+".tar.gz"))
		if err != nil {
			panic(err)
		}
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		add := func(name, body string) {
			tw.WriteHeader(&tar.Header{Name: "nacos/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
		add("target/nacos-server.jar", "fake")
		if ver != "0.0.1" {
			add("conf/application.properties", "nacos.server.main.port=8848\nnacos.console.port=8080\nnacos.core.auth.plugin.nacos.token.secret.key=\nnacos.core.auth.server.identity.key=\nnacos.core.auth.server.identity.value=\n")
			add("conf/nacos-logback.xml", "<configuration/>\n")
		}
		tw.Close()
		gz.Close()
		f.Close()
	}
}
