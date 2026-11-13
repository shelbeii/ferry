# 4. 多台部署：分批、先发一台、失败时回滚已切换的

> **0.13.98 已做（第 1～4 步；第 5 步摘流量的钩子没做）。** 和下面的设计不同的地方：
> - 回滚到**部署前在运行的那个版本**（预检后读 `releases` 记下的，接管来的回到 `legacy`），不是「上一个可用版本」；只回滚这次结果是 `OK` 的应用，结果改成新的 `REVERTED`。失败的那台由 agent 自己切回。
> - `DeployReport` 加的是 `Error`（这台失败的原因）和 `Reverted`，不是 `RolledBack`、`Skipped`；没切换的服务器不出现在结果里。通知按每台的 `Error` 写，不另发回滚通知。
> - 桌面版的部署是 JSON 输出，`Confirm` 不能弹窗：加了 `kind=ask` 的提问，桌面版弹窗「继续部署 / 停下」，回答经 `ferry serve` 的 `input` 送回（`ui.JSON.Ask`、`engine.Asker`）；取消任务时算「不」。命令行在终端里问；`--yes` 且问不了时观察完自动继续（AI 助手就是这样）。
> - 同时切换时 agent 的事件经 `targetReporter` 在应用名后面加 `@服务器`（`session.useEngine`）。
> - MCP 的 `deploy` 还加了 `targets`（原来只能一台）。
> - 代码：`internal/engine/rollout.go`（`switchAll`、`splitBatches`、`canaryGate`、`revertSwitched`）、`internal/config/rollout.go`、`cmd/ferry/rollout.go`；测试 `rollout_test.go`（假的切换、观察、回滚）、e2e「多台部署（rollout）」一节（两台同时切换、e2e-b 失败把 e2e-a 回滚、先发一台观察、经 `ferry serve` 问用户回答「不」）。

## 现状

`ferry deploy a,b,c`（`internal/engine/engine.go` 的 `deploy`）：

1. 全部目标先连接、预检、确认一次（任何一台有问题都不开始）。
2. 逐个目标构建、检查产物（同样的构建只做一次，`built`）。
3. `uploadAll`（`multi.go`）**同时**上传到各目标；有一台没传成功就都不切换（`dropStages`）。
4. **依次切换**（`switchTarget`）：一台失败就停，后面的不切换、删掉已上传的文件。**前面已经切换成功的不回滚**。

问题：

- 失败时线上是「前几台新版本、后几台旧版本」，接口不兼容就出事，要人工逐台 `ferry rollback`。
- 切换全部串行：每台要等启动、就绪（Java 常常几十秒），10 台就是几分钟。
- 不能「先发一台，看看没问题再发剩下的」。
- 切换期间不会通知前面的负载均衡把这台摘掉；没开 `zeroDowntime` 的应用，切换那几秒请求会失败。

## 目标

- 失败时可以选：停下（现在的做法）或**把已经切换的几台回滚到上一个版本**，让全部服务器保持同一个版本。
- **分批**：一次切换几台（`--batch 2`），批内同时切换，批与批之间串行。
- **先发一台**（`--canary`）：第一批（默认 1 台）切换成功后暂停，桌面版弹窗 / 命令行询问「继续部署剩下的 N 台？」，可以设观察时间（等几分钟、期间健康检查一直要通过）。
- **摘流量的钩子**（第二期）：每台切换前后在一台指定的机器上执行命令（如改 nginx upstream、调云 SLB 的接口）。

不做：

- 按百分比的流量放量（10% 走新版本）：要接管负载均衡，超出 Ferry 的定位。
- 跨目标的数据库迁移协调：`hooks.beforeSwitch` 已经能做「只在第一台执行」以外的事；只在一台执行的迁移以后再说。

## 配置

部署的行为放在目标组（项目级）上，命令行参数可以覆盖：

```yaml
rollout:                 # 一次部署到多台时（ferry deploy a,b,c，或桌面版多选）
  batch: 2               # 一次切换几台，默认 1（现在的行为）
  canary: 1              # 先切换几台，成功后暂停等确认；0 = 不暂停（默认）
  observe: 2m            # 先发的那几台切换后观察多久（健康检查一直通过才继续），默认 0
  onFailure: rollback    # stop（默认，和现在一样）| rollback（把这次已经切换的回滚到上一个版本）
```

命令行：`ferry deploy a,b,c --batch 2 --canary 1 --observe 2m --on-failure rollback`；`--yes` 时 canary 的暂停改成「观察完自动继续」。

第二期（摘流量）：

```yaml
rollout:
  drain:
    host: lb                          # targets 里的一个目标（负载均衡所在的机器），不写 = 在每台自己上执行
    before: "/opt/lb/disable.sh ${target.host}"   # 这台切换前执行（摘掉）
    after:  "/opt/lb/enable.sh ${target.host}"    # 切换成功后执行（加回来）
    wait: 10s                         # before 之后等多久再切换（让进行中的请求结束）
```

## 改动

### engine（主要工作量）

`engine.go` 的第 3 步改成：

```go
// switchAll 按 rollout 切换：分批（批内并发）、先发的暂停确认、失败时按 onFailure 处理。
func (e *Engine) switchAll(plans []*targetPlan, ups []uploaded, id string, git DeployGit, tmp string, o DeployOptions) ([]*DeployReport, error)
```

- **分批**：`batches := split(plans, canary, batch)`，第一批是 canary 的台数，之后每批 `batch` 台。批内用 `safe.Go` 并发调 `switchTarget`（每台本来就有自己的 `session`）。输出：复用 `uploadAll` 多台时「行首带服务器名、进度合并」的做法（`multi.go`），抽一个 `prefixReporter`。
- **暂停**：第一批成功后，`observe > 0` 时每 10 秒对这批的应用调一次 `status`（已有，返回 `Healthy`），有一次不健康就算失败；然后 `e.Rep.Confirm("先发的 a 已上线（观察 2 分钟正常），继续部署剩下的 b、c？")`。拒绝 = 停下（按 onFailure 处理已切换的）。
- **失败**：一批里任何一台失败：
  - 同一批里还在切换的等它们做完（不能中途打断 agent）。
  - `stop`：和现在一样，后面的 `dropStages`。
  - `rollback`：对这次**已经切换成功**的目标（含同批成功的），逐台调 agent 的 `rollback`（`To: ""` = 上一个可用版本，即这次部署前在跑的版本），用 `notifyRollback` 发通知。回滚失败的台单独列出来，返回的错误里写清楚哪几台是什么版本。
- `DeployReport` 加 `RolledBack bool`、`Skipped bool`；`notifyDeploy` 里按这两个字段写「已回滚」「未部署」。
- 部署历史：回滚由 agent 照常记一条 `rollback`，`Msg` 写「多台部署 <版本> 在 <失败的目标> 失败，自动回滚」。

### agent

不用改：`rollback` 已有，`status` 已经返回健康检查结果。第二期的 `drain` 在 engine 里用 `run`（目标上执行命令）或在 `drain.host` 那台的 session 上执行。

### 桌面版

- 部署对话框多选服务器时出现「部署方式」：一台一台（默认）/ 每次 N 台；「先发一台，确认后再发其余的」开关和观察时间；「失败时：停下 / 回滚已部署的」。初值来自 `rollout`。
- 暂停时：现在的确认框就够（`e.Rep.Confirm` 在桌面版是弹窗）；文字里写清楚先发的那台的状态和就绪用时。
- 部署结果列表：每台一行，状态加「已回滚」「未部署」。

### 命令行

`deploy` 加 `--batch`、`--canary`、`--observe`、`--on-failure`（注册到 `cmd/ferry/main.go` 的取值参数表）；帮助文字、`docs/config.md`「部署到多台」一节。

### MCP / AI

`deploy` 工具的参数加 `batch`、`canary`、`onFailure`；确认内容里写上分批方式。`canary` 在 MCP 里不能交互：没有 `--yes` 的确认通道时，暂停改成「观察完自动继续」，确认框里提前说明。

## 兼容

不写 `rollout`、不加参数时行为和现在完全一样（`batch 1`、不暂停、`stop`）。单台部署不受影响。

## 出错和边界

- 批内并发时，一台卡在「等待就绪」很久：其他台照常做完；整批的耗时等于最慢的那台。
- 回滚时连接断了：那台标「回滚失败」，提示手动 `ferry rollback <目标> <应用>`。
- 这次部署前那台上根本没有旧版本（第一次部署）：回滚 = 停掉服务（agent 的 rollback 在没有上一个版本时返回错误），提示用户。
- canary 观察期间用户关了桌面版：engine 进程跟着结束，后面的不部署，已上传的留在服务器的上传目录（下次部署会清理），提示里写明。

## 测试

- 单元：`split` 分批；`switchAll` 用假的 `switchTarget`（函数字段注入）覆盖：全部成功、第 2 批失败 + stop、失败 + rollback（已切换的都回滚，回滚又失败一台）、canary 拒绝、observe 期间不健康。
- e2e：`test/e2e` 已有两台容器（e2e-a、e2e-b）。加：`--batch 2` 两台同时切换成功；让 e2e-b 上的应用启动失败（已有 `bad` 版本的做法）+ `--on-failure rollback` → e2e-a 被回滚到旧版本（看 `current` 链接和部署历史）。

## 分步

1. 抽出 `switchAll`，行为不变（只是重构），e2e 全过。
2. `onFailure: rollback`（最有价值），单元 + e2e。
3. 分批并发 + 多台输出前缀。
4. canary + observe，桌面版部署对话框的选项。
5. 第二期：`drain` 钩子。

## 验收

- 三台部署第二台失败、`onFailure: rollback`：三台最后都是部署前的版本，通知里写「b 失败，已把 a 回滚」。
- 四台 `--batch 2`：耗时约等于两台的切换时间，不是四台。
- `--canary 1 --observe 1m`：第一台切换后等 1 分钟，弹窗确认，拒绝后其余三台没有改动。
