package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/captain/ferry/internal/proto"
)

func jobApp(schedule string) proto.AppSpec {
	s := javaApp("Report")
	s.UnitText = ManagedMarker + "\n# project=demo app=Report target=prod job=yes\n[Service]\nType=oneshot\nExecStart=/usr/bin/java -jar current/Report.jar\n"
	s.Health = proto.Health{}
	timer := "demo-report-schedule.timer"
	s.Job = &proto.JobSpec{Timer: timer, Schedule: []string{schedule}, TimeoutSec: 3600,
		TimerText: ManagedMarker + "\n# project=demo app=Report target=prod job=yes\n[Timer]\nOnCalendar=" + schedule + "\nUnit=demo-report.service\n"}
	return s
}

func calls(f *fakeSys) string { return strings.Join(f.calls, "\n") }

func TestJobDeployRollbackControl(t *testing.T) {
	f := newFake(t)
	job := jobApp("*-*-* 02:30:00")
	r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{job}, id1, map[string][]byte{"Report": []byte("v1")}))
	if r.code != 0 {
		t.Fatal(r.dump())
	}
	c := calls(f)
	if !strings.Contains(c, "enable --now demo-report-schedule.timer") || strings.Contains(c, "restart demo-report.service") {
		t.Fatalf("定时任务：打开定时器、不启动服务：\n%s", c)
	}
	if b, _ := os.ReadFile(filepath.Join(f.unitDir, job.Job.Timer)); !strings.Contains(string(b), "OnCalendar=*-*-* 02:30:00") {
		t.Fatalf("定时器单元：%s", b)
	}
	a := &Agent{Sys: f, req: &proto.Request{Root: f.root}}
	if a.jobTimerText(&job, id1) == nil {
		t.Fatal("定时器要随版本保存")
	}

	// 换了时间表再部署；回滚后用回旧版本的时间表
	job2 := jobApp("hourly")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{job2}, id2, map[string][]byte{"Report": []byte("v2")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	if b, _ := os.ReadFile(filepath.Join(f.unitDir, job.Job.Timer)); !strings.Contains(string(b), "OnCalendar=hourly") {
		t.Fatalf("%s", b)
	}
	f.calls = nil
	if r := exec1(t, f, "rollback", &proto.Request{Apps: []proto.AppSpec{job2}}); r.code != 0 || f.current("Report") != id1 {
		t.Fatal(r.dump())
	}
	if b, _ := os.ReadFile(filepath.Join(f.unitDir, job.Job.Timer)); !strings.Contains(string(b), "OnCalendar=*-*-* 02:30:00") {
		t.Fatalf("回滚后用那个版本的时间表：%s", b)
	}
	if strings.Contains(calls(f), "restart demo-report.service") {
		t.Fatalf("回滚也不启动服务：\n%s", calls(f))
	}

	// 停止 = 关掉定时器（记下手动停止）；启动 = 打开
	f.calls = nil
	if r := exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{job2}, Action: "stop"}); r.code != 0 {
		t.Fatal(r.dump())
	}
	if !strings.Contains(calls(f), "disable --now demo-report-schedule.timer") || !a.isStopped("Report") {
		t.Fatalf("\n%s", calls(f))
	}
	// 停着的时候部署：定时器照样打开（部署上去就是要运行的）
	f.calls = nil
	if r := exec1(t, f, "control", &proto.Request{Apps: []proto.AppSpec{job2}, Action: "start"}); r.code != 0 {
		t.Fatal(r.dump())
	}
	if !strings.Contains(calls(f), "enable --now demo-report-schedule.timer") || a.isStopped("Report") {
		t.Fatalf("\n%s", calls(f))
	}

	// 状态：下次运行、上次的结果
	f.shellOut = map[string]string{jobShowCmd(job.Job.Timer, job.Unit): "ActiveState=active\nNextElapseUSecRealtime=Fri 2026-11-13 02:30:00 CST\n---\n" +
		"ActiveState=failed\nResult=exit-code\nExecMainStatus=3\nExecMainStartTimestamp=Thu 2026-11-12 02:30:00 CST\nExecMainExitTimestamp=Thu 2026-11-12 02:30:05 CST\n"}
	r = exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{job2}})
	var st []proto.AppStatus
	json.Unmarshal(r.result.Data, &st)
	j := st[0].Job
	if j == nil || !j.TimerActive || j.Next == "" || j.Last == nil || j.Last.OK || j.Last.ExitCode != 3 || j.Last.Sec != 5 || st[0].Autostart != nil {
		t.Fatalf("%+v %+v", st[0], j)
	}
	// 时间换成带时区的（服务器的时区），桌面版再换成本机时间
	if want := time.Date(2026, 11, 13, 2, 30, 0, 0, time.Local).Format(time.RFC3339); j.Next != want || !strings.HasPrefix(j.Last.Start, "2026-11-12T02:30:00") {
		t.Fatalf("%q %q", j.Next, j.Last.Start)
	}
	// 老的 systemd（CentOS 7 的 219）：下次运行写成从 1970 年起算的时长
	f.shellOut = map[string]string{jobShowCmd(job.Job.Timer, job.Unit): "ActiveState=active\nNextElapseUSecRealtime=56y 10month 1w 4d 17h 15min\n---\n" +
		"ActiveState=failed\nResult=exit-code\nExecMainStatus=3\nExecMainStartTimestamp=Thu 2026-11-12 02:30:00 CST\nExecMainExitTimestamp=Thu 2026-11-12 02:30:05 CST\n"}
	r = exec1(t, f, "status", &proto.Request{Apps: []proto.AppSpec{job2}})
	st = nil
	json.Unmarshal(r.result.Data, &st)
	if want := time.Unix(1794536100, 0).Format(time.RFC3339); st[0].Job == nil || st[0].Job.Next != want {
		t.Fatalf("%+v want %s", st[0].Job, want)
	}
	f.shellOut = map[string]string{jobShowCmd(job.Job.Timer, job.Unit): "ActiveState=active\nNextElapseUSecRealtime=Fri 2026-11-13 02:30:00 CST\n---\n" +
		"ActiveState=failed\nResult=exit-code\nExecMainStatus=3\nExecMainStartTimestamp=Thu 2026-11-12 02:30:00 CST\nExecMainExitTimestamp=Thu 2026-11-12 02:30:05 CST\n"}

	// 告警：上次运行失败
	fs := a.jobFindings(job.Unit, "Report", "demo/prod")
	if len(fs) != 1 || fs[0].Key != "job:demo-report.service" || !strings.Contains(fs[0].Text, "退出码 3") || !strings.Contains(fs[0].Text, "11-12 02:30 开始") {
		t.Fatalf("%+v", fs)
	}
	// 定时器被关了：也告警
	f.shellOut[jobShowCmd(job.Job.Timer, job.Unit)] = "ActiveState=inactive\n---\nActiveState=inactive\nResult=success\nExecMainStatus=0\nExecMainStartTimestamp=Thu 2026-11-12 02:30:00 CST\nExecMainExitTimestamp=Thu 2026-11-12 02:30:05 CST\n"
	if fs := a.jobFindings(job.Unit, "Report", "demo/prod"); len(fs) != 1 || fs[0].Key != "job-timer:demo-report.service" {
		t.Fatalf("%+v", fs)
	}
	if !unitIsJob(filepath.Join(f.unitDir, job.Unit)) || unitIsJob(filepath.Join(f.unitDir, job.Job.Timer+"x")) {
		t.Fatal("单元第二行的 job=yes")
	}
}

// 改回常驻服务：去掉定时器
func TestJobToServiceRemovesTimer(t *testing.T) {
	f := newFake(t)
	job := jobApp("daily")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{job}, id1, map[string][]byte{"Report": []byte("v1")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	svc := javaApp("Report")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{svc}, id2, map[string][]byte{"Report": []byte("v2")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	if exists(filepath.Join(f.unitDir, job.Job.Timer)) || f.units[svc.Unit].active != "active" {
		t.Fatalf("\n%s", calls(f))
	}
}

// jobRunSys 现在运行：服务先「运行中」两次，然后结束（ok 决定成功、失败）。
type jobRunSys struct {
	*fakeSys
	unit  string
	polls int
}

func (j *jobRunSys) UnitProps(u string) UnitProps {
	if u != j.unit {
		return j.fakeSys.UnitProps(u)
	}
	j.polls++
	if j.polls <= 3 {
		return UnitProps{Load: "loaded", Active: "activating", Sub: "start"}
	}
	return UnitProps{Load: "loaded", Active: "inactive", Sub: "dead"}
}

func TestRunJob(t *testing.T) {
	f := newFake(t)
	job := jobApp("daily")
	if r := exec1(t, f, "deploy", f.stage(t, []proto.AppSpec{job}, id1, map[string][]byte{"Report": []byte("v1")})); r.code != 0 {
		t.Fatal(r.dump())
	}
	sys := &jobRunSys{fakeSys: f, unit: job.Unit}
	f.shellOut = map[string]string{jobShowCmd(job.Job.Timer, job.Unit): "ActiveState=active\n---\nActiveState=inactive\nResult=success\nExecMainStatus=0\n" +
		"ExecMainStartTimestamp=Thu 2026-11-12 10:00:00 CST\nExecMainExitTimestamp=Thu 2026-11-12 10:00:12 CST\n"}
	var out bytes.Buffer
	a := &Agent{Sys: sys, Out: &out}
	code := a.Run("runjob", &proto.Request{Proto: proto.Version, Root: f.root, Actor: "t", Apps: []proto.AppSpec{job}})
	if code != 0 || !strings.Contains(out.String(), "运行完了：成功（12 秒）") || !strings.Contains(out.String(), "BindException") {
		t.Fatalf("要输出日志和结果：\n%s", out.String())
	}
	if !strings.Contains(calls(f), "start --no-block demo-report.service") {
		t.Fatalf("\n%s", calls(f))
	}
	// 失败：返回失败，写进部署历史
	f.shellOut[jobShowCmd(job.Job.Timer, job.Unit)] = "ActiveState=active\n---\nActiveState=failed\nResult=timeout\nExecMainStatus=0\nExecMainStartTimestamp=Thu 2026-11-12 10:00:00 CST\nExecMainExitTimestamp=Thu 2026-11-12 11:00:00 CST\n"
	sys.polls = 0
	out.Reset()
	if code := a.Run("runjob", &proto.Request{Proto: proto.Version, Root: f.root, Actor: "t", Apps: []proto.AppSpec{job}}); code == 0 || !strings.Contains(out.String(), "超过最长运行时间") {
		t.Fatalf("\n%s", out.String())
	}
	// 不是定时任务的不能运行
	out.Reset()
	if code := a.Run("runjob", &proto.Request{Proto: proto.Version, Root: f.root, Apps: []proto.AppSpec{javaApp("System")}}); code == 0 {
		t.Fatal("不是定时任务应拒绝")
	}
}
