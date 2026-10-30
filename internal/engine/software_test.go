package engine

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/swpkg"
	"github.com/captain/ferry/internal/swtpl"
)

// 每个内置模板的每种装法都能渲染出安装计划（没有漏掉的变量、端口是数字）。
func TestRenderBuiltin(t *testing.T) {
	for _, tpl := range swtpl.Builtin() {
		for i := range tpl.Variants {
			v := &tpl.Variants[i]
			vars := swtpl.Vars{"name": tpl.ID, "dir": "/opt/ferry-software/" + tpl.ID, "user": v.User, "host": "10.0.0.1",
				"version": "1.0.0", "java": "/usr/local/lib/ferry/runtimes/java-17/bin/java", "package": "/x/pkg"}
			for _, p := range tpl.Params {
				val := p.Default
				switch p.Type {
				case "password":
					val = "Passw0rd-1"
				case "bool":
					val = "true"
				}
				vars[p.Key] = val
			}
			var pkg *swpkg.Meta
			if v.Package != nil {
				pkg = &swpkg.Meta{File: "pkg.tar.gz", SHA256: strings.Repeat("a", 64), Size: 1}
			}
			pl, err := renderPlan(tpl, v, vars, pkg)
			if err != nil {
				t.Errorf("%s / %s：%v", tpl.ID, v.Label, err)
				continue
			}
			if strings.Contains(pl.Exec, "${") && tpl.ID != "sentinel" {
				t.Errorf("%s：启动命令里还有没换掉的变量：%s", tpl.ID, pl.Exec)
			}
		}
	}
}

func TestRenderDetails(t *testing.T) {
	find := func(id string) *swtpl.Template {
		tp, err := swtpl.Find(t.TempDir(), id)
		if err != nil {
			t.Fatal(err)
		}
		return tp
	}
	base := func() swtpl.Vars {
		return swtpl.Vars{"name": "x", "dir": "/opt/ferry-software/x", "user": "u", "host": "h", "version": "1", "java": "/j", "package": ""}
	}
	// Sentinel：密码只在环境变量文件里，启动命令里留给 systemd 展开
	st := find("sentinel")
	vs := base()
	vs["port"], vs["public"], vs["memory"], vs["password"] = "8858", "false", "256m", "Secret-123"
	pl, err := renderPlan(st, &st.Variants[0], vs, &swpkg.Meta{File: "s.jar"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pl.Exec, "-Dsentinel.dashboard.auth.password=${SENTINEL_PASSWORD}") || strings.Contains(pl.Exec, "Secret-123") ||
		pl.Env["SENTINEL_PASSWORD"] != "Secret-123" || !strings.Contains(pl.Exec, "-Dserver.address=127.0.0.1") {
		t.Errorf("sentinel：%s %v", pl.Exec, pl.Env)
	}
	// MySQL：允许其他机器时多建 root@'%'；不允许时不建
	my := find("mysql")
	for _, public := range []string{"true", "false"} {
		vs := base()
		vs["port"], vs["public"], vs["password"], vs["lowerCase"] = "3306", public, "Secret-123", "false"
		pl, err := renderPlan(my, &my.Variants[0], vs, &swpkg.Meta{File: "m.tar.xz"})
		if err != nil {
			t.Fatal(err)
		}
		var sql, cnf string
		for _, f := range pl.Files {
			if strings.HasSuffix(f.Path, "ferry-init.sql") {
				sql = f.Content
			}
			if strings.HasSuffix(f.Path, "my.cnf") && f.At == "" {
				cnf = f.Content
			}
		}
		remote := strings.Contains(sql, "'root'@'%'")
		if remote != (public == "true") || !strings.Contains(sql, "IDENTIFIED BY 'Secret-123'") {
			t.Errorf("public=%s：%s", public, sql)
		}
		if bind := map[string]string{"true": "bind-address=*", "false": "bind-address=127.0.0.1"}[public]; !strings.Contains(cnf, bind) {
			t.Errorf("public=%s：my.cnf 里没有 %s", public, bind)
		}
	}
	// PostgreSQL：只有允许其他机器时才加 pg_hba 那一步
	pg := find("postgresql")
	for _, public := range []string{"true", "false"} {
		vs := base()
		vs["port"], vs["public"], vs["password"] = "5432", public, "Secret-123"
		pl, _ := renderPlan(pg, &pg.Variants[0], vs, nil)
		has := false
		for _, s := range pl.Steps {
			has = has || strings.Contains(s.Run, "pg_hba.conf")
		}
		if has != (public == "true") {
			t.Errorf("postgresql public=%s：pg_hba 步骤 %v", public, has)
		}
	}
}

// fakeELF 只有文件头的 64 位 Linux 可执行文件。
func fakeELF(t *testing.T, m elf.Machine) string {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[elf.EI_CLASS], b[elf.EI_DATA], b[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(b[18:], uint16(m))
	binary.LittleEndian.PutUint32(b[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(b[52:], 64)
	p := filepath.Join(t.TempDir(), "minio")
	os.WriteFile(p, b, 0o644)
	return p
}

// 装单个程序文件前比对 CPU 架构：不对就直接报错（不上传）。
func TestCheckProgramArch(t *testing.T) {
	if err := checkProgramArch(fakeELF(t, elf.EM_AARCH64), "arm64"); err != nil {
		t.Errorf("架构一致：%v", err)
	}
	err := checkProgramArch(fakeELF(t, elf.EM_X86_64), "arm64")
	if err == nil || !strings.Contains(err.Error(), "minio 是 amd64 版的程序，服务器是 arm64") || !strings.Contains(err.Error(), "Linux arm64 版") {
		t.Errorf("amd64 程序装到 arm64：%v", err)
	}
	if err := checkProgramArch(fakeELF(t, elf.EM_ARM), "arm64"); err == nil || !strings.Contains(err.Error(), "服务器是 arm64") {
		t.Errorf("32 位 ARM：%v", err)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "minio.exe")
	os.WriteFile(exe, []byte("MZ\x90\x00"), 0o644)
	if err := checkProgramArch(exe, "amd64"); err == nil || !strings.Contains(err.Error(), "不是 Linux 程序") {
		t.Errorf("Windows 程序：%v", err)
	}
	sh := filepath.Join(dir, "run.sh")
	os.WriteFile(sh, []byte("#!/bin/sh\necho hi\n"), 0o755)
	if err := checkProgramArch(sh, "amd64"); err != nil {
		t.Errorf("脚本不管架构：%v", err)
	}
}
