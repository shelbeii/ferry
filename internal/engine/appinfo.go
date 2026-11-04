package engine

import (
	"fmt"

	"github.com/captain/ferry/internal/proto"
)

// AppInfo 读取单个应用的实际运行环境、进程和部署位置。
func (e *Engine) AppInfo(target, app string) (*proto.AppInfo, error) {
	if app == "" {
		return nil, fmt.Errorf("请指定要查看的应用")
	}
	var out proto.AppInfo
	err := e.withSession(target, []string{app}, func(s *session, specs []proto.AppSpec) error {
		if len(specs) != 1 {
			return fmt.Errorf("查看运行情况一次只能指定一个应用")
		}
		return s.callData("appinfo", &proto.Request{Apps: specs}, &out)
	})
	return &out, err
}

func (e *Engine) AppRuntimes(target, app string) (*proto.AppRuntimeChoices, error) {
	var out proto.AppRuntimeChoices
	if app == "" {
		return nil, fmt.Errorf("请选择应用")
	}
	err := e.withSession(target, []string{app}, func(s *session, specs []proto.AppSpec) error {
		if len(specs) != 1 {
			return fmt.Errorf("请选择单个应用")
		}
		return s.callData("appruntimes", &proto.Request{Apps: specs}, &out)
	})
	return &out, err
}
