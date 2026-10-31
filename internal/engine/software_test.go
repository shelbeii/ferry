package engine

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
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
		return swtpl.Vars{"name": "x", "dir": "/opt/ferry-software/x", "user": "u", "host": "h", "version": "1", "java": "/j", "package": "", "account": "root"}
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
	// 有密码的软件：就绪后用密码登录一次；密码只写在文件里，不出现在命令行上
	for _, id := range []string{"mysql", "mariadb", "postgresql", "redis"} {
		tp := find(id)
		vs := base()
		for _, p := range tp.Params {
			vs[p.Key] = p.Default
		}
		vs["password"], vs["public"], vs["lowerCase"] = "Secret-123", "false", "false"
		pl, err := renderPlan(tp, &tp.Variants[0], vs, &swpkg.Meta{File: "m.tar.xz"})
		if err != nil {
			t.Fatal(err)
		}
		check := false
		for _, s := range pl.Steps {
			check = check || s.At == "ready" && (strings.Contains(s.Error, "登录不上") || strings.Contains(s.Error, "连不上"))
			if strings.Contains(s.Run, "Secret-123") {
				t.Errorf("%s：密码出现在命令里：%s", id, s.Run)
			}
		}
		if !check {
			t.Errorf("%s：没有就绪后用密码登录的步骤", id)
		}
	}
	// MySQL：选的账号有全部权限；允许其他机器时是这个账号能远程登录
	vs = base()
	vs["port"], vs["public"], vs["password"], vs["lowerCase"], vs["account"] = "3306", "true", "Secret-123", "false", "app_user"
	pl, _ = renderPlan(my, &my.Variants[0], vs, &swpkg.Meta{File: "m.tar.xz"})
	sql := ""
	for _, f := range pl.Files {
		if strings.HasSuffix(f.Path, "ferry-init.sql") {
			sql = f.Content
		}
	}
	for _, want := range []string{"'app_user'@'localhost' IDENTIFIED BY 'Secret-123'", "TO 'app_user'@'%' WITH GRANT OPTION", "ALTER USER 'root'@'localhost'"} {
		if !strings.Contains(sql, want) {
			t.Errorf("mysql 账号：没有 %s：%s", want, sql)
		}
	}
	if strings.Contains(sql, "'root'@'%'") {
		t.Errorf("账号不是 root 时 root 不能远程登录：%s", sql)
	}
	// MySQL 需要的系统库：从软件源装（Ubuntu 24.04 起包名不同）
	if sy := my.Variants[0].System; sy == nil || !slices.Contains(sy.DNF, "libaio") || !slices.Contains(sy.APT, "libaio1t64|libaio1") {
		t.Errorf("mysql 的系统库：%+v", sy)
	}
	// Nacos：设置管理员密码的步骤标明 sets: password
	nc := find("nacos")
	sets := false
	for _, s := range nc.Variants[0].Steps {
		sets = sets || s.Sets == "password" && s.Soft != ""
	}
	if !sets {
		t.Error("nacos：设置密码的步骤要写 sets: password")
	}
}

func TestPublicMode(t *testing.T) {
	for id, want := range map[string]map[string]string{
		"mysql": {"true": "chosen", "false": ""},
		"nacos": {"true": "always", "false": "always"},
	} {
		tp, err := swtpl.Find(t.TempDir(), id)
		if err != nil {
			t.Fatal(err)
		}
		for pub, w := range want {
			if got := publicMode(tp, swtpl.Vars{"public": pub}); got != w {
				t.Errorf("%s public=%s：%q，期望 %q", id, pub, got, w)
			}
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

func TestPortFreeAndDiskRoom(t *testing.T) {
	rep := proto.SoftwareReport{Ports: []int{8848, 9849}, UDPPorts: []int{9000}}
	nacos := swtpl.Param{Key: "port", Type: "port", Offsets: []int{1000}}
	if portFree(nacos, 8848, nil, nil, rep) || !portFree(nacos, 8850, nil, nil, rep) || portFree(nacos, 8849, nil, nil, rep) {
		t.Error("nacos 端口")
	}
	srt := swtpl.Param{Key: "srt", Type: "port", Proto: "udp"}
	if portFree(srt, 9000, nil, nil, rep) || !portFree(srt, 8848, nil, nil, rep) {
		t.Error("UDP 端口")
	}
	if portFree(nacos, 8850, []int{9850}, nil, rep) {
		t.Error("和前面的端口参数重复")
	}

	ok := proto.SwCheck{FreeMB: 10000, Inodes: -1, StageFreeMB: 10000}
	if err := diskRoom("a", "/opt/x", ok, 220<<20); err != nil {
		t.Error(err)
	}
	for what, c := range map[string]proto.SwCheck{
		"只读":    {FreeMB: 10000, Inodes: -1, ReadOnly: true},
		"空间不够":  {FreeMB: 500, Inodes: -1, StageFreeMB: 10000},
		"inode": {FreeMB: 10000, Inodes: 100, StageFreeMB: 10000},
		"上传目录":  {FreeMB: 10000, Inodes: -1, StageFreeMB: 50},
	} {
		if err := diskRoom("a", "/opt/x", c, 220<<20); err == nil {
			t.Errorf("%s：应当停下", what)
		}
	}
	if err := diskRoom("a", "/opt/x", proto.SwCheck{FreeMB: -1, Inodes: -1, StageFreeMB: -1}, 1<<30); err != nil {
		t.Errorf("不知道空间时不拦：%v", err)
	}
	for s, want := range map[string]int64{"512m": 512, "2g": 2048, "x": 0, "": 0} {
		if memoryMB(s) != want {
			t.Errorf("%s：%d", s, memoryMB(s))
		}
	}
}
