package agent

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/captain/ferry/internal/proto"
)

// memS3 内存里的桶（路径形式 /bkt/键）。
type memS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=AKID/") {
		w.WriteHeader(403)
		fmt.Fprint(w, "<Error><Code>InvalidAccessKeyId</Code></Error>")
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/bkt"), "/")
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && key == "":
		var keys []string
		for k := range m.objs {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, "<ListBucketResult>")
		for _, k := range keys {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-11-12T03:00:00Z</LastModified></Contents>", k, len(m.objs[k]))
		}
		fmt.Fprint(w, "</ListBucketResult>")
	case r.Method == http.MethodPut:
		m.objs[key] = body
	case r.Method == http.MethodGet:
		w.Write(m.objs[key])
	case r.Method == http.MethodDelete:
		delete(m.objs, key)
	}
}

func TestBackupUploadAndFetch(t *testing.T) {
	f := newFake(t)
	f.now = time.Date(2026, 11, 12, 3, 0, 0, 0, time.Local)
	s3 := &memS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(s3)
	defer srv.Close()

	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "a.txt"), []byte("hello backup"), 0o644)
	env := filepath.Join(t.TempDir(), "backup.env")
	os.WriteFile(env, []byte("S3_ACCESS_KEY_ID=AKID\nS3_SECRET_ACCESS_KEY=secret\nFERRY_BACKUP_PASSWORD=correct horse\n"), 0o600)
	it := proto.BackupItem{Name: "files", Kind: "paths", Paths: []string{data}, EnvFiles: []string{env}, Keep: 5,
		Upload: &proto.BackupUpload{Endpoint: srv.URL, Bucket: "bkt", Prefix: "ferry/shop/prod/", Encrypt: true, Keep: 2}}
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root}}

	// 手动备份三次：都传上去（加密），那边只留 2 份
	var last proto.BackupRun
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(time.Second)
		last = a.runBackup(f.root, it, "手动", "", "")
		if !last.OK || last.UploadError != "" || last.Uploaded != "s3://bkt/ferry/shop/prod/files/"+last.File+".age" {
			t.Fatalf("%+v", last)
		}
	}
	if len(s3.objs) != 2 {
		t.Fatalf("异地只留 2 份：%v", keysOf(s3.objs))
	}
	enc := s3.objs["ferry/shop/prod/files/"+last.File+".age"]
	local, _ := os.ReadFile(filepath.Join(backupItemDir(f.root, "files"), last.File))
	id, _ := age.NewScryptIdentity("correct horse")
	dr, err := age.Decrypt(bytes.NewReader(enc), id)
	if err != nil {
		t.Fatal(err)
	}
	if plain, _ := io.ReadAll(dr); !bytes.Equal(plain, local) {
		t.Fatal("解密后和服务器上的那份不一样")
	}
	if bytes.Contains(enc, []byte("hello backup")) || bytes.Equal(enc, local) {
		t.Fatal("传上去的应该是加密的")
	}

	// 部署前的那份不传
	f.now = f.now.Add(time.Second)
	if r := a.runBackup(f.root, it, "部署前", "", ""); !r.OK || r.Uploaded != "" || len(s3.objs) != 2 {
		t.Fatalf("%+v %v", r, keysOf(s3.objs))
	}

	// 列出异地的：服务器上也有的标出来
	files, err := a.remoteFiles(f.root, it)
	if err != nil || len(files) != 2 || !files[0].Remote || !files[0].Local || files[0].Name != last.File+".age" {
		t.Fatalf("%+v %v", files, err)
	}

	// 服务器上的删了：从异地取回（解密），和原来的一样
	os.Remove(filepath.Join(backupItemDir(f.root, "files"), last.File))
	if _, err := a.fetchBackup(f.root, it, last.File+".age"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(backupItemDir(f.root, "files"), last.File)); !bytes.Equal(got, local) {
		t.Fatal("取回的和原来的不一样")
	}
	if _, err := a.fetchBackup(f.root, it, last.File+".age"); err == nil || !strings.Contains(err.Error(), "已经有") {
		t.Fatalf("已经有了不再取：%v", err)
	}
	if _, err := a.fetchBackup(f.root, it, "../x.gz"); err == nil {
		t.Fatal("文件名不对应拒绝")
	}
	// 密码不对：说清楚
	os.Remove(filepath.Join(backupItemDir(f.root, "files"), last.File))
	os.WriteFile(env, []byte("S3_ACCESS_KEY_ID=AKID\nS3_SECRET_ACCESS_KEY=secret\nFERRY_BACKUP_PASSWORD=wrong\n"), 0o600)
	if _, err := a.fetchBackup(f.root, it, last.File+".age"); err == nil || !strings.Contains(err.Error(), "不是当初加密用的密码") {
		t.Fatalf("%v", err)
	}
	if exists(filepath.Join(backupItemDir(f.root, "files"), last.File)) {
		t.Fatal("解密失败时不留半个文件")
	}

	// 没写加密的密码：备份照样成功，记下没传上去；告警提醒
	os.WriteFile(env, []byte("S3_ACCESS_KEY_ID=AKID\nS3_SECRET_ACCESS_KEY=secret\n"), 0o600)
	f.now = f.now.Add(time.Second)
	r := a.runBackup(f.root, it, "定时", "", "")
	if !r.OK || !strings.Contains(r.UploadError, "FERRY_BACKUP_PASSWORD") {
		t.Fatalf("%+v", r)
	}
	saveJSON0600(backupConfigPath(f.root), backupConfig{On: true, Project: "shop", Target: "prod", Items: []proto.BackupItem{it}})
	a.recordRun(f.root, "files", r)
	probs := backupProblems(f.root, "shop", "prod")
	if len(probs) != 1 || probs[0].Key != "backup-upload:files" || probs[0].Level != "warn" {
		t.Fatalf("%+v", probs)
	}

	// 不加密：原样传
	it.Upload.Encrypt = false
	f.now = f.now.Add(time.Second)
	if r := a.runBackup(f.root, it, "定时", "", ""); r.UploadError != "" || !strings.HasSuffix(r.Uploaded, ".tar.gz") {
		t.Fatalf("%+v", r)
	}
	// 密钥不对
	os.WriteFile(env, []byte("S3_ACCESS_KEY_ID=WRONG\nS3_SECRET_ACCESS_KEY=secret\n"), 0o600)
	f.now = f.now.Add(time.Second)
	if r := a.runBackup(f.root, it, "定时", "", ""); !strings.Contains(r.UploadError, "访问密钥 ID 不对") {
		t.Fatalf("%+v", r)
	}
	os.WriteFile(env, []byte("X=1\n"), 0o600)
	if r := a.runBackup(f.root, it, "手动", "", ""); !strings.Contains(r.UploadError, "S3_ACCESS_KEY_ID") {
		t.Fatalf("%+v", r)
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestBackupUploadRealS3 在真的对象存储上走一遍：传上去、删掉服务器上的、取回来（见 internal/s3c 的 TestRealS3）。
func TestBackupUploadRealS3(t *testing.T) {
	ep := os.Getenv("S3C_ENDPOINT")
	if ep == "" {
		t.Skip("没有设 S3C_ENDPOINT")
	}
	f := newFake(t)
	f.now = time.Now()
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "big.bin"), bytes.Repeat([]byte("ferry "), 3<<20), 0o644)
	env := filepath.Join(t.TempDir(), "backup.env")
	os.WriteFile(env, []byte("S3_ACCESS_KEY_ID="+os.Getenv("S3C_KEY")+"\nS3_SECRET_ACCESS_KEY="+os.Getenv("S3C_SECRET")+"\nFERRY_BACKUP_PASSWORD=pw-for-test\n"), 0o600)
	it := proto.BackupItem{Name: "files", Kind: "paths", Paths: []string{data}, EnvFiles: []string{env}, Keep: 3,
		Upload: &proto.BackupUpload{Endpoint: ep, Bucket: os.Getenv("S3C_BUCKET"), Prefix: "ferry-agent-test/", Encrypt: true, Keep: 1}}
	a := &Agent{Sys: f, Out: io.Discard, req: &proto.Request{Root: f.root}}
	var r proto.BackupRun
	for i := 0; i < 2; i++ {
		f.now = f.now.Add(time.Second)
		if r = a.runBackup(f.root, it, "手动", "", ""); r.UploadError != "" || r.Uploaded == "" {
			t.Fatalf("%+v", r)
		}
	}
	files, err := a.remoteFiles(f.root, it)
	if err != nil || len(files) != 1 {
		t.Fatalf("异地留 1 份：%+v %v", files, err)
	}
	local, _ := os.ReadFile(filepath.Join(backupItemDir(f.root, "files"), r.File))
	os.Remove(filepath.Join(backupItemDir(f.root, "files"), r.File))
	if _, err := a.fetchBackup(f.root, it, files[0].Name); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(backupItemDir(f.root, "files"), r.File)); !bytes.Equal(got, local) {
		t.Fatal("取回的和原来的不一样")
	}
}
