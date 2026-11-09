package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

// fastScan 一台只有几个进程的假服务器：记下运行过的命令。
type fastScan struct {
	fp  *fakeProc
	mu  sync.Mutex
	ran []string
}

func newFastScan(t *testing.T) *fastScan {
	fp := &fakeProc{dir: t.TempDir(), links: map[string]string{}}
	os.WriteFile(filepath.Join(fp.dir, "stat"), []byte("btime 1700000000\n"), 0o644)
	return &fastScan{fp: fp}
}

func (f *fastScan) scanner(cache *verCache) *swScan {
	return &swScan{
		proc: f.fp.dir, unitDir: f.fp.dir, cache: cache,
		run: func(cmd string, _ time.Duration) (string, error) {
			f.mu.Lock()
			f.ran = append(f.ran, cmd)
			f.mu.Unlock()
			switch cmd {
			case "'/usr/sbin/mysqld' --version 2>&1":
				return "/usr/sbin/mysqld  Ver 10.6.19-MariaDB for Linux on x86_64 (MariaDB Server)\n", nil
			case "'/usr/sbin/nginx' -v 2>&1":
				return "nginx version: nginx/1.24.0\n", nil
			case "'/opt/redis/bin/redis-server' --version 2>&1":
				return "Redis server v=8.0.1\n", nil
			}
			return "", errors.New("exit status 127")
		},
		unitProps: func(string) UnitProps { return UnitProps{Load: "loaded", Active: "active"} },
		readlink: func(p string) (string, error) {
			if v, ok := f.fp.links[p]; ok {
				return v, nil
			}
			return "", os.ErrNotExist
		},
		listening: func(int) []string { return nil },
	}
}

func (f *fastScan) count(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.ran {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// 版本按程序文件的身份缓存：文件没变不再运行程序；换了文件（身份变了）重新取。MariaDB 靠缓存里的输出也认得出。
func TestSoftwareVersionCache(t *testing.T) {
	f := newFastScan(t)
	f.fp.add(t, 1, 0, "/usr/lib/systemd/systemd", "0::/init.scope\n", "net:[1]")
	f.fp.add(t, 100, 1, "/usr/sbin/mysqld", "0::/system.slice/mariadb.service\n", "net:[1]")
	ids := map[string]string{"/usr/sbin/mysqld": "ino-1"}
	saved := fileID
	fileID = func(p string) string { return ids[p] }
	defer func() { fileID = saved }()
	path := filepath.Join(t.TempDir(), "cache", "versions.json")

	scan := func() proto.Software {
		c := loadVerCache(path)
		rep := f.scanner(c).scan()
		c.save()
		if len(rep.Items) != 1 {
			t.Fatalf("应认出一个软件：%+v", rep.Items)
		}
		return rep.Items[0]
	}
	if it := scan(); it.Version != "10.6.19" || it.Kind != "mariadb" {
		t.Fatalf("第一次：%+v", it)
	}
	if it := scan(); it.Version != "10.6.19" || it.Kind != "mariadb" {
		t.Fatalf("用缓存：%+v", it)
	}
	if n := f.count("--version"); n != 1 {
		t.Fatalf("程序没变应只运行一次取版本，运行了 %d 次", n)
	}
	ids["/usr/sbin/mysqld"] = "ino-2" // 升级替换了文件
	scan()
	if n := f.count("--version"); n != 2 {
		t.Fatalf("文件换了应重新取版本，运行了 %d 次", n)
	}
	var stored map[string]verCached
	b, _ := os.ReadFile(path)
	if json.Unmarshal(b, &stored) != nil || len(stored) != 1 {
		t.Fatalf("缓存只留这次用到的：%s", b)
	}
}

// Ferry 装的软件用安装时记下的版本，不运行程序；运行中的程序文件被换掉的标出来。
func TestSoftwareKnownAndReplaced(t *testing.T) {
	f := newFastScan(t)
	f.fp.add(t, 1, 0, "/usr/lib/systemd/systemd", "0::/init.scope\n", "net:[1]")
	f.fp.add(t, 100, 1, "/opt/redis/bin/redis-server", "0::/system.slice/ferry-redis.service\n", "net:[1]")
	f.fp.add(t, 200, 1, "/usr/sbin/nginx (deleted)", "0::/system.slice/nginx.service\n", "net:[1]", "nginx: master process")
	f.fp.add(t, 201, 200, "/usr/sbin/nginx (deleted)", "0::/system.slice/nginx.service\n", "net:[1]", "nginx: worker process")
	sc := f.scanner(nil)
	sc.instances = []proto.SoftwareInstance{{Name: "redis", Kind: "redis", Title: "Redis", Unit: "ferry-redis.service", Version: "8.0.1"}}
	sc.exists = func(string) bool { return true }
	rep := sc.scan()
	got := map[string]proto.Software{}
	for _, it := range rep.Items {
		got[it.Unit] = it
	}
	if it := got["ferry-redis.service"]; it.Version != "8.0.1" || it.Instance != "redis" {
		t.Errorf("Ferry 装的：%+v", it)
	}
	if n := f.count("redis-server' --version"); n != 0 {
		t.Errorf("安装时记了版本的不该再运行程序取版本")
	}
	if it := got["nginx.service"]; it.Replaced != "replaced" || it.Path != "/usr/sbin/nginx" || it.Version != "1.24.0" {
		t.Errorf("程序文件被替换：%+v", it)
	}
	sc = f.scanner(nil)
	sc.exists = func(string) bool { return false }
	for _, it := range sc.scan().Items {
		if it.Unit == "nginx.service" && it.Replaced != "deleted" {
			t.Errorf("程序文件删了：%+v", it)
		}
	}
	for _, k := range []string{"读进程", "取版本、读容器和系统服务", "整理"} {
		if _, ok := rep.Took[k]; !ok {
			t.Errorf("没有记下 %s 用的时间：%v", k, rep.Took)
		}
	}
}

func TestDescendants(t *testing.T) {
	children := map[int][]int{1: {10, 20}, 10: {11}, 11: {12}, 20: {21}}
	got := descendants(children, 10)
	if !reflect.DeepEqual(got, map[int]bool{10: true, 11: true, 12: true}) {
		t.Fatalf("%v", got)
	}
}

func TestEachParallel(t *testing.T) {
	var mu sync.Mutex
	done := map[int]bool{}
	eachParallel(20, 3, func(i int) {
		mu.Lock()
		done[i] = true
		mu.Unlock()
	})
	if len(done) != 20 {
		t.Fatalf("应全部做完：%d", len(done))
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "出错了") {
			t.Fatalf("其中一件出错时应在调用方 panic：%v", r)
		}
	}()
	eachParallel(5, 2, func(i int) {
		if i == 3 {
			panic("出错了")
		}
	})
}
