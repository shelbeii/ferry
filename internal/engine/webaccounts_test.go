package engine

import (
	"fmt"
	"testing"
)

func TestWebEnvAccounts(t *testing.T) {
	cases := []struct {
		kind string
		env  map[string]string
		args []string
		want string
	}{
		{"minio", map[string]string{"MINIO_ROOT_USER": "admin", "MINIO_ROOT_PASSWORD": "pw1"}, nil, "[admin/pw1 false]"},
		{"minio", map[string]string{"MINIO_ACCESS_KEY": "ak", "MINIO_SECRET_KEY": "sk"}, nil, "[ak/sk false]"},
		{"minio", map[string]string{"PATH": "/bin"}, []string{"minio", "server"}, "[minioadmin/minioadmin true]"},
		{"minio", nil, nil, "[]"}, // 读不到进程：不猜
		{"sentinel", nil, []string{"java", "-Dsentinel.dashboard.auth.username=ops", "-Dsentinel.dashboard.auth.password=s3", "-jar", "x.jar"}, "[ops/s3 false]"},
		{"grafana", map[string]string{"GF_SECURITY_ADMIN_PASSWORD": "g"}, nil, "[admin/g false]"},
		{"rabbitmq", map[string]string{"RABBITMQ_DEFAULT_USER": "u", "RABBITMQ_DEFAULT_PASS": "p"}, nil, "[u/p false]"},
		{"nacos", map[string]string{"NACOS_AUTH_ENABLE": "true"}, nil, "[]"},
	}
	for _, c := range cases {
		var got []string
		for _, a := range webEnvAccounts(c.kind, c.env, c.args) {
			got = append(got, fmt.Sprintf("%s/%s %v", a.User, a.Password, a.Default))
		}
		if s := fmt.Sprint(got); s != c.want && !(c.want == "[]" && len(got) == 0) {
			t.Errorf("%s %v：%s，期望 %s", c.kind, c.env, s, c.want)
		}
	}
}

func TestProcessEnv(t *testing.T) {
	run := fakeRun(map[string]string{
		"cat /proc/42/environ": "PATH=/bin\x00MINIO_ROOT_USER=root\x00MINIO_ROOT_PASSWORD=a=b\x00",
		"cat /proc/42/cmdline": "/opt/minio\x00server\x00/data\x00",
	})
	env, args := processEnv(WebQuery{PID: 42}, run)
	if env["MINIO_ROOT_PASSWORD"] != "a=b" || len(args) != 3 || args[1] != "server" {
		t.Errorf("进程：%v %v", env, args)
	}
	run = fakeRun(map[string]string{"docker inspect 'm1'": `[{"Path":"/usr/bin/docker-entrypoint.sh","Args":["server","/data"],"Config":{"Env":["MINIO_ROOT_USER=x","MINIO_ROOT_PASSWORD=y"]}}]`})
	env, args = processEnv(WebQuery{Container: "m1", PID: 42}, run)
	if env["MINIO_ROOT_USER"] != "x" || len(args) != 3 {
		t.Errorf("容器：%v %v", env, args)
	}
}
