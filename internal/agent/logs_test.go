package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

// logApp 日志写在运行中的版本目录里（${logs}）的服务。
func logApp() proto.AppSpec {
	s := javaApp("Job")
	s.SharedDirs = nil
	return s
}

func appPath(f *fakeSys, app, rel string) string {
	return filepath.Join(f.root, "apps", app, filepath.FromSlash(rel))
}

// logPath 当前运行的日志文件：running/<rel>。
func logPath(f *fakeSys, app, rel string) string { return appPath(f, app, "running/"+rel) }

func writeLog(t *testing.T, f *fakeSys, app, rel, text string) {
	t.Helper()
	p := logPath(f, app, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runs(t *testing.T, f *fakeSys, s proto.AppSpec) []proto.LogRun {
	t.Helper()
	r := exec1(t, f, "logruns", &proto.Request{Apps: []proto.AppSpec{s}})
	var out []proto.LogRun
	if r.code != 0 || json.Unmarshal(r.result.Data, &out) != nil {
		t.Fatalf("logruns：\n%s", r.dump())
	}
	return out
}

func readLog(t *testing.T, f *fakeSys, s proto.AppSpec, rel, run, file string) proto.LogText {
	t.Helper()
	r := exec1(t, f, "logread", &proto.Request{Apps: []proto.AppSpec{s}, LogRelease: rel, LogRun: run, LogFile: file, Lines: 2})
	var out proto.LogText
	if r.code != 0 || json.Unmarshal(r.result.Data, &out) != nil {
		t.Fatalf("logread %s %s %s：\n%s", rel, run, file, r.dump())
	}
	return out
}

func TestLogsStayWithRelease(t *testing.T) {
	f := newFake(t)
	s := logApp()
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"Job": []byte("v1")})); r.code != 0 {
		t.Fatalf("v1：\n%s", r.dump())
	}
	writeLog(t, f, "Job", "logs/job/app.log", "v1 第 1 行\nv1 第 2 行\nv1 第 3 行\n")

	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{"Job": []byte("v2")}))
	if r.code != 0 || !r.has(proto.EvLogs, id1+" 这次运行的日志（1 个文件") {
		t.Fatalf("v2 应记下 v1 的日志：\n%s", r.dump())
	}
	// v1 放回 versions/，日志和程序一起；running/ 是 v2，日志从空的开始
	if b, err := os.ReadFile(appPath(f, "Job", "versions/"+id1+"/logs/job/app.log")); err != nil || !strings.Contains(string(b), "v1 第 3 行") {
		t.Fatalf("v1 的日志应和程序一起在 versions/%s/logs/ 下：%v", id1, err)
	}
	if b, _ := os.ReadFile(appPath(f, "Job", "versions/"+id1+"/Job.jar")); string(b) != "v1" {
		t.Fatal("v1 的程序应放回 versions/")
	}
	if b, _ := os.ReadFile(appPath(f, "Job", "running/Job.jar")); string(b) != "v2" {
		t.Fatal("running/ 应是 v2")
	}
	if fi, err := os.Stat(appPath(f, "Job", "running/logs")); err != nil || !fi.IsDir() || hasFiles(appPath(f, "Job", "running/logs")) {
		t.Fatal("v2 应从空的日志目录开始")
	}
	// running/ 和 versions/<id>/ 里只有程序和日志，Ferry 的文件在 .ferry/
	for _, d := range []string{"running", "versions/" + id1} {
		ents, _ := os.ReadDir(appPath(f, "Job", d))
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if strings.Join(names, ",") != "Job.jar,logs" {
			t.Errorf("%s 里应只有程序和日志：%v", d, names)
		}
	}
	writeLog(t, f, "Job", "logs/job/app.log", "v2 第 1 行\n")

	rs := runs(t, f, s)
	if len(rs) != 2 || !rs[0].Current || rs[0].Release != id2 || rs[0].Start.IsZero() || len(rs[0].Files) != 1 || rs[0].Dir != appPath(f, "Job", "running") {
		t.Fatalf("当前运行：%+v", rs)
	}
	old := rs[1]
	if old.Release != id1 || old.ID == "" || old.Start.IsZero() || old.End.IsZero() || len(old.Files) != 1 || old.Files[0].Path != "logs/job/app.log" || old.Size == 0 || old.Dir != appPath(f, "Job", "versions/"+id1) {
		t.Fatalf("v1 的运行：%+v", old)
	}
	if got := readLog(t, f, s, id1, old.ID, "logs/job/app.log"); !got.Truncated || strings.Join(got.Lines, "|") != "v1 第 2 行|v1 第 3 行" {
		t.Fatalf("读 v1：%+v", got)
	}
	if got := readLog(t, f, s, "", "", "logs/job/app.log"); strings.Join(got.Lines, "|") != "v2 第 1 行" {
		t.Fatalf("读当前：%+v", got)
	}
	rels := exec1(t, f, "releases", &proto.Request{Apps: []proto.AppSpec{s}})
	if !strings.Contains(string(rels.result.Data), `"logRuns":1`) {
		t.Fatalf("版本列表应带运行次数：%s", rels.result.Data)
	}

	// 手动回滚到 v1：v1 上一次运行的日志移到 .ferry/versions/<v1>/runs/<那次结束的时间>/，v1 从空的 logs/ 重新开始
	f.now = f.now.Add(2e9)
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{s}}); r.code != 0 || !r.has(proto.EvLogs, "上一次运行的日志移到了") {
		t.Fatalf("回滚：\n%s", r.dump())
	}
	stash := appPath(f, "Job", "../../.ferry/apps/Job/versions/"+id1+"/runs/"+old.ID+"/logs/job/app.log")
	if b, err := os.ReadFile(stash); err != nil || !strings.Contains(string(b), "v1 第 3 行") {
		t.Fatalf("v1 上一次运行的日志应在 %s：%v", stash, err)
	}
	if hasFiles(appPath(f, "Job", "running/logs")) {
		t.Fatal("v1 再次运行应从空的 logs/ 开始")
	}
	if b, _ := os.ReadFile(appPath(f, "Job", "running/Job.jar")); string(b) != "v1" {
		t.Fatal("回滚后 running/ 应是 v1")
	}
	if b, err := os.ReadFile(appPath(f, "Job", "versions/"+id2+"/logs/job/app.log")); err != nil || string(b) != "v2 第 1 行\n" {
		t.Fatalf("v2 的日志应和程序一起放回 versions/：%v", err)
	}
	rs = runs(t, f, s)
	if len(rs) != 3 || rs[0].Release != id1 || !rs[0].Current || rs[1].Release != id2 || rs[2].Release != id1 || !strings.Contains(rs[2].Dir, ".ferry") {
		t.Fatalf("回滚后：%+v", rs)
	}
	if got := readLog(t, f, s, id1, old.ID, "logs/job/app.log"); strings.Join(got.Lines, "|") != "v1 第 2 行|v1 第 3 行" {
		t.Fatalf("移动后读 v1 以前的运行：%+v", got)
	}

	// 部署失败自动回滚：v1 这次运行的日志留在 running/（失败的 v3 放回 versions/ 后，v1 再回到 running/）
	writeLog(t, f, "Job", "logs/job/app.log", "v1 回滚后运行\n")
	f.bad[id3] = true
	f.now = f.now.Add(2e9)
	r = exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id3, map[string][]byte{"Job": []byte("v3")}))
	if r.code != proto.CodeFailed || f.current("Job") != id1 {
		t.Fatalf("v3 应失败并切回 v1：\n%s", r.dump())
	}
	if b, _ := os.ReadFile(appPath(f, "Job", "running/Job.jar")); string(b) != "v1" {
		t.Fatal("切回后 running/ 应是 v1")
	}
	if _, err := os.Stat(appPath(f, "Job", "versions/"+id3+"/Job.jar")); err != nil {
		t.Fatal("失败的 v3 应放回 versions/")
	}
	var v1runs, v3runs int
	for _, x := range runs(t, f, s) {
		if x.Release == id3 {
			v3runs++
		}
		if x.Release == id1 && !x.Current {
			v1runs++
		}
	}
	if v3runs != 0 {
		t.Fatal("崩溃的版本没写日志（假系统里），不应产生空记录")
	}
	if v1runs != 2 {
		t.Fatalf("v1 应有两次以前的运行：%+v", runs(t, f, s))
	}

	// 越界的路径被拒绝
	for _, bad := range []string{"../x", "/etc/passwd", "logs/../../x"} {
		if r := exec1(t, f, "logread", &proto.Request{Apps: []proto.AppSpec{s}, LogFile: bad}); r.code != proto.CodeBadInput {
			t.Errorf("%q 应拒绝：%d %s", bad, r.code, r.result.Msg)
		}
	}
}

// 日志写在 data 下的配置（archiveLogs）：切换时移进离开的版本目录
func TestDataLogsMovedIntoRelease(t *testing.T) {
	f := newFake(t)
	s := javaApp("Job")
	s.ArchiveLogs = []string{"logs"}
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id1, map[string][]byte{"Job": []byte("v1")})); r.code != 0 {
		t.Fatalf("v1：\n%s", r.dump())
	}
	data := appPath(f, "Job", "data/logs")
	os.MkdirAll(filepath.Join(data, "job"), 0o755)
	os.WriteFile(filepath.Join(data, "job", "app.log"), []byte("v1 写在 data 下\n"), 0o644)
	rs := runs(t, f, s)
	if len(rs) != 1 || len(rs[0].Files) != 1 || rs[0].Dir != appPath(f, "Job", "data") {
		t.Fatalf("当前运行应读 data：%+v", rs)
	}
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{"Job": []byte("v2")})); r.code != 0 {
		t.Fatalf("v2：\n%s", r.dump())
	}
	// running/logs 为空（运行用户创建的日志目录），data 下的日志移进了 v1 的目录
	if b, err := os.ReadFile(appPath(f, "Job", "versions/"+id1+"/logs/job/app.log")); err != nil || string(b) != "v1 写在 data 下\n" {
		t.Fatalf("data 下的日志应移进 v1 的目录：%v", err)
	}
	if fi, err := os.Stat(data); err != nil || !fi.IsDir() || hasFiles(data) {
		t.Fatal("data/logs 应从空的开始")
	}
	rs = runs(t, f, s)
	if len(rs) != 2 || rs[1].Release != id1 || len(rs[1].Files) != 1 || rs[1].Files[0].Path != "logs/job/app.log" {
		t.Fatalf("v1 的运行：%+v", rs)
	}
}

func TestTailLines(t *testing.T) {
	got := tailLines(strings.NewReader("a\nb\nc\nd"), 3)
	if strings.Join(got.Lines, ",") != "b,c,d" || !got.Truncated {
		t.Fatalf("%+v", got)
	}
	got = tailLines(strings.NewReader("a\r\nb\n"), 5)
	if strings.Join(got.Lines, ",") != "a,b" || got.Truncated {
		t.Fatalf("%+v", got)
	}
}

func appFile(t *testing.T, app, rel, text string) {
	t.Helper()
	p := filepath.Join(app, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 以前的目录结构（0.12.15 及以前：releases/、shared/、legacy/、.run.json，Ferry 的文件放在版本目录里）：
// 换之前照常能看；下次部署时自动换成新的，以前的日志仍然能看
func TestMigrateOldLayout(t *testing.T) {
	f := newFake(t)
	s := javaApp("Job")
	s.ArchiveLogs = []string{"logs"} // 以前的配置：日志写在 shared/logs
	app := filepath.Join(f.root, "apps", "Job")
	put := func(rel, text string) { appFile(t, app, rel, text) }
	// id1：0.12.14 压缩的归档；id2：0.12.15 放在版本目录里的；id2 是当前版本
	put("releases/"+id1+"/Job.jar", "v1")
	put("releases/"+id1+"/release.json", `{"commit":"c1"}`)
	put("releases/"+id1+"/unit.service", s.UnitText)
	put("releases/"+id1+"/.failed", "x")
	src := filepath.Join(t.TempDir(), "old")
	os.MkdirAll(filepath.Join(src, "logs"), 0o755)
	os.WriteFile(filepath.Join(src, "logs", "app.log"), []byte("压缩的第 1 行\n压缩的第 2 行\n"), 0o644)
	os.MkdirAll(filepath.Join(app, "releases", id1, "logs"), 0o755)
	files, _, err := tarGzDir(src, filepath.Join(app, "releases", id1, "logs", "20261001-120000.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(proto.LogRun{Release: id1, ID: "20261001-120000", Files: files})
	put("releases/"+id1+"/logs/20261001-120000.json", string(b))
	put("releases/"+id2+"/Job.jar", "v2")
	put("releases/"+id2+"/release.json", `{"commit":"c2"}`)
	put("releases/"+id2+"/unit.service", strings.ReplaceAll(s.UnitText, "current/", filepath.ToSlash(app)+"/current/"))
	put("releases/"+id2+"/logs/app.log", "0.12.15 移过来的\n")
	b, _ = json.Marshal(proto.LogRun{Release: id2, ID: "20261002-120000", Dir: filepath.Join(app, "releases", id2),
		Files: []proto.LogFile{{Path: "logs/app.log"}}})
	put("releases/"+id2+"/.logruns/20261002-120000.json", string(b))
	put("shared/logs/app.log", "当前运行\n")
	put("legacy/"+s.Unit, "[Service]\n")
	put(".run.json", `{"release":"`+id2+`"}`)
	f.links[filepath.Join(app, "current")] = "releases/" + id2
	os.WriteFile(filepath.Join(f.unitDir, s.Unit), []byte(s.UnitText), 0o644)
	f.appOf[s.Unit] = "Job"
	f.unit(s.Unit).active = "active"

	// 换之前：照常能看
	rs := runs(t, f, s)
	if len(rs) != 3 || !rs[0].Current || rs[0].Release != id2 || len(rs[0].Files) != 1 {
		t.Fatalf("换之前：%+v", rs)
	}
	if got := readLog(t, f, s, id1, "20261001-120000", "logs/app.log"); strings.Join(got.Lines, "|") != "压缩的第 1 行|压缩的第 2 行" {
		t.Fatalf("换之前读压缩的：%+v", got)
	}
	rels := exec1(t, f, "releases", &proto.Request{Apps: []proto.AppSpec{s}})
	if !strings.Contains(string(rels.result.Data), `"failed":true`) || !strings.Contains(string(rels.result.Data), `"c2"`) {
		t.Fatalf("换之前的版本列表：%s", rels.result.Data)
	}

	// 部署 id3：先换结构
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id3, map[string][]byte{"Job": []byte("v3")}))
	if r.code != 0 || !r.has(proto.EvInfo, "目录换成新的结构") || f.current("Job") != id3 {
		t.Fatalf("迁移并部署：\n%s", r.dump())
	}
	for _, p := range []string{
		"running/Job.jar", "running/logs", "versions/" + id1 + "/Job.jar", "versions/" + id2 + "/Job.jar", "versions/" + id2 + "/logs/app.log",
		"../../.ferry/apps/Job/versions/" + id1 + "/release.json", "../../.ferry/apps/Job/versions/" + id1 + "/unit.service", "../../.ferry/apps/Job/versions/" + id1 + "/failed",
		"../../.ferry/apps/Job/versions/" + id1 + "/oldlogs/20261001-120000.tar.gz", "../../.ferry/apps/Job/versions/" + id2 + "/logruns/20261002-120000.json",
		"data", "../../.ferry/apps/Job/legacy/" + s.Unit, "../../.ferry/apps/Job/run.json", "../../.ferry/apps/Job/running", "../../.ferry/apps/Job/versions/" + id3 + "/release.json",
	} {
		if _, err := os.Stat(filepath.Join(app, filepath.FromSlash(p))); err != nil {
			t.Errorf("迁移后应有 %s", p)
		}
	}
	for _, p := range []string{"releases", "shared", "legacy", ".run.json", "current", "versions/" + id1 + "/release.json", "versions/" + id1 + "/logs", "running/.ferry", ".ferry", "versions/" + id2 + "/release.json"} {
		if _, err := os.Lstat(filepath.Join(app, filepath.FromSlash(p))); err == nil {
			t.Errorf("迁移后不应有 %s", p)
		}
	}
	if _, ok := f.links[filepath.Join(app, "current")]; ok {
		t.Error("以前的 current 链接应删掉")
	}
	// 切换时 shared 下的日志（当前运行的）移进 id2 的目录：那里原有 0.12.15 的 logs/，所以放进 .ferry/versions/<id2>/runs/
	rs = runs(t, f, s)
	var got []string
	for _, x := range rs {
		got = append(got, x.Release+":"+x.ID)
	}
	if len(rs) != 4 || !rs[0].Current || rs[0].Release != id3 {
		t.Fatalf("迁移后的运行：%v", got)
	}
	for _, x := range rs[1:] {
		switch {
		case x.Release == id2 && x.ID == "20261002-120000":
			if x.Dir != filepath.Join(app, "versions", id2) {
				t.Errorf("0.12.15 的记录应指向版本目录：%s", x.Dir)
			}
			if r := readLog(t, f, s, id2, x.ID, "logs/app.log"); strings.Join(r.Lines, "|") != "0.12.15 移过来的" {
				t.Errorf("读 0.12.15 的：%+v", r)
			}
		case x.Release == id2:
			if !strings.Contains(x.Dir, ".ferry") {
				t.Errorf("迁移时那次运行应放在 .ferry/versions/<id>/runs/ 下：%s", x.Dir)
			}
			if r := readLog(t, f, s, id2, x.ID, "logs/app.log"); strings.Join(r.Lines, "|") != "当前运行" {
				t.Errorf("读迁移时那次运行：%+v", r)
			}
		}
	}
	if r := readLog(t, f, s, id1, "20261001-120000", "logs/app.log"); strings.Join(r.Lines, "|") != "压缩的第 1 行|压缩的第 2 行" {
		t.Fatalf("迁移后读压缩的：%+v", r)
	}
	// 回滚到迁移前的版本：它保存的单元文件里的 current 路径换成 running
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{s}, To: id2}); r.code != 0 {
		t.Fatalf("回滚到 id2：\n%s", r.dump())
	}
	if b, _ := os.ReadFile(filepath.Join(f.unitDir, s.Unit)); !strings.Contains(string(b), filepath.ToSlash(app)+"/running/") || strings.Contains(string(b), "/current/") {
		t.Fatalf("回滚后的单元文件应指向 running：%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(app, "running", "Job.jar")); string(b) != "v2" {
		t.Fatal("回滚后 running/ 应是 id2")
	}
	// 回到接管前：用迁移后的位置
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{s}, To: LegacyID}); r.code != 0 {
		t.Fatalf("回到接管前：\n%s", r.dump())
	}
	if f.current("Job") != "" {
		t.Error("回到接管前后没有运行中的版本")
	}
}

// 0.12.16 的结构（current 链接 → versions/<id>，Ferry 的文件在版本目录的 .ferry/ 里）：下次部署时换成 running/
func TestMigrateCurrentLinkLayout(t *testing.T) {
	f := newFake(t)
	s := logApp()
	app := filepath.Join(f.root, "apps", "Job")
	oldUnit := ManagedMarker + "\n[Service]\nExecStart=/usr/bin/java -Dlogging.file.path=" + filepath.ToSlash(app) + "/current/logs/x -jar " + filepath.ToSlash(app) + "/current/Job.jar\n"
	put := func(rel, text string) { appFile(t, app, rel, text) }
	put("versions/"+id1+"/Job.jar", "v1")
	put("versions/"+id1+"/logs/app.log", "v1 的日志\n")
	put("versions/"+id1+"/.ferry/release.json", `{"commit":"c1"}`)
	put("versions/"+id1+"/.ferry/unit.service", oldUnit)
	b, _ := json.Marshal(proto.LogRun{Release: id1, ID: "20261001-120000", Dir: "runs/20261001-120000", Files: []proto.LogFile{{Path: "logs/app.log"}}})
	put("versions/"+id1+"/.ferry/logruns/20261001-120000.json", string(b))
	put("versions/"+id1+"/runs/20261001-120000/logs/app.log", "更早的日志\n")
	put("data/.keep", "")
	put(".ferry/run.json", `{"release":"`+id1+`"}`)
	f.links[filepath.Join(app, "current")] = "versions/" + id1
	os.WriteFile(filepath.Join(f.unitDir, s.Unit), []byte(oldUnit), 0o644)
	f.appOf[s.Unit] = "Job"
	f.unit(s.Unit).active = "active"

	// 换之前：能看
	rs := runs(t, f, s)
	if len(rs) != 2 || !rs[0].Current || len(rs[0].Files) != 1 || rs[1].Dir != filepath.Join(app, "versions", id1, "runs", "20261001-120000") {
		t.Fatalf("换之前：%+v", rs)
	}
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{"Job": []byte("v2")})); r.code != 0 {
		t.Fatalf("部署：\n%s", r.dump())
	}
	for _, p := range []string{"running/Job.jar", "running/logs", "versions/" + id1 + "/Job.jar", "versions/" + id1 + "/logs/app.log", "../../.ferry/apps/Job/running",
		"../../.ferry/apps/Job/versions/" + id1 + "/release.json", "../../.ferry/apps/Job/versions/" + id1 + "/unit.service", "../../.ferry/apps/Job/versions/" + id1 + "/runs/20261001-120000/logs/app.log"} {
		if _, err := os.Stat(filepath.Join(app, filepath.FromSlash(p))); err != nil {
			t.Errorf("迁移后应有 %s", p)
		}
	}
	for _, p := range []string{"versions/" + id1 + "/.ferry", "versions/" + id1 + "/runs", "current"} {
		if _, err := os.Lstat(filepath.Join(app, filepath.FromSlash(p))); err == nil {
			t.Errorf("迁移后不应有 %s", p)
		}
	}
	// v1 的运行：切换时记下的这次，和 0.12.16 记下的更早的那次（目录跟着换了位置）
	var here, earlier bool
	for _, x := range runs(t, f, s) {
		if x.Release != id1 || x.Current {
			continue
		}
		if x.Dir == filepath.Join(app, "versions", id1) {
			here = true
			if r := readLog(t, f, s, id1, x.ID, "logs/app.log"); strings.Join(r.Lines, "|") != "v1 的日志" {
				t.Errorf("读 v1 这次运行：%+v", r)
			}
		}
		if x.ID == "20261001-120000" {
			earlier = true
			if r := readLog(t, f, s, id1, x.ID, "logs/app.log"); strings.Join(r.Lines, "|") != "更早的日志" {
				t.Errorf("读 v1 更早的运行：%+v", r)
			}
		}
	}
	if !here || !earlier {
		t.Fatalf("v1 的两次运行都应能看：%+v", runs(t, f, s))
	}
	// 回滚到 v1：保存的单元文件里的 current 换成 running
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{s}, To: id1}); r.code != 0 {
		t.Fatalf("回滚：\n%s", r.dump())
	}
	if b, _ := os.ReadFile(filepath.Join(f.unitDir, s.Unit)); !strings.Contains(string(b), filepath.ToSlash(app)+"/running/Job.jar") {
		t.Fatalf("回滚后的单元文件：%s", b)
	}
}

// 0.12.17 的结构（应用目录里的 .ferry，工作目录是 data）：程序按相对路径写的 logs/… 在 data 下，
// 下次部署时并进离开的版本的 logs/，Ferry 的文件挪到部署目录的 .ferry/apps/<app>，用不到的 data 删掉
func TestMigrateAppFerryAndDataLogs(t *testing.T) {
	f := newFake(t)
	s := logApp()
	app := filepath.Join(f.root, "apps", "Job")
	put := func(rel, text string) { appFile(t, app, rel, text) }
	oldUnit := ManagedMarker + "\n[Service]\nWorkingDirectory=" + filepath.ToSlash(app) + "/data\nExecStart=/usr/bin/java -jar " + filepath.ToSlash(app) + "/running/Job.jar\n"
	put("running/Job.jar", "v1")
	put("running/logs/ShopMall-job/sentinel/metric.log", "sentinel\n")
	put("data/logs/ShopMall-job/info.log", "v1 的运行日志\n")
	put(".ferry/running", id1+"\n")
	put(".ferry/run.json", `{"release":"`+id1+`"}`)
	put(".ferry/versions/"+id1+"/release.json", `{"commit":"c1"}`)
	put(".ferry/versions/"+id1+"/unit.service", oldUnit)
	os.WriteFile(filepath.Join(f.unitDir, s.Unit), []byte(oldUnit), 0o644)
	f.appOf[s.Unit] = "Job"
	f.unit(s.Unit).active = "active"
	if rs := runs(t, f, s); len(rs) == 0 || rs[0].Release != id1 {
		t.Fatalf("换之前应能认出运行中的版本：%+v", rs)
	}
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{s}, id2, map[string][]byte{"Job": []byte("v2")})); r.code != 0 {
		t.Fatalf("部署：\n%s", r.dump())
	}
	for _, p := range []string{"versions/" + id1 + "/logs/ShopMall-job/info.log", "versions/" + id1 + "/logs/ShopMall-job/sentinel/metric.log",
		"running/Job.jar", "running/logs", "../../.ferry/apps/Job/versions/" + id1 + "/release.json", "../../.ferry/apps/Job/running"} {
		if _, err := os.Stat(filepath.Join(app, filepath.FromSlash(p))); err != nil {
			t.Errorf("应有 %s", p)
		}
	}
	for _, p := range []string{".ferry", "data"} {
		if _, err := os.Lstat(filepath.Join(app, p)); err == nil {
			t.Errorf("不应再有 apps/Job/%s", p)
		}
	}
	if f.current("Job") != id2 {
		t.Fatalf("运行中的版本：%q", f.current("Job"))
	}
	// 回滚到 v1：它的单元文件工作目录是 data，要先建出来
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{s}, To: id1}); r.code != 0 {
		t.Fatalf("回滚：\n%s", r.dump())
	}
	if !realDir(filepath.Join(app, "data")) {
		t.Fatal("回滚到工作目录是 data 的版本时应建出 data")
	}
}
