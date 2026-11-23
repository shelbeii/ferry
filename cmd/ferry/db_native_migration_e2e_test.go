package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/dbx"
	"github.com/captain/ferry/internal/proto"
)

func TestNativeDatabaseMigrationE2E(t *testing.T) {
	config, prefix := os.Getenv("FERRY_NATIVE_CONFIG"), os.Getenv("FERRY_NATIVE_PREFIX")
	if config == "" || prefix == "" {
		t.Skip("用 test/e2e/database-native-migration.sh 启动真实数据库和 SSH 实例")
	}
	for _, fixture := range []struct {
		name, kind, src, dst, user, adminDB, method string
	}{
		{"PostgreSQL", dbx.Postgres, "pg", "pg-target", "postgres", "postgres", "pg_dump / pg_restore"},
		{"MySQL", dbx.MySQL, "mysql", "mysql-target", "root", "", "MySQL Shell"},
		{"MariaDB", dbx.MySQL, "maria", "worker", "loader", "", "mydumper / myloader"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			t.Cleanup(cancel)
			password := os.Getenv("FERRY_NATIVE_PASSWORD")
			port := "3306"
			if fixture.kind == dbx.Postgres {
				port = "5432"
			}
			srcHost, dstHost := prefix+"-"+fixture.src, prefix+"-"+fixture.dst
			srcAddr := nativeDocker(t, "port", srcHost, port+"/tcp")
			dstAddr := nativeDocker(t, "port", dstHost, port+"/tcp")
			adminSrc := migrationTestDB(t, ctx, fixture.kind, srcAddr, fixture.user, password, fixture.adminDB)
			defer adminSrc.Close()
			adminDst := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, fixture.adminDB)
			defer adminDst.Close()
			migrationExec(t, adminSrc, "CREATE DATABASE native_full", "CREATE DATABASE native_second")
			migrationExec(t, adminDst, "CREATE DATABASE native_full", "CREATE DATABASE native_tables", "CREATE DATABASE native_cancel", "CREATE DATABASE native_fallback", "CREATE DATABASE native_sentinel")
			src := migrationTestDB(t, ctx, fixture.kind, srcAddr, fixture.user, password, "native_full")
			defer src.Close()
			second := migrationTestDB(t, ctx, fixture.kind, srcAddr, fixture.user, password, "native_second")
			defer second.Close()
			migrationExec(t, src, "CREATE TABLE payload (id INT PRIMARY KEY, note varchar(150))", "INSERT INTO payload VALUES (1,'a'),(2,NULL),(3,'中文')",
				"CREATE TABLE empty_table (id INT PRIMARY KEY, parent_id INT, FOREIGN KEY (parent_id) REFERENCES payload(id))", "CREATE INDEX payload_note ON payload (note)",
				"CREATE VIEW payload_view AS SELECT id, note FROM payload")
			migrationExec(t, second, "CREATE TABLE second_marker (n INT PRIMARY KEY)", "INSERT INTO second_marker VALUES (79)")
			if fixture.kind == dbx.Postgres {
				migrationExec(t, src, "CREATE TABLE partitioned(id int) PARTITION BY RANGE(id)",
					"CREATE TABLE partition_0 PARTITION OF partitioned FOR VALUES FROM (0) TO (10)", "INSERT INTO partitioned VALUES (1),(2)")
			}
			if fixture.name == "MySQL" {
				for i := range 168 {
					migrationExec(t, src, fmt.Sprintf("CREATE TABLE small_%03d (id INT PRIMARY KEY, bytes VARBINARY(512))", i))
				}
				for i := range 6 {
					migrationExec(t, src, fmt.Sprintf(`INSERT INTO small_%03d
						WITH digit AS (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4
							UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9),
						seq AS (SELECT a.n * 1000 + b.n * 100 + c.n * 10 + d.n AS n FROM digit a CROSS JOIN digit b CROSS JOIN digit c CROSS JOIN digit d)
						SELECT n, REPEAT(SHA2(CONCAT(n, '-payload'), 256), 8) FROM seq`, i))
				}
			}
			full := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, "native_full")
			defer full.Close()
			selected := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, "native_tables")
			defer selected.Close()
			sentinel := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, "native_sentinel")
			defer sentinel.Close()
			migrationExec(t, selected, "CREATE TABLE unrelated (id INT)", "INSERT INTO unrelated VALUES (42)")
			migrationExec(t, sentinel, "CREATE TABLE untouched (id INT)", "INSERT INTO untouched VALUES (101)")

			sidecar := startMigrationSidecar(t, ctx, password)
			source := sidecar.save(fixture.kind, srcHost+":"+port, "原生迁移源", fixture.user, true)
			target := sidecar.save(fixture.kind, dstHost+":"+port, "原生迁移目标", fixture.user, false)
			ro := sidecar.save(fixture.kind, dstHost+":"+port, "只读原生目标", fixture.user, true)
			call := func(sub, scope, dbName, directory, sourceSSH, cancelAt string, dst dbx.Conn, tables ...proto.DBTransferTable) migrationWireResult {
				return nativeMigrationCall(sidecar, config, "worker", false, source, dst, sub, scope, dbName, directory, sourceSSH, cancelAt, tables)
			}
			wantOK := func(r migrationWireResult, method string) proto.DBTransferResult {
				t.Helper()
				var result proto.DBTransferResult
				if !r.OK || json.Unmarshal(r.Data, &result) != nil || result.Method != method {
					t.Fatalf("需要 %s 成功，得到 %s %s\n%s", method, r.Error, r.Data, strings.Join(r.Logs[max(0, len(r.Logs)-30):], "\n"))
				}
				if strings.Contains(strings.Join(r.Logs, "\n"), password) {
					t.Fatal("迁移日志含数据库密码")
				}
				return result
			}
			wantError := func(r migrationWireResult, expect string) {
				t.Helper()
				if r.OK || !strings.Contains(r.Error, expect) || strings.Contains(strings.Join(r.Logs, "\n"), password) {
					t.Fatalf("需要错误 %q，得到 %s\n%s", expect, r.Error, strings.Join(r.Logs[max(0, len(r.Logs)-15):], "\n"))
				}
			}
			wantError(call("native-plan", "database", "native_full", "", "", "", ro), "可写")
			plan := wantOK(call("native-plan", "database", "native_full", "", "", "", target), fixture.method)
			count := 2
			if fixture.name == "MySQL" {
				count = 170
			} else if fixture.kind == dbx.Postgres {
				count = 3
			}
			if plan.Tables != count {
				t.Fatalf("预检表数量 %d，应为 %d", plan.Tables, count)
			}
			result := wantOK(call("native-run", "database", "native_full", "", "", "", target), fixture.method)
			t.Logf("%s 迁移 %d 张表耗时 %d ms（执行服务器本地测试网络）", fixture.method, result.Tables, result.ElapsedMs)
			checkMigrationValue(t, full, "SELECT note FROM payload_view WHERE id=3", "中文")
			checkMigrationValue(t, full, "SELECT count(*) FROM payload WHERE note IS NULL", "1")
			checkMigrationValue(t, full, "SELECT count(*) FROM empty_table", "0")
			if fixture.kind == dbx.Postgres {
				checkMigrationValue(t, full, "SELECT count(*) FROM pg_indexes WHERE indexname='payload_note'", "1")
			} else {
				checkMigrationValue(t, full, "SELECT count(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=database() AND INDEX_NAME='payload_note'", "1")
			}
			if fixture.name == "MySQL" {
				for i := range 6 {
					checkMigrationValue(t, full, fmt.Sprintf("SELECT SUM(length(bytes)) FROM small_%03d", i), "5120000")
				}
			}
			wantError(call("native-run", "database", "native_full", "", "", "", target), "空目标库")
			payload := proto.DBTransferTable{Name: "payload"}
			child := proto.DBTransferTable{Name: "empty_table"}
			wantError(call("native-plan", "tables", "native_tables", "", "", "", target, child), "依赖 payload")
			wantOK(call("native-run", "tables", "native_tables", "", "", "", target, payload), fixture.method)
			checkMigrationValue(t, selected, "SELECT count(*) FROM payload", "3")
			checkMigrationValue(t, selected, "SELECT id FROM unrelated", "42")
			if _, err := selected.Exec("SELECT * FROM empty_table"); err == nil {
				t.Fatal("指定表迁移复制了未选中的表")
			}
			wantOK(call("native-run", "tables", "native_tables", "", "", "", target, child), fixture.method)
			if _, err := selected.Exec("INSERT INTO empty_table (id,parent_id) VALUES (1,999)"); err == nil {
				t.Fatal("迁移后外键没有生效")
			}
			wantError(call("native-run", "tables", "native_tables", "", "", "", target, payload), "同名")
			wantError(call("native-plan", "tables", "native_tables", "", "", "", target, proto.DBTransferTable{Name: "missing"}), "源库表不存在")

			if fixture.kind == dbx.Postgres {
				wantOK(call("native-run", "tables", "native_tables", "", "", "", target, proto.DBTransferTable{Name: "partitioned"}), fixture.method)
				checkMigrationValue(t, selected, "SELECT sum(id) FROM partitioned", "3")
				lock, err := src.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Rollback()
				if _, err := lock.Exec("LOCK TABLE payload IN ACCESS EXCLUSIVE MODE"); err != nil {
					t.Fatal(err)
				}
				wantError(call("native-run", "tables", "native_cancel", "", "", "并行导出 PostgreSQL 数据", target, payload), "已取消")
				_ = lock.Rollback()
				cancelDB := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, "native_cancel")
				defer cancelDB.Close()
				checkMigrationValue(t, cancelDB, "SELECT coalesce(to_regclass('payload')::text, 'missing')", "missing")
			}
			if fixture.name == "MySQL" {
				migrationExec(t, adminDst, "SET GLOBAL local_infile=OFF")
				wantOK(call("native-run", "tables", "native_fallback", "", "", "", target, payload), "mydumper / myloader")
				fallback := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, "native_fallback")
				defer fallback.Close()
				checkMigrationValue(t, fallback, "SELECT count(*) FROM payload", "3")
				migrationExec(t, adminDst, "SET GLOBAL local_infile=ON")
			}

			cloneTarget, cloneMethod, cloneDir, cloneSSH := target, "MySQL Clone Plugin", "/native/mysql-clone", ""
			if fixture.kind == dbx.Postgres {
				cloneTarget, cloneMethod, cloneDir = dbx.Conn{}, "pg_basebackup", "/native/pg-clone"
			} else if fixture.name == "MariaDB" {
				cloneTarget, cloneMethod, cloneDir, cloneSSH = dbx.Conn{}, "mariadb-backup", "/native/maria-clone", srcHost
			}
			wantError(call("native-plan", "instance", "", "/", cloneSSH, "", cloneTarget), "全新的")
			wantOK(call("native-plan", "instance", "", cloneDir, cloneSSH, "", cloneTarget), cloneMethod)
			cloned := wantOK(call("native-run", "instance", "", cloneDir, cloneSSH, "", cloneTarget), cloneMethod)
			t.Logf("%s 克隆实例耗时 %d ms", cloneMethod, cloned.ElapsedMs)
			if fixture.name == "MySQL" {
				wantError(call("native-run", "instance", "", cloneDir, "", "", cloneTarget), "克隆失败")
			} else {
				wantError(call("native-run", "instance", "", cloneDir, cloneSSH, "", cloneTarget), "目录已存在")
			}
			checkMigrationValue(t, sentinel, "SELECT id FROM untouched", "101")
			verifyNativeClone(t, fixture.name, prefix, "worker", cloneDir, fixture.user, password)

			// The relay worker is on a separate network from every database.
			wantError(nativeMigrationCall(sidecar, config, "relay", false, source, target, "native-plan", "database", "native_full", "", "", "", nil), "执行服务器无法访问源库")
			localSource := sidecar.save(fixture.kind, srcAddr, "本机中转源", fixture.user, true)
			localTarget := sidecar.save(fixture.kind, dstAddr, "本机中转目标", fixture.user, false)
			relay := func(sub, scope, dbName, directory string, tables ...proto.DBTransferTable) migrationWireResult {
				return nativeMigrationCall(sidecar, config, "relay", true, localSource, localTarget, sub, scope, dbName, directory, "", "", tables)
			}
			if err := full.Close(); err != nil {
				t.Fatal(err)
			}
			migrationExec(t, adminDst, "DROP DATABASE native_full", "CREATE DATABASE native_full", "CREATE DATABASE native_relay_tables")
			wantOK(relay("native-plan", "database", "native_full", ""), fixture.method)
			relayed := wantOK(relay("native-run", "database", "native_full", ""), fixture.method)
			t.Logf("%s 经本机中转迁移 %d 张表耗时 %d ms（Docker 测试网络）", relayed.Method, relayed.Tables, relayed.ElapsedMs)
			if relayed.Tables != count {
				t.Fatalf("本机中转整库表数量 %d，应为 %d", relayed.Tables, count)
			}
			relayDB := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, "native_full")
			defer relayDB.Close()
			checkMigrationValue(t, relayDB, "SELECT note FROM payload_view WHERE id=3", "中文")
			if fixture.name == "MySQL" {
				for i := range 6 {
					checkMigrationValue(t, relayDB, fmt.Sprintf("SELECT SUM(length(bytes)) FROM small_%03d", i), "5120000")
				}
			}
			wantOK(relay("native-run", "tables", "native_relay_tables", "", payload), fixture.method)
			relayTables := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, "native_relay_tables")
			defer relayTables.Close()
			checkMigrationValue(t, relayTables, "SELECT count(*) FROM payload", "3")
			if _, err := relayTables.Exec("SELECT * FROM empty_table"); err == nil {
				t.Fatal("本机中转复制了未选中的表")
			}
			if fixture.kind == dbx.Postgres {
				migrationExec(t, adminDst, "CREATE DATABASE native_relay_cancel")
				tx, err := src.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := tx.Exec("LOCK TABLE payload IN ACCESS EXCLUSIVE MODE"); err != nil {
					t.Fatal(err)
				}
				wantError(nativeMigrationCall(sidecar, config, "relay", true, localSource, localTarget, "native-run", "tables", "native_relay_cancel", "", "", "并行导出 PostgreSQL 数据", []proto.DBTransferTable{payload}), "已取消")
				_ = tx.Rollback()
				wantOK(relay("native-run", "tables", "native_relay_cancel", "", payload), fixture.method)
				destination := "/native/pg-relay-copy"
				wantOK(nativeMigrationCall(sidecar, config, "relay", true, localSource, dbx.Conn{}, "native-run", "instance", "", destination, "", "", nil), "pg_basebackup")
				verifyNativeClone(t, fixture.name, prefix, "relay", destination, fixture.user, password)
			}
			if fixture.name == "MariaDB" {
				migrationExec(t, adminDst, "CREATE DATABASE native_relay_ssh")
				savedVia := func(server string, readOnly bool) dbx.Conn {
					t.Helper()
					c := dbx.Conn{Kind: fixture.kind, Name: server + " SSH 本机数据库", Host: "127.0.0.1", Port: 3306,
						User: fixture.user, ReadOnly: readOnly, Via: &dbx.Via{Config: config, Target: server}}
					r := sidecar.do([]string{"db", "save"}, map[string]string{"FERRY_DB_CONN": migrationJSON(c)}, "")
					if !r.OK || json.Unmarshal(r.Data, &c) != nil || c.ID == "" {
						t.Fatalf("保存 SSH 数据库连接：%+v", r)
					}
					return c
				}
				sshSource, sshTarget := savedVia("maria", true), savedVia("worker", false)
				wantError(nativeMigrationCall(sidecar, config, "worker", false, sshSource, sshTarget, "native-plan", "tables", "native_relay_ssh", "", "", "", []proto.DBTransferTable{payload}), "请启用通过本机中转")
				wantOK(nativeMigrationCall(sidecar, config, "worker", true, sshSource, sshTarget, "native-run", "tables", "native_relay_ssh", "", "", "", []proto.DBTransferTable{payload}), fixture.method)
				sshDB := migrationTestDB(t, ctx, fixture.kind, dstAddr, fixture.user, password, "native_relay_ssh")
				defer sshDB.Close()
				checkMigrationValue(t, sshDB, "SELECT note FROM payload WHERE id=3", "中文")
			}
			if fixture.name == "MySQL" {
				verifyNativeRemedies(t, ctx, sidecar, config, prefix, dstAddr, adminDst, localSource, localTarget)
			}
		})
	}
}

func nativeMigrationCall(sidecar *migrationSidecar, config, worker string, relay bool, source, dst dbx.Conn,
	sub, scope, dbName, directory, sourceSSH, cancelAt string, tables []proto.DBTransferTable, allowLocalInfile ...bool) migrationWireResult {
	request := map[string]any{"worker": dbx.Via{Config: config, Target: worker}, "relay": relay, "scope": scope, "targetConn": dst.ID,
		"targetDb": dbName, "directory": directory, "sourceSsh": sourceSSH, "tables": tables, "threads": 4}
	if len(allowLocalInfile) > 0 {
		request["allowLocalInfile"] = allowLocalInfile[0]
	}
	env := map[string]string{"FERRY_DB_REQ": migrationJSON(map[string]any{"nativeMigration": request}), dbx.PasswordEnv(source.ID): sidecar.pass}
	if dst.ID != "" {
		env[dbx.PasswordEnv(dst.ID)] = sidecar.pass
	}
	return sidecar.do([]string{"db", sub, source.ID, "native_full", "-c", config}, env, cancelAt)
}

func verifyNativeRemedies(t *testing.T, ctx context.Context, sidecar *migrationSidecar, config, prefix, dstAddr string, admin *sql.DB, source, target dbx.Conn) {
	t.Helper()
	worker := prefix + "-relay"
	t.Cleanup(func() {
		nativeDocker(t, "exec", worker, "sh", "-ec", `
			if [ -f /var/tmp/ferry-tools-repo/mydumper.deb ] && ! command -v mydumper >/dev/null; then
				dpkg -i /var/tmp/ferry-tools-repo/mydumper.deb
			fi
			if [ -L /usr/local/bin/mysqlsh.disabled ]; then mv /usr/local/bin/mysqlsh.disabled /usr/local/bin/mysqlsh; fi
		`)
	})
	// Expose a fixture package through this disposable worker's APT repository
	// using the tested MyDumper binaries already installed in the fixture image.
	nativeDocker(t, "exec", worker, "sh", "-ec", `
		repo=/var/tmp/ferry-tools-repo
		staging=$(mktemp -d)
		trap 'rm -rf "$staging"' EXIT
		mkdir -p "$repo" "$staging/DEBIAN" "$staging/usr/bin"
		dpkg-query -s mydumper | sed '/^Status:/d' > "$staging/DEBIAN/control"
		cp /usr/bin/mydumper /usr/bin/myloader "$staging/usr/bin/"
		dpkg-deb -b "$staging" "$repo/mydumper.deb"
		cd "$repo"
		{ dpkg-deb -f mydumper.deb; printf 'Filename: ./mydumper.deb\nSize: %s\nSHA256: %s\n\n' "$(wc -c < mydumper.deb)" "$(sha256sum mydumper.deb | cut -d ' ' -f 1)"; } > Packages
		printf 'deb [trusted=yes] file:/var/tmp/ferry-tools-repo ./\n' > /etc/apt/sources.list.d/ferry-e2e.list
		apt-get -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/ferry-e2e.list -o Dir::Etc::sourceparts=- update
		dpkg -r mydumper
		mv /usr/local/bin/mysqlsh /usr/local/bin/mysqlsh.disabled
	`)
	migrationExec(t, admin, "SET GLOBAL local_infile=OFF", "CREATE DATABASE native_repair", "CREATE DATABASE native_temp_enable",
		"CREATE DATABASE native_temp_cancel", "CREATE DATABASE native_temp_denied")
	call := func(sub, database, cancelAt string, dst dbx.Conn, enable bool) migrationWireResult {
		return nativeMigrationCall(sidecar, config, "relay", true, source, dst, sub, "tables", database, "", "", cancelAt,
			[]proto.DBTransferTable{{Name: "payload"}}, enable)
	}
	issue := func(r migrationWireResult, code string) proto.DBTransferProblem {
		t.Helper()
		var d struct {
			Kind          string                  `json:"kind"`
			NativeProblem proto.DBTransferProblem `json:"nativeProblem"`
		}
		if r.OK || json.Unmarshal(r.ErrorDetail, &d) != nil || d.Kind != "db-native-problem" || d.NativeProblem.Code != code {
			t.Fatalf("期望迁移问题 %s，得到 %s %s", code, r.Error, r.ErrorDetail)
		}
		return d.NativeProblem
	}
	success := func(r migrationWireResult, method string) {
		t.Helper()
		var plan proto.DBTransferResult
		if !r.OK || json.Unmarshal(r.Data, &plan) != nil || plan.Method != method {
			t.Fatalf("期望 %s，得到 %s %s\n%s", method, r.Error, r.Data, strings.Join(r.Logs, "\n"))
		}
	}
	assertOff := func() {
		t.Helper()
		checkMigrationValue(t, admin, "SELECT @@GLOBAL.local_infile + 0", "0")
	}
	missing := issue(call("native-plan", "native_repair", "", target, false), "missing-tools")
	if strings.Join(missing.Install, ",") != "mydumper,mysql-shell" || missing.CanEnableLocalInfile {
		t.Fatalf("缺工具时应提供安装选项：%+v", missing)
	}
	nativeDocker(t, "exec", worker, "mv", "/usr/local/bin/mysqlsh.disabled", "/usr/local/bin/mysqlsh")
	disabled := issue(call("native-plan", "native_temp_enable", "", target, false), "local-infile")
	if !disabled.CanEnableLocalInfile || strings.Join(disabled.Install, ",") != "mydumper" {
		t.Fatalf("local_infile 关闭时应提供两种处理方式：%+v", disabled)
	}
	success(call("native-plan", "native_temp_enable", "", target, true), "MySQL Shell")
	assertOff()
	success(call("native-run", "native_temp_enable", "", target, true), "MySQL Shell")
	assertOff()
	enabledDB := migrationTestDB(t, ctx, dbx.MySQL, dstAddr, target.User, sidecar.pass, "native_temp_enable")
	defer enabledDB.Close()
	checkMigrationValue(t, enabledDB, "SELECT note FROM payload WHERE id=3", "中文")
	cancelled := call("native-run", "native_temp_cancel", "本次迁移已临时开启目标 MySQL 的 local_infile", target, true)
	if cancelled.OK || !strings.Contains(cancelled.Error, "已取消") {
		t.Fatalf("期望取消临时开启迁移，得到 %s", cancelled.Error)
	}
	assertOff()
	migrationExec(t, admin, fmt.Sprintf("CREATE USER 'ferry_limited'@'%%' IDENTIFIED BY '%s'", sidecar.pass),
		"GRANT ALL ON native_temp_denied.* TO 'ferry_limited'@'%'")
	limited := sidecar.save(dbx.MySQL, dstAddr, "没有参数管理权限", "ferry_limited", false)
	issue(call("native-run", "native_temp_denied", "", limited, true), "local-infile-denied")
	assertOff()
	installed := sidecar.do([]string{"db", "native-install-tools", "relay", "mydumper", "-c", config}, nil, "")
	if !installed.OK {
		t.Fatalf("安装迁移工具失败：%s\n%s", installed.Error, strings.Join(installed.Logs, "\n"))
	}
	success(call("native-plan", "native_repair", "", target, false), "mydumper / myloader")
	success(call("native-run", "native_repair", "", target, false), "mydumper / myloader")
	assertOff()
	repairedDB := migrationTestDB(t, ctx, dbx.MySQL, dstAddr, target.User, sidecar.pass, "native_repair")
	defer repairedDB.Close()
	checkMigrationValue(t, repairedDB, "SELECT note FROM payload WHERE id=3", "中文")
	issue(call("native-plan", "native_repair", "", target, false), "target-table-exists")
	t.Log("验证缺工具安装、自动恢复 local_infile、取消及权限拒绝")
}

func nativeDocker(t *testing.T, args ...string) string {
	t.Helper()
	data, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v：%v\n%s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

func verifyNativeClone(t *testing.T, name, prefix, workerName, directory, user, password string) {
	t.Helper()
	worker := prefix + "-" + workerName
	if name == "PostgreSQL" {
		nativeDocker(t, "exec", worker, "chown", "-R", "postgres:postgres", directory)
		nativeDocker(t, "exec", "--user", "postgres", worker, "/usr/lib/postgresql/17/bin/pg_ctl", "-D", directory,
			"-l", "/tmp/ferry-pg-clone.log", "-o", "-p 5543 -h 127.0.0.1", "-w", "start")
		got := nativeDocker(t, "exec", "-e", "PGPASSWORD="+password, worker, "psql", "-h", "127.0.0.1", "-p", "5543", "-U", user, "-d", "native_second", "-At", "-c", "SELECT n FROM second_marker")
		if got != "79" {
			t.Fatalf("克隆实例的第二个库内容不一致：%q", got)
		}
		return
	}
	var dest, program, client, extra string
	if name == "MySQL" {
		dest, program, client, extra = prefix+"-mysql-target", "mysqld", "mysql", "--mysqlx=OFF"
	} else {
		dest, program, client, extra = worker, "mariadbd", "mariadb", "--skip-log-bin"
	}
	nativeDocker(t, "exec", dest, "chown", "-R", "mysql:mysql", directory)
	nativeDocker(t, "exec", "-d", dest, program, "--user=mysql", "--datadir="+directory, "--socket=/tmp/ferry-physical-clone.sock",
		"--pid-file=/tmp/ferry-physical-clone.pid", "--log-error=/tmp/ferry-physical-clone.log", "--port=3307", "--bind-address=127.0.0.1", extra)
	var lastQuery string
	for range 40 {
		args := []string{"exec"}
		if name == "MySQL" {
			args = append(args, "-e", "MYSQL_PWD="+password)
		}
		args = append(args, dest, client, "--no-defaults", "--socket=/tmp/ferry-physical-clone.sock", "--user=root",
			"--batch", "--skip-column-names", "--execute", "SELECT n FROM native_second.second_marker")
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) == "79" {
			return
		}
		lastQuery = strings.TrimSpace(string(out))
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("启动克隆实例后查询失败：%s\n%s", lastQuery, nativeDocker(t, "exec", dest, "cat", "/tmp/ferry-physical-clone.log"))
}
