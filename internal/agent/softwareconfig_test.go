package agent

import (
	"bytes"
	"encoding/json"
	"github.com/captain/ferry/internal/filetext"
	"github.com/captain/ferry/internal/proto"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFixture(t *testing.T) (*fakeSys, *Agent, proto.SoftwareConfigFile) {
	t.Helper()
	f := newFake(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(file, []byte("{\"port\":6379}\r\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.owners = map[string]Ownership{file: {UID: 1001, GID: 1001, Perm: 0o640}}
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root, FerryDir: filepath.Join(dir, "ferry")}}
	it := proto.Software{Kind: "redis", Instance: "cache", Unit: "ferry-cache.service", Dir: dir, Via: "systemd", Manager: "Ferry"}
	in := &proto.SoftwareInstance{Name: "cache", Kind: "redis", Dir: dir, Unit: it.Unit, Configs: []proto.SoftwareConfigFile{{Path: file, Label: "主配置", Format: "json", Apply: "restart"}}}
	a.instanceLookup = func(name string) (*proto.SoftwareInstance, error) {
		if name != in.Name {
			return nil, os.ErrNotExist
		}
		return in, nil
	}
	os.MkdirAll(a.swDir(), 0o755)
	b, _ := json.Marshal(in)
	os.WriteFile(a.instFile(in.Name), b, 0o644)
	r := a.discoverConfigs(it, in)
	if len(r.Files) != 1 {
		t.Fatalf("配置发现：%+v", r)
	}
	return f, a, r.Files[0]
}

func TestSoftwareConfigSaveRestoreConflict(t *testing.T) {
	_, a, c := configFixture(t)
	doc, _, _, err := a.readConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	newText := "{\"port\":6380}\n"
	q := &proto.SoftwareConfigRequest{Operation: "save", Revision: doc.Revision, Content: &newText}
	saved, err := a.saveConfig(c, q)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Backup || saved.LineEnding != "CRLF" {
		t.Fatalf("必须备份并保留换行：%+v", saved)
	}
	if got, _ := os.ReadFile(c.Path); string(got) != "{\"port\":6380}\r\n" {
		t.Fatal("文件没有正确保存")
	}
	if _, err := a.saveConfig(c, q); err == nil {
		t.Fatal("不能用旧版本覆盖文件")
	}
	restored, err := a.saveConfig(c, &proto.SoftwareConfigRequest{Operation: "restore", Revision: saved.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(restored.Content, "6379") {
		t.Fatal("恢复备份没有还原原内容")
	}
	invalid := "{"
	if _, err := a.saveConfig(c, &proto.SoftwareConfigRequest{Operation: "save", Revision: restored.Revision, Content: &invalid}); err == nil {
		t.Fatal("损坏的 JSON 不能写入")
	}
}

func TestSoftwareConfigCommandRestrictsIdentity(t *testing.T) {
	_, a, c := configFixture(t)
	r := execConfigured(t, a, "swconfig", &proto.Request{FerryDir: a.req.FerryDir, SoftwareConfig: &proto.SoftwareConfigRequest{Instance: "cache", Operation: "read", ID: c.ID}})
	var doc filetext.Document
	if r.code != 0 || json.Unmarshal(r.result.Data, &doc) != nil || doc.Path != c.Path {
		t.Fatalf("读取：%s", r.dump())
	}
	r = execConfigured(t, a, "swconfig", &proto.Request{FerryDir: a.req.FerryDir, SoftwareConfig: &proto.SoftwareConfigRequest{Instance: "cache", Operation: "read", ID: "../../etc/shadow"}})
	if r.code == 0 {
		t.Fatal("不能提供任意路径读取 root 文件")
	}
}

func TestContainerFileMountKeepsFileIdentity(t *testing.T) {
	f, a, c := configFixture(t)
	c.InPlace = true
	before, _ := os.Stat(c.Path)
	doc, _, _, _ := a.readConfig(c)
	content := "{\"port\":6381}\n"
	if _, err := a.saveConfig(c, &proto.SoftwareConfigRequest{Operation: "save", Revision: doc.Revision, Content: &content}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(c.Path)
	if !os.SameFile(before, after) {
		t.Fatal("单文件 bind mount 的保存必须保留 inode")
	}
	f.shellOut = map[string]string{}
	it := proto.Software{Kind: "mysql", Container: "db", Via: "docker"}
	f.shellOut["docker inspect 'db'"] = `[{"Mounts":[{"Type":"bind","Source":` + jsonString(c.Path) + `,"Destination":"/etc/mysql/my.cnf","RW":false}]}]`
	r := a.discoverConfigs(it, nil)
	if len(r.Files) != 1 || !r.Files[0].InPlace {
		t.Fatalf("应找到持久化挂载的配置：%+v", r)
	}
}
func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func execConfigured(t *testing.T, a *Agent, cmd string, req *proto.Request) run {
	t.Helper()
	req.Proto, req.Root = proto.Version, a.req.Root
	if req.FerryDir == "" {
		req.FerryDir = a.req.FerryDir
	}
	if req.Project == "" {
		req.Project = a.req.Project
	}
	var out bytes.Buffer
	a.Out = &out
	r := run{code: a.Run(cmd, req)}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var head struct{ T string }
		json.Unmarshal([]byte(line), &head)
		if head.T == "ev" {
			var e proto.Event
			json.Unmarshal([]byte(line), &e)
			r.events = append(r.events, e)
		} else {
			json.Unmarshal([]byte(line), &r.result)
		}
	}
	return r
}

func TestGeneratedConfigCannotBeEdited(t *testing.T) {
	f, a, c := configFixture(t)
	file := filepath.ToSlash(c.Path)
	os.WriteFile(c.Path, []byte("# 由 ferry 生成（project=demo app=web）\nhttp {}\n"), 0o644)
	it := proto.Software{Kind: "nginx", Manager: "Ferry", PID: 42, Path: "/usr/sbin/nginx", Unit: "project-web.service"}
	f.args = map[int][][]string{42: {{it.Path, "-c", file}}}
	f.shellOut = map[string]string{shellQuote(it.Path) + " -T -c " + shellQuote(file) + " 2>&1": "# configuration file " + file + ":\nhttp {}\n"}
	r := a.discoverConfigs(it, nil)
	if len(r.Files) != 1 || r.Files[0].ReadOnly == "" {
		t.Fatal("部署生成的配置应引导修改项目源配置")
	}
}

func TestSoftwareConfigAssociation(t *testing.T) {
	_, a, c := configFixture(t)
	it := proto.Software{Kind: "redis", Instance: "cache", Unit: "ferry-cache.service", Dir: filepath.Dir(c.Path), Via: "systemd"}
	file := filepath.Join(it.Dir, "custom-settings.txt")
	if err := os.WriteFile(file, []byte("port=6381\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := a.associateConfig(it, file); err != nil {
		t.Fatal(err)
	}
	report := a.discoverConfigs(it, nil)
	found := false
	for _, c := range report.Files {
		found = found || c.Path == file && c.Source == "manual"
	}
	if !found {
		t.Fatal("手工关联的文件应出现在配置列表")
	}
	other := filepath.Join(t.TempDir(), "outside.conf")
	os.WriteFile(other, []byte("secret\n"), 0o600)
	if err := a.associateConfig(it, other); err == nil {
		t.Fatal("不能关联此软件目录以外的文件")
	}
	processA := proto.Software{Kind: "redis", Path: "/opt/a/redis-server"}
	processB := proto.Software{Kind: "redis", Path: "/opt/b/redis-server"}
	if a.configLinksFile(processA) == a.configLinksFile(processB) {
		t.Fatal("不同进程实例不能混用关联列表")
	}
}
