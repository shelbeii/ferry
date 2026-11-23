package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/remote"
)

// DBTransferDial opens a database connection from Ferry's own network or over
// the database's saved SSH connection.
type DBTransferDial func(context.Context) (net.Conn, error)

func (e *Engine) InstallDBTransferTools(target, pkg string) error {
	if pkg != "mydumper" && pkg != "mysql-shell" {
		return errors.New("不支持的数据库迁移工具")
	}
	t, err := e.File.Target(target)
	if err != nil {
		return err
	}
	s, err := e.connectSSH(t)
	if err != nil {
		return err
	}
	defer s.close()
	if t.User != "root" && !s.allowsSudo() {
		return errors.New("安装迁移工具需要执行服务器上的 root 或已配置的 sudo")
	}
	if err := s.prepareAgent(); err != nil {
		return err
	}
	network := &proto.SoftwarePlan{System: &proto.PlanSystem{}}
	proxy, err := e.softwareNetwork(s, network, "auto")
	if err != nil {
		return err
	}
	if proxy != nil {
		defer e.closeNetProxy(proxy)
	}
	return s.callData("db-tools-install", &proto.Request{DBTools: pkg, Install: network}, nil)
}

// DBTransfer runs the tools on the selected server. A non-nil dialer for an
// endpoint lets those tools reach it through Ferry over SSH.
func (e *Engine) DBTransfer(target string, request proto.DBTransfer, source, destination DBTransferDial) (*proto.DBTransferResult, error) {
	t, err := e.File.Target(target)
	if err != nil {
		return nil, err
	}
	s, err := e.connectSSH(t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	if t.User != "root" && !s.allowsSudo() {
		return nil, errors.New("执行服务器需要 root 或已配置的 sudo 来运行 Ferry 组件")
	}
	if err := s.prepareAgent(); err != nil {
		return nil, err
	}
	ctx := e.ctx()
	mayChangeLocalInfile := request.AllowLocalInfile && !request.Plan
	// Keep the database routes alive while the agent restores a temporary setting
	// after cancellation. The task still owns and closes every relay.
	relayCtx := ctx
	if mayChangeLocalInfile {
		relayCtx = context.WithoutCancel(ctx)
	}
	for _, route := range []struct {
		name     string
		endpoint *proto.DBEndpoint
		dial     DBTransferDial
	}{
		{"源库", &request.Source, source},
		{"目标库", &request.Target, destination},
	} {
		if route.dial == nil {
			continue
		}
		port, closeRelay, err := s.relayDatabase(ctx, relayCtx, route.dial)
		if err != nil {
			return nil, fmt.Errorf("无法经本机中转%s：%w", route.name, err)
		}
		defer closeRelay()
		route.endpoint.Host, route.endpoint.Port = "127.0.0.1", port
	}
	if source != nil || destination != nil {
		e.Rep.Step("通过本机 SSH 通道中转数据库连接")
	}
	var pipe *remote.Pipe
	if s.passwordSudo() {
		pipe, err = s.c.StartSudoPipe(ctx, s.agentCommand("db-transfer"), s.sudoPassword)
	} else {
		pipe, err = s.c.StartPipeContext(ctx, s.sudo+s.agentCommand("db-transfer"))
	}
	if err != nil {
		return nil, s.sudoError(err)
	}
	defer pipe.Close()
	done := make(chan struct{})
	defer close(done)
	stop := context.AfterFunc(ctx, func() {
		_ = pipe.In.Close()
		grace := 5 * time.Second
		if mayChangeLocalInfile {
			grace = 12 * time.Second
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			_ = pipe.Close()
		}
	})
	defer stop()
	if err := json.NewEncoder(pipe.In).Encode(request); err != nil {
		return nil, errors.New("无法向执行服务器发送迁移请求")
	}
	var result *proto.DBTransferResult
	var failure string
	var problem *proto.DBTransferProblem
	lines := bufio.NewScanner(pipe.Out)
	lines.Buffer(make([]byte, 32<<10), 8<<20)
	for lines.Scan() {
		var ev proto.DBTransferEvent
		if err := json.Unmarshal(lines.Bytes(), &ev); err != nil {
			return nil, errors.New("执行服务器返回了无效的迁移结果")
		}
		switch ev.Kind {
		case "step":
			e.Rep.Step("%s", ev.Text)
		case "info":
			e.Rep.Info("%s", ev.Text)
		case "error":
			failure = ev.Text
			problem = ev.Problem
		case "result":
			result = ev.Result
		}
	}
	if err := lines.Err(); err != nil {
		return nil, dbTransferInterrupted(fmt.Errorf("读取服务器迁移结果：%w", err), mayChangeLocalInfile)
	}
	if err := pipe.Wait(); err != nil && result == nil && failure == "" && ctx.Err() == nil {
		return nil, dbTransferInterrupted(fmt.Errorf("执行服务器迁移命令失败：%v %s", err, pipe.Stderr()), mayChangeLocalInfile)
	}
	if problem != nil && (ctx.Err() == nil || problem.Code == "local-infile-restore") {
		problem.Message = failure
		return nil, problem
	}
	if ctx.Err() != nil {
		return nil, dbTransferInterrupted(fmt.Errorf("已取消，请检查目标库或新目录的状态：%w", ctx.Err()), mayChangeLocalInfile && result == nil && failure == "")
	}
	if failure != "" {
		return nil, errors.New(failure)
	}
	if result == nil {
		return nil, dbTransferInterrupted(errors.New("执行服务器没有返回迁移结果"), mayChangeLocalInfile)
	}
	return result, nil
}

func dbTransferInterrupted(err error, checkLocalInfile bool) error {
	if checkLocalInfile {
		return &proto.DBTransferProblem{Code: "local-infile-restore", Message: err.Error() + "。未收到 MySQL 设置恢复结果，请检查目标实例的 local_infile"}
	}
	return err
}

func (s *session) relayDatabase(ctx, relayCtx context.Context, dial DBTransferDial) (int, func(), error) {
	probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
	probe, err := dial(probeCtx)
	cancelProbe()
	if err != nil {
		return 0, nil, err
	}
	_ = probe.Close()

	ln, err := s.c.ListenRemote("127.0.0.1:0")
	if err != nil {
		return 0, nil, fmt.Errorf("执行服务器无法建立 SSH 反向端口转发，请检查 AllowTcpForwarding：%w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// sshd 的 GatewayPorts=yes 可能把请求的回环地址改成通配地址。
	if err := s.checkDatabaseRelayBinding(ctx, port); err != nil {
		_ = ln.Close()
		return 0, nil, err
	}
	ctx, cancel := context.WithCancel(relayCtx)
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	go func() {
		for {
			worker, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer worker.Close()
				dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
				db, err := dial(dialCtx)
				cancelDial()
				if err != nil {
					return
				}
				defer db.Close()
				stop := context.AfterFunc(ctx, func() {
					_ = worker.Close()
					_ = db.Close()
				})
				defer stop()
				copyTCP(worker, db)
			}()
		}
	}()
	return port, func() {
		stop()
		cancel()
		_ = ln.Close()
	}, nil
}

func (s *session) checkDatabaseRelayBinding(ctx context.Context, port int) error {
	var foundLoopback, foundOther bool
	suffix := fmt.Sprintf(":%04X", port)
	result, err := s.c.RunContext(ctx, "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null || cat /proc/net/tcp", nil, func(line string) {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" || !strings.HasSuffix(fields[1], suffix) {
			return
		}
		if fields[1] == "0100007F"+suffix {
			foundLoopback = true
		} else {
			foundOther = true
		}
	})
	if err != nil {
		return fmt.Errorf("无法检查执行服务器的转发监听地址：%w", err)
	}
	if result.Code != 0 || !foundLoopback || foundOther {
		return errors.New("SSH 反向转发需要只监听执行服务器的 127.0.0.1，请检查 sshd 的 GatewayPorts 设置")
	}
	return nil
}
