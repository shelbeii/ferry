package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/captain/ferry/internal/notify"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/secret"
)

// MonitorOptions 打开监控时的设置。Channels 是设置 → 通知里的 ID 或名字；空 = 全部开着的。
type MonitorOptions struct {
	Channels    []string
	IntervalMin int
	DiskPct     int
	CertDays    int
}

// Monitor 服务器监控：on 打开（把告警发到的地方和设置存到服务器上，装上定时器）、off 关闭、status 查看、
// check 立即检查一次（不发通知）、test 发一条测试告警。
func (e *Engine) Monitor(target, action string, o MonitorOptions) (proto.MonitorStatus, error) {
	var out proto.MonitorStatus
	t, err := e.File.Target(target)
	if err != nil {
		return out, err
	}
	spec := proto.MonitorSpec{Action: action, Project: e.File.Project, Target: t.Name, Host: t.Host,
		IntervalMin: o.IntervalMin, DiskPct: o.DiskPct, CertDays: o.CertDays}
	if action == "on" {
		if spec.Channels, err = e.monitorChannels(o.Channels); err != nil {
			return out, err
		}
	}
	err = e.withSession(target, []string{}, func(s *session, _ []proto.AppSpec) error {
		return s.callData("monitor", &proto.Request{Monitor: &spec}, &out)
	})
	return out, err
}

// monitorChannels 告警发到哪：设置里的通知方式（按 ID 或名字选），地址、加签密钥从凭据库取。
func (e *Engine) monitorChannels(want []string) ([]proto.MonitorChannel, error) {
	var out []proto.MonitorChannel
	var missing []string
	for _, ch := range e.Settings.Notify.Channels {
		if len(want) > 0 && !slices.Contains(want, ch.ID) && !slices.Contains(want, ch.Name) {
			continue
		}
		if len(want) == 0 && ch.Off {
			continue
		}
		u, _, err := secret.Lookup(e.getenv, "", secret.NotifyURLAccount(ch.ID))
		if err != nil || u == "" {
			missing = append(missing, ch.Name)
			continue
		}
		sign, _, _ := secret.Lookup(e.getenv, "", secret.NotifySignAccount(ch.ID))
		out = append(out, proto.MonitorChannel{Name: ch.Name, Kind: ch.Kind, URL: u, Sign: sign})
	}
	// 环境变量 FERRY_NOTIFY 里的（CI 里用；没选具体的时候也算上）
	if env, err := notify.EnvTargets(e.getenv); err == nil && len(want) == 0 {
		for _, t := range env {
			out = append(out, proto.MonitorChannel{Name: t.Channel.Name, Kind: t.Channel.Kind, URL: t.URL, Sign: t.Sign})
		}
	}
	if len(out) == 0 {
		if len(missing) > 0 {
			return nil, fmt.Errorf("%s 没有保存地址：在设置 → 通知里填上", strings.Join(missing, "、"))
		}
		return nil, errors.New("还没有地方发告警：先在设置 → 通知里添加企业微信、钉钉、飞书群机器人或自定义接口")
	}
	return out, nil
}
