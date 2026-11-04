package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func preparedJava(t *testing.T, f *fakeSys, app proto.AppSpec) proto.Prepared {
	t.Helper()
	r := exec1(t, f, "prepare", &proto.Request{Apps: []proto.AppSpec{app}, Seed: true})
	var p proto.Prepared
	if r.code != 0 || json.Unmarshal(r.result.Data, &p) != nil || len(p.Libs) == 0 {
		t.Fatalf("prepare should extract dependencies: %s", r.dump())
	}
	return p
}

func cacheFor(f *fakeSys, stage string) string {
	return filepath.Join(f.root, ".ferry", "cache", filepath.Base(stage), "lib")
}

func TestUploadCacheRemovedOnEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "verify", "failed", "locked", "invalid", "discard"} {
		t.Run(outcome, func(t *testing.T) {
			f := newFake(t)
			app := javaApp("System")
			jar := storedJar(t, map[string]string{"BOOT-INF/lib/common.jar": "common"})
			if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{app}, id1, map[string][]byte{app.Name: jar})); r.code != 0 {
				t.Fatal(r.dump())
			}
			// 旧抽取标记不能阻止新上传重新提取。
			os.WriteFile(filepath.Join(f.root, ".ferry", "apps", app.Name, "versions", id1, "libs-seeded"), nil, 0o644)
			p := preparedJava(t, f, app)
			os.WriteFile(filepath.Join(p.Stage, app.FileName), jar, 0o644)
			sum := sha256.Sum256(jar)
			req := &proto.Request{Stage: p.Stage, Apps: []proto.AppSpec{app}, Releases: []proto.ReleaseSpec{{App: app.Name, ID: id2, File: app.FileName, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(jar))}}}
			cmd, want := "deploy", proto.CodeOK
			switch outcome {
			case "verify":
				req.Releases[0].Size++
				want = proto.CodeVerify
			case "failed":
				f.bad[id2] = true
				want = proto.CodeFailed
			case "locked":
				f.locked = true
				want = proto.CodeLocked
			case "invalid":
				req.Releases = nil
				want = proto.CodeBadInput
			case "discard":
				cmd = "discard"
			}
			if r := exec1(t, f, cmd, req); r.code != want {
				t.Fatalf("want %d: %s", want, r.dump())
			}
			for _, path := range []string{p.Stage, filepath.Dir(cacheFor(f, p.Stage))} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("upload residue: %s (%v)", path, err)
				}
			}
			if _, err := os.Stat(filepath.Join(f.root, "apps", app.Name, "running", app.FileName)); err != nil {
				t.Fatalf("running application was removed: %v", err)
			}
		})
	}
}

func TestUploadCachesAreIsolated(t *testing.T) {
	f := newFake(t)
	app := javaApp("System")
	jar := storedJar(t, map[string]string{"BOOT-INF/lib/common.jar": "common"})
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{app}, id1, map[string][]byte{app.Name: jar})); r.code != 0 {
		t.Fatal(r.dump())
	}
	a, b := preparedJava(t, f, app), preparedJava(t, f, app)
	if a.Stage == b.Stage {
		t.Fatal("uploads must have distinct directories")
	}
	left, right := filepath.Join(cacheFor(f, a.Stage), a.Libs[0]+".jar"), filepath.Join(cacheFor(f, b.Stage), b.Libs[0]+".jar")
	want, err := os.ReadFile(right)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(left, []byte("corrupt first upload"), 0o644)
	if r := exec1(t, f, "discard", &proto.Request{Stage: a.Stage}); r.code != 0 {
		t.Fatal(r.dump())
	}
	if got, err := os.ReadFile(right); err != nil || string(got) != string(want) {
		t.Fatalf("discarding another upload changed this cache: %q, %v", got, err)
	}
	if _, err := os.Stat(b.Stage); err != nil {
		t.Fatal("other upload directory was removed", err)
	}
}

func TestPrepareReclaimsAbandonedUploadCaches(t *testing.T) {
	f := newFake(t)
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root}}
	if err := a.ensureMeta(); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(a.incomingDir(), id1)
	active := filepath.Join(a.incomingDir(), id2)
	for _, stage := range []string{stale, active} {
		os.Mkdir(stage, 0o700)
		if err := a.ensureUploadCache(stage); err != nil {
			t.Fatal(err)
		}
	}
	old := f.now.Add(-staleIncoming - time.Hour)
	os.Chtimes(stale, old, old)
	os.Chtimes(active, f.now, f.now)
	orphan := filepath.Join(a.cacheRootDir(), id3)
	legacy := filepath.Join(a.cacheRootDir(), "lib")
	os.MkdirAll(orphan, 0o700)
	os.MkdirAll(legacy, 0o755)
	a.cleanIncoming()
	for _, path := range []string{stale, filepath.Dir(a.uploadCacheDir(stale)), orphan} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("abandoned upload survived: %s, %v", path, err)
		}
	}
	for _, path := range []string{active, a.uploadCacheDir(active), legacy} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active upload or legacy cache was removed: %s, %v", path, err)
		}
	}
	a.finishUpload(active)
	a.cleanIncoming()
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("idle legacy cache should be removed: %v", err)
	}
}

func TestDiscardRefusesPathsOutsideUpload(t *testing.T) {
	f := newFake(t)
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	os.WriteFile(marker, []byte("keep"), 0o600)
	for _, stage := range []string{outside, filepath.Join(f.root, ".ferry", "incoming"), filepath.Join(f.root, ".ferry", "incoming", "..", "cache")} {
		if r := exec1(t, f, "discard", &proto.Request{Stage: stage}); r.code != proto.CodeBadInput {
			t.Fatal(r.dump())
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("outside data was touched", err)
	}
}
