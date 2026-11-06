package engine

import (
	"errors"
	"strings"
	"testing"
)

func fakeRun(outputs map[string]string) func(string) (string, bool) {
	return func(cmd string) (string, bool) {
		for k, v := range outputs {
			if strings.Contains(cmd, k) {
				return v, true
			}
		}
		return "", false
	}
}

func candText(cs []dbCand) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.user+"/"+c.password+" ← "+c.from)
	}
	return strings.Join(out, "\n")
}

func TestDockerCands(t *testing.T) {
	cases := []struct {
		name, kind, inspect string
		extra               map[string]string
		want                []string // 每个候选「用户/密码」，按顺序
		note                string
	}{
		{"1Panel MySQL：应用目录的 .env 在前（1Panel 里改过密码）", "mysql",
			`[{"Path":"docker-entrypoint.sh","Args":["mysqld"],"Config":{"Env":["MYSQL_ROOT_PASSWORD=old_pw","PATH=/bin"],"Labels":{"com.docker.compose.project.working_dir":"/opt/1panel/apps/mysql/mysql"}}}]`,
			map[string]string{"cat '/opt/1panel/apps/mysql/mysql/.env'": "CONTAINER_NAME=\"1Panel-mysql-AbC\"\nPANEL_DB_ROOT_PASSWORD=\"new_pw\"\nPANEL_APP_PORT_HTTP=3307\n"},
			[]string{"root/new_pw", "root/old_pw"}, ""},
		{"_FILE", "mariadb", `[{"Config":{"Env":["MARIADB_ROOT_PASSWORD_FILE=/run/secrets/db"]}}]`, map[string]string{"head -c 65536 '/run/secrets/db'": "s3cret\n"},
			[]string{"root/s3cret"}, ""},
		{"随机 root 密码", "mysql", `[{"Config":{"Env":["MYSQL_RANDOM_ROOT_PASSWORD=yes"]}}]`, map[string]string{"GENERATED ROOT PASSWORD": "2026-11-06 [Note] [Entrypoint]: GENERATED ROOT PASSWORD: Xy12\n"},
			[]string{"root/Xy12"}, ""},
		{"只有普通账号", "mysql", `[{"Config":{"Env":["MYSQL_USER=app","MYSQL_PASSWORD=pw"]}}]`, nil, []string{"app/pw"}, ""},
		{"MySQL 没有密码变量", "mysql", `[{"Config":{"Env":["PATH=/bin"]}}]`, nil, nil, "没有 root 密码"},
		{"PostgreSQL", "postgresql", `[{"Config":{"Env":["POSTGRES_USER=app","POSTGRES_PASSWORD=pg"]}}]`, nil, []string{"app/pg"}, ""},
		{"PostgreSQL trust", "postgresql", `[{"Config":{"Env":["POSTGRES_HOST_AUTH_METHOD=trust"]}}]`, nil, []string{"postgres/"}, ""},
		{"Redis 命令行", "redis", `[{"Path":"docker-entrypoint.sh","Args":["redis-server","--requirepass","r p"],"Config":{"Env":[]}}]`, nil, []string{"/r p"}, ""},
		{"Redis 配置文件", "redis", `[{"Path":"docker-entrypoint.sh","Args":["redis-server","/etc/redis/redis.conf"],"Config":{"Env":[]}}]`,
			map[string]string{"'/etc/redis/redis.conf'": "bind 0.0.0.0\nrequirepass \"a b\\\"c\"\n"}, []string{`/a b"c`}, ""},
		{"Redis 没设密码", "redis", `[{"Path":"docker-entrypoint.sh","Args":["redis-server"],"Config":{"Env":[]}}]`, nil, []string{"/"}, ""},
	}
	for _, c := range cases {
		out := map[string]string{"docker inspect 'db1'": c.inspect}
		for k, v := range c.extra {
			out[k] = v
		}
		cands, note := dockerCands(DBCredQuery{Kind: c.kind, Container: "db1"}, fakeRun(out))
		var got []string
		for _, x := range cands {
			got = append(got, x.user+"/"+x.password)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") || !strings.Contains(note, c.note) || (len(c.want) == 0 && note == "") {
			t.Errorf("%s：\n%s\n说明：%q", c.name, candText(cands), note)
		}
	}
}

func TestPickCred(t *testing.T) {
	cands := []dbCand{{"root", "a", "来源 A"}, {"root", "b", "来源 B"}}
	try := func(good string, netErr error) func(dbCand) (bool, error) {
		return func(c dbCand) (bool, error) {
			if netErr != nil {
				return false, netErr
			}
			return c.password == good, nil
		}
	}
	// 第二个对：用第二个，说试过
	if r := pickCred(cands, "root", "", try("b", nil)); !r.Found || !r.Verified || r.Password != "b" || r.From != "来源 B" {
		t.Errorf("第二个对：%+v", r)
	}
	// 都不对：不填密码，说都登录不上
	if r := pickCred(cands, "root", "", try("x", nil)); r.Found || r.Password != "" || !strings.Contains(r.Note, "都登录不上") || !strings.Contains(r.Note, "来源 A") {
		t.Errorf("都不对：%+v", r)
	}
	// 连不上数据库：填第一个，说没能试
	if r := pickCred(cands, "root", "", try("", errors.New("connection refused"))); !r.Found || r.Verified || r.Password != "a" || !strings.Contains(r.Note, "没能试") {
		t.Errorf("连不上：%+v", r)
	}
	// 没读到
	if r := pickCred(nil, "root", "没有 root 密码：请填写", nil); r.Found || r.User != "root" || r.Note != "没有 root 密码：请填写" {
		t.Errorf("没读到：%+v", r)
	}
}

func TestHostCands(t *testing.T) {
	// Redis：配置文件从单元的启动命令里找
	run := fakeRun(map[string]string{
		"systemctl show -p ExecStart --no-pager 'redis.service'": "ExecStart={ path=/usr/bin/redis-server ; argv[]=/usr/bin/redis-server /etc/redis.conf --supervised systemd ; }\n",
		"cat '/etc/redis.conf'":                                  "# requirepass old\nrequirepass first\ninclude /etc/redis/local.conf\n",
		"cat '/etc/redis/local.conf'":                            "requirepass second\n",
	})
	if c, _ := hostCands(DBCredQuery{Kind: "redis", Unit: "redis.service", PID: 10}, "root", run); len(c) != 1 || c[0].password != "second" || !strings.Contains(c[0].from, "local.conf") {
		t.Errorf("Redis：%s", candText(c))
	}
	if c, note := hostCands(DBCredQuery{Kind: "redis"}, "root", fakeRun(nil)); len(c) != 0 || note == "" {
		t.Errorf("Redis 读不到：%s %q", candText(c), note)
	}
	// MySQL：~/.my.cnf
	run = fakeRun(map[string]string{"cat '/home/ops/.my.cnf'": "[mysqld]\npassword=no\n[client]\nuser = admin\npassword = \"p w\"\n"})
	if c, _ := hostCands(DBCredQuery{Kind: "mysql"}, "ops", run); len(c) != 1 || c[0].user != "admin" || c[0].password != "p w" {
		t.Errorf("MySQL：%s", candText(c))
	}
	if c, note := hostCands(DBCredQuery{Kind: "mariadb"}, "root", fakeRun(nil)); len(c) != 0 || !strings.Contains(note, "unix_socket") {
		t.Errorf("MariaDB 读不到：%s %q", candText(c), note)
	}
	// PostgreSQL：.pgpass 按端口
	run = fakeRun(map[string]string{"cat '/var/lib/pgsql/.pgpass'": "db.example.com:5432:*:x:y\nlocalhost:5433:*:a:b\n*:5432:*:app:p\\:w\n"})
	if c, _ := hostCands(DBCredQuery{Kind: "postgresql", Port: 5432}, "root", run); len(c) != 1 || c[0].user != "app" || c[0].password != "p:w" {
		t.Errorf("PostgreSQL：%s", candText(c))
	}
}

func TestRedisConfFields(t *testing.T) {
	for in, want := range map[string]string{
		`requirepass abc`:         "requirepass|abc",
		`requirepass "a b"`:       "requirepass|a b",
		`requirepass 'x y' # no`:  "requirepass|x y|#|no",
		"  # requirepass hidden":  "",
		"requirepass\t\"q\\\"z\"": `requirepass|q"z`,
	} {
		if got := strings.Join(redisConfFields(strings.TrimSpace(in)), "|"); got != want {
			t.Errorf("%q：%q，期望 %q", in, got, want)
		}
	}
}

func TestParseDotenv(t *testing.T) {
	m := parseDotenv("# x\nexport A=1\nB = \"two words\"\nC='q'\nbad line\n")
	if m["A"] != "1" || m["B"] != "two words" || m["C"] != "q" || len(m) != 3 {
		t.Errorf("%v", m)
	}
}
