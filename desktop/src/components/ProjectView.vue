<script setup lang="ts">
import LoadingBlock from './LoadingBlock.vue'
import { computed, defineAsyncComponent, nextTick, onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, watch, type Component } from 'vue'
import { ElMessage, ElMessageBox, type TableInstance } from 'element-plus'
import { Refresh, Upload, VideoPlay, VideoPause, RefreshRight, Clock, EditPen, Key, Monitor, ChatDotRound, Grid, List, SwitchButton, Setting, ArrowDown, Tools, Bell, Box, Connection, User, DataLine } from '@element-plus/icons-vue'
import { call, FerryError, latencyInfo, latencyTip, shortCommit, time, type AppStatus, type ConnStatus, type LockHolder, type ExtraPort, type Inspect, type InspectTarget } from '../ferry'
import { addProject, addTerminal, aiAskRequest, startTask, trustRequest, trustedTick, secretRequest, localRequest, appSettings, tasks, projectSummary, type Project } from '../state'
import SoftwarePanel from './SoftwarePanel.vue'
import PortPanel from './PortPanel.vue'
import SshPanel from './SshPanel.vue'
import ServerInfoPanel from './ServerInfoPanel.vue'
import { serverInfoCards } from '../serverinfo'
import MonitorDialog from './MonitorDialog.vue'
import BackupDialog from './BackupDialog.vue'
import { isMap, isScalar, isSeq, parseDocument } from 'yaml'
import { pickFile, readConfig } from '../platform'
import { confirmProduction } from '../ui'
import GlassButton from './glass/GlassButton.vue'
import GlassTabs from './glass/GlassTabs.vue'
import GlassPane from './glass/GlassPane.vue'
import { glass, setVar } from '../glass/runtime'

// 用到时才加载（编辑器、终端、AI 助手和各个对话框不放进启动时加载的代码里）
const AiDrawer = defineAsyncComponent(() => import('./AiDrawer.vue'))
const DeployDialog = defineAsyncComponent(() => import('./DeployDialog.vue'))
const ReleasesDialog = defineAsyncComponent(() => import('./ReleasesDialog.vue'))
const LogsDialog = defineAsyncComponent(() => import('./LogsDialog.vue'))
const HistoryDialog = defineAsyncComponent(() => import('./HistoryDialog.vue'))
const ConfigEditor = defineAsyncComponent(() => import('./ConfigEditor.vue'))
const KeygenDialog = defineAsyncComponent(() => import('./KeygenDialog.vue'))
const LocalDialog = defineAsyncComponent(() => import('./LocalDialog.vue'))

const props = defineProps<{ project: Project }>()

const info = ref<Inspect>()
const loadError = ref('')
const activeTarget = ref('')
const target = computed<InspectTarget | undefined>(() => info.value?.targets.find((t) => t.name === activeTarget.value))

// 每个目标的状态单独缓存
const statuses = ref<Record<string, AppStatus[]>>({})
const statusError = ref<Record<string, { msg: string; unknownHost: boolean; keyMissing?: boolean }>>({})
// 窗口窄时（页面不到 900px 宽）顶部的「终端」「AI 助手」「编辑配置」只显示图标，名字放到悬停提示里：一行放得下
const pageEl = ref<HTMLElement>()
const compact = ref(false)
let pageResize: ResizeObserver | undefined
onMounted(() => {
  pageResize = new ResizeObserver(([e]) => (compact.value = e.contentRect.width < 900))
  if (pageEl.value) pageResize.observe(pageEl.value)
})
onBeforeUnmount(() => { pageResize?.disconnect(); floatResize?.disconnect() })

// 液态玻璃：列表往上延伸到功能切换和工具栏下面，滚动时内容从它们下面经过。
// 量出功能切换的上沿到列表原来位置的距离，列表往上挪这么多、顶部再留出同样的空白，不滚动时位置不变。
const bodyEl = ref<HTMLElement>()
let floatResize: ResizeObserver | undefined
function measureFloat() {
  const body = bodyEl.value
  const list = body?.querySelector<HTMLElement>(':scope > .list')
  const tabs = body?.querySelector<HTMLElement>(':scope > .workspace-tabs > .workspace-switch')
  if (!body || !list || !tabs || !glass.enabled) { body?.style.removeProperty('--float-h'); body?.style.removeProperty('--float-cut'); return }
  // 列表已经往上挪了 --float-h：它原来的位置 = 现在的上沿 + 挪掉的距离（不用先清零再量，少一次布局、少写一次变量）
  const moved = parseFloat(body.style.getPropertyValue('--float-h')) || 0
  const height = Math.max(0, Math.round(list.getBoundingClientRect().top + moved - tabs.getBoundingClientRect().top))
  setVar(body, '--float-h', `${height}px`)
  measureCut()
}
// 列表内容滚过工具栏上沿就不再显示：从列表上沿（= 功能切换上沿）到工具栏上沿这段遮掉。
// 应用、登录用户、服务器信息是页面的工具栏；端口、软件是面板自己贴在顶上的工具栏（停在 --float-h 往上 3px 处）。
// 上面的内容加载时高度会变，滚动时也顺便重看一次（只读位置，不改布局）
function measureCut() {
  const body = bodyEl.value
  const list = body?.querySelector<HTMLElement>(':scope > .list')
  const tabs = body?.querySelector<HTMLElement>(':scope > .workspace-tabs > .workspace-switch')
  if (!body || !list || !tabs || !glass.enabled) return
  const height = parseFloat(body.style.getPropertyValue('--float-h')) || 0
  const bar = body.querySelector<HTMLElement>(':scope > .toolbar')
  const cut = bar ? Math.round(bar.getBoundingClientRect().top - tabs.getBoundingClientRect().top)
    : list.querySelector('.fw-line, .software-toolbar') ? height - 3 : height
  setVar(body, '--float-cut', `${Math.max(0, cut)}px`)
}
let cutFrame: number | undefined
function onListScroll() {
  if (glass.enabled && cutFrame === undefined) cutFrame = requestAnimationFrame(() => { cutFrame = undefined; measureCut() })
}
function observeFloat() {
  floatResize?.disconnect()
  const body = bodyEl.value
  if (!body || !glass.enabled) { measureFloat(); return }
  floatResize = new ResizeObserver(measureFloat)
  for (const element of body.querySelectorAll(':scope > .workspace-tabs, :scope > .toolbar')) floatResize.observe(element)
  measureFloat()
}

// 正在读取状态的服务器（各记各的：一台还在连接时切到另一台，不显示成另一台在连接）
const loadingTargets = ref<Record<string, number>>({})
const loadingStatus = computed(() => !!loadingTargets.value[activeTarget.value])
// 勾选的应用（按名字记）：表格切到「软件」再切回来、刷新状态时会重建，这里记住、重建后再勾回去
const selection = ref<string[]>([])
const table = ref<TableInstance>()
let syncing = false

// 只听用户点的勾选（select / select-all）：表格换数据时自己发的 selection-change 不算。
// 筛选时表格里只有一部分应用：没显示出来的那些原来勾没勾保持不变
function onSelectionChange(v: AppStatus[]) {
  if (syncing) return
  const shown = new Set(visibleRows.value.map((r) => r.name))
  selection.value = [...selection.value.filter((n) => !shown.has(n)), ...v.map((r) => r.name)]
}

// 卡片：点卡片或勾选框切换勾选
const isSelected = (name: string) => selection.value.includes(name)
// 连点、按住 Shift 点卡片时浏览器会选中（甚至跨卡片选中）文字：这时不让它选
function noTextSelect(e: MouseEvent) {
  if (e.shiftKey || e.detail > 1) e.preventDefault()
}
function toggleSel(name: string) {
  selection.value = isSelected(name) ? selection.value.filter((n) => n !== name) : [...selection.value, name]
}
// 在卡片里拖选文字（比如复制版本号）松手时也会触发 click：这时只选文字，不切换勾选
function cardClick(name: string) {
  const s = window.getSelection()
  if (s && !s.isCollapsed && s.toString().trim()) return
  toggleSel(name)
}
const allShownSelected = computed(() => visibleRows.value.length > 0 && visibleRows.value.every((r) => isSelected(r.name)))
const someShownSelected = computed(() => !allShownSelected.value && visibleRows.value.some((r) => isSelected(r.name)))
function toggleAllShown(on: boolean | string | number) {
  const shown = new Set(visibleRows.value.map((r) => r.name))
  const rest = selection.value.filter((n) => !shown.has(n))
  selection.value = on ? [...rest, ...shown] : rest
}

async function syncSelection() {
  await nextTick()
  const t = table.value
  if (!t) return
  syncing = true
  try {
    t.clearSelection()
    for (const r of visibleRows.value) {
      if (selection.value.includes(r.name)) t.toggleRowSelection(r, true)
    }
  } finally {
    syncing = false
  }
}

const deployOpen = ref(false)
const releasesApp = ref('')
const logsApp = ref('')
const logsRelease = ref('') // 从「版本/回滚」打开某个版本的运行日志
const historyOpen = ref(false)
// AI 助手：每台服务器一个对话（切换服务器时各自保留）
const aiOpen = ref<Record<string, boolean>>({})
const aiTargets = ref<string[]>([])
const aiDrawers: Record<string, { ask: (text: string) => Promise<void> } | null> = {}
// question：直接问这句话（新开一个对话），「让 AI 看看」用
async function openAi(t: string, question = '') {
  if (!aiTargets.value.includes(t)) aiTargets.value.push(t)
  aiOpen.value[t] = true
  if (!question) return
  await nextTick()
  aiDrawers[t]?.ask(question)
}
// 任务面板里点了「让 AI 看看」：是这个项目的就打开对应服务器的 AI 助手
watch(
  aiAskRequest,
  (r) => {
    if (!r || r.config !== props.project.path) return
    aiAskRequest.value = null
    activeTarget.value = r.target
    openAi(r.target, r.text)
  },
  { immediate: true }, // 换到这个项目之前就发来的
)
// 应用异常时问 AI：带上看得到的状态，让它查日志、找原因
function askAboutApp(s: AppStatus) {
  const parts = [`应用 ${s.name}（${s.type}${s.unit ? '，单元 ' + s.unit : ''}）现在的状态：${stateText(s)}`]
  if (shownProblem(s)) parts.push(`问题：${shownProblem(s)}`)
  if (s.healthy === false) parts.push('健康检查没通过')
  if (s.current) parts.push(`运行的版本 ${s.current.id}，${time(s.current.deployedAt)} 由 ${s.current.deployedBy} 部署`)
  if (riskyPorts(s).length) parts.push(`对外开放了不宜对外的端口：${riskyPorts(s).map((e) => e.addr).join('、')}`)
  openAi(activeTarget.value, parts.join('；') + '。\n请查看它的状态、最近的日志（有需要也看看配置、端口、磁盘），找出原因，告诉我怎么修。先只查看，不要改动。')
}
const editorOpen = ref(false)
const keygenOpen = ref(false)
// 配置免密登录的目标（从本机设置里点的可能不是当前服务器）
const keygenTarget = ref('')
const keygenFor = computed(() => info.value?.targets.find((t) => t.name === (keygenTarget.value || activeTarget.value)))
function openKeygen(name = '') {
  keygenTarget.value = name
  keygenOpen.value = true
}
// 本机设置：只属于这台电脑的私钥、登录方式、变量的值
const localOpen = ref(false)
// 服务器告警（ferry monitor）
const monitorOpen = ref(false)
const backupOpen = ref(false)

/** 别人正在这台服务器上部署、回滚、启停：这台电脑上没有这个目标在跑的任务时才显示 */
const busyOther = computed(() => {
  const b = statuses.value[activeTarget.value]?.find((s) => s.busy)?.busy
  if (!b) return undefined
  const mine = tasks.some((x) => x.status === 'running' && x.config === props.project.path && x.target === activeTarget.value)
  return mine ? undefined : b
})
function busyText(b: LockHolder) {
  const min = Math.max(0, Math.round((Date.now() - new Date(b.since).getTime()) / 60000))
  const ago = min < 1 ? '刚刚' : min < 60 ? `${min} 分钟前` : `${Math.round(min / 60)} 小时前`
  return `${b.actor || '有人'}正在${b.action}${b.apps?.length ? ' ' + b.apps.join('、') : ''}（${ago}开始）：等对方做完再部署，或者先联系一下`
}
// 导入的配置在这台电脑上缺东西：进来就打开本机设置
watch(
  localRequest,
  (p) => {
    if (p && p === props.project.path) {
      localOpen.value = true
      localRequest.value = null
    }
  },
  { immediate: true },
)
// 这台电脑上缺的东西（当前服务器的私钥、构建目录）；连接时已经报了私钥的，不重复列
const problems = computed(() =>
  (info.value?.problems ?? []).filter(
    (p) => (p.kind !== 'key-missing' || p.target === activeTarget.value) && !(p.kind === 'key-missing' && statusError.value[activeTarget.value]?.keyMissing),
  ),
)
const localOn = (t: InspectTarget) => !!t.local || !!t.localVars?.length
function localTip(t: InspectTarget): string {
  const parts: string[] = []
  const l = t.local
  if (l) parts.push(l.auth === 'password' ? '密码登录' : l.identityFile ? `私钥 ${l.identityFile}` : '默认私钥 / ssh-agent')
  if (t.localVars?.length) parts.push(`变量 ${t.localVars.join('、')} 用这台服务器自己的值`)
  return `这台电脑上 ${t.name} 的本机设置：${parts.join('；')}（ferry.yaml 没改，点击修改）`
}
// 连接时发现私钥不在这台电脑上：选一个私钥 / 改用密码登录（记到本机设置）
async function setLocalLogin(how: 'key' | 'password') {
  const t = activeTarget.value
  const args = ['local', 'set', t, '-c', props.project.path]
  if (how === 'key') {
    const p = await pickFile('选择这台电脑上的私钥', '私钥', [])
    if (!p) return
    args.push('--key', p)
  } else args.push('--password')
  try {
    await call(args)
    ElMessage.success('已记到这台电脑的本机设置')
    delete statusError.value[t]
    await loadInspect()
    await loadStatus()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}
const editorFocus = ref<string[] | undefined>()

// 服务器下的标签：应用 / 软件 / 端口 / 登录用户 / 服务器信息
const view = ref<'apps' | 'software' | 'ports' | 'ssh' | 'server'>('apps')
const targetDetails = ref(false)
const workspaceTabs: { value: typeof view.value; label: string; icon: Component }[] = [
  { value: 'apps', label: '应用', icon: Grid },
  { value: 'software', label: '软件', icon: Box },
  { value: 'ports', label: '端口', icon: Connection },
  { value: 'ssh', label: '登录用户', icon: User },
  { value: 'server', label: '服务器信息', icon: DataLine },
]
// 页面出来、切换视图（工具栏有时没有）、开关液态玻璃时重新量
watch([bodyEl, view, () => glass.enabled], () => nextTick(observeFloat), { flush: 'post' })
const swPanel = ref<InstanceType<typeof SoftwarePanel>>()
const portPanel = ref<InstanceType<typeof PortPanel>>()
const sshPanel = ref<InstanceType<typeof SshPanel>>()
const serverPanel = ref<InstanceType<typeof ServerInfoPanel>>()
// 从应用的端口点过来：切到「端口」并定位到这个端口
const portFocus = ref<number>()
function goPort(addr: string | number) {
  portFocus.value = undefined // 再点同一个端口也要定位
  view.value = 'ports'
  nextTick(() => (portFocus.value = typeof addr === 'number' ? addr : Number(portOf(addr))))
}

// 每个目标在 ferry.yaml 里实际写了哪些字段（用来区分「来自配置」与「默认值」）
const setFields = ref<Record<string, Set<string>>>({})

async function loadSetFields() {
  try {
    const doc = parseDocument(await readConfig(props.project.path))
    const out: Record<string, Set<string>> = {}
    const targets = doc.get('targets')
    if (isMap(targets)) {
      for (const p of targets.items) {
        if (!isScalar(p.key) || !isMap(p.value)) continue
        out[String(p.key.value)] = new Set(p.value.items.filter((i) => isScalar(i.key)).map((i) => String((i.key as { value: unknown }).value)))
      }
    }
    setFields.value = out
    // 每项备份只在哪些服务器上（null = 全部）
    const backups = doc.get('backups')
    backupTargets.value = isSeq(backups)
      ? backups.items.map((b) => {
          const t = isMap(b) ? b.get('targets') : undefined
          return isSeq(t) ? t.items.map((x) => String(isScalar(x) ? x.value : x)) : null
        })
      : []
  } catch {
    setFields.value = {}
    backupTargets.value = []
  }
}

const isSet = (field: string) => !!setFields.value[activeTarget.value]?.has(field)

// 配置里有这台服务器的备份时才显示「数据备份」（打开、恢复、下载）；要备份什么在编辑配置里写
const backupTargets = ref<(string[] | null)[]>([])
const hasBackups = computed(() => backupTargets.value.some((t) => !t || t.includes(activeTarget.value)))

// 「配置免密登录」只在需要时显示：密码登录、私钥不在这台电脑上，或没指定私钥又登录不上
const needKeygen = computed(() => {
  const t = target.value
  if (!t) return false
  if (t.auth === 'password' || t.keyMissing || statusError.value[activeTarget.value]?.keyMissing) return true
  return !t.identityFile && !!statusError.value[activeTarget.value]
})

const defaults: Record<string, string> = {
  root: '/opt/<project>',
  port: '22',
  user: 'root',
  keepReleases: '5',
  auth: 'key（私钥 / ssh-agent）',
  arch: '自动检测',
  sudo: 'false',
}

function fieldTip(field: string): string {
  const path = `targets.${activeTarget.value}.${field}`
  if (isSet(field)) return `来自 ferry.yaml：${path}（点击编辑）`
  return `默认值${defaults[field] ? ' ' + defaults[field] : ''}：ferry.yaml 没有设置 ${path}（点击添加）`
}

function openEditor(field?: string) {
  targetDetails.value = false
  editorFocus.value = field ? ['targets', activeTarget.value, field] : undefined
  editorOpen.value = true
}

async function loadInspect() {
  loadError.value = ''
  try {
    info.value = await call<Inspect>(['inspect', '-c', props.project.path])
    if (info.value.project !== props.project.name) await addProject({ path: props.project.path, name: info.value.project })
    if (!info.value.targets.some((t) => t.name === activeTarget.value)) activeTarget.value = info.value.targets[0]?.name ?? ''
    loadSetFields()
  } catch (e) {
    info.value = undefined
    loadError.value = (e as Error).message
  }
}

// background：定时的自动刷新（间隔见设置 → 连接）。只用已有的连接、不推迟空闲断开；读不到（连接刚好断开）就算了，不显示错误
async function loadStatus(background = false) {
  const t = activeTarget.value
  if (!t) return
  loadingTargets.value[t] = (loadingTargets.value[t] ?? 0) + 1
  try {
    statuses.value[t] = await call<AppStatus[]>(['status', t, '-c', props.project.path, ...(background ? ['--background'] : [])])
    statusAt.value[t] = Date.now()
    now.value = Date.now()
    delete statusError.value[t]
  } catch (e) {
    if (background) return
    const err = e as FerryError
    const keyMissing = err instanceof FerryError && err.keyMissing
    // 私钥不在：下面有按钮，不用列命令行的办法
    const msg = keyMissing
      ? `${err.detail?.local ? '本机设置里' : 'ferry.yaml 里'}指定的私钥 ${err.detail?.key} 在这台电脑上没有，默认私钥、ssh-agent 也登录不了。私钥只在配置它的那台电脑上，不会跟着 ferry.yaml 走。`
      : err.message
    statusError.value[t] = { msg, unknownHost: err instanceof FerryError && err.unknownHost, keyMissing }
    if (err instanceof FerryError && err.needSecret && err.detail) {
      secretRequest.value = { detail: err.detail, config: props.project.path, retry: loadStatus }
    }
  } finally {
    if (--loadingTargets.value[t] <= 0) delete loadingTargets.value[t]
    loadConns()
  }
}

// 点「刷新」：重新读配置、应用状态和当前打开的面板。
// 刷新时盖一层「正在刷新」，完了提示一下：数据没变时也看得出刷新过
const refreshing = ref(false)
async function refresh() {
  if (refreshing.value) return
  refreshing.value = true
  const t = activeTarget.value
  const panel = view.value === 'software' ? swPanel.value : view.value === 'ports' ? portPanel.value : view.value === 'ssh' ? sshPanel.value : view.value === 'server' ? serverPanel.value : undefined
  const started = Date.now()
  try {
    await loadInspect()
    await Promise.all([loadStatus(), panel?.load()])
    // 复用连接时不到 0.1 秒就读完了：至少显示半秒，不然一闪而过看不出来
    await new Promise((r) => setTimeout(r, Math.max(0, 500 - (Date.now() - started))))
  } finally {
    refreshing.value = false
  }
  if (!loadError.value && !(panel ? panel.error : statusError.value[t])) ElMessage.success({ message: '已刷新', duration: 1500 })
}

// 到各目标的常驻连接（桌面版在操作之间复用连接，空闲后自动断开）
const conns = ref<Record<string, ConnStatus>>({})
const conn = computed(() => conns.value[activeTarget.value])
// 服务器下拉里每台的连接灯
function targetDot(name: string) {
  const live = !!conns.value[name]?.connected
  return { live, busy: !!loadingTargets.value[name] && !live }
}
function targetState(name: string) {
  const { live, busy } = targetDot(name)
  return busy ? '正在连接' : live ? '已连接' : '未连接'
}

async function loadConns() {
  try {
    const list = await call<ConnStatus[]>(['pool', '-c', props.project.path])
    conns.value = Object.fromEntries(list.map((c) => [c.target, c]))
  } catch {
    conns.value = {}
  }
}

async function disconnect() {
  try {
    await call(['disconnect', activeTarget.value, '-c', props.project.path])
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
  loadConns()
}

const latency = computed(() => latencyInfo(conn.value?.rtt, conn.value?.slow))

const connTip = computed(() => {
  const c = conn.value
  const idle = appSettings.value?.connect.idleMinutes ?? 15
  if (!c?.connected) return `未连接：下次操作时自动连接，之后的操作复用这条连接（空闲 ${idle} 分钟后断开）`
  const using = c.inUse ? `，${c.inUse} 个操作正在使用` : ''
  return `${c.label}${latency.value ? `，往返 ${latency.value.text}` : ''}，连接于 ${time(c.since)}，最近使用 ${time(c.lastUsed)}${using}；空闲 ${idle} 分钟后自动断开`
})

// 项目页切走后保留（KeepAlive）：切回来先显示上次的状态，再在后台刷新；连接本来就一直保持（直到空闲超时）
let connTimer: ReturnType<typeof setInterval> | undefined
let mounted = false
let active = true // 切到别的项目后，这一页的后台刷新暂停
const startTimer = () => {
  clearInterval(connTimer)
  connTimer = setInterval(() => {
    now.value = Date.now()
    loadConns().then(autoRefresh)
  }, 10000)
}

// 应用状态自动更新：页面开着、窗口没最小化、到这台服务器的连接还连着时，按设置的间隔读（默认每分钟，复用连接，很快）。
// 自动读取不算「使用」连接，不推迟空闲断开；连接断开（空闲超时、手动断开）后不自动重连：显示上次读取的时间，点刷新再读
// 改了设置马上按新的间隔：下面每 10 秒检查一次是否到时间
const autoMinutes = computed(() => appSettings.value?.connect.statusMinutes ?? 1)
const statusAt = ref<Record<string, number>>({})
const now = ref(Date.now())
function autoRefresh() {
  const t = activeTarget.value
  if (!active || !t || document.hidden || refreshing.value || loadingTargets.value[t]) return
  if (autoMinutes.value < 0 || !conns.value[t]?.connected || statusError.value[t]) return
  if (Date.now() - (statusAt.value[t] ?? 0) < autoMinutes.value * 60_000) return
  loadStatus(true)
}
const readAgo = computed(() => {
  const at = statusAt.value[activeTarget.value]
  if (!at) return ''
  const m = Math.floor((now.value - at) / 60_000)
  return m < 1 ? '刚刚' : m < 60 ? `${m} 分钟前` : `${Math.floor(m / 60)} 小时前`
})
const readTip = computed(() =>
  autoMinutes.value < 0
    ? '已关闭自动读取应用状态（在 设置 → 连接 里可以打开）；点右上角的刷新马上读'
    : conn.value?.connected
      ? `连着服务器时每${autoMinutes.value === 1 ? '' : ` ${autoMinutes.value} `}分钟自动读一次应用状态（在 设置 → 连接 里可以改）；点右上角的刷新马上读`
      : '连接已断开，不再自动读取（下次操作时自动连接）；点右上角的刷新重新读',
)
onMounted(async () => {
  startTimer()
  await loadInspect()
  await loadConns() // 本机就能答复：先知道是不是已经连着，加载提示才说得准
  await loadStatus()
  mounted = true
})
onActivated(async () => {
  active = true
  if (!mounted) return // 第一次显示时 onMounted 已经在加载
  startTimer()
  await loadConns()
  await loadInspect() // 配置可能在别处改过
  await loadStatus()
})
onDeactivated(() => {
  targetDetails.value = false
  active = false
  clearInterval(connTimer)
})
onBeforeUnmount(() => clearInterval(connTimer))
// 任务（部署、启停、日志等）结束后刷新连接状态
watch(
  () => tasks.filter((t) => t.status === 'running').length,
  () => active && loadConns(),
)
// 这个项目的任务（部署、启停、回滚……）结束后刷新状态。不靠对话框的回调：部署一开始对话框就关了，回调送不到
const finishedHere = computed(() =>
  tasks
    .filter((t) => t.config === props.project.path && t.status !== 'running')
    .map((t) => t.id)
    .join(','),
)
// 切到别的项目时结束的不用管：切回来时 onActivated 本来就会刷新
watch(finishedHere, () => active && loadStatus())
// 版本 / 历史 / 日志窗口打开后（连接已建立）、关闭后刷新
watch([releasesApp, logsApp, historyOpen], () => setTimeout(loadConns, 1500))
watch(trustedTick, () => loadStatus())
watch(activeTarget, (t) => {
  targetDetails.value = false
  selection.value = []
  filter.value = 'all'
  if (t && !statuses.value[t]) loadStatus()
})

const rows = computed<AppStatus[]>(() => statuses.value[activeTarget.value] ?? [])
// 「端口」里用：应用的单元 → 应用名；不宜对外开放的附加端口 → 用途
const appUnits = computed(() => Object.fromEntries(rows.value.filter((r) => r.unit).map((r) => [r.unit!, r.name])))
const riskyUse = computed(() => {
  const out: Record<number, string> = {}
  for (const r of rows.value) for (const e of r.extra ?? []) if (e.risky) out[Number(portOf(e.addr))] = `${r.name} 的${e.use ?? '附加端口'}`
  return out
})
// 只算这台服务器上还有的应用（配置里删掉的不算）
const selectedNames = computed(() => {
  if (!rows.value.length) return selection.value
  const have = new Set(rows.value.map((r) => r.name))
  return selection.value.filter((n) => have.has(n))
})

function stateType(s: AppStatus): 'success' | 'danger' | 'warning' | 'info' {
  if (s.type === 'static' && !s.unit) return s.current ? 'success' : 'info'
  if (s.active === 'active' && s.healthy !== false) return 'success'
  if (s.active === 'failed' || s.healthy === false) return 'danger'
  if (s.active === 'activating' || s.sub === 'auto-restart') return 'warning'
  return 'info'
}

// ---- 概览数字与筛选：点「运行中 / 已停止 / 需要留意」只看这些，再点一次恢复全部
const riskyPorts = (s: AppStatus) => (s.extra ?? []).filter((e) => e.risky && !isLocal(e.addr))
const isUp = (s: AppStatus) => stateType(s) === 'success'
// agent 写的「未部署」是有单元但没有 Ferry 的版本；「单元不存在」是连单元都没有。
// 有单元、在运行（或启动中、失败了）的是别的方式部署的：照 systemd 的状态显示，版本处注明不是 Ferry 部署的
const notFerry = (s: AppStatus) => s.problem === '未部署' && !s.current && !!s.active && s.active !== 'inactive'
// 这台服务器上还没部署过、也没在运行：不算异常，状态显示「未部署」
const notDeployed = (s: AppStatus) => (s.problem === '单元不存在' || s.problem === '未部署') && !s.current && !notFerry(s)
const shownProblem = (s: AppStatus) => (notDeployed(s) || notFerry(s) ? '' : (s.problem ?? ''))
const isAbnormal = (s: AppStatus) => stateType(s) === 'danger' || stateType(s) === 'warning' || !!shownProblem(s)
const needsAttention = (s: AppStatus) => isAbnormal(s) || riskyPorts(s).length > 0
type Filter = 'all' | 'up' | 'down' | 'attention'
const filter = ref<Filter>('all')
const appQuery = ref('')
const sessionQuery = ref('')
const filterLabel: Record<Filter, string> = { all: '', up: '运行中', down: '已停止', attention: '需要留意' }
function setFilter(f: Filter) {
  filter.value = filter.value === f ? 'all' : f
}
const visibleRows = computed(() => {
  const f = filter.value
  const matching = rows.value.filter((s) => `${s.name} ${s.type} ${s.unit ?? ''} ${s.port ?? ''}`.toLowerCase().includes(appQuery.value.trim().toLowerCase()))
  if (f === 'up') return matching.filter(isUp)
  if (f === 'down') return matching.filter((s) => !isUp(s))
  if (f === 'attention') return matching.filter(needsAttention)
  return matching
})
watch([table, visibleRows], syncSelection)

const stats = computed(() => {
  const r = rows.value
  const up = r.filter(isUp).length
  const abnormal = r.filter(isAbnormal).length
  const fresh = r.filter((s) => !isUp(s) && notDeployed(s)).length // 「已停止」里有几个是还没部署过的
  const ports = r.reduce((n, s) => n + riskyPorts(s).length, 0)
  const why = [abnormal ? `${abnormal} 个应用异常` : '', ports ? `${ports} 个端口对外开放` : ''].filter(Boolean).join('，')
  let last: AppStatus | undefined
  for (const s of r) {
    if (s.current?.deployedAt && (!last || s.current.deployedAt > (last.current?.deployedAt ?? ''))) last = s
  }
  return { total: r.length, up, down: r.length - up, fresh, attention: r.filter(needsAttention).length, why, last }
})
// 「应用」的概览：还没读到状态时格子照样显示，正在读时数字处是占位，读不到时是「—」

// 「软件 / 端口 / 登录用户」上方的四格概览：第一次打开、还没读到时格子照样显示，
// 正在查看时数字处是占位，读不到时是「—」。bare：只有标题的格子
type StatCard = { label: string; num?: string | number; small?: string; cls?: string; sub?: string; title?: string; bare?: boolean }
const panelStats = computed<{ ready: boolean; busy: boolean; cards: StatCard[] } | null>(() => {
  if (view.value === 'apps') return null
  const panel = view.value === 'software' ? swPanel.value : view.value === 'ports' ? portPanel.value : view.value === 'server' ? serverPanel.value : sshPanel.value
  // 面板还没挂上时也算正在查看
  const busy = !panel || panel.loading
  let cards: StatCard[]
  if (view.value === 'server') {
    return { ready: !!serverPanel.value?.report, busy, cards: serverPanel.value?.cards ?? serverInfoCards() }
  }
  if (view.value === 'software') {
    const c = swPanel.value?.counts
    cards = [
      { label: '运行中', num: c?.running, small: ` / ${c?.total}`, cls: 'ok', sub: `共 ${c?.total} 个软件` },
      { label: '已停止', num: c ? c.total - c.running : undefined, cls: 'off', sub: c && c.total > c.running ? '可以在下面启动' : '全部在运行' },
    ]
    return { ready: !!c, busy, cards }
  }
  if (view.value === 'ports') {
    const c = portPanel.value?.counts
    cards = [
      {
        label: '防火墙',
        num: c?.fw.active ? '在运行' : '没开',
        cls: c?.fw.active ? 'ok' : 'warn',
        sub: `${c?.fw.kind || '没有找到防火墙'}${c?.fw.active && !c.fw.manage ? ' · 只能查看' : ''}`,
      },
      { label: '对外开放', num: c?.open, cls: c?.open ? 'warn' : 'off', sub: `共 ${c?.listening} 个端口在监听` },
      { label: '已拦住', num: c?.blocked, cls: 'off', sub: '只有服务器本机能连' },
      { label: '需要留意', num: c?.attention, cls: c?.attention ? 'warn' : 'off', sub: c?.why || '没有要处理的', title: c?.why },
    ]
    return { ready: !!c, busy, cards }
  }
  const c = sshPanel.value?.counts
  cards = [
    { label: '连接', num: c?.total, sub: `${c?.users} 个用户` },
    { label: '来源 IP', num: c?.ips, sub: '从几个地址连进来' },
    { label: '开着终端', num: c?.terminals, sub: '其余是传文件、执行命令等' },
    { label: '正在登录', num: c?.pending, cls: c?.pending ? 'warn' : 'off', sub: c?.pending ? '还没通过验证' : '没有' },
  ]
  return { ready: !!c, busy, cards }
})

// 左边项目列表里的「运行中 / 全部」：读过状态的服务器加起来
watch(
  statuses,
  (all) => {
    const list = Object.values(all).flat()
    if (list.length) projectSummary[props.project.path] = { up: list.filter(isUp).length, total: list.length }
  },
  { deep: true },
)

// ---- 卡片 / 列表：Ferry 每次启动都先用卡片；切换后这次运行期间保留（「应用」「软件」各自一个），不记到下次
const viewMode = ref<'card' | 'list'>('card')
const swViewMode = ref<'card' | 'list'>('card')

// 状态的小圆点颜色
function stateDot(s: AppStatus): string {
  return { success: 'ok', danger: 'bad', warning: 'warn', info: 'off' }[stateType(s)]
}

// 监听地址：127.0.0.1:9201 → 9201；只在本机（回环地址）的标出来
const portOf = (l: string) => l.slice(l.lastIndexOf(':') + 1)
const isLocal = (l: string) => l.startsWith('127.') || l.startsWith('[::1]')
function listenTip(l: string): string {
  const host = l.slice(0, l.lastIndexOf(':'))
  if (host === '*') return `监听所有地址（${l}），外部可以访问（取决于防火墙）`
  if (isLocal(l)) return `只监听本机（${l}）：外部访问不到，通常经网关 / nginx 转发`
  return `监听 ${l}`
}

// 主端口：业务端口（有健康检查时就是它的端口）
function mainTip(s: AppStatus, l: string): string {
  const what = s.port && portOf(l) === s.port ? '业务端口（配置里健康检查的端口）' : '业务端口'
  return `${what}。${listenTip(l)}`
}

// 附加端口：用途 + 监听范围；不宜对外开放却监听了所有地址时提醒
function extraTip(e: ExtraPort): string {
  const use = e.use || '用途未识别：不是配置里的业务端口（健康检查端口），可能是程序里的组件自己开的'
  const warn = e.risky && !isLocal(e.addr) ? '。监听了所有地址，防火墙没拦的话外部也能连上：点一下到「端口」里查看、关闭' : ''
  return `${use}。${listenTip(e.addr)}${warn}`
}

// 静态站点：这个监听地址上是哪些 nginx server（域名、location、配置文件）
function siteTip(s: AppStatus, l: string): string {
  const lines = (s.sites ?? [])
    .filter((x) => x.listen.includes(l))
    .map((x) => {
      const name = x.names?.length ? x.names.join(' ') : '（未设域名）'
      const loc = x.location && x.location !== '/' ? `，路径 ${x.location}` : ''
      return `${name}${loc}${x.ssl ? '，https' : ''}（${x.file ?? ''}）`
    })
  return `nginx ${listenTip(l)}；站点：${lines.join('；')}`
}

function stateText(s: AppStatus): string {
  if (s.type === 'static' && !s.unit) return s.current ? '已发布' : '未部署'
  if (notDeployed(s)) return '未部署'
  if (!s.active) return '-'
  if (s.sub === 'auto-restart') return '自动重启中'
  if (s.active === 'active' && s.sub === 'exited') return '已退出'
  return activeText[s.active] ?? s.active
}

// systemd 的状态（ActiveState）用中文显示；更细的说明（SubState）悬停可以看到
const activeText: Record<string, string> = {
  active: '运行中',
  inactive: '已停止',
  failed: '启动失败',
  activating: '启动中',
  deactivating: '停止中',
  reloading: '重新加载中',
  maintenance: '维护中',
}

// 悬停提示：systemd 的 SubState 比 ActiveState 细，认识的先用它，不认识再看 ActiveState
const subTip: Record<string, string> = {
  running: '正在运行',
  dead: '已停止',
  failed: '启动失败',
  exited: '程序已退出',
  condition: '正在启动',
  'start-pre': '正在启动',
  start: '正在启动',
  'start-post': '正在启动',
  reload: '正在重新加载',
  'reload-signal': '正在重新加载',
  'reload-notify': '正在重新加载',
  stop: '正在停止',
  'stop-watchdog': '正在停止',
  'stop-sigterm': '正在停止',
  'stop-sigkill': '正在停止',
  'stop-post': '正在停止',
  'final-watchdog': '正在停止',
  'final-sigterm': '正在停止',
  'final-sigkill': '正在停止',
  'auto-restart': '异常退出，等待自动重启',
  'auto-restart-queued': '异常退出，等待自动重启',
  'dead-before-auto-restart': '异常退出，准备自动重启',
  'failed-before-auto-restart': '异常退出，准备自动重启',
  cleaning: '正在清理',
}
const activeTip: Record<string, string> = {
  active: '正在运行',
  inactive: '已停止',
  failed: '启动失败',
  activating: '正在启动',
  deactivating: '正在停止',
  reloading: '正在重新加载',
  refreshing: '正在刷新',
  maintenance: '维护中',
}
// systemd 的时间带星期和时区（「Tue 2026-10-20 15:15:27 CST」，中文环境下星期是「二」），只留日期和时间
function sinceText(v?: string): string {
  const m = v?.match(/(\d{4}-\d{2}-\d{2}) (\d{2}:\d{2}(:\d{2})?)/)
  return m ? `${m[1]} ${m[2]}` : ''
}
function stateTip(s: AppStatus): string {
  if (!s.active) return stateText(s)
  const parts: string[] = []
  if (notDeployed(s)) parts.push(s.problem === '单元不存在' ? '还没部署过：服务器上没有这个服务' : '还没通过 Ferry 部署过，没有版本记录')
  const what = (s.sub && subTip[s.sub]) || activeTip[s.active] || `${s.active}${s.sub ? '/' + s.sub : ''}`
  const at = sinceText(s.since)
  // 「从…起」只对在运行的说；停了的，这个时间是上次启动的时间
  parts.push(at ? (s.active === 'active' ? `${what}，${at} 起` : `${what}（上次启动于 ${at}）`) : what)
  if (notFerry(s)) parts.push('不是通过 Ferry 部署的，没有版本记录')
  return parts.join('；')
}

function afterTask() {
  loadStatus()
}

// 启动 / 重启 / 停止和部署一样只作用于勾选的应用：没勾选时按钮是灰的
async function control(action: 'start' | 'stop' | 'restart') {
  const names = selectedNames.value
  const label = { start: '启动', stop: '停止', restart: '重启' }[action]
  if (!names.length) {
    ElMessage.warning(`请先勾选要${label}的应用`)
    return
  }
  const what = names.join('、')
  if (action !== 'start' && target.value?.production) {
    if (!(await confirmProduction(`${label} ${what}`, activeTarget.value))) return
  } else if (action !== 'start') {
    try {
      await ElMessageBox.confirm(`${label} ${activeTarget.value} 上的 ${what}？`, label, { type: 'warning' })
    } catch {
      return
    }
  }
  const args = [action, activeTarget.value, names.join(','), '-c', props.project.path, '--yes'] // 上面已经确认过
  startTask(`${label} ${activeTarget.value}：${what}`, args, afterTask)
}

// 部署要先勾选应用：不勾选就部署全部太容易误操作（要全部部署就勾表头的全选）
function openDeploy() {
  if (!selectedNames.value.length) {
    ElMessage.warning('请先勾选要部署的应用（全部部署可以点「全选」或表头的勾选框）')
    return
  }
  deployOpen.value = true
}

function openTrust() {
  trustRequest.value = { target: activeTarget.value }
}

function onSaved() {
  ElMessage.success('配置已保存')
  loadInspect()
}
</script>

<template>
  <div ref="pageEl" class="page" :class="{ compact }">
    <div class="head">
      <!-- 服务器：下拉选择，放在最前面。灯是各台服务器的连接状态（绿：已连接，橙色闪：正在连接） -->
      <el-dropdown v-if="info?.targets.length" trigger="click" placement="bottom-start" class="target-pick" @command="(name: string) => (activeTarget = name)">
        <GlassPane as="button" type="button" radius="pill" class="target-trigger" aria-label="选择服务器" aria-haspopup="menu">
          <span class="tdot target-led" :class="targetDot(activeTarget)" />
          <span class="mono target-name">{{ activeTarget }}</span>
          <span v-if="target?.production" class="prod">生产</span>
          <el-icon class="target-chevron"><ArrowDown /></el-icon>
        </GlassPane>
        <template #dropdown>
          <el-dropdown-menu class="target-menu">
            <el-dropdown-item v-for="t in info.targets" :key="t.name" :command="t.name" :class="{ 'target-on': t.name === activeTarget }">
              <span class="tdot" :class="targetDot(t.name)" :title="targetState(t.name)" />
              <span class="mono target-menu-name">{{ t.name }}</span>
              <span class="mono target-menu-host">{{ t.user }}@{{ t.host }}</span>
              <span v-if="t.production" class="prod">生产</span>
            </el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
      <!-- 项目名胶囊：项目名、当前服务器的地址（点地址改配置）；窄窗口时地址放回第二行 -->
      <GlassPane radius="pill" class="title-pill">
        <el-tooltip :show-after="HOVER_DELAY" :content="project.path" placement="bottom-start">
          <h2>{{ info?.project ?? project.name }}</h2>
        </el-tooltip>
        <el-tooltip v-if="target" :show-after="HOVER_DELAY" :content="fieldTip('host')" placement="bottom">
          <span class="field mono title-addr" @click="openEditor('host')">{{ target.user }}@{{ target.host }}:{{ target.port }}</span>
        </el-tooltip>
      </GlassPane>
      <span class="spacer" />
      <template v-if="target">
        <el-tooltip :show-after="HOVER_DELAY" :content="connTip" placement="bottom">
          <span class="conn" :class="{ on: conn?.connected }"><span class="conn-led" />{{ conn?.connected ? '已连接' : loadingStatus ? '正在连接' : '未连接' }}</span>
        </el-tooltip>
        <el-tooltip v-if="conn?.connected && latency" :show-after="HOVER_DELAY" :content="latencyTip(latency.level)" placement="bottom">
          <span class="latency" :class="latency.level">延迟 {{ latency.text }}</span>
        </el-tooltip>
        <el-tooltip v-if="conn?.connected" :show-after="HOVER_DELAY" content="断开与这台服务器的连接" placement="bottom">
          <GlassButton :icon="SwitchButton" aria-label="断开连接" @click="disconnect">断开</GlassButton>
        </el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" placement="bottom">
          <template #content>
            <b v-if="compact">终端：</b>打开这台服务器的命令行，以 {{ target.user }} 登录{{ target.user !== 'root' ? '（需要 root 权限时用 sudo）' : '' }}<br />
            可以开多个
          </template>
          <GlassButton :icon="Monitor" aria-label="终端" @click="addTerminal(project.path, target.name)">终端</GlassButton>
        </el-tooltip>
        <el-tooltip
          :show-after="HOVER_DELAY"
          :content="(compact ? 'AI 助手：' : '') + '让 AI 查看、排查这台服务器；允许改动时，每个改动都先问你'"
          placement="bottom"
        >
          <GlassButton :icon="ChatDotRound" aria-label="AI 助手" @click="openAi(target.name)">AI 助手</GlassButton>
        </el-tooltip>
      </template>
      <el-tooltip :show-after="HOVER_DELAY" content="编辑配置（ferry.yaml）" placement="bottom" :disabled="!compact">
        <GlassButton :icon="EditPen" aria-label="编辑配置" @click="openEditor()">编辑配置</GlassButton>
      </el-tooltip>
      <el-tooltip :show-after="HOVER_DELAY" content="重新读取配置和状态" placement="bottom">
        <GlassButton :icon="Refresh" :loading="loadingStatus || refreshing" aria-label="刷新" @click="refresh" />
      </el-tooltip>
    </div>

    <el-alert v-if="loadError" type="error" :closable="false" show-icon title="配置有错误">
      <pre class="mono err">{{ loadError }}</pre>
    </el-alert>

    <template v-if="info">
      <!-- 部署参数放在浮层中，打开时不改变信息栏和概览的位置。 -->
      <div v-if="target" class="server-line">
        <el-tooltip :show-after="HOVER_DELAY" :content="fieldTip('host')" placement="bottom">
          <span class="field mono line-addr" @click="openEditor('host')">{{ target.user }}@{{ target.host }}:{{ target.port }}</span>
        </el-tooltip>
        <!-- 点击打开、再点收起：不用 Element Plus 默认的 0.2 秒隐藏延迟（那是给悬停用的），收起才不拖 -->
        <el-popover v-model:visible="targetDetails" trigger="click" :hide-after="0" role="dialog" placement="bottom-start" popper-class="cg-menu-popover" transition="el-zoom-in-top" :width="420" :popper-style="{ maxWidth: 'calc(100vw - 40px)' }">
          <template #reference>
            <GlassPane as="button" type="button" radius="pill" class="deployment-toggle" :class="{ active: targetDetails }" aria-haspopup="dialog" :aria-expanded="targetDetails" @keydown.esc.stop="targetDetails = false">
              <el-icon><Setting /></el-icon>
              <span>部署参数</span>
              <el-icon class="deployment-chevron" :class="{ open: targetDetails }"><ArrowDown /></el-icon>
            </GlassPane>
          </template>
          <section class="deployment-details" aria-label="部署参数详情" @keydown.esc.stop="targetDetails = false">
            <div class="deployment-heading"><strong>部署参数</strong><span>点击参数可编辑</span></div>
            <el-tooltip :show-after="HOVER_DELAY" :content="fieldTip('arch')" placement="left">
              <button type="button" class="deployment-field cg-menu-item" @click="openEditor('arch')"><span>架构</span><span class="mono">{{ target.arch || '自动识别' }}</span><el-icon><EditPen /></el-icon></button>
            </el-tooltip>
            <el-tooltip :show-after="HOVER_DELAY" :content="fieldTip('root')" placement="left">
              <button type="button" class="deployment-field cg-menu-item" @click="openEditor('root')"><span>部署目录</span><span class="deployment-value"><span class="mono">{{ target.root }}</span><span v-if="!isSet('root')" class="dflt">默认</span></span><el-icon><EditPen /></el-icon></button>
            </el-tooltip>
            <el-tooltip :show-after="HOVER_DELAY" :content="fieldTip('runAs')" placement="left">
              <button type="button" class="deployment-field cg-menu-item" @click="openEditor('runAs')"><span>运行用户</span><span>{{ target.runAs || '未设置' }}</span><el-icon><EditPen /></el-icon></button>
            </el-tooltip>
            <el-tooltip :show-after="HOVER_DELAY" :content="fieldTip('keepReleases')" placement="left">
              <button type="button" class="deployment-field cg-menu-item" @click="openEditor('keepReleases')"><span>版本保留</span><span>{{ target.keepReleases }} 个版本<span v-if="!isSet('keepReleases')" class="dflt">默认</span></span><el-icon><EditPen /></el-icon></button>
            </el-tooltip>
            <el-tooltip v-if="target.sudo" :show-after="HOVER_DELAY" :content="fieldTip('sudo')" placement="left">
              <button type="button" class="deployment-field cg-menu-item" @click="openEditor('sudo')"><span>权限提升</span><span>sudo</span><el-icon><EditPen /></el-icon></button>
            </el-tooltip>
            <el-tooltip :show-after="HOVER_DELAY" :content="fieldTip('auth')" placement="left">
              <button type="button" class="deployment-field cg-menu-item" @click="openEditor('auth')"><span>登录方式</span><span>{{ target.auth === 'password' ? '密码登录' : '密钥登录' }}</span><el-icon><EditPen /></el-icon></button>
            </el-tooltip>
            <el-button v-if="target.keyMissing" size="small" plain type="danger" :icon="Setting" @click="targetDetails = false; localOpen = true">私钥不在这台电脑上，打开本机设置</el-button>
          </section>
        </el-popover>
        <el-button v-if="target.keyMissing" size="small" type="danger" plain :icon="Key" @click="localOpen = true">私钥缺失</el-button>
        <el-button v-if="needKeygen" :icon="Key" size="small" @click="openKeygen()">配置免密登录</el-button>
        <!-- 这个目标套用了本机设置时标出「已生效」，提示里写用的是什么 -->
        <el-tooltip
          :show-after="HOVER_DELAY"
          :content="localOn(target) ? localTip(target) : '这台服务器自己的登录方式、路径（JDK、脚本……），以及这台电脑上的源码目录，不写进 ferry.yaml'"
          placement="bottom"
        >
          <GlassPane as="button" type="button" radius="pill" class="server-glass-button" @click="localOpen = true">
            <el-icon><Tools /></el-icon>
            <span>本机设置<span v-if="localOn(target)" class="local-on">已生效</span></span>
          </GlassPane>
        </el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" content="服务器上定时检查应用、磁盘、证书、端口，有问题发到群里（可以随时打开、关闭）" placement="bottom">
          <GlassPane as="button" type="button" radius="pill" class="server-glass-button" @click="monitorOpen = true">
            <el-icon><Bell /></el-icon>
            <span>服务器告警</span>
          </GlassPane>
        </el-tooltip>
        <el-tooltip
          v-if="hasBackups"
          :show-after="HOVER_DELAY"
          content="配置里写的备份：在服务器上打开按时备份、现在备份、恢复、下载到这台电脑（要备份什么在「编辑配置」里改）"
          placement="bottom"
        >
          <el-button :icon="Box" size="small" @click="backupOpen = true">数据备份</el-button>
        </el-tooltip>
        <el-tooltip v-if="readAgo" :show-after="HOVER_DELAY" :content="readTip" placement="bottom">
          <span class="read-at">状态{{ readAgo }}读取{{ autoMinutes < 0 ? '（不自动更新）' : conn?.connected ? '' : '（已断开，不自动更新）' }}</span>
        </el-tooltip>
      </div>

      <!-- 别人正在这台服务器上部署（这台电脑上自己的任务不算） -->
      <el-alert v-if="busyOther" type="warning" :closable="false" show-icon class="machine" :title="busyText(busyOther)" />

      <!-- 这台电脑上缺的东西（别的电脑上的私钥、源码目录）：在本机设置里补上 -->
      <el-alert v-if="problems.length" type="warning" :closable="false" show-icon class="machine" title="这台电脑上缺这些（配置可能是从别的电脑拿来的）">
        <ul>
          <li v-for="p in problems" :key="p.kind + p.path">{{ p.msg }}</li>
        </ul>
        <el-button size="small" type="primary" class="trust-btn" @click="localOpen = true">打开本机设置</el-button>
      </el-alert>

      <!-- 概览和下面的列表：点「刷新」时盖一层「正在刷新」 -->
      <div ref="bodyEl" v-loading="refreshing" element-loading-text="正在刷新…" class="body" :class="{ 'float-glass': glass.enabled }">
      <!-- 概览：点一下只看这些应用，再点一次恢复全部 -->
      <div v-if="view === 'apps' && statuses[activeTarget] !== undefined" class="stats">
        <button class="stat" :class="{ on: filter === 'up' }" :aria-pressed="filter === 'up'" @click="setFilter('up')">
          <span class="stat-label stat-head">运行中<span class="stat-hint">点击筛选查看</span></span>
          <span class="stat-num ok">{{ stats.up }}<small> / {{ stats.total }}</small></span>
          <span class="stat-sub">共 {{ stats.total }} 个应用</span>
        </button>
        <button class="stat" :class="{ on: filter === 'down' }" :aria-pressed="filter === 'down'" @click="setFilter('down')">
          <span class="stat-label stat-head">已停止<span class="stat-hint">点击筛选查看</span></span>
          <span class="stat-num off">{{ stats.down }}</span>
          <span class="stat-sub">{{ stats.fresh ? (stats.fresh === stats.down ? '都还没部署过' : `其中 ${stats.fresh} 个还没部署过`) : stats.down ? '点一下只看这些' : '全部在运行' }}</span>
        </button>
        <button class="stat" :class="{ on: filter === 'attention' }" :aria-pressed="filter === 'attention'" @click="setFilter('attention')">
          <span class="stat-label stat-head">需要留意<span class="stat-hint">点击筛选查看</span></span>
          <span class="stat-num" :class="stats.attention ? 'warn' : 'off'">{{ stats.attention }}</span>
          <span class="stat-sub" :title="stats.why">{{ stats.why || '没有要处理的' }}</span>
        </button>
        <!-- 只显示最近一次部署；部署历史在下面工具栏的「部署历史」里看 -->
        <div class="stat static">
          <span class="stat-label">最近部署</span>
          <template v-if="stats.last">
            <span class="stat-num">{{ time(stats.last.current?.deployedAt).slice(5, 16) }}</span>
            <span class="stat-sub">{{ stats.last.name }} · {{ stats.last.current?.deployedBy }}</span>
          </template>
          <template v-else>
            <span class="stat-num off">—</span>
            <span class="stat-sub">还没有部署过</span>
          </template>
        </div>
      </div>
      <div v-else-if="view === 'apps'" class="stats">
        <div v-for="l in ['运行中', '已停止', '需要留意', '最近部署']" :key="l" class="stat static">
          <span class="stat-label">{{ l }}</span>
          <template v-if="loadingStatus">
            <span class="stat-num"><span class="stat-wait" /></span>
            <span class="stat-sub"><span class="stat-wait sub" /></span>
          </template>
          <template v-else>
            <span class="stat-num off">—</span>
            <span class="stat-sub">&nbsp;</span>
          </template>
        </div>
      </div>

      <!-- 「软件 / 端口 / 登录用户」的四格：还没读到时照样显示，数字处是占位 -->
      <div v-else-if="panelStats" class="stats" :style="{ gridTemplateColumns: `repeat(${panelStats.cards.length}, minmax(0, 1fr))` }">
        <div v-for="c in panelStats.cards" :key="c.label" class="stat static">
          <span class="stat-label">{{ c.label }}</span>
          <template v-if="c.bare" />
          <template v-else-if="panelStats.ready">
            <span class="stat-num" :class="c.cls">{{ c.num }}<small v-if="c.small">{{ c.small }}</small></span>
            <span class="stat-sub" :title="c.title">{{ c.sub }}</span>
          </template>
          <template v-else-if="panelStats.busy">
            <span class="stat-num"><span class="stat-wait" /></span>
            <span class="stat-sub"><span class="stat-wait sub" /></span>
          </template>
          <!-- 读不到：原因在下面的提示里 -->
          <template v-else>
            <span class="stat-num off">—</span>
            <span class="stat-sub">&nbsp;</span>
          </template>
        </div>
      </div>

      <nav class="workspace-tabs" aria-label="服务器功能">
        <GlassTabs v-model="view" :options="workspaceTabs" class="workspace-switch" label="切换服务器功能" />
        <el-tag v-if="view === 'apps' && filter !== 'all'" closable class="filter-tag" @close="filter = 'all'">只看{{ filterLabel[filter] }}（{{ visibleRows.length }}）</el-tag>
      </nav>
      <div v-if="view !== 'software' && view !== 'ports'" class="toolbar">
        <GlassPane v-if="glass.enabled" class="cg-strip" :radius="14" :adaptive="false" aria-hidden="true" />
        <template v-if="view === 'server'">
          <el-button :icon="Refresh" :loading="serverPanel?.loading" circle aria-label="刷新服务器信息" @click="serverPanel?.load()" />
          <span class="spacer" />
          <span class="muted small">{{ serverPanel?.readTime ? `上次采样 ${serverPanel.readTime} · ` : '' }}{{ serverPanel?.autoTip }}</span>
        </template>
        <template v-else-if="view === 'ssh'">
          <el-input v-model="sessionQuery" class="session-search" placeholder="搜索用户、来源 IP 或命令" clearable aria-label="搜索登录会话" />
          <el-button
            type="danger"
            plain
            :icon="SwitchButton"
            :disabled="!sshPanel?.selected.length"
            :loading="sshPanel?.kicking"
            @click="sshPanel?.kickSelected()"
          >
            断开{{ sshPanel?.selected.length ? `（${sshPanel.selected.length}）` : '所选' }}
          </el-button>
          <el-button :icon="Refresh" :loading="sshPanel?.loading" circle aria-label="重新查看" @click="sshPanel?.load()" />
        </template>
        <template v-else>
          <el-input v-model="appQuery" class="app-search" placeholder="搜索应用或端口" clearable aria-label="搜索应用或端口" />
          <el-button type="primary" :icon="Upload" class="deploy-btn" :disabled="!selectedNames.length" @click="openDeploy">
            部署{{ selectedNames.length ? `（${selectedNames.length}）` : '' }}
          </el-button>
          <el-button-group class="app-controls">
            <el-button :icon="VideoPlay" :disabled="!selectedNames.length" aria-label="启动所选应用" title="启动所选应用" @click="control('start')"><span class="action-label">启动</span></el-button>
            <el-button :icon="RefreshRight" :disabled="!selectedNames.length" aria-label="重启所选应用" title="重启所选应用" @click="control('restart')"><span class="action-label">重启</span></el-button>
            <el-button :icon="VideoPause" :disabled="!selectedNames.length" aria-label="停止所选应用" title="停止所选应用" @click="control('stop')"><span class="action-label">停止</span></el-button>
          </el-button-group>
          <el-button class="history-btn" :icon="Clock" aria-label="部署历史" title="部署历史" @click="historyOpen = true"><span class="action-label">部署历史</span></el-button>
          <el-checkbox
            v-if="viewMode === 'card' && visibleRows.length"
            :model-value="allShownSelected"
            :indeterminate="someShownSelected"
            @change="toggleAllShown"
          >全选</el-checkbox>
          <el-radio-group v-model="viewMode" class="view-switch" aria-label="显示方式">
            <el-tooltip :show-after="HOVER_DELAY" content="卡片" placement="bottom">
              <el-radio-button value="card" aria-label="卡片"><el-icon><Grid /></el-icon></el-radio-button>
            </el-tooltip>
            <el-tooltip :show-after="HOVER_DELAY" content="列表" placement="bottom">
              <el-radio-button value="list" aria-label="列表"><el-icon><List /></el-icon></el-radio-button>
            </el-tooltip>
          </el-radio-group>
          <span class="spacer" />
          <el-tooltip :show-after="HOVER_DELAY" :disabled="!selectedNames.length" :content="selectedNames.join('、')" placement="bottom">
            <span class="hint" :class="selectedNames.length ? 'muted' : 'need-sel'">{{ selectedNames.length ? `已选 ${selectedNames.length} 个` : '先勾选要操作的应用' }}</span>
          </el-tooltip>
        </template>
      </div>

      <!-- 列表：内容多时只有这里滚动，上面的服务器信息、概览、工具栏不动 -->
      <div class="list" @scroll.passive="onListScroll">
      <ServerInfoPanel
        v-if="view === 'server' && activeTarget"
        ref="serverPanel"
        :config="project.path"
        :target="activeTarget"
        :connected="!!conn?.connected"
        :active="view === 'server'"
        @loaded="loadConns"
      />
      <SoftwarePanel
        v-else-if="view === 'software' && activeTarget"
        ref="swPanel"
        :config="project.path"
        :target="activeTarget"
        :host="target?.host ?? ''"
        :connected="!!conn?.connected"
        v-model:mode="swViewMode"
        @loaded="loadConns"
        @go-port="goPort"
      />
      <SshPanel
        v-else-if="view === 'ssh' && activeTarget"
        ref="sshPanel"
        v-model:query="sessionQuery"
        :config="project.path"
        :target="activeTarget"
        :connected="!!conn?.connected"
        @loaded="loadConns"
      />
      <PortPanel
        v-else-if="view === 'ports' && activeTarget"
        ref="portPanel"
        :config="project.path"
        :target="activeTarget"
        :connected="!!conn?.connected"
        :apps="appUnits"
        :risky="riskyUse"
        :focus="portFocus"
        @loaded="loadConns"
      />
      <template v-else>

        <el-alert v-if="statusError[activeTarget]" type="error" :closable="false" show-icon class="status-err">
          <template #title>{{
            statusError[activeTarget].unknownHost ? '还没有核对这台服务器的主机公钥' : statusError[activeTarget].keyMissing ? '这台电脑上没有登录用的私钥' : '读取状态失败'
          }}</template>
          <div class="mono err">{{ statusError[activeTarget].msg }}</div>
          <div v-if="statusError[activeTarget].keyMissing" class="trust-btn">
            <el-button type="primary" size="small" :icon="Key" @click="openKeygen()">配置这台电脑的免密登录</el-button>
            <el-button size="small" @click="setLocalLogin('key')">选这台电脑上的私钥…</el-button>
            <el-button size="small" @click="setLocalLogin('password')">改用密码登录</el-button>
            <div class="muted small-tip">只记到这台电脑的本机设置，不改 ferry.yaml，别的电脑照常用。</div>
          </div>
          <el-button v-if="statusError[activeTarget].unknownHost" type="primary" size="small" :icon="Key" class="trust-btn" @click="openTrust">
            核对并记录主机公钥
          </el-button>
        </el-alert>

        <LoadingBlock
          v-if="loadingStatus && rows.length === 0 && !statusError[activeTarget]"
          :text="conn?.connected ? `正在读取 ${activeTarget} 上各应用的状态（复用已有连接）…` : `正在连接 ${activeTarget}，读取各应用状态…`"
          :rows="6"
          min-height="220px"
          class="status-loading"
        />
        <!-- 卡片：点卡片勾选 / 取消 -->
        <div v-else-if="viewMode === 'card'" class="cards">
          <article
            v-for="row in visibleRows"
            :key="row.name"
            class="card"
            :class="{ sel: isSelected(row.name) }"
            @mousedown="noTextSelect"
            @click="cardClick(row.name)"
          >
            <div class="card-top">
              <span @click.stop><el-checkbox :model-value="isSelected(row.name)" :aria-label="`选择 ${row.name}`" @change="toggleSel(row.name)" /></span>
              <div class="card-name">
                <span class="name">{{ row.name }}</span>
                <el-tooltip :show-after="HOVER_DELAY" v-if="target?.appRoots?.[row.name]" placement="top">
                  <template #content>
                    单独设置的部署目录：<span class="mono">{{ target.appRoots[row.name] }}/apps/{{ row.name }}</span><br />
                    其他应用在 <span class="mono">{{ target.root }}</span>
                  </template>
                  <span class="own-root mono">{{ target.appRoots[row.name] }}</span>
                </el-tooltip>
              </div>
              <el-tooltip :show-after="HOVER_DELAY" :content="stateTip(row)" placement="top"><span class="state" :class="stateDot(row)"><span class="dot" />{{ stateText(row) }}</span></el-tooltip>
            </div>
            <dl class="card-fields">
              <div>
                <dt>端口</dt>
                <dd class="mono">
                  <template v-if="row.listen?.length">
                    <el-tooltip :show-after="HOVER_DELAY" v-for="l in row.listen" :key="l" placement="top" :content="row.sites?.length ? siteTip(row, l) : mainTip(row, l)">
                      <span class="port" :class="{ local: isLocal(l) }">{{ portOf(l) }}</span>
                    </el-tooltip>
                  </template>
                  <span v-else-if="row.port" class="muted">{{ row.port }}</span>
                  <span v-else class="muted">—</span>
                </dd>
              </div>
              <div>
                <dt>PID</dt>
                <dd class="mono">{{ row.pid || '—' }}</dd>
              </div>
              <div class="wide">
                <dt>版本</dt>
                <dd v-if="notFerry(row)" class="ellipsis muted" title="不是通过 Ferry 部署的，没有版本记录">非 Ferry 部署</dd>
                <dd v-else class="mono ellipsis" :title="row.current?.id">{{ row.current?.id ?? '—' }}</dd>
                <dd v-if="row.current" class="sub ellipsis">{{ time(row.current.deployedAt).slice(5, 16) }} · {{ row.current.deployedBy }}</dd>
              </div>
            </dl>
            <div class="card-foot">
              <el-tooltip :show-after="HOVER_DELAY" v-for="e in riskyPorts(row)" :key="e.addr" placement="top" :content="extraTip(e)">
                <span class="chip warn mono clickable" @click.stop="goPort(e.addr)">{{ portOf(e.addr) }} 对外开放</span>
              </el-tooltip>
              <span v-if="row.healthy === false" class="chip bad">不健康</span>
              <el-tooltip :show-after="HOVER_DELAY" v-if="shownProblem(row)" :content="shownProblem(row)" placement="top">
                <span class="problem">{{ shownProblem(row) }}</span>
              </el-tooltip>
              <span class="spacer" />
              <el-button v-if="isAbnormal(row)" link type="warning" size="small" @click.stop="askAboutApp(row)">让 AI 看看</el-button>
              <el-button v-if="row.type !== 'static' || row.unit" link type="primary" size="small" @click.stop="logsApp = row.name">日志</el-button>
              <el-button link type="primary" size="small" @click.stop="releasesApp = row.name">版本</el-button>
            </div>
          </article>
          <el-empty v-if="!visibleRows.length && !statusError[activeTarget]" :description="appQuery.trim() ? '没有匹配的应用，请调整搜索条件' : filter === 'all' ? '这台服务器上没有应用' : `没有${filterLabel[filter]}的应用`" :image-size="60" class="cards-empty" />
        </div>
        <div v-else class="table-box">
          <el-table
            :data="visibleRows"
            row-key="name"
            ref="table"
            :empty-text="appQuery.trim() ? '没有匹配的应用，请调整搜索条件' : filter === 'all' ? '暂无数据' : `没有${filterLabel[filter]}的应用`"
            @select="onSelectionChange"
            @select-all="onSelectionChange"
          >
            <el-table-column type="selection" width="44" />
            <el-table-column label="应用" min-width="120">
              <template #default="{ row }">
                <span class="name">{{ row.name }}</span>
                <el-tooltip :show-after="HOVER_DELAY" v-if="target?.appRoots?.[row.name]" placement="top">
                  <template #content>
                    单独设置的部署目录：<span class="mono">{{ target.appRoots[row.name] }}/apps/{{ row.name }}</span><br />
                    其他应用在 <span class="mono">{{ target.root }}</span>
                  </template>
                  <span class="own-root mono">{{ target.appRoots[row.name] }}</span>
                </el-tooltip>
              </template>
            </el-table-column>
            <el-table-column label="状态" width="150">
              <template #default="{ row }">
                <el-tooltip :show-after="HOVER_DELAY" :content="stateTip(row)" placement="top"><span class="state" :class="stateDot(row)"><span class="dot" />{{ stateText(row) }}</span></el-tooltip>
                <span v-if="row.healthy === false" class="chip bad ml">不健康</span>
              </template>
            </el-table-column>
            <el-table-column label="PID" width="80">
              <template #default="{ row }"><span class="mono">{{ row.pid || '' }}</span></template>
            </el-table-column>
            <el-table-column label="主端口" min-width="90">
              <template #default="{ row }">
                <template v-if="row.listen?.length">
                  <el-tooltip :show-after="HOVER_DELAY" v-for="l in row.listen" :key="l" placement="top" :content="row.sites?.length ? siteTip(row, l) : mainTip(row, l)">
                    <span class="port mono" :class="{ local: isLocal(l) }">{{ portOf(l) }}</span>
                  </el-tooltip>
                </template>
                <el-tooltip :show-after="HOVER_DELAY" v-else-if="row.port" placement="top" :content="row.pid ? '没有找到它监听的端口（可能还在启动）；这是配置里健康检查的端口' : '没有运行；这是配置里健康检查的端口'">
                  <span class="port mono muted">{{ row.port }}</span>
                </el-tooltip>
                <el-tooltip :show-after="HOVER_DELAY" v-else-if="row.serving" placement="top" :content="row.serving">
                  <span class="muted">{{ row.serving.startsWith('nginx 里没有') ? '未接入 nginx' : '—' }}</span>
                </el-tooltip>
              </template>
            </el-table-column>
            <el-table-column label="附加端口" min-width="100">
              <template #default="{ row }">
                <el-tooltip :show-after="HOVER_DELAY" v-for="e in row.extra ?? []" :key="e.addr" placement="top" :content="extraTip(e)">
                  <span class="port mono extra" :class="{ risky: e.risky && !isLocal(e.addr), clickable: e.risky && !isLocal(e.addr) }" @click="e.risky && !isLocal(e.addr) && goPort(e.addr)">{{ portOf(e.addr) }}</span>
                </el-tooltip>
              </template>
            </el-table-column>
            <el-table-column label="当前版本" min-width="190">
              <template #default="{ row }">
                <span v-if="notFerry(row)" class="muted" title="不是通过 Ferry 部署的，没有版本记录">非 Ferry 部署</span>
                <span v-else class="mono">{{ row.current?.id ?? '-' }}</span>
              </template>
            </el-table-column>
            <el-table-column label="提交" width="100">
              <template #default="{ row }">
                <span class="mono commit">{{ shortCommit(row.current?.commit, row.current?.dirty) }}</span>
              </template>
            </el-table-column>
            <el-table-column label="部署时间" width="170">
              <template #default="{ row }">{{ time(row.current?.deployedAt) }}</template>
            </el-table-column>
            <el-table-column label="部署人" min-width="120">
              <template #default="{ row }">{{ row.current?.deployedBy ?? '' }}</template>
            </el-table-column>
            <!-- 问题：只有「未接管，旧服务在运行」这类情况才有（未部署已经在状态里），整列都空时不显示 -->
            <el-table-column v-if="visibleRows.some((r) => shownProblem(r))" label="问题" min-width="140">
              <template #default="{ row }"><span class="ev-warn">{{ shownProblem(row) }}</span></template>
            </el-table-column>
            <el-table-column label="操作" width="210" fixed="right">
              <template #default="{ row }">
                <el-button v-if="isAbnormal(row)" link type="warning" @click="askAboutApp(row)">AI 排查</el-button>
              <el-button link type="primary" @click="releasesApp = row.name">版本/回滚</el-button>
                <el-button v-if="row.type !== 'static' || row.unit" link type="primary" @click="logsApp = row.name">日志</el-button>
              </template>
            </el-table-column>
          </el-table>
        </div>
      </template>
      </div>
      </div>
    </template>

    <DeployDialog
      v-if="deployOpen && info"
      :config="project.path"
      :info="info"
      :target="activeTarget"
      :apps="selectedNames"
      @close="deployOpen = false"
      @started="(t) => (deployOpen = false)"
      @finished="afterTask"
    />
    <ReleasesDialog
      v-if="releasesApp"
      :config="project.path"
      :target="activeTarget"
      :app="releasesApp"
      :confirm="target?.confirm ?? true"
      :production="target?.production"
      @close="releasesApp = ''"
      @logs="(r: string) => ((logsRelease = r), (logsApp = releasesApp))"
      @finished="afterTask"
    />
    <LogsDialog
      v-if="logsApp"
      :config="project.path"
      :target="activeTarget"
      :app="logsApp"
      :release="logsRelease || undefined"
      @close="(logsApp = ''), (logsRelease = '')"
    />
    <HistoryDialog v-if="historyOpen" :config="project.path" :target="activeTarget" @close="historyOpen = false" />
    <AiDrawer
      v-for="tn in aiTargets"
      :key="tn"
      :ref="(el) => (aiDrawers[tn] = el as { ask: (text: string) => Promise<void> } | null)"
      v-model="aiOpen[tn]"
      :config="project.path"
      :target="tn"
      :title="tn"
      :production="info?.targets.find((x) => x.name === tn)?.production"
      :no-changes="info?.targets.find((x) => x.name === tn)?.aiChanges === false"
    />
    <MonitorDialog v-if="monitorOpen" :config="project.path" :target="activeTarget" @close="monitorOpen = false" />
    <BackupDialog v-if="backupOpen" :config="project.path" :target="activeTarget" :production="target?.production" @close="backupOpen = false" />
    <LocalDialog
      v-if="localOpen"
      :config="project.path"
      :focus="activeTarget"
      @close="localOpen = false"
      @saved="loadInspect().then(() => loadStatus())"
      @keygen="openKeygen"
    />
    <ConfigEditor v-if="editorOpen" :path="project.path" :focus="editorFocus" @close="editorOpen = false" @saved="onSaved" />
    <KeygenDialog
      v-if="keygenOpen && keygenFor && info"
      :config="project.path"
      :project="info.project"
      :target="keygenFor"
      @close="keygenOpen = false"
      @finished="loadInspect().then(() => loadStatus())"
    />
  </div>
</template>

<style scoped>
/* 上下多留出地方：按住时浮起的液滴比导航条高，不能被裁掉 */
.workspace-tabs { display: flex; align-items: center; gap: 12px; overflow-x: auto; flex-shrink: 0; padding: 10px 6px; margin: -10px -6px; }
.workspace-switch { flex-shrink: 0; }
.deployment-toggle.active { color: var(--el-color-primary); }
.deployment-chevron { transition: transform var(--ferry-ease); }
.deployment-chevron.open { transform: rotate(180deg); }
/* 给滚动区域里的焦点描边和圆角留出空间，负外边距保持原来的内容位置。 */
.deployment-details { max-height: min(480px, calc(100dvh - 100px)); overflow: auto; padding: 6px; margin: -6px; }
.deployment-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 4px 8px 12px; border-bottom: 1px solid var(--ferry-stroke); }
.deployment-heading strong { color: var(--el-text-color-primary); }
.deployment-heading > span { color: var(--el-text-color-secondary); font-size: 12px; }
.deployment-field { display: grid; grid-template-columns: 64px minmax(0, 1fr) 14px; align-items: start; gap: 12px; width: 100%; border: 0; border-radius: 6px; padding: 11px 8px; background: transparent; color: var(--el-text-color-primary); text-align: left; cursor: pointer; font-size: 13px; line-height: 1.6; }
.deployment-field > :first-child, .deployment-field > .el-icon { color: var(--el-text-color-secondary); }
.deployment-field > :nth-child(2) { min-width: 0; overflow-wrap: anywhere; }
.deployment-field > .el-icon { margin-top: 4px; }
.deployment-field:hover, .deployment-field:focus-visible { background: var(--el-fill-color-light); }
.deployment-field:hover > .el-icon { color: var(--el-color-primary); }
.deployment-value .mono { font-size: inherit; }
.toolbar > .app-search { flex: 0 1 180px; min-width: 100px; }
.toolbar > .session-search { flex: 0 1 320px; min-width: 120px; }
.workspace-tabs .filter-tag { align-self: center; margin-left: auto; flex-shrink: 0; }
.stat, .card { box-shadow: var(--ferry-shadow); }
.stat { position: relative; overflow: hidden; }
.stat.on::before { content: ''; position: absolute; inset: 0 auto 0 0; width: 3px; background: var(--el-color-primary); }
.card { border-top: 2px solid var(--ferry-stroke); }
.card:has(.state.ok) { border-top-color: var(--el-color-success); }
.card:has(.state.bad) { border-top-color: var(--el-color-danger); }
.card:has(.state.warn) { border-top-color: var(--el-color-warning); }
.page,
.body,
.list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
/* 页面占满右侧、不整页滚动：列表放不下时只有列表滚动。
   窗口太矮、列表连 200px 都留不出时，才让整页滚动 */
.page {
  height: 100%;
  min-width: 0;
  container: workspace / inline-size;
}
.body {
  flex: 1;
  min-height: 0;
}
.list {
  flex: 1;
  min-height: 200px;
  overflow-y: auto;
  /* 选中卡片的外圈阴影不被裁掉 */
  margin: -3px;
  padding: 3px;
}
/* 列表里的东西放不下时由列表滚动，不能被压扁（表格外框裁剪了溢出，会被压到列表那么高、下面的行看不到也滚不动） */
.list > * {
  flex-shrink: 0;
}
/* 表格比列表宽时，横向滚动条贴在列表可见区域的底部，不用先滚到表格最下面才能拖。
   贴底（sticky）要求和列表之间没有会滚动的裁剪层：表格这几层的 overflow: hidden 换成 clip（一样裁掉、圆角也在） */
.list :deep(.table-box),
.list :deep(.table-box .el-table),
.list :deep(.table-box .el-table__body-wrapper) {
  overflow: clip;
}
.list :deep(.table-box .el-table__body-wrapper > .el-scrollbar) {
  overflow: visible;
}
.list :deep(.table-box .el-table__body-wrapper .el-scrollbar__bar.is-horizontal) {
  position: sticky;
  bottom: 2px;
  margin: -6px 2px 0;
  z-index: 5;
}
/* 液态玻璃：列表往上延伸到功能切换、工具栏下面（高度 --float-h 由 measureFloat 量出），
   滚动时卡片和行从磨砂的工具栏下面经过，过了工具栏上沿就不再显示（--float-cut） */
.body.float-glass > .workspace-tabs,
.body.float-glass > .toolbar {
  position: relative;
  z-index: 2;
}
/* 上下多留 3px 给磨砂条；左右和原来一样是 3px，不能超出列表和页面（否则会出横向滚动条） */
.body.float-glass > .toolbar {
  padding: 6px 3px;
  margin: -6px -3px;
}
.body.float-glass > .list {
  margin-top: calc(-3px - var(--float-h, 0px));
  padding-top: calc(3px + var(--float-h, 0px));
  scroll-padding-top: var(--float-h, 0px);
  -webkit-mask-image: linear-gradient(to bottom, transparent var(--float-cut, 0px), #000 calc(var(--float-cut, 0px) + 4px));
  mask-image: linear-gradient(to bottom, transparent var(--float-cut, 0px), #000 calc(var(--float-cut, 0px) + 4px));
}
/* 端口、软件页自己的工具栏原来贴在列表顶上（sticky）：现在列表顶在功能切换下面，
   改成贴在功能切换下方、不随滚动移动，实心底换成磨砂玻璃 */
.body.float-glass > .list :deep(.fw-line),
.body.float-glass > .list :deep(.software-toolbar) {
  /* sticky 从列表的上内边距（已含 --float-h）往下算：和负外边距相同，就停在不滚动时的位置 */
  top: -6px;
  padding: 6px 3px;
  margin: -6px -3px;
  border-radius: 14px;
  /* 实色底去掉：底下垫的是和应用页工具栏一样的玻璃条（面板里的 GlassPane .cg-strip，样式见 GlassPane.vue） */
  background: transparent;
}
/* 「正在刷新」那层盖在内容上 */
.body {
  --el-mask-color: color-mix(in srgb, var(--el-bg-color-page) 70%, transparent);
}
/* 列表很长时转圈放在上面，不用往下翻才看得到 */
.body > :deep(.el-loading-mask .el-loading-spinner) {
  top: 60px;
  margin-top: 0;
}
/* ---- 第一行：项目名、服务器切换、右边的操作 */
.head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: nowrap;
  min-width: 0;
}
.compact .head .latency { display: none; }
/* 窄窗口：顶部按钮只留图标 */
.compact .head :deep(.el-button [class*='el-icon'] + span),
.compact .head :deep(.cg-button-label) {
  display: none;
}
.compact .head .cg-button { width: 34px; padding: 0; }
/* 玻璃按钮和这一行的其他控件一样高 */
.head .cg-button { height: 34px; padding-inline: 14px; }
.head .cg-button.cg-icon-only { width: 34px; padding: 0; }
.head h2 {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
  cursor: default;
  max-width: 170px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
/* 项目名胶囊（和按钮一样高）：地址太长时省略，窄窗口时地址回到第二行 */
.title-pill {
  display: inline-flex;
  align-items: center;
  gap: 10px;
  height: 34px;
  padding: 0 16px 0 14px;
  box-sizing: border-box;
  min-width: 0;
  flex-shrink: 1;
}
.title-pill h2 { font-size: 16px; letter-spacing: -.2px; }
.title-addr {
  font-size: 12px;
  color: var(--el-text-color-secondary);
  max-width: 230px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.server-line > .line-addr { display: none; }
.compact .title-addr { display: none; }
.compact .server-line > .line-addr { display: inline; }
.head .el-button + .el-button {
  margin-left: 0;
}
/* 服务器选择和第二行操作共用玻璃胶囊；第二行按钮保持紧凑尺寸。 */
.target-pick { flex-shrink: 0; }
.target-trigger,
.deployment-toggle,
.server-glass-button {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  height: 34px;
  padding: 0 12px 0 14px;
  box-sizing: border-box;
  border: 0;
  background: none;
  font: inherit;
  color: var(--el-text-color-primary);
  cursor: pointer;
}
.target-trigger { max-width: 240px; }
.deployment-toggle,
.server-glass-button { height: 28px; gap: 6px; padding-inline: 10px; font-size: 12px; }
.target-trigger:focus-visible,
.deployment-toggle:focus-visible,
.server-glass-button:focus-visible { outline: 2px solid var(--el-color-primary); outline-offset: 3px; }
.deployment-toggle:focus-visible,
.server-glass-button:focus-visible { outline-offset: -2px; }
.target-name {
  min-width: 0;
  font-size: 13px;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.target-led { width: 8px; height: 8px; flex-shrink: 0; }
.target-led.live { box-shadow: 0 0 0 3px var(--el-color-success-light-8), 0 0 8px var(--el-color-success); }
.target-chevron,
.deployment-chevron { flex-shrink: 0; font-size: 12px; color: var(--el-text-color-secondary); }
/* 下拉菜单挂在 body 上，菜单项不带本组件的 scoped 标记：用全局选择器 */
:global(.target-menu .el-dropdown-menu__item) { gap: 8px; }
.target-menu-name { font-size: 13px; }
.target-menu-host { font-size: 11.5px; color: var(--el-text-color-secondary); }
:global(.target-menu .el-dropdown-menu__item.target-on) { color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
:global(.target-menu .target-on .target-menu-name) { font-weight: 600; }
.tdot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--el-text-color-placeholder);
}
.tdot.live {
  background: var(--el-color-success);
}
/* 正在连接：橙色、一闪一闪 */
.tdot.busy {
  background: var(--el-color-warning);
  animation: tdot-pulse 1s ease-in-out infinite;
}
@keyframes tdot-pulse {
  50% {
    opacity: 0.25;
  }
}
.prod {
  font-family: inherit;
  font-size: 11px;
  padding: 0 5px;
  border-radius: 4px;
  background: var(--el-color-danger);
  color: #fff;
}
.conn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
  white-space: nowrap;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.conn-led {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--el-text-color-placeholder);
}
.conn.on {
  color: var(--el-color-success);
}
.latency {
  font-size: 12px;
  font-variant-numeric: tabular-nums;
  cursor: default;
}
.latency.good {
  color: var(--el-color-success);
}
.latency.fair {
  color: var(--el-color-warning);
}
.latency.poor {
  color: var(--el-color-danger);
}
.conn.on .conn-led {
  background: var(--el-color-success);
  box-shadow: 0 0 0 3px var(--el-color-success-light-8);
}
/* ---- 第二行：服务器信息 */
.server-line {
  display: flex;
  align-items: center;
  min-height: 32px;
  flex-shrink: 0;
  gap: 8px;
  font-size: 12.5px;
  color: var(--el-text-color-secondary);
  white-space: nowrap;
  overflow-x: auto;
  padding: 4px;
  margin: -8px -4px -4px;
}
.server-line > * {
  flex-shrink: 0;
}
.server-line > .el-button {
  height: 28px;
  margin-left: 0;
  padding-inline: 10px;
  --el-button-font-size: 12px;
}
.server-line > .el-button:focus-visible { outline-offset: -2px; }
.read-at {
  color: var(--el-text-color-placeholder);
  cursor: default;
}
.local-on {
  margin-left: 4px;
  padding: 0 4px;
  border-radius: 3px;
  font-size: 11px;
  line-height: 16px;
  color: var(--el-color-success);
  background: var(--el-color-success-light-9);
}
.field {
  cursor: pointer;
  border-bottom: 1px dashed transparent;
}
.field:hover {
  color: var(--el-color-primary);
  border-bottom-color: var(--el-color-primary);
}
.dflt {
  margin-left: 4px;
  font-size: 11px;
  padding: 0 4px;
  border-radius: 3px;
  background: var(--el-fill-color);
  color: var(--el-text-color-secondary);
}
/* ---- 概览数字 */
.stats {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 10px;
  flex-shrink: 0;
}
.stat {
  display: grid;
  grid-template-rows: 18px 29px 18px;
  align-content: center;
  gap: 4px;
  height: 108px;
  box-sizing: border-box;
  min-width: 0;
  padding: 12px 16px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ferry-card-radius);
  background: var(--el-bg-color);
  font: inherit;
  color: inherit;
  text-align: left;
  cursor: pointer;
}
button.stat:hover {
  border-color: var(--el-color-primary-light-5);
}
.stat.static {
  cursor: default;
}
.stat.on {
  border-color: var(--el-color-primary);
  box-shadow: 0 0 0 3px var(--el-color-primary-light-8);
}
.stat:focus-visible {
  outline: 2px solid var(--el-color-primary);
  outline-offset: 1px;
}
/* 标题一行：左边是标题，右上角是「点击…」的提示（和标题同样的颜色） */
.stat-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  width: 100%;
  white-space: nowrap;
}
.stat-hint {
  min-width: 0;
  margin-left: auto;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.need-sel {
  color: var(--el-color-primary);
}
.stat-label {
  font-size: 12px;
  line-height: 18px;
  color: var(--el-text-color-secondary);
}
.stat-num {
  font-size: 22px;
  font-weight: 600;
  line-height: 1.3;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.stat-num.bad {
  color: var(--el-color-danger);
}
/* 还没读到时数字、说明处的占位条：高度和文字一样，读到后格子不跳 */
.stat-wait {
  display: inline-block;
  width: 64px;
  height: 18px;
  vertical-align: middle;
  border-radius: 4px;
  background: var(--el-fill-color);
  animation: stat-wait 1.2s ease-in-out infinite;
}
.stat-wait.sub {
  width: 96px;
  height: 11px;
}
@keyframes stat-wait {
  50% {
    opacity: 0.45;
  }
}
@media (prefers-reduced-motion: reduce) {
  .stat-wait {
    animation: none;
  }
}
.stat-num small {
  font-size: 13px;
  font-weight: 400;
  color: var(--el-text-color-secondary);
}
.stat-sub {
  font-size: 12px;
  line-height: 18px;
  color: var(--el-text-color-secondary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.stat-num.ok {
  color: var(--el-color-success);
}
.stat-num.warn {
  color: var(--el-color-warning);
}
.stat-num.off {
  color: var(--el-text-color-regular);
}
/* ---- 工具栏 */
.toolbar {
  display: flex;
  flex-wrap: nowrap;
  flex-shrink: 0;
  gap: 8px 10px;
  align-items: center;
  white-space: nowrap;
  overflow-x: auto;
  padding: 3px;
  margin: -3px;
}
/* 优先压缩搜索和提示；更窄时收起次要按钮文字，极窄区域横向滚动。 */
.toolbar > * {
  flex-shrink: 0;
}
.toolbar > .spacer {
  flex-shrink: 1;
  min-width: 0;
}
.toolbar > .hint {
  flex-shrink: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
}
.toolbar .el-radio-group {
  flex-wrap: nowrap;
}
.toolbar .el-button + .el-button,
.toolbar .el-button-group + .el-button {
  margin-left: 0;
}
.sep {
  width: 1px;
  height: 18px;
  background: var(--el-border-color);
}
.view-switch :deep(.el-radio-button__inner) {
  padding: 8px 10px;
}
.spacer {
  flex: 1;
}
.hint {
  font-size: 12px;
}
/* ---- 状态：小圆点 + 文字（卡片和列表共用） */
.state {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 1px 8px;
  border-radius: 10px;
  font-size: 12px;
  white-space: nowrap;
}
.state .dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
}
.state.ok {
  background: var(--el-color-success-light-9);
  color: var(--el-color-success-dark-2);
}
.state.ok .dot {
  background: var(--el-color-success);
}
.state.bad {
  background: var(--el-color-danger-light-9);
  color: var(--el-color-danger);
}
.state.bad .dot {
  background: var(--el-color-danger);
}
.state.warn {
  background: var(--el-color-warning-light-9);
  color: var(--el-color-warning-dark-2);
}
.state.warn .dot {
  background: var(--el-color-warning);
}
.state.off {
  background: var(--el-fill-color);
  color: var(--el-text-color-secondary);
}
.state.off .dot {
  background: var(--el-text-color-placeholder);
}
.chip {
  display: inline-block;
  padding: 0 7px;
  border-radius: 5px;
  font-size: 12px;
  line-height: 20px;
  white-space: nowrap;
}
.clickable {
  cursor: pointer;
}
.clickable:hover {
  text-decoration: underline;
}
.chip.warn {
  background: var(--el-color-warning-light-9);
  color: var(--el-color-warning-dark-2);
}
.chip.bad {
  background: var(--el-color-danger-light-9);
  color: var(--el-color-danger);
}
/* ---- 卡片 */
.cards {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
  gap: 12px;
}
.cards-empty {
  grid-column: 1 / -1;
}
.card {
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-width: 0;
  padding: 12px 14px 8px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ferry-card-radius);
  background: var(--el-bg-color);
  cursor: pointer;
  transition: border-color 0.15s, box-shadow 0.15s;
  /* 点卡片是勾选：连点、按住 Shift 点时不要把文字选上（版本号等值仍可以选中复制） */
  user-select: none;
}
.card-fields dd {
  user-select: text;
}
/* 「部署（N）」：勾选数量变化时按钮宽度不变，旁边的按钮不跳 */
.deploy-btn {
  min-width: 124px; /* 放得下「部署（99）」 */
  font-variant-numeric: tabular-nums;
}
@container workspace (max-width: 1100px) {
  .toolbar > .hint { display: none; }
  .stat-hint { display: none; }
}
@container workspace (max-width: 820px) {
  .toolbar { gap: 8px; }
  .app-controls .el-button, .history-btn { padding: 8px 10px; }
  .toolbar .action-label { display: none; }
  .app-controls :deep(.el-icon + span), .history-btn :deep(.el-icon + span) { margin-left: 0; }
  .toolbar > .app-search { flex-basis: 160px; }
  .deploy-btn { min-width: 108px; padding-inline: 10px; }
}
@container workspace (max-width: 620px) {
  .toolbar { gap: 6px; }
  .toolbar > .app-search { flex-basis: 120px; }
  .deploy-btn { min-width: 96px; }
  .app-controls .el-button, .history-btn { padding-inline: 8px; }
  .view-switch :deep(.el-radio-button__inner) { padding-inline: 8px; }
}
/* 功能切换放不下时先去掉图标、只留文字（液滴里那排放大的文字跟着一起去掉） */
@container workspace (max-width: 520px) {
  .workspace-switch :deep(.cg-tab > .el-icon) { display: none; }
}
.card:hover {
  border-color: var(--el-color-primary-light-5);
}
.card.sel {
  border-color: var(--el-color-primary);
  box-shadow: 0 0 0 3px var(--el-color-primary-light-8);
}
.card-top {
  display: flex;
  align-items: center;
  gap: 8px;
}
.card-top .el-checkbox {
  height: auto;
}
.card-name {
  flex: 1;
  min-width: 0;
}
.name {
  font-weight: 600;
}
.card-fields {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 6px 12px;
  margin: 0;
  font-size: 12px;
}
.card-fields .wide {
  grid-column: 1 / -1;
}
.card-fields dt {
  color: var(--el-text-color-secondary);
}
.card-fields dd {
  margin: 1px 0 0;
  color: var(--el-text-color-primary);
}
.card-fields dd.sub {
  color: var(--el-text-color-secondary);
}
.ellipsis {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* 底部一栏始终贴着卡片底边：同一行的卡片高度一样，「日志 · 版本」就在一条线上 */
.card-foot {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px 12px;
  min-height: 32px;
  margin-top: auto;
  padding-top: 6px;
  border-top: 1px solid var(--el-border-color-extra-light);
}
.card-foot .el-button {
  height: 24px;
  padding: 0;
}
/* 卡片里在运行的端口用正常颜色（只听本机的仍有虚线），没运行时配置里的端口才是灰色 */
.card-fields .port.local {
  color: inherit;
}
.card-foot .el-button + .el-button {
  margin-left: 0;
}
.problem {
  flex: 1 1 100%;
  font-size: 12px;
  color: var(--el-color-warning-dark-2);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* ---- 列表 */
.table-box {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ferry-card-radius);
  overflow: hidden;
  background: var(--el-bg-color);
}
.table-box :deep(.el-table th.el-table__cell) {
  background: var(--el-fill-color-lighter);
  font-weight: 500;
  font-size: 12.5px;
}
.table-box :deep(.el-table__inner-wrapper::before) {
  display: none;
}
.commit {
  color: var(--el-color-primary);
}
.own-root {
  display: block;
  font-size: 11px;
  color: var(--el-text-color-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.port {
  display: inline-block;
  margin-right: 6px;
}
.port.extra {
  color: var(--el-text-color-regular);
}
.port.risky {
  color: var(--el-color-warning);
}
.port.local {
  color: var(--el-text-color-secondary);
  border-bottom: 1px dashed var(--el-border-color);
}
.err {
  white-space: pre-wrap;
  margin: 4px 0 0;
}
.trust-btn {
  margin-top: 8px;
}
.ml {
  margin-left: 4px;
}
.status-loading {
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ferry-card-radius);
  padding: 14px 16px;
}
.status-err :deep(.el-alert__content) {
  width: 100%;
}
.small-tip {
  font-size: 12px;
  margin-top: 6px;
}
.machine {
  margin-bottom: 10px;
}
.machine ul {
  margin: 4px 0 0;
  padding-left: 18px;
  line-height: 1.7;
}
</style>
