package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/captain/ferry/internal/engine"
	"github.com/captain/ferry/internal/proto"
	"github.com/captain/ferry/internal/ui"
)

func softwareConfigCmd(c *cli, cio *cliIO, e *engine.Engine, js *ui.JSON, fail func(error) int) int {
	if len(c.pos) < 3 {
		return fail(errors.New("用法：ferry software config list|read|save|apply|restore|associate <目标> [实例] [--unit 服务 | --container 容器 | --pid PID] [--id 配置ID]"))
	}
	q := &proto.SoftwareConfigRequest{Operation: c.pos[1], Unit: c.flags["unit"], Container: c.flags["container"], ID: c.flags["id"]}
	q.PID, _ = strconv.Atoi(c.flags["pid"])
	if q.Operation == "associate" {
		var value struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(cio.getenv("FERRY_FILE_VALUE")), &value) != nil || value.Path == "" {
			return fail(errors.New("请通过 FERRY_FILE_VALUE 提供配置路径"))
		}
		q.FilePath = value.Path
	}
	if len(c.pos) > 3 {
		q.Instance = c.pos[3]
	}
	if q.Operation == "save" || q.Operation == "restore" {
		var value struct {
			Revision string  `json:"revision"`
			Content  *string `json:"content"`
		}
		if err := json.Unmarshal([]byte(cio.getenv("FERRY_FILE_VALUE")), &value); err != nil {
			return fail(errors.New("请通过 FERRY_FILE_VALUE 提供配置版本与内容"))
		}
		q.Revision, q.Content = value.Revision, value.Content
	}
	r, err := e.SoftwareConfig(c.pos[2], q)
	if err != nil {
		return fail(err)
	}
	if js != nil {
		return ok0(js, r)
	}
	fmt.Fprintln(cio.stdout, string(r))
	return 0
}
