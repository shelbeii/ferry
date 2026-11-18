package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func (a *Agent) checkServiceRequirements(spec *proto.AppSpec, report *proto.Preflight) {
	required := spec.Requirements
	if required == nil {
		return
	}
	if len(required.RequiredEnv) > 0 {
		env := map[string]bool{}
		for _, line := range unitValues(spec.UnitText, "Environment") {
			for _, item := range sdSplit(line) {
				if key, value, ok := strings.Cut(item, "="); ok {
					env[key] = value != ""
				}
			}
		}
		for _, file := range unitValues(spec.UnitText, "EnvironmentFile") {
			file, ok := plainPath(strings.TrimPrefix(file, "-"))
			if !ok || isAppSecretsFile(file) {
				continue
			}
			values, err := readEnvFile(file)
			if err != nil {
				continue // 文件存在性由 checkEnvFiles 报告
			}
			for key, value := range values {
				env[key] = value != ""
			}
		}
		for _, key := range required.AvailableSecretEnv {
			env[key] = true
		}
		for _, key := range required.RequiredEnv {
			if !env[key] {
				report.Problems = append(report.Problems, fmt.Sprintf("%s 缺少非空的必需环境变量 %s", spec.Name, key))
			}
		}
	}
	if len(required.Dependencies) > 16 {
		report.Problems = append(report.Problems, spec.Name+" 的预检依赖超过 16 项")
		return
	}
	var wg sync.WaitGroup
	problems := make([]string, len(required.Dependencies))
	for i, dep := range required.Dependencies {
		wg.Go(func() {
			if err := a.probeServiceDependency(dep); err != nil {
				problems[i] = fmt.Sprintf("%s 的依赖 %s 检查失败：%v", spec.Name, dep.Name, err)
			}
		})
	}
	wg.Wait()
	for _, problem := range problems {
		if problem != "" {
			report.Problems = append(report.Problems, problem)
		}
	}
}

func (a *Agent) probeServiceDependency(dep proto.ServiceDependency) error {
	const timeout = 3 * time.Second
	switch dep.Protocol {
	case "http", "nacos":
		u, err := url.Parse(dep.Address)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
			return errors.New("需要 HTTP 或 HTTPS 健康检查地址，省去用户信息和查询参数")
		}
		return a.Sys.Probe(proto.Health{HTTP: dep.Address, TimeoutSec: 3})
	case "", "tcp", "mysql", "redis":
	default:
		return errors.New("预检协议无效")
	}
	if _, _, err := net.SplitHostPort(dep.Address); err != nil {
		return errors.New("地址要写成主机:端口")
	}
	conn, err := net.DialTimeout("tcp", dep.Address, timeout)
	if err != nil {
		return fmt.Errorf("无法连接 %s：%w", dep.Address, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	switch dep.Protocol {
	case "mysql":
		var hello [5]byte
		if _, err := io.ReadFull(conn, hello[:]); err != nil {
			return fmt.Errorf("读取 MySQL 握手失败：%w", err)
		}
		if hello[3] != 0 || hello[4] != 10 {
			return errors.New("目标没有返回 MySQL 协议握手")
		}
	case "redis":
		if _, err := io.WriteString(conn, "*1\r\n$4\r\nPING\r\n"); err != nil {
			return fmt.Errorf("发送 Redis PING 失败：%w", err)
		}
		reply, err := bufio.NewReaderSize(conn, 256).ReadSlice('\n')
		if err != nil {
			return fmt.Errorf("读取 Redis PING 失败：%w", err)
		}
		if string(reply) != "+PONG\r\n" && !strings.HasPrefix(string(reply), "-NOAUTH ") {
			return errors.New("目标没有返回 Redis PING 响应")
		}
	}
	return nil
}
