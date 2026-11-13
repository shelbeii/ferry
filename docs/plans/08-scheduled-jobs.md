# 8. 定时任务（cron）作为应用

> **0.13.97 已做。** 和下面的设计不同的地方：
> - 配置收在一个 `job:` 里（`job.schedule`，不是应用上的 `schedule`），`job.catchUp` 代替 `onBoot`，没有 `overlap`（systemd 本来就不会同时跑两次）。
> - 定时器叫 `<服务名>-schedule.timer`（`<服务名>.timer` 已经被开机延迟启动 `autostartDelay` 用了）。
> - 超时用 `TimeoutStartSec`（oneshot 的整个运行都算启动；老 systemd 默认只给 90 秒），不是 `RuntimeMaxSec`。
> - 「现在运行」用 `systemctl start --no-block` + 每秒读一次 journal 把新输出发出去，跑完返回结果；部署历史记一条 `run`。
> - 「启动 / 重启」都只是打开定时器，不马上运行。
> - 代码：`internal/agent/job.go`、`internal/engine/render.go`（`jobSpec`、`JobTimer`）、`internal/config` 的 `Job`、`checkJob`；测试 `internal/agent/job_test.go`、e2e「定时任务」一节（每 10 秒的定时器真的触发）。

## 现状

- 配置里没有定时、一次性任务的写法；agent 不管 crontab。每天跑的报表、清理脚本、数据同步只能自己写 `service.unitExtra` 或登录服务器手写 crontab：不随版本部署、不回滚、失败没有告警。
- Ferry 自己已经大量使用 systemd timer：告警 `ferry-monitor.timer`（`monitor.go` 的 `installMonitorTimer`）、数据备份 `ferry-backup-*.timer`（`backup.go` 的 `applyBackup`）、开机延迟启动的定时器（`ops.go` 的 `bootTimer`）、证书续期 `ferry-acme`。写法现成。

## 目标

- 一个应用可以是**定时任务**：按时间表运行一次就结束；和普通应用一样构建、上传、随版本走、回滚、看日志（每次运行一段）、看部署历史。
- 失败（退出码非 0、超时）开着告警就发告警。
- 可以「现在运行一次」。

不做：分布式调度（多台只跑一台）——多台时在 `targets` 上选一台，或用 `apps` 过滤。

## 配置

```yaml
apps:
  - name: report
    type: java                     # 程序类型照常：java / binary / python / node / dotnet
    artifact: {path: report/target/report.jar}
    schedule: "*-*-* 02:30:00"     # 新：有 schedule 就是定时任务。daily | hourly | weekly | systemd 的 OnCalendar
    job:
      timeout: 1h                  # 最长运行多久（RuntimeMaxSec），默认 6h
      onBoot: false                # 错过的时间（关机期间）开机后补跑一次（Persistent=），默认 true
      overlap: skip                # 上一次还没跑完时：skip（默认，systemd 本来就这样）
    service:
      args: [--date=yesterday]
      envFile: /etc/report/report.env
```

## 设计

### 单元（engine `render.go`）

- 服务单元 `Type=oneshot`，**没有** `[Install]`（不开机自启），`RuntimeMaxSec=<timeout>`，其余（运行用户、工作目录、环境、Java 参数）和普通应用一样。
- 新的 timer 单元 `<单元名>.timer`：`OnCalendar=<schedule>`、`Persistent=<onBoot>`、`RandomizedDelaySec=` 不加（定时任务要准时）、`[Install] WantedBy=timers.target`。第二行写 `# project=… app=… target=… job=yes`，告警、管理页靠它认出来。
- `proto.AppSpec` 加 `TimerText string`、`Job bool`。

### 部署（agent）

- 切换：`running` 链接换到新版本、写服务和 timer 单元、`daemon-reload`、`systemctl enable --now <timer>`。**不 restart 服务**（不是常驻的）。
- 就绪判断：没有健康检查。可选 `job.runOnDeploy: true`：部署时立刻跑一次，退出码 0 才算成功，否则回滚（适合迁移类任务）。默认不跑，只检查单元文件 `systemd-analyze verify` 通过、timer 激活。
- 回滚：换回旧版本 + 旧单元，和普通应用一样。
- 停止 / 启动（在 Ferry 里）：对 timer 做 `disable --now` / `enable --now`，「已停止」标记照用（`markStopped`）。

### 运行一次

`ferry run-job <目标> <应用>` / 桌面版「现在运行」：`systemctl start <服务>`（oneshot，等它结束），输出实时显示（`journalctl -f -u` 直到单元退出），返回退出码和用时。

### 状态和日志

- `status` 对 job 应用返回：下次运行时间（`systemctl show -p NextElapseUSecRealtime <timer>`，备份已经这么用）、上次运行的开始时间、结果（`Result=`、`ExecMainStatus`）、用时。
- 日志页按「每次运行」分段：`journalctl -u <服务> _SYSTEMD_INVOCATION_ID=…`（agent 的日志按次归档 `logruns` 已经是按 invocation 的，确认对 oneshot 也成立）。

### 告警（`monitor.go`）

- `monitorCheck` 的应用循环：认出 `job=yes` 的单元，**不**按「停了」报（oneshot 平时就是 inactive）。改成：timer 没激活（被人关了且没有「已停止」标记）→ bad；上次运行失败（`Result` 不是 success）→ bad，键 `job:<单元>`，下次成功后恢复；超过 timeout 被杀也算失败。
- 部署了但从来没跑过、过了下次运行时间还没跑（timer 卡住）→ warn。

## 改动清单

- config：`App.Schedule`、`App.Job`；校验 schedule 格式（复用 `backup.go` 的 `reCalendar` 和 `OnCalendar()` 映射）；static 不能有 schedule；和 `zeroDowntime` 互斥。schema 等。
- engine：`render.go` 的 job 分支、`Spec`；`RunJob`；命令行 `run-job`。
- agent：部署切换的 job 分支、`status` 的 job 字段、`monitor` 的 job 判断、`run-job` 命令（流式输出，复用 `Stream`）。
- 桌面版：应用卡片显示「定时任务 · 每天 2:30 · 下次 明天 02:30 · 上次 成功（12 秒）」；「现在运行」按钮；向导里「这是一个定时任务」开关。
- MCP / AI：`app_status` 带上 job 信息；`app_control` 加 `run`（要确认）。

## 测试

- 单元：timer 文本渲染；schedule 映射；monitor 对 job 单元的判断（成功、失败、恢复、timer 关了）。
- e2e：binary 应用 `schedule: minutely`（测试里允许 `*:*:00`）+ 一个打印时间然后退出的程序；部署后等一分钟看到一次成功运行；`run-job` 立即运行；坏版本（退出码 1）→ 告警的检查结果里有 `job:` 问题；回滚后恢复。

## 分步

1. config + 单元渲染 + agent 部署 / 回滚（命令行可用）。
2. status、`run-job`、日志按次。
3. 告警。
4. 桌面版。

## 验收

- 一个 Python 清理脚本写成 `schedule: daily` 的应用，部署后服务器上 `systemctl list-timers` 能看到，到点执行，失败时群里收到告警。
- 回滚到上一个版本后，下一次运行跑的是旧版本的代码。
