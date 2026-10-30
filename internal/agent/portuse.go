package agent

import (
	"archive/zip"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/proto"
)

// 把服务监听的端口分成主端口与附加端口，并尽量说明附加端口的用途。
//   主端口：配置里健康检查的端口；没配健康检查时，除了认出用途的附加端口，其余都算主端口。
//   用途：先看进程的启动参数（-Dcsp.sentinel.api.port=…、-agentlib:jdwp=… 等），
//   再看 jar 里打包的依赖（带 Sentinel 的服务会在 8719 往上开一个端口……）。

type portUse struct {
	use   string
	risky bool
}

var (
	useSentinel = portUse{"Sentinel 客户端端口：Sentinel 控制台通过它查看实时监控、下发限流规则。默认 8719，被占用时依次加 1。没有认证，不宜对外开放", true}
	useJDWP     = portUse{"Java 远程调试端口（JDWP）：连上就能控制整个进程，不应对外开放；生产环境一般应关闭", true}
	useJMX      = portUse{"JMX 远程管理端口：可查看、调整 JVM 运行状态，不宜对外开放", true}
	useJMXRMI   = portUse{"JMX 的 RMI 数据端口：配合 JMX 管理端口使用，不宜对外开放", true}
	useActuator = portUse{"Spring Boot 管理端口（actuator）：健康检查、监控指标等，可能暴露配置信息，不宜对外开放", true}
	useXXLJob   = portUse{"XXL-JOB 执行器端口：调度中心通过它触发定时任务", false}
	useDubbo    = portUse{"Dubbo 服务端口：其他服务通过它远程调用这个服务", false}
	useDubboQoS = portUse{"Dubbo QoS 运维端口：可以在线上下线服务，不宜对外开放", true}
)

// argPorts 启动参数里的端口属性（-Dname=值 或 --name=值）。
var argPorts = map[string]portUse{
	"csp.sentinel.api.port":                 useSentinel,
	"spring.cloud.sentinel.transport.port":  useSentinel,
	"com.sun.management.jmxremote.port":     useJMX,
	"com.sun.management.jmxremote.rmi.port": useJMXRMI,
	"management.server.port":                useActuator,
	"xxl.job.executor.port":                 useXXLJob,
	"dubbo.protocol.port":                   useDubbo,
	"dubbo.application.qos-port":            useDubboQoS,
	"dubbo.application.qos.port":            useDubboQoS,
	"server.port":                           {}, // 业务端口
}

// libPorts jar 里打包了这些依赖时，默认端口范围内的端口就是它开的。
var libPorts = []struct {
	markers []string // 依赖 jar 名或类路径的片段
	lo, hi  int      // 端口范围（含被占用后递增的余量）
	portUse
}{
	{[]string{"sentinel-transport-", "com/alibaba/csp/sentinel/transport/"}, 8719, 8819, useSentinel},
	{[]string{"xxl-job-core", "com/xxl/job/core/"}, 9949, 10049, useXXLJob},
	{[]string{"dubbo-", "org/apache/dubbo/"}, 20880, 20980, useDubbo},
	{[]string{"dubbo-", "org/apache/dubbo/"}, 22222, 22232, useDubboQoS},
	{[]string{"powerjob-worker", "tech/powerjob/worker/"}, 27777, 27877, portUse{"PowerJob 执行器端口：调度服务器通过它派发任务", false}},
	{[]string{"arthas-", "com/taobao/arthas/"}, 3658, 3658, portUse{"Arthas 诊断端口（telnet）：能查看、修改运行中的程序，不应对外开放", true}},
	{[]string{"arthas-", "com/taobao/arthas/"}, 8563, 8563, portUse{"Arthas 诊断端口（http）：能查看、修改运行中的程序，不应对外开放", true}},
	{[]string{"grpc-server-spring-boot", "net/devh/boot/grpc/server/"}, 9090, 9099, portUse{"gRPC 服务端口：其他服务通过它远程调用这个服务", false}},
	{[]string{"hazelcast", "com/hazelcast/"}, 5701, 5801, portUse{"Hazelcast 集群通信端口：集群节点之间同步数据", true}},
	{[]string{"spring-boot-devtools", "org/springframework/boot/devtools/"}, 35729, 35729, portUse{"Spring Boot DevTools 自动刷新端口：开发用，生产环境不应带上 devtools", true}},
}

func (a *Agent) classifyPorts(s *proto.AppSpec, main string, all []string, pid int) (listen []string, extra []proto.ExtraPort) {
	if len(all) == 0 {
		return nil, nil
	}
	known, mainArgs := portsFromArgs(a.Sys.ProcArgs(pid))
	var libs map[int]bool // libPorts 里命中的下标；用到时才读 jar
	libUse := func(port int) (portUse, bool) {
		if libs == nil {
			libs = jarLibs(a.jarPath(s))
		}
		for i, l := range libPorts {
			if libs[i] && port >= l.lo && port <= l.hi {
				return l.portUse, true
			}
		}
		return portUse{}, false
	}
	for _, addr := range all {
		port := portNum(addr)
		p := strconv.Itoa(port)
		switch {
		case main != "" && p == main:
			listen = append(listen, addr)
		case known[port].use != "":
			extra = append(extra, proto.ExtraPort{Addr: addr, Use: known[port].use, Risky: known[port].risky})
		case main == "" && mainArgs[port]:
			listen = append(listen, addr)
		default:
			if u, ok := libUse(port); ok {
				extra = append(extra, proto.ExtraPort{Addr: addr, Use: u.use, Risky: u.risky})
			} else if main != "" {
				extra = append(extra, proto.ExtraPort{Addr: addr}) // 用途未识别
			} else {
				listen = append(listen, addr)
			}
		}
	}
	return listen, extra
}

// portsFromArgs 从启动参数里认出端口用途；mainArgs 为 server.port 指定的业务端口。
func portsFromArgs(procs [][]string) (known map[int]portUse, mainArgs map[int]bool) {
	known, mainArgs = map[int]portUse{}, map[int]bool{}
	for _, args := range procs {
		for _, arg := range args {
			if v, ok := jdwpAddress(arg); ok {
				if n, err := strconv.Atoi(v[strings.LastIndexByte(v, ':')+1:]); err == nil {
					known[n] = useJDWP
				}
				continue
			}
			var kv string
			switch {
			case strings.HasPrefix(arg, "-D"):
				kv = arg[2:]
			case strings.HasPrefix(arg, "--"):
				kv = arg[2:]
			default:
				continue
			}
			name, val, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			u, ok := argPorts[name]
			n, err := strconv.Atoi(val)
			if !ok || err != nil {
				continue
			}
			if name == "server.port" {
				mainArgs[n] = true
			} else if u.use != "" {
				known[n] = u
			}
		}
	}
	return known, mainArgs
}

// jdwpAddress -agentlib:jdwp=transport=dt_socket,server=y,address=*:5005 → *:5005（-Xrunjdwp: 同理）。
func jdwpAddress(arg string) (string, bool) {
	var opts string
	switch {
	case strings.HasPrefix(arg, "-agentlib:jdwp="):
		opts = strings.TrimPrefix(arg, "-agentlib:jdwp=")
	case strings.HasPrefix(arg, "-Xrunjdwp:"):
		opts = strings.TrimPrefix(arg, "-Xrunjdwp:")
	default:
		return "", false
	}
	for _, o := range strings.Split(opts, ",") {
		if v, ok := strings.CutPrefix(o, "address="); ok {
			return v, true
		}
	}
	return "", false
}

// jarPath 当前版本的 jar（java 应用）；其他类型为空。
func (a *Agent) jarPath(s *proto.AppSpec) string {
	if s.Type != proto.TypeJava || s.FileName == "" {
		return ""
	}
	cur := a.current(s.Name)
	if cur == "" {
		return ""
	}
	return filepath.Join(a.releaseDir(s.Name, cur), s.FileName)
}

// jarLibs jar 里打包了 libPorts 中的哪些依赖（只读 zip 目录，不解压）。
func jarLibs(jar string) map[int]bool {
	hit := map[int]bool{}
	if jar == "" {
		return hit
	}
	z, err := zip.OpenReader(jar)
	if err != nil {
		return hit
	}
	defer z.Close()
	for _, f := range z.File {
		for i, l := range libPorts {
			if hit[i] {
				continue
			}
			for _, m := range l.markers {
				if strings.Contains(f.Name, m) {
					hit[i] = true
					break
				}
			}
		}
	}
	return hit
}
