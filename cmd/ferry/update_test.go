package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/appdir"
	"github.com/captain/ferry/internal/update"
)

func TestUpdateCmdForceCheckAndGuard(t *testing.T) {
	asset, _ := update.Asset("0.18.9", runtime.GOOS, runtime.GOARCH)
	if asset == "" {
		t.Skip("当前平台没有安装包")
	}
	body := []byte("fake-installer")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pre := "/" + update.Repo + "/releases/"
		switch r.URL.Path {
		case pre + "latest":
			http.Redirect(w, r, "/"+update.Repo+"/releases/tag/v0.18.9", http.StatusFound)
		case "/repos/" + update.Repo + "/releases/tags/v0.18.9":
			fmt.Fprintf(w, `{"body":"## 修复说明","published_at":"2026-11-24T00:00:00Z","assets":[{"name":%q,"size":%d}]}`, asset, len(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("FERRY_HOME", t.TempDir())
	appdir.Reset()
	t.Cleanup(appdir.Reset)

	oldVer := Version
	Version = "0.18.9"
	t.Cleanup(func() { Version = oldVer })

	getenv := func(k string) string {
		switch k {
		case "FERRY_UPDATE_BASE", "FERRY_UPDATE_API":
			return srv.URL
		}
		return ""
	}
	execCLI := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := run(args, &cliIO{
			ctx:    context.Background(),
			stdout: &stdout,
			stderr: &stderr,
			getenv: getenv,
		})
		return code, stdout.String() + stderr.String()
	}

	// 同版本不带 --force：check 不返回 notes，download 被拒绝
	code, out := execCLI("update", "check", "--json")
	if code != 0 {
		t.Fatalf("update check 失败（%d）：%s", code, out)
	}
	var res struct {
		OK   bool        `json:"ok"`
		Data update.Info `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("解析输出失败：%v（%s）", err, out)
	}
	if !res.OK || res.Data.Newer || res.Data.Notes != "" {
		t.Fatalf("普通 check 同版本结果不对：%+v", res)
	}

	code, out = execCLI("update", "download", "--json")
	if code == 0 || !strings.Contains(out, "已是最新版本") {
		t.Fatalf("同版本不带 --force 应拒绝下载：code=%d out=%s", code, out)
	}

	// 同版本带 --force：check 返回 notes 与 size，download 会继续走到校验阶段（不会被「已是最新版本」拦下）
	code, out = execCLI("update", "check", "--force", "--json")
	if code != 0 {
		t.Fatalf("update check --force 失败（%d）：%s", code, out)
	}
	res = struct {
		OK   bool        `json:"ok"`
		Data update.Info `json:"data"`
	}{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("解析输出失败：%v（%s）", err, out)
	}
	if !res.OK || res.Data.Newer || res.Data.Notes != "## 修复说明" || res.Data.Size != int64(len(body)) {
		t.Fatalf("check --force 结果不对：%+v", res)
	}

	// download --force 不会被「已是最新版本」拦截，而是继续验证签名/校验和（这里 mock 没放 SHA256SUMS.txt，所以应报取校验和失败）
	code, out = execCLI("update", "download", "--force", "--json")
	if code == 0 || strings.Contains(out, "已是最新版本") || !strings.Contains(out, "取校验和失败") {
		t.Fatalf("download --force 应跳过版本拦截并严格校验签名：code=%d out=%s", code, out)
	}
}
