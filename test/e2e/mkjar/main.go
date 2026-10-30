// 生成一个结构与 Spring Boot fat jar 相同的测试包：依赖 jar 未压缩存放在 BOOT-INF/lib（内容固定），
// 业务类随版本变化。用于验证增量上传：只改版本时应该只上传很少的字节。
//
//	go run ./test/e2e/mkjar <输出路径> <版本>
package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"math/rand"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "用法：mkjar <输出路径> <版本>")
		os.Exit(2)
	}
	out, version := os.Args[1], os.Args[2]
	f, err := os.Create(out)
	if err != nil {
		panic(err)
	}
	w := zip.NewWriter(f)
	write := func(name string, method uint16, data []byte) {
		h := &zip.FileHeader{Name: name, Method: method}
		fw, err := w.CreateHeader(h)
		if err != nil {
			panic(err)
		}
		fw.Write(data)
	}
	write("META-INF/MANIFEST.MF", zip.Deflate, []byte("Manifest-Version: 1.0\nMain-Class: org.springframework.boot.loader.launch.JarLauncher\nStart-Class: demo.App\n"))
	// 真的 class 文件头：版本 61（Java 17），ferry 据此检查服务器上的 java 版本
	write("BOOT-INF/classes/demo/App.class", zip.Deflate, append([]byte{0xCA, 0xFE, 0xBA, 0xBE, 0, 0, 0, 61}, []byte("class App "+version)...))
	for i := 0; i < 24; i++ {
		name := fmt.Sprintf("lib-%02d-1.0.jar", i)
		if i == 0 {
			name = "demo-common-core-1.0.jar"
		}
		write("BOOT-INF/lib/"+name, zip.Store, innerJar(int64(i)))
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
}

// innerJar 内容由种子决定，每次生成都一样（服务器缓存才能复用）。
func innerJar(seed int64) []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	h := &zip.FileHeader{Name: fmt.Sprintf("lib%d/A.class", seed), Method: zip.Store}
	h.Modified = h.Modified.UTC() // 固定时间
	fw, _ := w.CreateHeader(h)
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(seed)).Read(data)
	fw.Write(data)
	w.Close()
	return b.Bytes()
}
