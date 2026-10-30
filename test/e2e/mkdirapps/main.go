// 端到端测试用：生成 Python、Node 服务端的测试应用（上传目录的类型）。依赖是本地的包（wheel、npm 的 tgz），
// 服务器上不用连外网就能装：
//
//	<out>/pyapp    server.py、requirements.txt（./vendor/hello_e2e-1.0-py3-none-any.whl）
//	<out>/nodeapp  server.js、package.json、package-lock.json（file:vendor/hello-e2e-1.0.0.tgz）
//
// 还各放一个开发时才有的目录（.venv、node_modules），检查它们不会被上传。
//
//	mkdirapps <out>
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：mkdirapps <输出目录>")
		os.Exit(2)
	}
	out := os.Args[1]
	if err := python(filepath.Join(out, "pyapp")); err != nil {
		fail(err)
	}
	if err := node(filepath.Join(out, "nodeapp")); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func write(p, text string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(text), 0o644)
}

func python(dir string) error {
	os.RemoveAll(dir)
	files := map[string]string{
		"hello_e2e/__init__.py":            "GREETING = 'hello from wheel'\n",
		"hello_e2e-1.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: hello-e2e\nVersion: 1.0\n",
		"hello_e2e-1.0.dist-info/WHEEL":    "Wheel-Version: 1.0\nGenerator: ferry-e2e\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var record strings.Builder
	for _, n := range []string{"hello_e2e/__init__.py", "hello_e2e-1.0.dist-info/METADATA", "hello_e2e-1.0.dist-info/WHEEL"} {
		w, _ := zw.Create(n)
		w.Write([]byte(files[n]))
		sum := sha256.Sum256([]byte(files[n]))
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", n, base64.RawURLEncoding.EncodeToString(sum[:]), len(files[n]))
	}
	record.WriteString("hello_e2e-1.0.dist-info/RECORD,,\n")
	w, _ := zw.Create("hello_e2e-1.0.dist-info/RECORD")
	w.Write([]byte(record.String()))
	if err := zw.Close(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "vendor"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor", "hello_e2e-1.0-py3-none-any.whl"), buf.Bytes(), 0o644); err != nil {
		return err
	}
	for p, text := range map[string]string{
		"requirements.txt": "./vendor/hello_e2e-1.0-py3-none-any.whl\n",
		"server.py": `import http.server, os
import hello_e2e

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = ("UP" if self.path == "/health" else hello_e2e.GREETING).encode()
        self.send_response(200)
        self.end_headers()
        self.wfile.write(body)

print("python app on", os.environ["PORT"])
http.server.HTTPServer(("127.0.0.1", int(os.environ["PORT"])), H).serve_forever()
`,
		".venv/junk":             "开发时的虚拟环境：不该上传\n",
		"__pycache__/server.pyc": "x",
	} {
		if err := write(filepath.Join(dir, p), text); err != nil {
			return err
		}
	}
	return nil
}

func node(dir string) error {
	os.RemoveAll(dir)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, c := range map[string]string{
		"package/package.json": `{"name":"hello-e2e","version":"1.0.0","main":"index.js"}`,
		"package/index.js":     "exports.greeting = 'hello from tgz'\n",
	} {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	gz.Close()
	if err := os.MkdirAll(filepath.Join(dir, "vendor"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor", "hello-e2e-1.0.0.tgz"), buf.Bytes(), 0o644); err != nil {
		return err
	}
	sum := sha512.Sum512(buf.Bytes())
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	for p, text := range map[string]string{
		"package.json": `{"name":"nodeapp","version":"1.0.0","private":true,"dependencies":{"hello-e2e":"file:vendor/hello-e2e-1.0.0.tgz"}}`,
		"package-lock.json": `{
  "name": "nodeapp",
  "version": "1.0.0",
  "lockfileVersion": 2,
  "requires": true,
  "packages": {
    "": {"name": "nodeapp", "version": "1.0.0", "dependencies": {"hello-e2e": "file:vendor/hello-e2e-1.0.0.tgz"}},
    "node_modules/hello-e2e": {"version": "1.0.0", "resolved": "file:vendor/hello-e2e-1.0.0.tgz", "integrity": "` + integrity + `"}
  },
  "dependencies": {
    "hello-e2e": {"version": "file:vendor/hello-e2e-1.0.0.tgz", "integrity": "` + integrity + `"}
  }
}
`,
		"server.js": `const http = require('http')
const hello = require('hello-e2e')
http.createServer((q, s) => s.end(q.url === '/health' ? 'UP' : hello.greeting)).listen(+process.env.PORT, '127.0.0.1')
console.log('node app on', process.env.PORT)
`,
		"node_modules/junk/index.js": "// 开发时的依赖：不该上传\n",
	} {
		if err := write(filepath.Join(dir, p), text); err != nil {
			return err
		}
	}
	return nil
}
