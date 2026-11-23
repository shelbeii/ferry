// Package dbnative runs database vendor tools on the Linux server selected in Ferry.
package dbnative

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type transfer struct {
	spec           proto.DBTransfer
	source, target *sql.DB
	result         proto.DBTransferResult
	output         func(proto.DBTransferEvent)
	mariaSourceID  string
}

func Run(ctx context.Context, spec proto.DBTransfer, emit func(proto.DBTransferEvent)) (proto.DBTransferResult, error) {
	started := time.Now()
	if spec.Threads == 0 {
		spec.Threads = 4
	}
	if err := validate(spec); err != nil {
		return proto.DBTransferResult{}, err
	}
	t := transfer{spec: spec, output: emit}
	t.stage("检查原生工具、连接和目标范围")
	if err := t.plan(ctx); err != nil {
		return t.result, err
	}
	defer t.source.Close()
	if t.target != nil {
		defer t.target.Close()
	}
	if spec.Plan {
		return t.result, nil
	}
	var err error
	if spec.Scope == "instance" {
		err = t.physical(ctx)
	} else {
		err = t.logical(ctx)
	}
	t.result.ElapsedMs = time.Since(started).Milliseconds()
	return t.result, err
}

func validate(s proto.DBTransfer) error {
	if s.Threads < 2 || s.Threads > 16 {
		return errors.New("并行任务数须在 2 到 16 之间")
	}
	if !slices.Contains([]string{"mysql", "postgres"}, s.Engine) {
		return errors.New("服务器端迁移支持 MySQL、MariaDB 和 PostgreSQL")
	}
	switch s.Scope {
	case "instance":
		if len(s.Tables) != 0 || s.Target.Database != "" || !filepath.IsAbs(s.Directory) || filepath.Clean(s.Directory) != s.Directory || s.Directory == "/" || strings.ContainsAny(s.Directory, "\x00\r\n") {
			return errors.New("实例克隆须指定一个全新的 Linux 绝对数据目录")
		}
	case "tables", "database":
		if s.Source.Database == "" || s.Target.Database == "" || (s.Scope == "tables") != (len(s.Tables) > 0) || s.Directory != "" || s.SourceSSH != "" {
			return errors.New("请选择源库、目标库及迁移范围")
		}
		for _, db := range []string{s.Source.Database, s.Target.Database} {
			if slices.Contains([]string{"mysql", "information_schema", "performance_schema", "sys", "template0", "template1"}, strings.ToLower(db)) {
				return errors.New("请选择业务数据库")
			}
		}
	default:
		return errors.New("请选择表、数据库或实例范围")
	}
	for _, e := range []proto.DBEndpoint{s.Source, s.Target} {
		if e == (proto.DBEndpoint{}) {
			continue // physical PostgreSQL and MariaDB write a directory on the worker
		}
		if e.Host == "" || e.User == "" || e.Port < 1 || e.Port > 65535 || strings.ContainsAny(e.Host+e.User+e.Password+e.Database, "\x00\r\n") || !slices.Contains([]string{"", "off", "require", "verify"}, e.TLS) {
			return errors.New("数据库连接参数无效")
		}
	}
	if s.Source == (proto.DBEndpoint{}) || s.Scope != "instance" && s.Target == (proto.DBEndpoint{}) {
		return errors.New("缺少数据库连接")
	}
	if s.Source.Host == s.Target.Host && s.Source.Port == s.Target.Port && (s.Scope == "instance" || s.Source.Database == s.Target.Database) {
		return errors.New("源和目标相同")
	}
	return nil
}

func (t *transfer) plan(ctx context.Context) (err error) {
	t.source, err = openDB(t.spec.Engine, t.spec.Source)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			t.source.Close()
			if t.target != nil {
				t.target.Close()
			}
		}
	}()
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err = t.source.QueryRowContext(probe, "SELECT VERSION()").Scan(&t.result.SourceVersion); err != nil {
		return &proto.DBTransferProblem{Code: "source-unreachable", Message: fmt.Sprintf("执行服务器无法访问源库：%v", err)}
	}
	if t.spec.Target != (proto.DBEndpoint{}) {
		t.target, err = openDB(t.spec.Engine, t.spec.Target)
		if err != nil {
			return err
		}
		if err = t.target.QueryRowContext(probe, "SELECT VERSION()").Scan(&t.result.TargetVersion); err != nil {
			return &proto.DBTransferProblem{Code: "target-unreachable", Message: fmt.Sprintf("执行服务器无法访问目标库：%v", err)}
		}
		if t.isMaria() != strings.Contains(strings.ToLower(t.result.TargetVersion), "mariadb") {
			return errors.New("请选择相同数据库引擎")
		}
	}
	if t.spec.Scope == "instance" {
		return t.planPhysical(probe)
	}
	return t.planLogical(probe)
}

func (t *transfer) isMaria() bool {
	return strings.Contains(strings.ToLower(t.result.SourceVersion), "mariadb")
}

func openDB(kind string, e proto.DBEndpoint) (*sql.DB, error) {
	if kind == "postgres" {
		uri := pgURL(e)
		uri.User = url.UserPassword(e.User, e.Password)
		return sql.Open("pgx", uri.String())
	}
	cfg := mysql.NewConfig()
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	cfg.User, cfg.Passwd, cfg.DBName = e.User, e.Password, e.Database
	cfg.Timeout, cfg.InterpolateParams = 6*time.Second, true
	switch e.TLS {
	case "off":
	case "verify":
		cfg.TLS = &tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12}
	case "require":
		cfg.TLSConfig = "skip-verify"
	default:
		cfg.TLSConfig = "preferred"
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

func pgURL(e proto.DBEndpoint) *url.URL {
	mode := map[string]string{"off": "disable", "verify": "verify-full", "require": "require", "": "prefer"}[e.TLS]
	db := e.Database
	if db == "" {
		db = "postgres"
	}
	return &url.URL{Scheme: "postgresql", Host: net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), User: url.User(e.User), Path: "/" + db,
		RawQuery: url.Values{"sslmode": {mode}, "connect_timeout": {"6"}}.Encode()}
}

func requireTools(names ...string) error {
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("执行服务器缺少 %s，请先安装相应数据库客户端工具", name)
		}
	}
	return nil
}

func newDestination(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("目标目录已存在：%s", path)
	}
	if parent, err := os.Stat(filepath.Dir(path)); err != nil || !parent.IsDir() {
		return errors.New("目标目录的上一级目录需要先创建")
	}
	return nil
}

func (t *transfer) stage(text string) { t.output(proto.DBTransferEvent{Kind: "step", Text: text}) }

func (t *transfer) filter(text string) string {
	for _, pwd := range []string{t.spec.Source.Password, t.spec.Target.Password} {
		if pwd != "" {
			text = strings.ReplaceAll(text, pwd, "***")
			text = strings.ReplaceAll(text, url.QueryEscape(pwd), "***")
			text = strings.ReplaceAll(text, strings.TrimPrefix(url.UserPassword("", pwd).String(), ":"), "***")
		}
	}
	return text
}

func (t *transfer) command(ctx context.Context, env []string, name string, args ...string) error {
	cmd := nativeCommand(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out := &commandOutput{log: func(line string) { t.output(proto.DBTransferEvent{Kind: "info", Text: t.filter(line)}) }}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	out.flush()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("%s：%v", name, err)
	}
	return nil
}

// Stdout and stderr use the same writer so the subprocess cannot interleave
// fragments of two lines. Bound the size of a line before sending it to SSH.
type commandOutput struct {
	mu  sync.Mutex
	buf []byte
	log func(string)
}

func (w *commandOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range p {
		if c == '\n' || c == '\r' {
			w.flush()
		} else if len(w.buf) < 16<<10 {
			w.buf = append(w.buf, c)
		}
	}
	return len(p), nil
}

func (w *commandOutput) flush() {
	if len(w.buf) != 0 {
		w.log(string(w.buf))
		w.buf = w.buf[:0]
	}
}
