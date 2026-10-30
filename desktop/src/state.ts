// 全局状态：设置（含项目列表，保存在 ferry 数据目录的 settings.json）、后台任务、主机公钥核对与密码请求。
import { markRaw, reactive, ref } from 'vue'
import { ElMessageBox } from 'element-plus'
import { call, ferry, isSecretKind, type AppSettings, type BusyItem, type Connection, type ErrorDetail, type FerryLine, type SettingsResponse } from './ferry'
import { getCurrentWindow, UserAttentionType } from '@tauri-apps/api/window'
import { isTauri, storeGet } from './platform'
import { glass, GLASS_TRANSPARENCY_DEFAULT } from './glass/runtime'

// ---------------------------------------------------------------- 设置

export const appSettings = ref<AppSettings | null>(null)
export const settingsInfo = ref<SettingsResponse | null>(null)

export async function loadSettings(): Promise<AppSettings> {
  const r = await call<SettingsResponse>(['settings'])
  r.settings.ui.glassTransparency ??= GLASS_TRANSPARENCY_DEFAULT // 旧版 ferry 没有这一项
  settingsInfo.value = r
  appSettings.value = r.settings
  applyTheme(r.settings.ui.theme)
  glass.enabled = r.settings.ui.glass !== false
  glass.transparency = r.settings.ui.glassTransparency
  return r.settings
}

/** 保存设置（整份替换；值经环境变量传给 ferry）。 */
export async function saveSettings(s: AppSettings): Promise<void> {
  await call(['settings', 'save'], { FERRY_SETTINGS_VALUE: JSON.stringify(s) })
  await loadSettings()
}

// ---------------------------------------------------------------- 主题

let media: MediaQueryList | undefined

export function applyTheme(theme: string) {
  // 记下来：下次启动时 public/theme-init.js 在界面画出来之前就按它切好
  setUiPref('theme', theme)
  media ??= window.matchMedia('(prefers-color-scheme: dark)')
  const set = () => {
    const dark = theme === 'dark' || (theme === 'system' && !!media?.matches)
    document.documentElement.classList.toggle('dark', dark)
  }
  media.onchange = theme === 'system' ? set : null
  set()
  // 窗口标题栏跟着 Ferry 的主题，不然会跟着 Windows 自己的深色 / 浅色设置走（浅色主题配深色标题栏）
  if (isTauri) {
    getCurrentWindow()
      .setTheme(theme === 'light' || theme === 'dark' ? theme : null)
      .catch(() => {})
  }
}

// ---------------------------------------------------------------- 项目

export interface Project {
  path: string // ferry.yaml 绝对路径
  name: string // 项目名（inspect 得到；读取失败时用文件名）
}

export const projects = ref<Project[]>([])
export const currentPath = ref<string>('')

export async function loadProjects() {
  const s = await loadSettings()
  projects.value = s.desktop.projects ?? []
  currentPath.value = s.desktop.current || projects.value[0]?.path || ''
  if (projects.value.length === 0) {
    // 旧版本把项目列表存在 Tauri 自己的存储里：搬到 settings.json
    const legacy = await storeGet<Project[]>('projects').catch(() => undefined)
    if (legacy?.length) {
      projects.value = legacy
      currentPath.value = (await storeGet<string>('current').catch(() => undefined)) ?? legacy[0].path
      await saveProjects()
    }
  }
}

async function saveProjects() {
  const s = appSettings.value ?? (await loadSettings())
  await saveSettings({ ...s, desktop: { projects: projects.value, current: currentPath.value } })
}

export async function addProject(p: Project) {
  const i = projects.value.findIndex((x) => x.path === p.path)
  if (i >= 0) projects.value[i] = p
  else projects.value.push(p)
  currentPath.value = p.path
  await saveProjects()
}

export async function removeProject(path: string) {
  projects.value = projects.value.filter((p) => p.path !== path)
  if (currentPath.value === path) currentPath.value = projects.value[0]?.path ?? ''
  await saveProjects()
}

/** 配置文件移动了（ferry move-config 已经搬好数据、改好设置里的列表）：界面上的列表、任务跟着改 */
export async function relocateProject(from: string, to: string) {
  for (const p of projects.value) if (p.path === from) p.path = to
  if (currentPath.value === from) currentPath.value = to
  for (const t of tasks) if (t.config === from) t.config = to
  if (projectSummary[from]) {
    projectSummary[to] = projectSummary[from]
    delete projectSummary[from]
  }
  await saveProjects()
}

export async function selectProject(path: string) {
  currentPath.value = path
  await saveProjects()
}

/** 左边项目列表里显示的「运行中 / 全部」：项目页读到状态后填（只算已经打开过、读过状态的服务器） */
export const projectSummary = reactive<Record<string, { up: number; total: number }>>({})

/** 本机界面偏好（项目列表收起、各项目用卡片还是列表）：只是方便，读写失败就用默认 */
export function uiPref(key: string): string | null {
  try {
    return localStorage.getItem('ferry.' + key)
  } catch {
    return null
  }
}
export function setUiPref(key: string, value: string) {
  try {
    localStorage.setItem('ferry.' + key, value)
  } catch {
    // 存不了就算了，下次用默认
  }
}

// ---------------------------------------------------------------- 系统通知

/**
 * 发系统通知：只在窗口不在前台时（在别的程序里、最小化了）发，设置里可以关掉；任务栏图标同时闪一下。
 * 浏览器调试时不发。发不出来（系统关了通知）也不影响别的。
 */
export async function notifyUser(title: string, body: string, urgent = false) {
  if (!isTauri || appSettings.value?.ui.notify === false) return
  try {
    const w = getCurrentWindow()
    if ((await w.isFocused()) && !(await w.isMinimized())) return
    const n = await import('@tauri-apps/plugin-notification')
    let ok = await n.isPermissionGranted()
    if (!ok) ok = (await n.requestPermission()) === 'granted'
    if (ok) n.sendNotification({ title, body })
    await w.requestUserAttention(urgent ? UserAttentionType.Critical : UserAttentionType.Informational)
  } catch (e) {
    console.warn('系统通知发送失败', e)
  }
}

/** 用时多久的任务结束时才通知：很快结束的（看状态、开关几秒钟的操作）不打扰 */
const notifyAfterMs = 10_000

// ---------------------------------------------------------------- 任务

export interface Task {
  id: number
  /** 任务记录的 ID（数据目录 tasks/<hid>.json，重启后还在） */
  hid: string
  title: string
  /** interrupted：上次桌面版关闭时还没结束，结果未知 */
  status: 'running' | 'ok' | 'failed' | 'interrupted'
  lines: FerryLine[]
  /** 一共收到过几行输出（超过上限时较早的行会删掉，用它判断有没有新输出） */
  received?: number
  progress?: { label: string; done: number; total: number; speed?: number; eta?: number }
  /** 当前在做什么（最近一个步骤或等待事件），at 为开始时间 */
  step?: { text: string; at: number; waitMax?: number }
  /** 最近一次有输出（含进度）的时间 */
  lastAt: number
  startedAt: number
  endedAt?: number
  error?: string
  /** 失败时的结构化信息（如 java-mismatch） */
  detail?: ErrorDetail
  data?: unknown
  kill?: () => Promise<void>
  /** 重新执行同一个任务 */
  retry?: () => void
  /** 加上几个参数重新执行（如同意加权限的 --fix-parents） */
  retryWith?: (extra: string[]) => void
  /** 任务的 ferry.yaml */
  config?: string
  /** 任务作用的服务器（部署、启停等；「让 AI 看看」用）。只在这次打开 Ferry 期间有 */
  target?: string
  /** 以前的任务（从记录里读的）：输出等打开时才读，读之前为 false */
  loaded?: boolean
  loadingDetail?: boolean
  detailLoadError?: string
}

export const tasks = reactive<Task[]>([])
export const drawerOpen = ref(false)
export const selectedTaskId = ref<number | null>(null)
let nextId = 1

/** 启动一个后台 ferry 任务，过程显示在任务面板里。env：本次额外的环境变量（如一次性的密码）。 */
export function startTask(title: string, args: string[], onDone?: (t: Task) => void, env: Record<string, string> = {}): Task {
  const now = Date.now()
  const hid = now.toString(36) + '-' + Math.random().toString(36).slice(2, 8)
  const task = reactive<Task>({ id: nextId++, hid, title, status: 'running', lines: [], startedAt: now, lastAt: now, step: { text: '启动 ferry', at: now } }) as Task
  let sample: { t: number; done: number } | undefined
  tasks.unshift(task)
  selectedTaskId.value = task.id
  trimTasks()
  drawerOpen.value = true
  const run = ferry(
    args,
    (l) => {
      const t = Date.now()
      task.lastAt = t
      if (l.kind === 'progress') {
        if (l.done === undefined || !l.total || (l.label === '' && l.done === l.total)) {
          task.progress = undefined
          sample = undefined
          return
        }
        // 上传速度：按 1 秒以上的间隔取样，平滑一下
        let speed = task.progress?.speed
        if (!sample || l.done < sample.done) sample = { t, done: l.done }
        else if (t - sample.t >= 1000) {
          const v = ((l.done - sample.done) * 1000) / (t - sample.t)
          speed = speed ? speed * 0.6 + v * 0.4 : v
          sample = { t, done: l.done }
        }
        const eta = speed && speed > 0 ? (l.total - l.done) / speed : undefined
        task.progress = { label: l.label ?? '', done: l.done, total: l.total, speed, eta }
        if (task.step?.text !== '上传 ' + (l.label ?? '')) task.step = { text: '上传 ' + (l.label ?? ''), at: task.step?.text.startsWith('上传') ? task.step.at : t }
        return
      }
      if (l.kind === 'result') return
      pushLine(task, l)
      if (l.kind === 'step') task.step = { text: l.msg ?? '', at: t }
      else if (l.kind === 'event' && l.code === 'WAIT') {
        // 「127.0.0.1:9201（最多 180s）」：记下最长等待
        const m = /最多 (\d+)s/.exec(l.msg ?? '')
        task.step = { text: `等待 ${l.app} 就绪：${(l.msg ?? '').replace(/（最多 \d+s）/, '')}`, at: t, waitMax: m ? Number(m[1]) : undefined }
      } else if (l.kind === 'event' && ['START', 'STOP', 'SWITCH', 'ASSEMBLE', 'VERIFY', 'ROLLBACK'].includes(l.code ?? '')) {
        const verbs: Record<string, string> = { START: '启动', STOP: '停止', SWITCH: '切换', ASSEMBLE: '拼装', VERIFY: '校验', ROLLBACK: '回滚' }
        task.step = { text: `${verbs[l.code!]} ${l.app ?? ''}`.trim(), at: t }
      }
    },
    env,
  )
  task.kill = run.kill
  task.config = configOf(args)
  task.target = targetOf(args)
  task.retry = () => startTask(title, args, onDone, env)
  task.retryWith = (extra) => startTask(title, [...args, ...extra], onDone, env)
  saveTask(task)
  run.done.then((r) => {
    task.status = r.ok ? 'ok' : 'failed'
    task.error = r.error
    task.detail = r.detail
    task.data = r.data
    task.endedAt = Date.now()
    task.progress = undefined
    task.kill = undefined
    saveTask(task)
    if (task.endedAt - task.startedAt >= notifyAfterMs) {
      const took = Math.round((task.endedAt - task.startedAt) / 1000)
      if (r.ok) notifyUser(`${title}：完成`, `用时 ${took} 秒`)
      else notifyUser(`${title}：失败`, (r.error ?? '').split('\n')[0] || '打开 Ferry 的任务面板看原因', true)
    }
    const d = r.detail
    if (d?.kind === 'unknown-host' || (!d && r.error?.includes('未知主机'))) {
      // 核对并记录后用同样的参数、同样的环境变量（如这次输入的密码，只在内存里）接着做，不用再点一次、再输一次密码
      trustRequest.value = { target: d?.target || guessTarget(args), config: configOf(args) || undefined, retry: () => startTask(title, args, onDone, env) }
    } else if (d?.kind === 'busy-artifacts' && d.items?.length) {
      // 本机有程序占着要构建的 jar：弹窗列出来，可以结束它们后重新部署
      busyRequest.value = { config: configOf(args), target: d.target ?? '', items: d.items, retry: () => startTask(title, args, onDone, env) }
    } else if (d && isSecretKind(d.kind)) {
      // 需要密码：弹框询问，填好后重新执行同一个任务
      secretRequest.value = { detail: d, config: configOf(args), retry: () => startTask(title, args, onDone, env) }
    } else if (d?.kind === 'blocked-dirs' && !args.includes('--fix-parents')) {
      // 安装软件：运行用户进不了上级目录（还没上传、没改动）。问一下，同意就加上权限接着装
      askFixParents(d).then((yes) => yes && task.retryWith?.(['--fix-parents']))
    }
    onDone?.(task)
  })
  return task
}

/** 运行用户进不了安装目录的上级目录：说明会改什么，问要不要改 */
export async function askFixParents(d: ErrorDetail): Promise<boolean> {
  const dirs = (d.dirs ?? []).join('、')
  try {
    await ElMessageBox.confirm(
      `运行用户 ${d.user} 进不了 ${dirs}（这一级目录不允许其他用户进入），软件装进去也启动不了。

` +
        `给 ${d.user} 加上进入 ${dirs} 的权限，然后继续安装？只能进入，看不到里面有哪些文件；服务器支持 ACL 时只对 ${d.user} 开放，否则对所有用户开放进入（o+x）。

` +
        `还没有上传安装包，服务器上没有做任何改动。`,
      '需要目录权限',
      { type: 'warning', confirmButtonText: '加上权限并继续安装', cancelButtonText: '不装了', customStyle: { whiteSpace: 'pre-line' } },
    )
    return true
  } catch {
    return false
  }
}

/** 每个任务最多保留的输出行数：超过时删掉较早的普通输出（步骤、提示、错误、服务器上的事件留着），开头注明省略了几行 */
export const maxTaskLines = 4000
const keptKinds = new Set(['step', 'warn', 'error', 'event'])
const omittedPrefix = '（前面省略了 '

export function pushLine(task: Task, l: FerryLine) {
  task.received = (task.received ?? task.lines.length) + 1
  task.lines.push(markRaw(l)) // 行本身不会再改：不用深层响应，几千行时界面不卡
  if (task.lines.length <= maxTaskLines + 200) return // 攒一些再删，不用每行都删一次
  const head = task.lines[0]?.kind === 'info' && task.lines[0].msg?.startsWith(omittedPrefix) ? task.lines.shift()! : undefined
  let omitted = head ? Number(/\d+/.exec(head.msg!.slice(omittedPrefix.length))?.[0] ?? 0) : 0
  let extra = task.lines.length - maxTaskLines
  const kept: FerryLine[] = []
  // 先删最早的普通输出；全是步骤、提示、错误时再按先后删
  for (const x of task.lines) {
    if (extra > 0 && !keptKinds.has(x.kind)) {
      extra--
      omitted++
    } else kept.push(x)
  }
  if (extra > 0) {
    kept.splice(0, extra)
    omitted += extra
  }
  kept.unshift(markRaw({ kind: 'info', msg: `${omittedPrefix}${omitted} 行较早的输出，没有保留）` }))
  task.lines = kept
}

/** 已完成的任务超过设置的保留数时，删掉最旧的 */
function trimTasks() {
  const keep = appSettings.value?.ui.taskKeep ?? 50
  let finished = 0
  for (let i = 0; i < tasks.length; i++) {
    if (tasks[i].status === 'running') continue
    if (++finished > keep) tasks.splice(i--, 1)
  }
}

// ---------------------------------------------------------------- 任务记录（重启后还在）

/** 每个任务上次保存时的输出行数，和保存的先后（一个接一个，后存的不会被先存的覆盖） */
const saved = new Map<string, { lines: number; chain: Promise<void> }>()
let saveTimer: number | undefined

/** 保存任务记录：开始时、进行中每隔几秒（有新输出时）、结束时各存一次。保存失败不影响任务本身。 */
function saveTask(t: Task) {
  const s = saved.get(t.hid) ?? { lines: -1, chain: Promise.resolve() }
  s.lines = t.received ?? t.lines.length
  const rec = {
    id: t.hid,
    title: t.title,
    status: t.status,
    startedAt: t.startedAt,
    endedAt: t.endedAt,
    error: t.error,
    config: t.config,
    detail: t.detail,
    data: t.data,
    lines: t.lines,
  }
  s.chain = s.chain
    .then(() => call<{ removed: string[] }>(['task-history', 'save'], { FERRY_TASK_VALUE: JSON.stringify(rec) }))
    .then((r) => dropTasks(r.removed ?? []))
    .catch((e) => console.warn('保存任务记录失败', e))
  saved.set(t.hid, s)
  saveTimer ??= window.setInterval(() => {
    const live = tasks.filter((x) => x.status === 'running')
    for (const x of live) if ((x.received ?? x.lines.length) !== saved.get(x.hid)?.lines) saveTask(x)
    if (live.length === 0) {
      clearInterval(saveTimer)
      saveTimer = undefined
    }
  }, 5000)
}

/** 按设置清理掉的记录：面板里也去掉（进行中的不动） */
export function dropTasks(hids: string[]) {
  if (!hids.length) return
  const gone = new Set(hids)
  for (let i = tasks.length - 1; i >= 0; i--) if (tasks[i].status !== 'running' && gone.has(tasks[i].hid)) tasks.splice(i, 1)
}

interface TaskSummary {
  id: string
  title: string
  status: Task['status']
  startedAt: number
  endedAt?: number
  error?: string
  config?: string
}

/** 桌面版启动时读以前的任务（只读概要，输出在打开时才读）；上次没跑完的标成中断。 */
export async function loadTaskHistory() {
  const r = await call<{ items: TaskSummary[] }>(['task-history', 'list', '--interrupt'])
  const have = new Set(tasks.map((t) => t.hid))
  for (const s of r.items ?? []) {
    if (have.has(s.id)) continue
    tasks.push(
      reactive<Task>({
        id: nextId++,
        hid: s.id,
        title: s.title,
        status: s.status,
        lines: [],
        startedAt: s.startedAt,
        endedAt: s.endedAt ?? s.startedAt,
        lastAt: s.endedAt ?? s.startedAt,
        error: s.error,
        config: s.config,
        loaded: false,
      }) as Task,
    )
  }
  tasks.sort((a, b) => b.startedAt - a.startedAt)
}

/** 以前的任务：打开时读输出、结果 */
export async function loadTaskDetail(t: Task) {
  if (t.loaded !== false || t.loadingDetail) return
  t.loadingDetail = true
  t.detailLoadError = ''
  try {
    const r = await call<{ lines?: FerryLine[]; data?: unknown; detail?: ErrorDetail; error?: string }>(['task-history', 'show', t.hid])
    t.lines = r.lines ?? []
    t.data = r.data
    t.detail = r.detail
    if (r.error) t.error = r.error
    t.loaded = true
  } catch (e) {
    t.detailLoadError = `读取任务记录失败：${(e as Error).message}`
  } finally {
    t.loadingDetail = false
  }
}

/** 从面板和记录里删掉这些任务（「清除已完成」） */
export async function removeTasks(list: Task[]) {
  const hids = list.map((t) => t.hid)
  if (hids.length) await call(['task-history', 'delete', ...hids])
  const gone = new Set(hids)
  for (let i = tasks.length - 1; i >= 0; i--) if (gone.has(tasks[i].hid)) tasks.splice(i, 1)
}

function guessTarget(args: string[]): string {
  // 命令后的第一个位置参数是目标（多目标时取第一个）
  const pos = args.filter((a, i) => !a.startsWith('-') && !(i > 0 && ['-c', '--to', '-n', '--limit', '--expect', '--key'].includes(args[i - 1])))
  // ferry software <子命令> <目标> …
  if (pos[0] === 'software' && ['install', 'start', 'stop', 'restart', 'remove', 'logs', 'password'].includes(pos[1] ?? '')) return pos[2] ?? ''
  return (pos[1] ?? '').split(',')[0]
}

function configOf(args: string[]): string {
  const i = args.indexOf('-c')
  return i >= 0 ? args[i + 1] : ''
}

// ---------------------------------------------------------------- 主机公钥核对

/** config 为空时用当前项目的配置文件（连接页没有当前项目，一定要带上）；retry：记录以后接着执行刚才的操作 */
export const trustRequest = ref<{ target: string; config?: string; retry?: () => void } | null>(null)

/**
 * 每个连接删了几次（按名字）：连接页按它区分，删除后再建同名的连接是新的页面；
 * 被删的连接的页面（还在后台保留着）看到它变了就关掉自己的终端。
 */
export const connGen = reactive<Record<string, number>>({})

/** 「新建连接」的服务器连着的终端数（正在连接的也算） */
export function connTerminals(name: string): TermTab[] {
  return pageTerminals.filter((t) => t.config === connectionsPath.value && t.target === name && t.state !== 'closed')
}

/**
 * 手动断开一个连接：它的终端都断开（标签留着，按回车重新连接），再断开后台复用的 SSH 连接
 * （文件、登录用户也走这条连接，下次用到时自动重新连接）。
 */
export async function hangupConnection(name: string, title = name) {
  const open = connTerminals(name).filter((t) => t.state === 'open').length
  if (open) {
    try {
      await ElMessageBox.confirm(`断开「${title}」？${open} 个终端会断开，里面正在运行的命令会被结束。终端标签留着，按回车可以重新连接。`, '断开连接', {
        type: 'warning',
        confirmButtonText: '断开',
      })
    } catch {
      return
    }
  }
  for (const t of connTerminals(name)) t.hangup = (t.hangup ?? 0) + 1
  try {
    await call(['disconnect', name, '-c', connectionsPath.value])
  } catch {
    // 本来就没连着
  }
}

/** 「让 AI 看看」：打开这个项目、这台服务器的 AI 助手，新开一个对话问这句话（项目页接着处理） */
export const aiAskRequest = ref<{ config: string; target: string; text: string } | null>(null)

// 命令的第二项是服务器的（deploy prod-a,prod-b → prod-a）
const targetCommands = new Set(['deploy', 'start', 'stop', 'restart', 'rollback', 'status', 'preflight', 'software', 'port', 'who', 'logs'])
function targetOf(args: string[]): string | undefined {
  if (!targetCommands.has(args[0]) || !args[1] || args[1].startsWith('-')) return undefined
  const t = ['software', 'port', 'who'].includes(args[0]) && args[2] && !args[2].startsWith('-') && ['install', 'start', 'stop', 'restart', 'remove', 'open', 'close', 'enable', 'disable', 'kick'].includes(args[1]) ? args[2] : args[1]
  return t.split(',')[0]
}
/** 每记录一次主机公钥加一：页面据此重新读取状态 */
export const trustedTick = ref(0)

// ---------------------------------------------------------------- 终端

/** 一个终端标签：连到某个配置文件里的某个目标 */
export interface TermTab {
  id: number
  config: string
  target: string
  title: string
  state: 'connecting' | 'open' | 'closed'
  /** 旁边显示服务器上的文件 */
  files: boolean
  /** 同一台服务器的第几个终端（标题里的序号；关掉的序号会再用） */
  num?: number
  /** 加 1 = 断开这个终端（标签留着，按回车重新连接） */
  hangup?: number
}

/** 还没被占用的最小序号：开着 1、3 时是 2，全关了又从 1 开始 */
export function freeNumber(tabs: TermTab[]): number {
  const used = new Set(tabs.map((t) => t.num))
  let n = 1
  while (used.has(n)) n++
  return n
}

export const terminals = reactive<TermTab[]>([])
/** 终端窗口是否显示（最小化后终端仍然连着） */
export const terminalShown = ref(false)
export const activeTerminal = ref(0)
/** 终端窗口的高度（像素，可以拖动上边调整） */
export const terminalHeight = ref(0)
let nextTermId = 1

/** 新开一个连到 target 的终端标签。title：标签上显示的名字（默认目标名）；files：同时打开文件。 */
export function addTerminal(config: string, target: string, opts: { title?: string; files?: boolean } = {}) {
  const name = opts.title || target
  const num = freeNumber(terminals.filter((t) => t.config === config && t.target === target))
  const tab = reactive<TermTab>({
    id: nextTermId++,
    config,
    target,
    title: num > 1 ? `${name} (${num})` : name,
    state: 'connecting',
    files: !!opts.files,
    num,
  }) as TermTab
  terminals.push(tab)
  activeTerminal.value = tab.id
  // 第一次打开时定高度：窗口高度的 45%
  if (terminalHeight.value < 160) terminalHeight.value = Math.max(Math.round(window.innerHeight * 0.45), 240)
  terminalShown.value = true
}

export function removeTerminal(id: number) {
  const i = terminals.findIndex((t) => t.id === id)
  if (i < 0) return
  terminals.splice(i, 1)
  if (activeTerminal.value === id) activeTerminal.value = terminals[Math.min(i, terminals.length - 1)]?.id ?? 0
  if (terminals.length === 0) terminalShown.value = false
}

// ---------------------------------------------------------------- 新建连接保存的服务器

export const connections = ref<Connection[]>([])
/** 保存它们的文件（数据目录里的 connections.yaml），连接时用 -c 指定 */
export const connectionsPath = ref('')

/** 项目列表里选中的是连接时，currentPath 是 conn:<名字> */
export const CONN_PREFIX = 'conn:'

/** 连接页面里打开的终端（关窗前确认用）；底部终端窗口的在 terminals 里 */
export const pageTerminals = reactive<TermTab[]>([])

export function newTermTab(config: string, target: string, title: string, num?: number): TermTab {
  return reactive<TermTab>({ id: nextTermId++, config, target, title, state: 'connecting', files: false, num }) as TermTab
}

export async function loadConnections() {
  const r = await call<{ path: string; connections: Connection[] }>(['connection', 'list'])
  connectionsPath.value = r.path
  connections.value = r.connections
}

// ---------------------------------------------------------------- 密码

/** 部署时本机有程序占着要构建的 jar；retry 重新部署 */
export const busyRequest = ref<{ config: string; target: string; items: BusyItem[]; retry: () => void } | null>(null)

/** 打开设置并切到某一页（如 AI 助手里点「模型设置」） */
export const settingsRequest = ref('')

/** 需要用户输入密码 / 私钥密码；retry 在填好后重新执行原来的操作 */
/** 导入的配置在这台电脑上缺东西（别的电脑上的私钥、源码目录）：进入项目后打开「本机设置」 */
export const localRequest = ref<string | null>(null)

export const secretRequest = ref<{ detail: ErrorDetail; config: string; retry: () => void } | null>(null)
