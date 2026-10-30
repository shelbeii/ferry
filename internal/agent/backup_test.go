package agent

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func backupAgent(t *testing.T) (*fakeSys, *Agent) {
	f := newFake(t)
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root, FerryDir: t.TempDir()}}
	return f, a
}

func gunzipFile(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestDumpCommand(t *testing.T) {
	my := proto.BackupItem{Name: "db", Kind: "mysql", Database: "shop"}
	got := dumpCommand(my, nil, false)
	for _, want := range []string{"mysqldump", "--single-transaction", "-u 'root'", "-h '127.0.0.1' -P 3306", "'shop'"} {
		if !strings.Contains(got, want) {
			t.Errorf("mysql 备份命令里要有 %q：%s", want, got)
		}
	}
	if got := dumpCommand(my, nil, true); !strings.Contains(got, "command -v mysql ") || strings.Contains(got, "mysqldump") {
		t.Errorf("恢复用 mysql：%s", got)
	}
	// 容器里：不写 host 就不加 -h；密码没写就用容器的 MYSQL_ROOT_PASSWORD；envFile 里的变量用 -e 传进去
	my.Container = "1Panel-mysql-abc"
	got = dumpCommand(my, []string{"MYSQL_PWD=x"}, false)
	if !strings.HasPrefix(got, "docker exec -i -e 'MYSQL_PWD' '1Panel-mysql-abc' sh -c ") || strings.Contains(got, "-h ") ||
		!strings.Contains(got, "MYSQL_ROOT_PASSWORD") {
		t.Errorf("容器里的 mysql：%s", got)
	}
	pg := proto.BackupItem{Name: "pg", Kind: "postgres", Database: "app", Container: "pg"}
	got = dumpCommand(pg, nil, false)
	if !strings.Contains(got, "pg_dump --clean --if-exists") || !strings.Contains(got, "POSTGRES_USER:-postgres") || !strings.Contains(got, "POSTGRES_PASSWORD") {
		t.Errorf("容器里的 postgres：%s", got)
	}
	pg.Container, pg.User, pg.Port = "", "app", 6432
	if got = dumpCommand(pg, nil, true); !strings.Contains(got, "psql -q -v ON_ERROR_STOP=1 -U 'app' -h '127.0.0.1' -p 6432 -d 'app'") {
		t.Errorf("postgres 恢复：%s", got)
	}
}

func TestBackupRunKeepRestore(t *testing.T) {
	f, a := backupAgent(t)
	env := filepath.Join(t.TempDir(), "db.env")
	os.WriteFile(env, []byte("MYSQL_PWD=secret\n"), 0o600)
	it := proto.BackupItem{Name: "db", Kind: "mysql", Database: "shop", EnvFiles: []string{env}, OnCalendar: "*-*-* 03:00:00", Keep: 2}
	dump := dumpCommand(it, []string{"MYSQL_PWD=secret"}, false)
	f.pipeOut = map[string]string{dump: "CREATE TABLE t;\n"}
	spec := func(action string) *proto.BackupSpec {
		return &proto.BackupSpec{Action: action, Project: "shop", Target: "prod", Items: []proto.BackupItem{it}, Name: "db"}
	}

	// 手动备份 3 次：只留 2 份；密码从 envFile 来
	for i := 0; i < 3; i++ {
		a.req.Backup = spec("run")
		if rc := a.backup(); rc != 0 {
			t.Fatalf("备份失败：%v", f.calls)
		}
		f.now = f.now.Add(time.Hour)
	}
	if e := f.pipeEnv[dump]; len(e) != 1 || e[0] != "MYSQL_PWD=secret" {
		t.Fatalf("环境变量：%v", e)
	}
	files := listBackupFiles(f.root, "db")
	if len(files) != 2 {
		t.Fatalf("只留 2 份：%+v", files)
	}
	if got := gunzipFile(t, filepath.FromSlash(files[0].Path)); got != "CREATE TABLE t;\n" {
		t.Fatalf("备份的内容：%q", got)
	}

	// 恢复：先备份一份现在的（不删掉要恢复的那份），再把备份的内容送进 mysql
	oldest := files[1].Name
	a.req.Backup = spec("restore")
	a.req.Backup.File = oldest
	if rc := a.backup(); rc != 0 {
		t.Fatal("恢复失败")
	}
	restore := dumpCommand(it, []string{"MYSQL_PWD=secret"}, true)
	if f.pipeIn[restore] != "CREATE TABLE t;\n" {
		t.Fatalf("恢复时送进去的：%q", f.pipeIn[restore])
	}
	files = listBackupFiles(f.root, "db")
	names := []string{}
	for _, x := range files {
		names = append(names, x.Name)
	}
	if !strings.Contains(strings.Join(names, " "), "-pre-restore.sql.gz") || !strings.Contains(strings.Join(names, " "), oldest) {
		t.Fatalf("恢复前的备份、要恢复的那份都要在：%v", names)
	}

	// 备份失败：记下来，监控能看到
	f.pipeFail = map[string]string{dump: "mysqldump: Got error: 1045: Access denied"}
	a.req.Backup = spec("on")
	if rc := a.backup(); rc != 0 {
		t.Fatal("打开失败")
	}
	a.req.Backup = spec("run")
	if rc := a.backup(); rc == 0 {
		t.Fatal("备份失败时要返回失败")
	}
	probs := backupProblems(f.root, "shop", "prod")
	if len(probs) != 1 || !strings.Contains(probs[0].Text, "Access denied") {
		t.Fatalf("监控要看到失败：%+v", probs)
	}
	// 不让恢复用不存在的、别的项目的文件
	a.req.Backup = spec("restore")
	a.req.Backup.File = "../../etc/passwd"
	if rc := a.backup(); rc == 0 {
		t.Fatal("不该恢复别的文件")
	}
}

func TestBackupOnOffBeforeDeploy(t *testing.T) {
	f, a := backupAgent(t)
	it := proto.BackupItem{Name: "data", Kind: "command", Command: "cat /srv/data", OnCalendar: "*-*-* 03:00:00", Keep: 7, BeforeDeploy: true}
	f.pipeOut = map[string]string{"cat /srv/data": "hello"}
	spec := func(action string, items ...proto.BackupItem) *proto.BackupSpec {
		return &proto.BackupSpec{Action: action, Project: "shop", Target: "prod", Items: items}
	}
	// 关着：部署前不备份
	a.req.Backup = spec("before", it)
	if rc := a.backup(); rc != 0 || len(listBackupFiles(f.root, "")) != 0 {
		t.Fatal("关着时部署前不备份")
	}
	// 打开：装上定时器
	a.req.Backup = spec("on", it)
	if rc := a.backup(); rc != 0 {
		t.Fatal("打开失败")
	}
	u := backupUnitBase(f.root, "data")
	timer, err := os.ReadFile(filepath.Join(f.unitDir, u+".timer"))
	if err != nil || !strings.Contains(string(timer), "OnCalendar=*-*-* 03:00:00") {
		t.Fatalf("定时器：%s %v", timer, err)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "enable --now "+u+".timer") {
		t.Fatalf("要打开定时器：%v", f.calls)
	}
	// 部署前：备份一次
	a.req.Backup = spec("before", it)
	if rc := a.backup(); rc != 0 || len(listBackupFiles(f.root, "data")) != 1 {
		t.Fatal("开着时部署前要备份")
	}
	// 配置里换了名字：部署时同步，旧的定时器去掉
	it2 := it
	it2.Name, it2.BeforeDeploy = "files", false
	a.req.Backup = spec("before", it2)
	if rc := a.backup(); rc != 0 {
		t.Fatal("同步失败")
	}
	if _, err := os.Stat(filepath.Join(f.unitDir, u+".timer")); err == nil {
		t.Fatal("配置里没有了的定时器要去掉")
	}
	if _, err := os.Stat(filepath.Join(f.unitDir, backupUnitBase(f.root, "files")+".timer")); err != nil {
		t.Fatal("新的要装上")
	}
	// 状态
	a.req.Backup = spec("status", it2)
	a.backup()
	// 关闭：定时器去掉，文件留着
	a.req.Backup = spec("off", it2)
	if rc := a.backup(); rc != 0 {
		t.Fatal("关闭失败")
	}
	ents, _ := os.ReadDir(f.unitDir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), backupUnitPrefix) {
			t.Fatalf("关闭后还有 %s", e.Name())
		}
	}
	if len(listBackupFiles(f.root, "")) != 1 {
		t.Fatal("已有的备份文件留着")
	}
	// 定时器运行时：关着就不备份
	if rc := BackupRun(f, io.Discard, f.root, "files"); rc != 0 || len(listBackupFiles(f.root, "files")) != 0 {
		t.Fatal("关着时定时器不备份")
	}
}

func TestPathsBackupRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("服务器上的路径")
	}
	f, a := backupAgent(t)
	dir := filepath.Join(t.TempDir(), "uploads")
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "1.txt"), []byte("one"), 0o640)
	os.Symlink("a/1.txt", filepath.Join(dir, "link"))
	it := proto.BackupItem{Name: "uploads", Kind: "paths", Paths: []string{dir}, Keep: 7}
	r := a.runBackup(f.root, it, "手动", "", "")
	if !r.OK {
		t.Fatal(r.Error)
	}
	// 改坏：多一个文件、内容变了
	os.WriteFile(filepath.Join(dir, "a", "1.txt"), []byte("changed"), 0o640)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644)
	a.req.Backup = &proto.BackupSpec{Action: "restore", Project: "p", Target: "t", Items: []proto.BackupItem{it}, Name: "uploads", File: r.File}
	if rc := a.backup(); rc != 0 {
		t.Fatal("恢复失败")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a", "1.txt")); string(b) != "one" {
		t.Fatalf("内容：%q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Fatal("备份之后才有的文件要去掉")
	}
	if l, _ := os.Readlink(filepath.Join(dir, "link")); l != "a/1.txt" {
		t.Fatalf("链接：%q", l)
	}
	if st, _ := os.Stat(filepath.Join(dir, "a", "1.txt")); st.Mode().Perm() != 0o640 {
		t.Fatalf("权限：%v", st.Mode())
	}
}
