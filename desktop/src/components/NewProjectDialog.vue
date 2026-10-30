<script setup lang="ts">
// 新建项目向导：选项目目录 → 识别应用（可修改、可手动添加）→ 填服务器（可从已有连接填入、测试连接）→ 预览生成的 ferry.yaml → 创建（可以接着预检）。
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Plus, Delete, FolderOpened, MagicStick, ChatDotRound, CircleCheckFilled, WarningFilled, CircleCloseFilled, Connection as ConnIcon } from '@element-plus/icons-vue'
import { call, ferry, FerryError, type AiDetected, type Detected, type FerryLine, type Inspect } from '../ferry'
import { pickDir, pickFile, pickMany } from '../platform'
import { addProject, appSettings, connections, connectionsPath, loadConnections, localRequest, settingsRequest, trustRequest, secretRequest, trustedTick } from '../state'
import { findModelByName, usageText } from '../ai'

const emit = defineEmits<{ close: []; created: [path: string] }>()
const visible = ref(true)
const step = ref(0)

// ---------------------------------------------------------------- 第 1 步：目录

const dir = ref('')
const project = ref('')
const configPath = ref('')
const editConfig = ref(false)
const configExists = ref(false)
const detecting = ref(false)
const detected = ref<Detected>()
const detectError = ref('')

interface AppForm {
  include: boolean
  name: string
  type: AppType
  lastType: AppType // 改类型时用来重置相关设置
  kind: string
  notes: string[]
  workdir: string
  command: string
  env: Record<string, string>
  artifact: string
  /** 附带文件：本机路径（相对项目目录）→ 放到服务器上叫什么（空 = 同名） */
  files: { from: string; to: string; exec: boolean }[]
  /** 识别到的、看着像配置的文件（还没加进附带文件的才显示） */
  configFiles: string[]
  root: string
  sharedBuild: boolean
  buildGroup: string
  groupCommand: string
  module: string
  unit: string
  java: string
  jvmArgs: string // 一行一个
  args: string // 一行一个
  port?: number
  needJava?: number
  javaSource: 'ferry' | 'server' // JDK 由 Ferry 提供，还是用服务器上已有的
  runtimeJava: number
  more: boolean // 展开「更多设置」
  health: 'tcp' | 'http' | 'none'
  healthUrl: string
  fileLogs: boolean
  link: string
  reload: string
  // 前端：绑定软件中已安装的 nginx，或只部署文件后手动接入。
  serve: 'ferry' | 'server'
  domains?: string
  proxies: { path: string; to: string }[]
  spa: boolean
  // python、node、dotnet：启动命令（一行一个参数）、服务的环境变量（一行一个 KEY=值）、不上传的（一行一个）、装依赖的镜像源
  exec: string
  svcEnv: string
  exclude: string
  registry: string
}

type AppType = 'java' | 'binary' | 'static' | 'python' | 'node' | 'dotnet'
/** 上传整个目录、由启动命令运行的类型 */
const isDirApp = (t: AppType) => t === 'python' || t === 'node' || t === 'dotnet'
/** 手动添加、换类型时的默认启动命令 */
const defaultExec: Record<string, string> = {
  python: '${running}/.venv/bin/uvicorn\nmain:app\n--host\n127.0.0.1\n--port\n8000',
  node: 'node\n${running}/dist/main.js',
  dotnet: '${running}/MyApp',
}
const defaultPort: Record<string, number> = { python: 8000, node: 3000, dotnet: 5000 }

// 前端由 Ferry 的 nginx 运行时的默认端口（多个前端依次加 1）
const defaultWebPort = 8081
const proxyHint = 'http://127.0.0.1:8080'

/** 测试连接的结果（ferry probe） */
interface ProbeCheck {
  level: 'ok' | 'warn' | 'error'
  text: string
  /** create-user：可以现在创建运行用户；goarch：可以把 Go 程序改成服务器的架构 */
  fix?: string
}
interface ProbeResult {
  os: string
  arch: string
  kernel: string
  priv: 'root' | 'sudo' | 'none'
  memMB: number
  freeMB: number
  javas: { path: string; version: string; major: number }[] | null
  nginx: string
  nginxInstances?: {name: string; title?: string; version?: string; unit: string; readyPort: number; hosting?: {system?: boolean; sites: string}}[]
  runAsOK: boolean
  checks: ProbeCheck[]
}

/** 预检结果（ferry preflight，只用到的字段） */
interface Preflight {
  os: string
  arch: string
  systemd: number
  rootFreeMB: number
  problems: string[] | null
  warnings: string[] | null
  fixable?: string[] | null
}

interface TargetForm {
  nginxInstance?: string
  name: string
  host: string
  port: number
  user: string
  sudo: boolean
  auth: 'key' | 'password'
  identityFile: string
  root: string
  runAs: string
  arch: '' | 'amd64' | 'arm64'
  ferryDir: string
  /** 从哪个已有连接填入（沿用它固定的主机公钥） */
  connection: string
  more: boolean
  probing: boolean
  probe?: ProbeResult
  probeError: string
  /** 测试时的设置；之后改过就提示重新测试 */
  probedKey: string
  creatingUser: boolean
  // 创建后的预检
  checking: boolean
  preflight?: Preflight
  preflightError: string
}

const apps = ref<AppForm[]>([])
const targets = ref<TargetForm[]>([])
/** 第 2 步展开的应用（有提醒、没填端口的默认展开） */
const openApps = ref<number[]>([])

async function chooseDir() {
  const d = await pickDir('选择项目目录（代码所在的目录）')
  if (d) {
    dir.value = d
    await detect()
  }
}

function appForm(s: Detected['apps'][number]): AppForm {
  return {
    include: true,
    name: s.name,
    type: s.type,
    lastType: s.type,
    kind: s.ai ? `AI 识别 · ${s.kind}` : s.kind,
    // AI 识别的：把它的判断依据放在提醒的最前面，提醒用户核对
    notes: [...(s.ai ? [`AI 识别的，可能看错：请核对构建命令、产物路径和端口。${s.reason ? '判断依据：' + s.reason : ''}`] : []), ...(s.notes ?? [])],
    workdir: s.workdir,
    command: s.command,
    env: s.env ?? {},
    artifact: s.artifact,
    files: [],
    configFiles: s.configFiles ?? [],
    root: '',
    sharedBuild: !!s.buildGroup,
    buildGroup: s.buildGroup ?? '',
    groupCommand: s.groupCommand ?? '',
    module: s.module ?? '',
    unit: '',
    java: '/usr/bin/java',
    jvmArgs: s.type === 'java' ? '-Xms256m\n-Xmx512m' : '',
    args: '',
    port: s.type === 'static' ? undefined : s.port || undefined,
    needJava: s.java || undefined,
    javaSource: 'ferry',
    runtimeJava: s.java && s.java > 8 ? s.java : 17,
    more: false,
    health: s.port ? 'tcp' : 'none',
    healthUrl: s.port ? `http://127.0.0.1:${s.port}/` : '',
    fileLogs: s.type === 'java',
    link: '',
    reload: '',
    serve: 'ferry',
    proxies: Object.entries(s.proxy ?? {}).map(([path, to]) => ({ path, to })),
    spa: true,
    exec: (s.exec ?? []).join('\n'),
    svcEnv: Object.entries(s.serviceEnv ?? {}).map(([k, v]) => `${k}=${v}`).join('\n'),
    exclude: (s.exclude ?? []).join('\n'),
    registry: '',
  }
}

async function detect() {
  if (!dir.value.trim()) return
  detecting.value = true
  detectError.value = ''
  aiSummary.value = ''
  try {
    await applyDetected(await call<Detected>(['detect', dir.value.trim()]))
  } catch (e) {
    detectError.value = (e as Error).message
    detected.value = undefined
  } finally {
    detecting.value = false
  }
}

/** 用识别结果（规则或 AI 的）填好第 2 步的应用 */
async function applyDetected(d: Detected) {
  detected.value = d
  dir.value = d.dir
  project.value = d.project
  configPath.value = d.dir.replace(/\/$/, '') + '/ferry.yaml'
  editConfig.value = false
  apps.value = d.apps.map(appForm)
  // 前端各用一个端口（不和后端、彼此重复），接口转发指向本项目里的后端
  for (const a of apps.value) if (a.type === 'static') a.port = nextWebPort(a)
  linkProxies()
  openApps.value = apps.value.flatMap((a, i) => (needsLook(a) ? [i] : []))
  await checkConfig()
}

// ---------------------------------------------------------------- AI 识别（ferry detect --ai）

const aiOpen = ref(false)
const aiHint = ref('')
const aiModelId = ref('')
const aiRunning = ref(false)
const aiLog = ref<{ kind: 'read' | 'say'; text: string }[]>([])
const aiError = ref('')
const aiSummary = ref('')
const aiUsage = ref('')
let aiKill: (() => Promise<void>) | undefined
let aiStopped = false
const aiModels = computed(() => appSettings.value?.ai?.models ?? [])
const aiModel = computed(
  () => aiModels.value.find((m) => m.id === aiModelId.value) ?? aiModels.value.find((m) => m.id === appSettings.value?.ai?.default) ?? aiModels.value[0],
)
const aiLogBox = ref<HTMLElement>()

function onAiLine(l: FerryLine) {
  if (l.kind !== 'ai') return
  const ev = l as FerryLine & { type: string; text?: string; call?: { name: string }; result?: string; isError?: boolean }
  const last = aiLog.value[aiLog.value.length - 1]
  if (ev.type === 'output' && ev.text) aiLog.value.push({ kind: 'read', text: ev.text })
  else if (ev.type === 'text' && ev.text?.trim()) aiLog.value.push({ kind: 'say', text: ev.text.trim() })
  // 没让 AI 看的（.env、私钥、项目外的路径……）在那一行后面注明
  else if (ev.type === 'result' && ev.isError && ev.call?.name !== 'submit_apps' && last?.kind === 'read')
    last.text += `（没给 AI 看：${(ev.result ?? '').split('\n')[0]}）`
  else if (ev.type === 'result' && ev.isError && ev.call?.name === 'submit_apps') aiLog.value.push({ kind: 'read', text: '交回的结果有问题，让 AI 改' })
  else return
  setTimeout(() => aiLogBox.value?.scrollTo({ top: aiLogBox.value.scrollHeight }))
}

async function aiDetect() {
  const m = aiModel.value
  if (!dir.value.trim() || !m || aiRunning.value) return
  aiRunning.value = true
  aiStopped = false
  aiLog.value = []
  aiError.value = ''
  aiSummary.value = ''
  aiUsage.value = ''
  const env: Record<string, string> = aiHint.value.trim() ? { FERRY_AI_MESSAGE: aiHint.value.trim() } : {}
  const run = ferry<AiDetected>(['detect', dir.value.trim(), '--ai', '--model', m.id], onAiLine, env)
  aiKill = run.kill
  const r = await run.done
  aiKill = undefined
  aiRunning.value = false
  if (r.data?.usage && r.data.usage.input + r.data.usage.output > 0) {
    aiUsage.value = usageText(r.data.usage, findModelByName(aiModels.value, r.data.model ?? '') ?? m)
  }
  if (aiStopped) {
    aiError.value = '已停止，识别结果没有变'
    return
  }
  if (!r.ok || !r.data) {
    aiError.value = r.error || 'AI 识别失败'
    return
  }
  detectError.value = ''
  await applyDetected(r.data)
  aiSummary.value = r.data.summary || `找到 ${r.data.apps.length} 个应用`
}

async function aiStop() {
  aiStopped = true
  await aiKill?.()
}

function openAiSettings() {
  settingsRequest.value = 'ai'
}

/** 配置文件的位置是不是已经有文件了（有就不能创建，可以直接添加它） */
async function checkConfig() {
  configExists.value = false
  if (!/\.ya?ml$/i.test(configPath.value)) return
  try {
    const r = await call<{ exists: boolean }>(['create', '-c', configPath.value, '--check'])
    configExists.value = r.exists
  } catch {
    // 检查不了时到最后一步再说
  }
}

async function addExisting(p: string) {
  if (!p) return
  try {
    const info = await call<Inspect>(['inspect', '-c', p])
    await addProject({ path: p, name: info.project })
    if (info.problems?.length) localRequest.value = p // 这台电脑上缺私钥、源码目录：打开本机设置
  } catch {
    await addProject({ path: p, name: project.value || 'project' })
  }
  ElMessage.success('已添加')
  emit('created', p)
  visible.value = false
}

function addApp(type: AppForm['type']) {
  const a: AppForm = {
    include: true, name: type === 'static' ? 'web' : 'app', type, lastType: type, kind: '手动添加', notes: [], workdir: '.', command: '', env: {},
    artifact: type === 'static' ? 'dist' : type === 'java' ? 'target/app.jar' : type === 'dotnet' ? 'build/publish' : isDirApp(type) ? '.' : 'build/app', files: [], configFiles: [], root: '', sharedBuild: false,
    buildGroup: '', groupCommand: '', module: '', unit: '', java: '/usr/bin/java', jvmArgs: type === 'java' ? '-Xms256m\n-Xmx512m' : '',
    args: '', port: undefined, needJava: undefined, javaSource: 'ferry', runtimeJava: 17, more: false, health: 'none', healthUrl: '', fileLogs: type === 'java', link: '', reload: '',
    serve: 'ferry', proxies: [], spa: true, exec: defaultExec[type] ?? '', svcEnv: '', exclude: '', registry: '',
  }
  if (type === 'dotnet') a.command = 'dotnet publish -c Release -r linux-x64 --self-contained true -o build/publish'
  if (isDirApp(type)) {
    a.port = defaultPort[type]
    a.health = 'tcp'
  }
  if (type === 'static') a.port = nextWebPort(a)
  apps.value.push(a)
  openApps.value = [...openApps.value, apps.value.length - 1]
}

// ---------------------------------------------------------------- 第 2 步：应用

/** 这个应用在服务器上要占一个端口 */
const usesPort = (a: AppForm) => a.type !== 'static' || a.serve === 'ferry'

/** 前端的下一个空闲端口：从 8081 起，跳过其他应用已用的 */
function nextWebPort(self: AppForm): number {
  const used = new Set(apps.value.filter((a) => a !== self && usesPort(a) && a.port).map((a) => a.port))
  let p = defaultWebPort
  while (used.has(p)) p++
  return p
}

/** 有后端端口的应用（接口转发可以指向它们） */
const backends = computed(() => apps.value.filter((a) => a.include && a.type !== 'static' && a.port))

const reLocal = /^(https?:\/\/)(?:localhost|127\.0\.0\.1|0\.0\.0\.0)(?::(\d+))?(\/.*)?$/

/** 把转发地址的端口换成某个后端的（保留末尾的路径，它决定是否去掉前缀） */
function proxyTo(to: string, port: number): string {
  const m = reLocal.exec(to.trim())
  return `http://127.0.0.1:${port}${m?.[3] ?? ''}`
}

/** 识别出的接口转发来自开发配置（通常是 localhost:8080）：本项目只有一个后端时直接指向它 */
function linkProxies() {
  const bs = backends.value
  for (const w of apps.value.filter((a) => a.type === 'static')) {
    for (const p of w.proxies) {
      const m = reLocal.exec(p.to.trim())
      if (!m || !bs.length) continue
      const port = Number(m[2] || 80)
      if (bs.some((b) => b.port === port)) continue
      if (bs.length === 1) {
        p.to = proxyTo(p.to, bs[0].port!)
        w.notes.push(`接口转发 ${p.path} 改成了指向本项目的 ${bs[0].name}（端口 ${bs[0].port}）`)
      } else {
        w.notes.push(`接口转发 ${p.path} 指向的端口 ${port} 不是本项目里任何应用的端口：用右边的「指向」选一个后端`)
      }
    }
  }
}

/** 要显示的提醒：用 Ferry 提供的 JDK 且版本够用时，「需要 Java N」的提醒就不用看了 */
function visibleNotes(a: AppForm): string[] {
  return a.notes.filter((n) => !(n.startsWith('需要 Java') && a.javaSource === 'ferry' && a.runtimeJava >= (a.needJava ?? 0)))
}

const needsLook = (a: AppForm) => visibleNotes(a).length > 0 || (a.type !== 'static' && !a.port)

function onTypeChange(a: AppForm) {
  const prev = a.lastType
  a.lastType = a.type
  if (a.type === 'static') {
    if (/\.jar$/i.test(a.artifact) || a.artifact.startsWith('build/')) a.artifact = 'dist'
    a.serve = 'ferry'
    a.port = nextWebPort(a)
  } else {
    if (prev === 'static') a.port = undefined
    if (a.type === 'java' && !/\.jar$/i.test(a.artifact)) a.artifact = 'target/app.jar'
    if (a.type === 'binary' && (/\.jar$/i.test(a.artifact) || /(^|\/)dist$/.test(a.artifact))) a.artifact = 'build/app'
    if (isDirApp(a.type) && (/\.jar$/i.test(a.artifact) || a.artifact.startsWith('build/app'))) a.artifact = a.type === 'dotnet' ? 'build/publish' : '.'
    if (isDirApp(a.type) && !a.exec.trim()) a.exec = defaultExec[a.type]
    if (isDirApp(a.type) && !a.port) a.port = defaultPort[a.type]
  }
  a.jvmArgs = a.type === 'java' ? a.jvmArgs || '-Xms256m\n-Xmx512m' : ''
  a.fileLogs = a.type === 'java'
  onPortChange(a)
}

/** 填了端口默认按端口判断启动成功，清空端口就改成看进程；启动命令、环境变量里的旧端口跟着改 */
function onPortChange(a: AppForm, old?: number) {
  if (a.type === 'static') return
  if (old && a.port && old !== a.port && isDirApp(a.type)) {
    const re = new RegExp(`(^|[^0-9])${old}(?![0-9])`, 'g')
    a.exec = a.exec.replace(re, `$1${a.port}`)
    a.svcEnv = a.svcEnv.replace(re, `$1${a.port}`)
  }
  if (a.port && a.health === 'none') a.health = 'tcp'
  if (!a.port && a.health === 'tcp') a.health = 'none'
  if (a.port && (!a.healthUrl || /^http:\/\/127\.0\.0\.1:\d*\/$/.test(a.healthUrl))) a.healthUrl = `http://127.0.0.1:${a.port}/`
}

// ---------------------------------------------------------------- 第 3 步：服务器

function newTarget(): TargetForm {
  return {
    name: targets.value.length ? `server-${targets.value.length + 1}` : 'prod', host: '', port: 22, user: 'root', sudo: false,
    auth: 'key', identityFile: '', root: `/opt/${project.value || 'myapp'}`, runAs: project.value || 'myapp', arch: '',
    ferryDir: '/usr/local/lib/ferry', connection: '', more: false, probing: false, probeError: '', probedKey: '', creatingUser: false,
    checking: false, preflightError: '',
  }
}

const included = computed(() => apps.value.filter((a) => a.include))
const needsRunAs = computed(() => included.value.some((a) => a.type !== 'static'))
const needsNginx = computed(() => included.value.some((a) => a.type === 'static' && a.serve === 'ferry'))

onMounted(() => {
  loadConnections().catch(() => {})
})

const connOf = (name: string) => connections.value.find((c) => c.name === name)

/** 从已有连接填入地址、登录方式；私钥的相对路径按连接文件所在目录换算 */
function fillFromConnection(t: TargetForm, name: string) {
  const c = connOf(name)
  t.connection = name
  if (!c) return
  t.host = c.host
  t.port = c.port || 22
  t.user = c.user || 'root'
  t.auth = c.auth
  t.sudo = t.user !== 'root'
  let id = c.identityFile
  if (id && !/^([A-Za-z]:[\\/]|[\\/]|~)/.test(id)) id = connectionsPath.value.replace(/[\\/][^\\/]*$/, '') + '/' + id
  t.identityFile = id
}

/** 连接固定的主机公钥：地址、端口、用户都没改过时才沿用 */
function pinnedKey(t: TargetForm): string {
  const c = connOf(t.connection)
  return c && c.host === t.host.trim() && (c.port || 22) === t.port && (c.user || 'root') === (t.user || 'root') ? c.hostKey : ''
}

const portList = () => included.value.filter((a) => a.type !== 'static' && usesPort(a) && a.port).map((a) => ({ app: a.name, port: a.port! }))

const targetKey = (t: TargetForm) =>
  JSON.stringify([t.name, t.host, t.port, t.user, t.auth, t.identityFile, t.root, needsRunAs.value ? t.runAs : '', t.ferryDir, portList()])

/** 测试连接用的临时配置（ferry probe prepare 写的） */
const probePath = ref('')
let pendingTrust: (() => void) | null = null

watch(trustedTick, () => {
  const f = pendingTrust
  pendingTrust = null
  f?.()
})
// 没记录就关掉了核对窗口：不再等（记录时先触发上面的 trustedTick，再关窗口）
watch(trustRequest, (v) => {
  if (!v) pendingTrust = null
})

/** 连接、核对主机公钥、输入密码都失败时：弹出对应的对话框，处理后重新执行 */
function handleConnError(e: unknown, config: string, target: string, retry: () => void): string {
  const err = e as FerryError
  if (err instanceof FerryError && err.unknownHost) {
    pendingTrust = retry
    trustRequest.value = { target, config }
    return '第一次连接这台服务器：请先核对主机公钥'
  }
  if (err instanceof FerryError && err.needSecret && err.detail) {
    secretRequest.value = { detail: err.detail, config, retry }
    return err.detail.kind.endsWith('rejected') ? '密码不对，请重新输入' : '需要输入密码'
  }
  return err.message
}

async function probe(t: TargetForm) {
  t.probing = true
  t.probeError = ''
  const key = targetKey(t)
  try {
    const prep = await call<{ path: string }>(['probe', 'prepare'], {
      FERRY_PROBE_VALUE: JSON.stringify({
        projectDir: dir.value,
        targets: targets.value.map((x) => ({
          name: x.name, host: x.host.trim(), port: x.port, user: x.user || 'root', auth: x.auth, identityFile: x.identityFile.trim(), connection: x.connection,
        })),
      }),
    })
    probePath.value = prep.path
    const r = await call<ProbeResult>(['probe', t.name, '-c', prep.path], {
      FERRY_PROBE_VALUE: JSON.stringify({ root: t.root, runAs: needsRunAs.value ? t.runAs : '', ferryDir: t.ferryDir, ports: portList() }),
    })
    t.probe = r
    if (r.nginxInstances?.length === 1 && !t.nginxInstance) t.nginxInstance = r.nginxInstances[0].name
    t.probedKey = key
    if (r.priv === 'sudo' && t.user !== 'root') t.sudo = true
  } catch (e) {
    t.probe = undefined
    t.probeError = handleConnError(e, probePath.value, t.name, () => probe(t))
  } finally {
    t.probing = false
  }
}

async function createUser(t: TargetForm) {
  t.creatingUser = true
  try {
    await call(['probe', t.name, '-c', probePath.value, '--create-user', t.runAs])
    ElMessage.success(`已创建运行用户 ${t.runAs}`)
    await probe(t)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    t.creatingUser = false
  }
}

/** 服务器的检查结果，加上和向导里应用设置有关的几条 */
function allChecks(t: TargetForm): ProbeCheck[] {
  const p = t.probe
  if (!p) return []
  const out = [...p.checks]
  const javas = p.javas ?? []
  for (const a of included.value) {
    if (a.type === 'java' && a.javaSource === 'server') {
      const j = javas.find((j) => j.path === a.java.trim())
      const found = javas.length ? `找到的有：${javas.map((j) => `${j.path}（Java ${j.major || j.version}）`).join('、')}` : '服务器上没有找到 java'
      if (!j) out.push({ level: 'warn', text: `${a.name}：服务器上没有 ${a.java}。${found}；也可以改用「由 Ferry 提供」的 JDK` })
      else if (a.needJava && j.major && j.major < a.needJava) out.push({ level: 'error', text: `${a.name} 需要 Java ${a.needJava}，${j.path} 是 Java ${j.major}。${found}` })
      else out.push({ level: 'ok', text: `${a.name}：${j.path} 是 Java ${j.major || j.version}` })
    }
    if (a.type === 'static' && a.serve === 'server' && !p.nginx) {
      out.push({ level: 'warn', text: `${a.name} 选了「用服务器上已有的 nginx」，但服务器上没有 nginx：可以改成由 Ferry 自带的 nginx 运行` })
    }
    const goarch = a.env.GOARCH
    if (a.type === 'binary' && goarch && (p.arch === 'amd64' || p.arch === 'arm64') && goarch !== p.arch) {
      out.push({ level: 'warn', text: `${a.name} 按 ${goarch} 编译，服务器是 ${p.arch}`, fix: 'goarch' })
    }
  }
  return out
}

function fixGoarch(t: TargetForm) {
  for (const a of included.value) if (a.type === 'binary' && a.env.GOARCH) a.env.GOARCH = t.probe!.arch
  ElMessage.success(`已把 Go 程序的 GOARCH 改成 ${t.probe!.arch}`)
}

const probeErrors = (t: TargetForm) => allChecks(t).filter((c) => c.level === 'error').length
const probeStale = (t: TargetForm) => !!t.probe && t.probedKey !== targetKey(t)

// ---------------------------------------------------------------- 校验与预览

const reProject = /^[a-z0-9][a-z0-9._-]*$/
const reName = /^[A-Za-z0-9][A-Za-z0-9._-]*$/

const stepError = computed(() => {
  if (step.value === 0) {
    if (aiRunning.value) return 'AI 正在识别，等它看完或点「停止」'
    if (!detected.value) return '先选择项目目录'
    if (!reProject.test(project.value)) return '项目名只能是小写字母、数字和 . _ -，且以字母或数字开头'
    if (!/\.ya?ml$/i.test(configPath.value)) return '配置文件要以 .yaml 或 .yml 结尾'
    if (configExists.value) return '配置文件的位置已经有文件了：直接添加它，或换一个位置'
  }
  if (step.value === 1) {
    if (!included.value.length) return '至少选择一个应用'
    const names = new Set<string>()
    for (const a of included.value) {
      if (!reName.test(a.name)) return `应用名「${a.name}」只能包含字母、数字和 . _ -`
      if (names.has(a.name.toLowerCase())) return `应用名「${a.name}」重复`
      names.add(a.name.toLowerCase())
      if (!a.artifact.trim()) return `${a.name}：请填写或选择产物`
      if (a.type !== 'static') {
        const tos = new Set<string>()
        for (const f of a.files) {
          if (!f.from.trim()) continue
          const to = fileTo(f)
          const top = to.split('/')[0]
          if (!to || to.startsWith('/') || to.includes('\\') || to.split('/').some((x) => x === '' || x === '.' || x === '..'))
            return `${a.name}：附带文件「${f.from}」放到的位置要写程序所在目录下的相对路径，如 config/application.yml`
          if (top === baseName(a.artifact)) return `${a.name}：附带文件「${to}」和程序文件同名`
          if (reservedTop.includes(top))
            return `${a.name}：附带文件不能放到「${to}」：${top === 'logs' ? 'logs 是程序运行日志的目录' : top + ' 是 Ferry 自己用的名字'}`
          for (const o of tos) if (o === to || to.startsWith(o + '/') || o.startsWith(to + '/')) return `${a.name}：附带文件「${to}」和「${o}」放到了同一个位置`
          tos.add(to)
        }
      }
      if (a.type === 'static' && a.serve === 'ferry') {
        if (!a.port) return `${a.name}：请填写网站的端口`
        for (const p of a.proxies) {
          if (!p.path.trim() && !p.to.trim()) continue
          if (!/^\/[A-Za-z0-9._~/-]*$/.test(p.path.trim())) return `${a.name}：接口转发的路径要以 / 开头，如 /api/`
          if (!/^https?:\/\/[^\s]+$/.test(p.to.trim())) return `${a.name}：接口转发的后端要写 http://地址:端口，如 http://127.0.0.1:8080`
        }
        continue
      }
      if (a.health === 'http' && !/^https?:\/\//.test(a.healthUrl)) return `${a.name}：健康检查地址要以 http:// 开头`
      if (a.health === 'tcp' && !a.port) return `${a.name}：按端口检查健康时请填写端口`
    }
    const used = new Map<number, string>()
    for (const a of included.value) {
      if (!usesPort(a) || !a.port) continue
      if (a.type === 'static' && a.serve === 'ferry' && a.domains?.trim()) continue
      const other = used.get(a.port)
      if (other) return `${other} 和 ${a.name} 都用端口 ${a.port}：同一台服务器上要用不同的端口`
      used.set(a.port, a.name)
    }
  }
  if (step.value === 2) {
    if (!targets.value.length) return '至少添加一台服务器'
    const names = new Set<string>()
    for (const t of targets.value) {
      if (!reName.test(t.name)) return `服务器名称「${t.name}」只能包含字母、数字和 . _ -`
      if (names.has(t.name)) return `服务器名称「${t.name}」重复`
      names.add(t.name)
      if (!t.host.trim()) return `${t.name}：请填写服务器地址`
      if (needsNginx.value && (!t.probe || probeStale(t))) return `${t.name}：请先测试连接，读取已安装的 nginx 实例`
      if (needsNginx.value && !t.probe?.nginxInstances?.some((n) => n.name === t.nginxInstance)) return `${t.name}：未找到可接入的 nginx，请查看检测结果，或在「软件」中安装独立实例后重新测试`
      if (!t.root.startsWith('/')) return `${t.name}：部署目录必须是绝对路径，如 /opt/${project.value}`
      if (needsRunAs.value && (!t.runAs || t.runAs === 'root')) return `${t.name}：后端服务需要一个非 root 的运行用户`
    }
  }
  return ''
})

const preview = ref('')
const problems = ref<string[]>([])
const exists = ref(false)
const previewing = ref(false)
const creating = ref(false)
/** 创建好的配置文件；创建后留在这一页，可以接着预检 */
const createdPath = ref('')

const lines = (s: string) =>
  s
    .split('\n')
    .map((x) => x.trim())
    .filter(Boolean)

// ---------------------------------------------------------------- 附带文件

/** 版本目录里归 Ferry 用的名字（和 proto.CheckExtraFile 一致） */
const reservedTop = ['logs', 'www', '.ferry', 'release.json', 'unit.service', 'nginx.conf', '.failed', '.libs-seeded', '.logruns']
const baseName = (p: string) => p.replace(/[\\/]+$/, '').split(/[\\/]/).pop() ?? ''
/** 放到服务器上的位置：不写 = 本机文件名 */
const fileTo = (f: { from: string; to: string }) => f.to.trim().replace(/\/+$/, '') || baseName(f.from.trim())

/** 项目目录里的路径写成相对项目目录的（用 /） */
function projectRel(p: string): string {
  const norm = (s: string) => s.replace(/\\/g, '/').replace(/\/+$/, '')
  const root = norm(dir.value)
  const full = norm(p)
  if (root && full.toLowerCase().startsWith(root.toLowerCase() + '/')) return full.slice(root.length + 1)
  return full
}

// 产物：网站选目录，jar 和程序选文件（现成的产物常常不在项目目录里，选了就写完整路径）
async function pickArtifact(a: AppForm) {
  const picked =
    a.type === 'static'
      ? await pickDir('选择网站目录（里面要有 index.html）')
      : a.type === 'java'
        ? await pickFile('选择可执行 jar', 'jar', ['jar'])
        : isDirApp(a.type)
          ? await pickDir('选择要上传的目录')
          : await pickFile('选择 Linux 可执行文件', '', [])
  if (picked) a.artifact = projectRel(picked)
}

// 不构建时说「现成的」，否则说「构建出的」
function artifactHint(a: AppForm) {
  const made = (a.sharedBuild && a.buildGroup) || a.command.trim() ? '构建出的' : '现成的'
  const what =
    a.type === 'static'
      ? '网站目录（里面要有 index.html）'
      : a.type === 'java'
        ? '可执行 jar'
        : a.type === 'dotnet'
          ? 'dotnet publish 输出的目录'
          : isDirApp(a.type)
            ? `目录（整个上传，${a.type === 'python' ? '.venv、__pycache__' : 'node_modules'}、.git 不传）`
            : 'Linux 可执行文件'
  return `${made}${what}；可以是项目目录里的相对路径，也可以是完整路径`
}

async function addFiles(a: AppForm, directory: boolean) {
  const picked = await pickMany(directory ? '选择要附带的文件夹' : '选择要附带的文件', directory)
  for (const p of picked) a.files.push({ from: projectRel(p), to: '', exec: false })
}

const fileCandidates = (a: AppForm) => a.configFiles.filter((c) => !a.files.some((f) => f.from.trim() === c))

function proxyMap(a: AppForm): Record<string, string> {
  const m: Record<string, string> = {}
  for (const p of a.proxies) if (p.path.trim() && p.to.trim()) m[p.path.trim()] = p.to.trim()
  return m
}

function spec() {
  return {
    projectDir: dir.value,
    project: project.value,
    targets: targets.value.map((t) => ({
      name: t.name, host: t.host.trim(), port: t.port, user: t.user || 'root', sudo: t.user !== 'root' && t.sudo, auth: t.auth,
      identityFile: t.identityFile.trim(), root: t.root, runAs: t.runAs, arch: t.arch, ferryDir: t.ferryDir, hostKey: pinnedKey(t), nginxInstance: needsNginx.value ? t.nginxInstance : '',
    })),
    apps: included.value.map((a) => ({
      name: a.name, type: a.type, workdir: a.workdir, command: a.command, env: a.env, artifact: a.artifact, root: a.root.trim(),
      files: a.type === 'static' || isDirApp(a.type) ? [] : a.files.filter((f) => f.from.trim()).map((f) => ({ from: f.from.trim(), to: f.to.trim(), exec: f.exec })),
      sharedBuild: a.sharedBuild && !!a.buildGroup, buildGroup: a.buildGroup, groupCommand: a.groupCommand, module: a.module,
      unit: a.unit.trim(), java: a.type === 'java' && a.javaSource === 'server' ? a.java.trim() : '',
      runtimeJava: a.type === 'java' && a.javaSource === 'ferry' ? a.runtimeJava : 0, jvmArgs: a.type === 'java' ? lines(a.jvmArgs) : [],
      args: lines(a.args), port: a.type === 'static' && a.serve !== 'ferry' ? 0 : a.port ?? 0,
      health: a.type === 'static' ? 'none' : a.health, healthUrl: a.healthUrl.trim(), fileLogs: a.type === 'java' && a.fileLogs,
      link: a.type === 'static' && a.serve === 'server' ? a.link.trim() : '', reload: a.type === 'static' && a.serve === 'server' ? a.reload.trim() : '',
      proxy: a.type === 'static' && a.serve === 'ferry' ? proxyMap(a) : {}, noSpa: a.type === 'static' && a.serve === 'ferry' && !a.spa,
      nginxInstance: a.type === 'static' && a.serve === 'ferry' ? '${nginxInstance}' : '', domains: a.domains?.split(/[,\s]+/).filter(Boolean) ?? [],
      exec: isDirApp(a.type) ? lines(a.exec) : [], exclude: isDirApp(a.type) ? lines(a.exclude) : [],
      serviceEnv: isDirApp(a.type) ? envMap(a.svcEnv) : {}, registry: a.type === 'python' || a.type === 'node' ? a.registry.trim() : '',
    })),
  }
}

interface CreateResult {
  path: string
  content: string
  exists: boolean
  problems?: string[]
}

async function loadPreview() {
  previewing.value = true
  try {
    const r = await call<CreateResult>(['create', '-c', configPath.value, '--dry-run'], { FERRY_CREATE_VALUE: JSON.stringify(spec()) })
    preview.value = r.content
    problems.value = r.problems ?? []
    exists.value = r.exists
  } catch (e) {
    preview.value = ''
    problems.value = [(e as Error).message]
  } finally {
    previewing.value = false
  }
}

async function next() {
  if (stepError.value) return
  if (step.value === 1 && !targets.value.length) targets.value.push(newTarget())
  step.value++
  if (step.value === 3) await loadPreview()
}

async function create() {
  creating.value = true
  try {
    const r = await call<CreateResult>(['create', '-c', configPath.value], { FERRY_CREATE_VALUE: JSON.stringify(spec()) })
    await addProject({ path: r.path, name: project.value })
    createdPath.value = r.path
    emit('created', r.path)
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    creating.value = false
  }
}

/** 创建后：对一台服务器预检（会装上 Ferry 的 agent，不改动应用） */
async function preflight(t: TargetForm) {
  t.checking = true
  t.preflightError = ''
  try {
    t.preflight = await call<Preflight>(['preflight', t.name, '-c', createdPath.value])
  } catch (e) {
    t.preflight = undefined
    t.preflightError = handleConnError(e, createdPath.value, t.name, () => preflight(t))
  } finally {
    t.checking = false
  }
}

const typeLabel: Record<AppForm['type'], string> = {
  java: 'Java（jar）',
  binary: '原生程序',
  static: '前端静态站点',
  python: 'Python',
  node: 'Node 服务端',
  dotnet: '.NET',
}

const registryHint: Record<string, string> = { python: '可选，如 https://mirrors.aliyun.com/pypi/simple', node: '可选，如 https://registry.npmmirror.com' }
const depsHow: Record<string, string> = { python: 'venv + pip install -r requirements.txt', node: 'npm ci' }
const execPlaceholder = '一行一个参数；${running} 是服务器上运行中的版本目录'
const execHint: Record<string, string> = {
  python: '第一项是程序：${running}/.venv/bin/ 下是服务器上装好依赖的 venv（uvicorn、gunicorn 写在 requirements.txt 里）',
  node: '第一项是程序：node 在服务器的 PATH 里找（要先装 Node.js），也可以写完整路径',
  dotnet: '第一项是程序：自带运行时发布的程序；用 dotnet 运行时写 dotnet 和 .dll 的路径',
}

/** 一行一个 KEY=值 → 映射 */
function envMap(text: string): Record<string, string> {
  const m: Record<string, string> = {}
  for (const l of lines(text)) {
    const i = l.indexOf('=')
    if (i > 0) m[l.slice(0, i).trim()] = l.slice(i + 1).trim()
  }
  return m
}

/** 第 4 步的大白话摘要：每个应用一行 */
function appSummary(a: AppForm): string {
  const port = a.port ? `，端口 ${a.port}` : ''
  const build =
    a.sharedBuild && a.buildGroup
      ? `构建：${a.groupCommand.replaceAll('${each:module}', a.module)}（和同工程的应用一起部署时只打包一次）`
      : a.command
        ? `构建：${a.command}`
        : '不构建，直接用现有产物'
  if (a.type === 'static') {
    if (a.serve === 'server') return `${a.name}：前端网站，放到服务器上由已有的 nginx 提供访问。${build}`
    const px = proxyMap(a)
    const fw = Object.keys(px).length ? `，${Object.entries(px).map(([p, to]) => `${p} → ${to}`).join('，')}` : ''
    return `${a.name}：前端网站，由选定的 nginx 软件实例托管${port}${fw}。${build}`
  }
  if (a.type === 'java') {
    const jdk = a.javaSource === 'ferry' ? `JDK ${a.runtimeJava}（由 Ferry 提供）` : `用服务器上的 ${a.java}`
    return `${a.name}：Java 程序，${jdk}${port}。${build}`
  }
  if (isDirApp(a.type)) return `${a.name}：${typeLabel[a.type]}（${a.kind}）${port}，启动命令 ${lines(a.exec).join(' ')}。${build}`
  return `${a.name}：原生程序${port}。${build}`
}

const sizeText = (mb: number) => (mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`)

const form = reactive({ labelWidth: '108px' })
const levelIcon = { ok: CircleCheckFilled, warn: WarningFilled, error: CircleCloseFilled }
</script>

<template>
  <el-dialog v-model="visible" title="新建项目" width="min(1080px, 94vw)" top="4vh" class="project-wizard" :close-on-click-modal="false" @closed="emit('close')">
    <el-steps :active="createdPath ? 4 : step" finish-status="success" simple class="steps">
      <el-step title="项目目录" />
      <el-step title="应用" />
      <el-step title="服务器" />
      <el-step title="预览并创建" />
    </el-steps>

    <div class="body">
      <!-- 1. 目录 -->
      <template v-if="step === 0">
        <el-form :label-width="form.labelWidth">
          <el-form-item label="项目目录">
            <div class="row">
              <el-input v-model="dir" placeholder="代码所在的目录，如 D:\work\myshop" class="mono" @keyup.enter="detect" />
              <el-button :icon="FolderOpened" @click="chooseDir">选择</el-button>
              <el-button :icon="MagicStick" :loading="detecting" :disabled="!dir.trim() || aiRunning" @click="detect">识别</el-button>
              <el-button :icon="ChatDotRound" :type="aiOpen ? 'primary' : 'default'" :plain="aiOpen" :disabled="!dir.trim()" @click="aiOpen = !aiOpen">AI 识别</el-button>
            </div>
          </el-form-item>
          <!-- AI 识别：规则认不出、认不全时，让 AI 看项目里的文件 -->
          <el-form-item v-if="aiOpen" label="AI 识别">
            <div class="ai-box">
              <template v-if="!aiModels.length">
                <span class="muted small">还没有配置 AI 模型。</span>
                <el-button link type="primary" size="small" @click="openAiSettings">去「设置 → AI」添加</el-button>
              </template>
              <template v-else>
                <div class="ai-row">
                  <el-select v-if="aiModels.length > 1" v-model="aiModelId" :placeholder="aiModel?.name" size="small" class="ai-model" :disabled="aiRunning">
                    <el-option v-for="m in aiModels" :key="m.id" :label="m.name" :value="m.id" />
                  </el-select>
                  <el-input
                    v-model="aiHint"
                    size="small"
                    placeholder="可以补充说明（可选），如：后端在 server 目录，用 make build 构建；前端是 admin 目录"
                    :disabled="aiRunning"
                    @keyup.enter="aiDetect"
                  />
                  <el-button v-if="!aiRunning" type="primary" size="small" @click="aiDetect">开始识别</el-button>
                  <el-button v-else size="small" @click="aiStop">停止</el-button>
                </div>
                <div class="muted small">
                  会把项目的目录结构和构建文件、配置文件等发给「{{ aiModel?.name }}」；.env、私钥、证书这类文件不读，看着像密码、密钥的内容会遮掉。
                  AI 只看文件，不执行命令、不改文件；在规则识别的结果上检查、修正、补充。
                </div>
                <div v-if="aiLog.length || aiRunning" ref="aiLogBox" class="ai-log">
                  <div v-for="(l, i) in aiLog" :key="i" :class="l.kind === 'read' ? 'muted mono' : ''">{{ l.kind === 'read' ? '· ' + l.text : l.text }}</div>
                  <div v-if="aiRunning" class="muted"><el-icon class="is-loading"><MagicStick /></el-icon> AI 正在看……</div>
                </div>
                <el-alert v-if="aiError" type="error" :title="aiError" :closable="false" show-icon />
                <div v-if="aiSummary" class="ev-ok small">AI 识别完成：{{ aiSummary }}</div>
                <div v-if="aiUsage" class="muted small">用量：{{ aiUsage }}</div>
              </template>
            </div>
          </el-form-item>
          <el-alert v-if="detectError" type="error" :title="detectError" :closable="false" show-icon class="gap" />
          <template v-if="detected">
            <el-form-item label="项目名">
              <el-input v-model="project" class="mono short" />
              <span class="muted small ml">小写字母、数字和 . _ -；默认的部署目录 /opt/&lt;项目名&gt; 和 systemd 单元名会用到</span>
            </el-form-item>
            <el-form-item label="识别结果">
              <div v-if="detected.apps.length" class="found">
                <div>找到 {{ detected.apps.length }} 个应用，下一步可以修改、取消勾选或手动添加：</div>
                <div class="found-list">
                  <el-tag v-for="a in detected.apps" :key="a.name + a.workdir" size="small" :type="a.ai ? 'warning' : 'info'" class="mono">
                    {{ a.name }} · {{ a.kind }}{{ a.ai ? ' · AI' : '' }}
                  </el-tag>
                </div>
                <div v-if="detected.apps.some((a) => a.ai)" class="muted small">标了 AI 的是 AI 识别新加或改过的，下一步里请核对</div>
              </div>
              <span v-else class="muted">
                没有识别出应用（会找项目目录下两层，支持 Maven / Gradle 的 Java、带 build 脚本的前端、Go、Python、Node 服务端、.NET）。
                可以点上面的「AI 识别」让 AI 看看，或者下一步手动添加
              </span>
            </el-form-item>
            <el-form-item label="配置文件">
              <div class="full">
                <template v-if="!editConfig">
                  <span class="mono">{{ configPath }}</span>
                  <el-button link type="primary" size="small" class="ml" @click="editConfig = true">换个位置</el-button>
                </template>
                <el-input v-else v-model="configPath" class="mono" @change="checkConfig" />
                <div class="muted small">建议放在项目根目录，跟代码一起提交：相对路径按它所在的目录计算{{ detected.git ? '' : '；这个目录不是 git 仓库，版本列表里不会显示提交和分支' }}</div>
              </div>
            </el-form-item>
            <el-alert v-if="configExists" type="warning" :closable="false" show-icon class="gap">
              <template #title>
                <span class="mono">{{ configPath }}</span> 已经有了，不会覆盖
                <el-button size="small" type="primary" link @click="addExisting(configPath)">直接添加它</el-button>
                <el-button v-if="!editConfig" size="small" link @click="editConfig = true">换个位置</el-button>
              </template>
            </el-alert>
          </template>
        </el-form>
      </template>

      <!-- 2. 应用 -->
      <template v-else-if="step === 1">
        <div class="toolbar">
          <span class="muted small">路径都相对项目目录 <span class="mono">{{ dir }}</span>；有提醒、没填端口的应用已展开</span>
          <span class="spacer" />
          <el-dropdown @command="addApp">
            <el-button :icon="Plus" size="small">添加应用</el-button>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="java">Java（jar）</el-dropdown-item>
                <el-dropdown-item command="binary">原生程序（Go 等）</el-dropdown-item>
                <el-dropdown-item command="static">前端静态站点</el-dropdown-item>
                <el-dropdown-item command="python">Python（FastAPI、Flask、Django）</el-dropdown-item>
                <el-dropdown-item command="node">Node 服务端（Next.js、NestJS、Express）</el-dropdown-item>
                <el-dropdown-item command="dotnet">.NET（ASP.NET Core）</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
        <el-empty v-if="!apps.length" description="还没有应用：点右上角「添加应用」" :image-size="60" />
        <el-collapse v-model="openApps">
          <el-collapse-item v-for="(a, i) in apps" :key="i" :name="i">
            <template #title>
              <div class="app-title" @click.stop>
                <el-checkbox v-model="a.include" />
              </div>
              <span class="app-name mono" :class="{ off: !a.include }">{{ a.name }}</span>
              <el-tag size="small" type="info" class="ml">{{ typeLabel[a.type] }}</el-tag>
              <el-tag v-if="a.include && visibleNotes(a).length" size="small" type="warning" class="ml">{{ visibleNotes(a).length }} 条提醒</el-tag>
              <span class="muted small ml">{{ a.kind }}</span>
              <span v-if="a.port && usesPort(a)" class="muted small ml">端口 {{ a.port }}</span>
              <span v-else-if="a.include && a.type !== 'static'" class="warn-text small ml">没填端口</span>
              <span class="muted small ml mono ellipsis">{{ a.artifact }}</span>
            </template>
            <el-form :label-width="form.labelWidth" size="small" :disabled="!a.include">
              <div class="grid">
                <el-form-item label="应用名"><el-input v-model="a.name" class="mono" /></el-form-item>
                <el-form-item label="类型">
                  <el-select v-model="a.type" @change="onTypeChange(a)">
                    <el-option v-for="(l, t) in typeLabel" :key="t" :value="t" :label="l" />
                  </el-select>
                </el-form-item>
              </div>
              <el-form-item v-if="a.buildGroup" label="共用构建">
                <el-checkbox v-model="a.sharedBuild">和同一个 Maven 工程的其他应用一起部署时只打包一次</el-checkbox>
                <div v-if="a.sharedBuild" class="muted small mono">{{ a.groupCommand }}（module = {{ a.module }}）</div>
              </el-form-item>
              <template v-if="!a.sharedBuild || !a.buildGroup">
                <el-form-item label="构建命令">
                  <el-input v-model="a.command" class="mono" placeholder="不填 = 不构建，直接用现有产物" />
                </el-form-item>
                <el-form-item label="构建目录"><el-input v-model="a.workdir" class="mono" placeholder="." /></el-form-item>
              </template>
              <el-form-item label="构建产物">
                <el-input v-model="a.artifact" class="mono">
                  <template #append>
                    <el-button :icon="FolderOpened" @click="pickArtifact(a)">{{ a.type === 'static' || isDirApp(a.type) ? '选择文件夹' : '选择文件' }}</el-button>
                  </template>
                </el-input>
                <div class="muted small">{{ artifactHint(a) }}</div>
              </el-form-item>
              <template v-if="isDirApp(a.type)">
                <el-form-item label="启动命令">
                  <el-input v-model="a.exec" type="textarea" :rows="3" class="mono" :placeholder="execPlaceholder" />
                  <div class="muted small">
                    {{ execHint[a.type] }}
                  </div>
                </el-form-item>
                <div class="grid">
                  <el-form-item label="环境变量">
                    <el-input v-model="a.svcEnv" type="textarea" :rows="2" class="mono" placeholder="一行一个，如 PORT=3000" />
                  </el-form-item>
                  <el-form-item label="不上传">
                    <el-input v-model="a.exclude" type="textarea" :rows="2" class="mono" placeholder="一行一个，如 tests、*.log（常见的已默认不传）" />
                  </el-form-item>
                </div>
                <el-form-item v-if="a.type !== 'dotnet'" label="依赖镜像源">
                  <el-input v-model="a.registry" class="mono" :placeholder="registryHint[a.type]" />
                  <div class="muted small">
                    依赖在服务器上装（{{ depsHow[a.type] }}），依赖文件没变的部署直接用上次装好的；国内服务器建议填镜像源
                  </div>
                </el-form-item>
              </template>
              <el-form-item v-if="a.type !== 'static' && !isDirApp(a.type)" label="附带文件">
                <div class="proxies">
                  <div v-for="(f, j) in a.files" :key="j" class="row proxy-row">
                    <el-input v-model="f.from" class="mono" placeholder="本机路径，如 config.yaml" />
                    <span class="muted arrow">→</span>
                    <el-input v-model="f.to" class="mono file-to" :placeholder="baseName(f.from) || '同名'" />
                    <el-tooltip :show-after="HOVER_DELAY" content="脚本等要能执行的文件" placement="top">
                      <el-checkbox v-model="f.exec">可执行</el-checkbox>
                    </el-tooltip>
                    <el-button :icon="Delete" text @click="a.files.splice(j, 1)" />
                  </div>
                  <div class="row file-add">
                    <el-button :icon="Plus" size="small" link type="primary" @click="addFiles(a, false)">添加文件</el-button>
                    <el-button :icon="Plus" size="small" link type="primary" @click="addFiles(a, true)">添加文件夹</el-button>
                    <el-button size="small" link type="primary" @click="a.files.push({ from: '', to: '', exec: false })">手动填写</el-button>
                  </div>
                  <div v-if="fileCandidates(a).length" class="file-cands">
                    <span class="muted small">项目里找到：</span>
                    <el-tag v-for="c in fileCandidates(a)" :key="c" size="small" class="mono file-cand" @click="a.files.push({ from: c, to: '', exec: false })">
                      + {{ c }}
                    </el-tag>
                  </div>
                  <div class="muted small">
                    程序要读的配置文件、证书等。部署时和程序放在同一个目录（也是程序运行时的当前目录），每次部署都重新上传，回滚时跟着回到旧版本。
                    右边填放到服务器上叫什么，可以带目录，如 config/application.yml；不填就用原来的名字。程序只能读、不能改这些文件
                  </div>
                </div>
              </el-form-item>
              <el-alert v-for="n in visibleNotes(a)" :key="n" type="warning" :title="n" :closable="false" show-icon class="note" />

              <template v-if="a.type === 'static'">
                <el-form-item label="运行方式">
                  <el-radio-group v-model="a.serve" @change="a.serve === 'ferry' && !a.port && (a.port = nextWebPort(a))">
                    <el-radio value="ferry">选择已安装的 nginx 实例</el-radio>
                    <el-radio value="server">只部署文件，手动接入 nginx</el-radio>
                  </el-radio-group>
                  <div class="muted small">
                    {{
                      a.serve === 'ferry'
                        ? '先在连接的「软件」中安装 nginx，下一步选择实例；部署只更新本站点，可与其他项目共用 nginx'
                        : '只把文件放到服务器上，由服务器上已有的 nginx 提供访问（需要自己配置 nginx 指向它）'
                    }}
                  </div>
                </el-form-item>
                <template v-if="a.serve === 'ferry'">
                  <el-form-item label="站点域名"><el-input v-model="a.domains" placeholder="可选，多个域名用空格分隔" /><div class="muted small">不填为此端口的默认站点；同端口的多个站点请填写不同域名。</div></el-form-item>
                  <el-form-item label="端口">
                    <el-input-number v-model="a.port" :min="1" :max="65535" controls-position="right" />
                    <span class="muted small ml">网站的访问端口，如 http://服务器地址:{{ a.port || defaultWebPort }}/</span>
                  </el-form-item>
                  <el-form-item label="接口转发">
                    <div class="proxies">
                      <div v-for="(p, j) in a.proxies" :key="j" class="row proxy-row">
                        <el-input v-model="p.path" class="mono proxy-path" placeholder="/api/" />
                        <span class="muted arrow">→</span>
                        <el-input v-model="p.to" class="mono" :placeholder="proxyHint" />
                        <el-dropdown v-if="backends.length" @command="(port: number) => (p.to = proxyTo(p.to, port))">
                          <el-button size="small" text>指向…</el-button>
                          <template #dropdown>
                            <el-dropdown-menu>
                              <el-dropdown-item v-for="b in backends" :key="b.name" :command="b.port">{{ b.name }}（端口 {{ b.port }}）</el-dropdown-item>
                            </el-dropdown-menu>
                          </template>
                        </el-dropdown>
                        <el-button :icon="Delete" text @click="a.proxies.splice(j, 1)" />
                      </div>
                      <el-button
                        :icon="Plus"
                        size="small"
                        link
                        type="primary"
                        @click="a.proxies.push({ path: '/api/', to: backends.length === 1 ? `http://127.0.0.1:${backends[0].port}` : proxyHint })"
                      >
                        添加一条
                      </el-button>
                      <div class="muted small">
                        页面请求后端接口的路径前缀转给后端。后端地址末尾带 / 时去掉前缀再转发（/prod-api/user → 后端的 /user），不带时原样转发
                      </div>
                    </div>
                  </el-form-item>
                  <el-form-item label="前端路由">
                    <el-checkbox v-model="a.spa">history 路由：找不到的路径都返回 index.html（Vue Router、React Router 的 history 模式需要）</el-checkbox>
                  </el-form-item>
                </template>
                <template v-else>
                  <el-form-item label="nginx 链接">
                    <el-input v-model="a.link" class="mono" placeholder="可选，如 /usr/share/nginx/html/myshop" />
                    <div class="muted small">维护一个指向当前版本的链接，nginx 的 root 指向它；不填则 root 指向 &lt;部署目录&gt;/apps/{{ a.name }}/running</div>
                  </el-form-item>
                  <el-form-item label="切换后执行"><el-input v-model="a.reload" class="mono" placeholder="可选，如 nginx -t && nginx -s reload" /></el-form-item>
                </template>
              </template>
              <template v-else>
                <el-form-item v-if="a.type === 'java'" label="JDK">
                  <el-radio-group v-model="a.javaSource">
                    <el-radio value="ferry">由 Ferry 提供（推荐）</el-radio>
                    <el-radio value="server">用服务器上已有的 java</el-radio>
                  </el-radio-group>
                  <div class="row jdk-row">
                    <template v-if="a.javaSource === 'ferry'">
                      <el-select v-model="a.runtimeJava" style="width: 120px">
                        <el-option v-for="v in [8, 11, 17, 21, 25]" :key="v" :value="v" :label="`JDK ${v}`" />
                      </el-select>
                      <span class="muted small">部署时自动下载（本机已有就不下载）并上传到服务器，服务器不用预先安装</span>
                    </template>
                    <el-input v-else v-model="a.java" class="mono" placeholder="服务器上 java 的绝对路径，如 /usr/lib/jvm/java-17/bin/java" />
                  </div>
                  <div class="muted small">
                    {{ a.needJava ? `这个项目需要 Java ${a.needJava} 或更高。` : '' }}部署前会检查版本是否够用{{ a.javaSource === 'server' ? '；下一步「测试连接」会列出服务器上的 java' : '' }}
                  </div>
                </el-form-item>
                <div class="grid">
                  <el-form-item v-if="a.type === 'java'" label="JVM 参数">
                    <el-input v-model="a.jvmArgs" type="textarea" :rows="2" class="mono" placeholder="一行一个" />
                  </el-form-item>
                  <el-form-item v-if="!isDirApp(a.type)" label="程序参数">
                    <el-input v-model="a.args" type="textarea" :rows="2" class="mono" placeholder="一行一个，如 --spring.profiles.active=prod" />
                  </el-form-item>
                </div>
                <div class="grid">
                  <el-form-item label="端口">
                    <el-input-number v-model="a.port" :min="1" :max="65535" controls-position="right" @change="(n: number | undefined, o: number | undefined) => onPortChange(a, o)" />
                    <div v-if="!a.port" class="warn-text small">没填端口：按「进程持续运行 10 秒」判断是否启动成功，也检查不了端口冲突</div>
                  </el-form-item>
                  <el-form-item label="就绪判断">
                    <el-select v-model="a.health">
                      <el-option value="tcp" label="端口能连上" />
                      <el-option value="http" label="HTTP 地址返回成功" />
                      <el-option value="none" label="进程持续运行 10 秒" />
                    </el-select>
                  </el-form-item>
                </div>
                <el-form-item v-if="a.health === 'http'" label="检查地址"><el-input v-model="a.healthUrl" class="mono" /></el-form-item>
                <el-form-item v-if="a.type === 'java'" label="日志">
                  <el-checkbox v-model="a.fileLogs">日志文件写到运行中版本目录的 logs/（和程序放在一起，可在 Ferry 里查看以往版本的日志）</el-checkbox>
                </el-form-item>
              </template>
              <el-form-item label=" ">
                <el-button link type="primary" size="small" @click="a.more = !a.more">{{ a.more ? '收起更多设置' : a.type === 'static' ? '更多设置（单独的部署目录）' : '更多设置（单元名、单独的部署目录）' }}</el-button>
              </el-form-item>
              <template v-if="a.more">
                <el-form-item v-if="a.type !== 'static'" label="单元名">
                  <el-input v-model="a.unit" class="mono" :placeholder="`默认 ${project}-${a.name.toLowerCase()}；服务器上已有的同名服务会被接管`" />
                </el-form-item>
                <el-form-item label="单独部署目录">
                  <el-input v-model="a.root" class="mono" placeholder="一般不用填" />
                  <div class="muted small">
                    默认放在服务器的部署目录（下一步填写）下的 apps/{{ a.name }}；只有要把这个应用放到别的分区（如数据盘 /data/xxx）时才填，对所有服务器生效
                  </div>
                </el-form-item>
              </template>
              <el-button :icon="Delete" size="small" text type="danger" @click="apps.splice(i, 1)">删除这个应用</el-button>
            </el-form>
          </el-collapse-item>
        </el-collapse>
      </template>

      <!-- 3. 服务器 -->
      <template v-else-if="step === 2">
        <div class="toolbar">
          <span class="muted small">可以添加多台服务器（如测试、生产）；密码不写在配置里，第一次连接时输入</span>
          <span class="spacer" />
          <el-button :icon="Plus" size="small" @click="targets.push(newTarget())">添加服务器</el-button>
        </div>
        <div v-for="(t, i) in targets" :key="i" class="target">
          <el-form :label-width="form.labelWidth" size="small">
            <el-form-item v-if="connections.length" label="从连接填入">
              <el-select :model-value="t.connection" placeholder="选一个已有的连接，自动填好地址和登录方式" clearable style="width: 420px" @change="(v: string) => fillFromConnection(t, v ?? '')">
                <el-option v-for="c in connections" :key="c.name" :value="c.name" :label="c.title">
                  <span>{{ c.title }}</span>
                  <span class="muted small mono ml">{{ c.user }}@{{ c.host }}:{{ c.port }}</span>
                </el-option>
              </el-select>
            </el-form-item>
            <div class="grid3">
              <el-form-item label="名称"><el-input v-model="t.name" class="mono" placeholder="如 prod、test" /></el-form-item>
              <el-form-item label="地址"><el-input v-model="t.host" class="mono" placeholder="IP 或域名" /></el-form-item>
              <el-form-item label="SSH 端口"><el-input-number v-model="t.port" :min="1" :max="65535" controls-position="right" /></el-form-item>
            </div>
            <div class="grid3">
              <el-form-item label="登录用户"><el-input v-model="t.user" class="mono" /></el-form-item>
              <el-form-item label="登录方式">
                <el-radio-group v-model="t.auth">
                  <el-radio value="key">密钥</el-radio>
                  <el-radio value="password">密码</el-radio>
                </el-radio-group>
              </el-form-item>
              <el-form-item v-if="t.user && t.user !== 'root'" label="sudo">
                <el-checkbox v-model="t.sudo">用免密 sudo 执行部署</el-checkbox>
              </el-form-item>
            </div>
            <el-form-item v-if="t.auth === 'key'" label="私钥">
              <el-input v-model="t.identityFile" class="mono" placeholder="可选：不填用 ~/.ssh 下的默认私钥或 ssh-agent；也可以创建后用「配置免密登录」生成" />
            </el-form-item>
            <div v-else class="muted small hint">
              密码不写进配置：点下面的「测试连接」，连上服务器后会弹出输入密码的框（可以记到系统凭据库，以后不用再输）
            </div>
            <div class="grid3">
              <el-form-item label="部署目录">
                <el-input v-model="t.root" class="mono" />
              </el-form-item>
              <el-form-item label="运行用户">
                <el-input v-model="t.runAs" class="mono" :disabled="!needsRunAs" />
              </el-form-item>
              <el-form-item label="CPU 架构">
                <el-select v-model="t.arch" :empty-values="[null, undefined]">
                  <el-option value="" :label="t.probe?.arch ? `自动（检测到 ${t.probe.arch}）` : '自动'" />
                  <el-option value="amd64" label="amd64（x86_64）" />
                  <el-option value="arm64" label="arm64（aarch64）" />
                </el-select>
              </el-form-item>
            </div>
            <div class="muted small hint">
              各应用会放在：<span v-for="(a, k) in included" :key="a.name" class="mono">{{ k ? '；' : '' }}{{ a.name }} → {{ (a.root.trim() || t.root || '?').replace(/\/$/, '') }}/apps/{{ a.name }}{{ a.root.trim() ? '（单独设置）' : '' }}</span>
            </div>
            <div class="muted small hint">部署目录不存在会自动创建；权限不对、运行用户不存在时，部署会自动处理</div>
            <el-form-item label=" ">
              <el-button link type="primary" size="small" @click="t.more = !t.more">{{ t.more ? '收起更多设置' : '更多设置（Ferry 组件目录）' }}</el-button>
            </el-form-item>
            <el-form-item v-if="t.more" label="Ferry 组件目录">
              <el-input v-model="t.ferryDir" class="mono" />
              <div class="muted small">ferry 的服务器端组件（agent）和 Ferry 提供的 JDK 放在这里，同一台服务器上的项目共用；必须归 root、其他用户不可写</div>
            </el-form-item>

            <div class="probe">
              <div class="row probe-bar">
                <el-button :icon="ConnIcon" :loading="t.probing" :disabled="!t.host.trim()" @click="probe(t)">{{ t.probe ? '重新测试' : '测试连接并检查服务器' }}</el-button>
                <span v-if="probeStale(t)" class="warn-text small">测试之后改过设置，建议重新测试</span>
                <span v-else-if="!t.probe && !t.probeError" class="muted small">连上服务器看看权限、运行用户、端口、磁盘空间、java 等，只查看、不改动服务器</span>
              </div>
              <el-alert v-if="t.probeError" type="error" :title="t.probeError" :closable="false" show-icon class="gap" />
              <el-form-item v-if="needsNginx" label="nginx 实例" class="gap"><div class="full"><el-select v-model="t.nginxInstance" :placeholder="t.probe ? '选择已识别的 nginx' : '先测试连接，再选择 nginx'"><el-option v-for="n in t.probe?.nginxInstances ?? []" :key="n.name" :value="n.name" :label="`${n.hosting?.system ? '系统 Nginx' : n.name}（${n.unit}）`" /></el-select><div class="muted small">可选择系统 nginx 或 Ferry 安装的独立实例；只向已识别的站点目录添加本站点配置。未识别到时查看下方检测说明，或在连接的「软件」中安装 Nginx 后重新测试。</div><div v-if="t.probe?.nginxInstances?.find((n) => n.name === t.nginxInstance)?.hosting?.sites" class="muted small mono">站点目录：{{ t.probe.nginxInstances.find((n) => n.name === t.nginxInstance)?.hosting?.sites }}</div></div></el-form-item>
              <ul v-if="t.probe" class="checks">
                <li v-for="(c, k) in allChecks(t)" :key="k" :class="c.level">
                  <el-icon class="check-icon"><component :is="levelIcon[c.level]" /></el-icon>
                  <span>{{ c.text }}</span>
                  <el-button v-if="c.fix === 'create-user'" link type="primary" size="small" :loading="t.creatingUser" @click="createUser(t)">现在创建</el-button>
                  <el-button v-if="c.fix === 'goarch'" link type="primary" size="small" @click="fixGoarch(t)">改成 {{ t.probe.arch }}</el-button>
                </li>
              </ul>
            </div>
            <el-button v-if="targets.length > 1" :icon="Delete" size="small" text type="danger" @click="targets.splice(i, 1)">删除这台服务器</el-button>
          </el-form>
        </div>
      </template>

      <!-- 4. 预览；创建后可以预检 -->
      <template v-else>
        <template v-if="createdPath">
          <el-alert type="success" :closable="false" show-icon class="gap">
            <template #title>已创建 <span class="mono">{{ createdPath }}</span> 并加入项目列表，可以随时在「编辑配置」里修改</template>
          </el-alert>
          <p class="muted small">部署前可以先预检：连上服务器，检查系统、权限、运行用户、端口、磁盘空间、java 等，列出部署时会自动处理的地方。会在服务器上装好 Ferry 的组件（agent），不改动应用。</p>
          <div v-for="t in targets" :key="t.name" class="target">
            <div class="row probe-bar">
              <b class="mono">{{ t.name }}</b>
              <span class="muted small mono">{{ t.user || 'root' }}@{{ t.host }}:{{ t.port }}</span>
              <span class="spacer" />
              <el-button size="small" :loading="t.checking" @click="preflight(t)">{{ t.preflight ? '重新预检' : '预检' }}</el-button>
            </div>
            <el-alert v-if="t.preflightError" type="error" :title="t.preflightError" :closable="false" show-icon class="gap" />
            <ul v-if="t.preflight" class="checks">
              <li class="ok">
                <el-icon class="check-icon"><CircleCheckFilled /></el-icon>
                <span>{{ t.preflight.os }}，{{ t.preflight.arch }}，systemd {{ t.preflight.systemd }}，部署目录所在分区剩余 {{ sizeText(t.preflight.rootFreeMB) }}</span>
              </li>
              <li v-for="f in t.preflight.fixable ?? []" :key="'f' + f" class="ok">
                <el-icon class="check-icon"><CircleCheckFilled /></el-icon><span>部署时会自动处理：{{ f }}</span>
              </li>
              <li v-for="w in t.preflight.warnings ?? []" :key="'w' + w" class="warn">
                <el-icon class="check-icon"><WarningFilled /></el-icon><span>{{ w }}</span>
              </li>
              <li v-for="p in t.preflight.problems ?? []" :key="'p' + p" class="error">
                <el-icon class="check-icon"><CircleCloseFilled /></el-icon><span>{{ p }}</span>
              </li>
              <li v-if="!(t.preflight.problems ?? []).length" class="ok">
                <el-icon class="check-icon"><CircleCheckFilled /></el-icon><span>预检通过，可以部署</span>
              </li>
            </ul>
          </div>
        </template>
        <template v-else>
          <div class="summary">
            <div v-for="t in targets" :key="t.name" class="sum-line">
              部署到 <b class="mono">{{ t.name }}</b>（{{ t.user || 'root' }}@{{ t.host }}:{{ t.port }}），部署目录 <span class="mono">{{ t.root }}</span>
              <template v-if="needsRunAs">，运行用户 <span class="mono">{{ t.runAs }}</span></template>
              <el-tag v-if="!t.probe" size="small" type="warning" class="ml">还没测试连接</el-tag>
              <el-tag v-else-if="probeErrors(t)" size="small" type="danger" class="ml">测试发现 {{ probeErrors(t) }} 个问题</el-tag>
              <el-tag v-else size="small" type="success" class="ml">测试通过</el-tag>
            </div>
            <ul class="sum-apps">
              <li v-for="a in included" :key="a.name">{{ appSummary(a) }}</li>
            </ul>
          </div>
          <div class="muted small gap">将写入 <span class="mono">{{ configPath }}</span>，创建后可以随时在「编辑配置」里修改</div>
          <el-alert v-if="exists" type="error" title="这个位置已经有文件了，不会覆盖：回到第一步换一个位置，或直接添加那个文件" :closable="false" show-icon class="gap" />
          <el-alert v-if="problems.length" type="error" title="生成的配置有问题，请回到前面的步骤修改" :closable="false" show-icon class="gap">
            <div v-for="p in problems" :key="p" class="mono small">{{ p }}</div>
          </el-alert>
          <pre v-loading="previewing" class="yaml mono">{{ preview }}</pre>
        </template>
      </template>
    </div>

    <template #footer>
      <template v-if="createdPath">
        <el-button type="primary" @click="visible = false">完成</el-button>
      </template>
      <template v-else>
        <span v-if="stepError" class="step-error">{{ stepError }}</span>
        <el-button @click="visible = false">取消</el-button>
        <el-button v-if="step > 0" @click="step--">上一步</el-button>
        <el-button v-if="step < 3" type="primary" :disabled="!!stepError" @click="next">下一步</el-button>
        <el-button v-else type="primary" :loading="creating" :disabled="previewing || exists || problems.length > 0" @click="create">创建</el-button>
      </template>
    </template>
  </el-dialog>
</template>

<style scoped>
.steps {
  margin-bottom: 14px;
  flex-shrink: 0;
}
.ai-box {
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  background: var(--el-fill-color-lighter);
}
.ai-row {
  display: flex;
  gap: 8px;
  align-items: center;
}
.ai-model {
  width: 180px;
  flex: none;
}
.ai-log {
  max-height: 180px;
  overflow: auto;
  font-size: 12.5px;
  line-height: 1.7;
  padding: 6px 8px;
  border-radius: 6px;
  background: var(--el-bg-color);
  white-space: pre-wrap;
}
.body {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding-right: 4px;
}
.row {
  display: flex;
  gap: 8px;
  width: 100%;
}
.full {
  width: 100%;
}
.short {
  width: 240px;
}
.ml {
  margin-left: 8px;
}
.small {
  font-size: 12px;
}
.gap {
  margin-bottom: 12px;
}
.warn-text {
  color: var(--el-color-warning);
}
.toolbar {
  display: flex;
  align-items: center;
  margin-bottom: 8px;
}
.spacer {
  flex: 1;
}
.found {
  width: 100%;
}
.found-list {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 4px;
}
.app-title {
  display: inline-flex;
  margin-right: 8px;
}
.app-name {
  font-weight: 600;
}
.app-name.off {
  color: var(--el-text-color-placeholder);
  text-decoration: line-through;
}
.ellipsis {
  max-width: 300px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  column-gap: 12px;
}
.grid3 {
  display: grid;
  grid-template-columns: 1fr 1fr 1fr;
  column-gap: 12px;
}
@media (max-width: 900px) { .grid3 { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
.jdk-row {
  margin-top: 6px;
  align-items: center;
}
.note {
  margin: -6px 0 12px 108px;
  width: auto;
}
.target {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 14px 14px 6px;
  margin-bottom: 12px;
}
.hint {
  margin: -6px 0 8px 108px;
}
.probe {
  margin: 4px 0 10px 108px;
}
.probe-bar {
  align-items: center;
  margin-bottom: 8px;
}
.checks {
  list-style: none;
  margin: 0 0 8px;
  padding: 0;
  font-size: 13px;
  line-height: 1.6;
}
.checks li {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  padding: 2px 0;
}
.check-icon {
  flex: none;
  margin-top: 3px;
}
.checks .ok .check-icon {
  color: var(--el-color-success);
}
.checks .warn .check-icon {
  color: var(--el-color-warning);
}
.checks .error .check-icon {
  color: var(--el-color-danger);
}
.summary {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 10px 14px;
  margin-bottom: 12px;
  line-height: 1.7;
}
.sum-apps {
  margin: 6px 0 0;
  padding-left: 18px;
}
.yaml {
  margin: 0;
  padding: 12px 14px;
  min-height: 300px;
  border-radius: 6px;
  background: var(--el-fill-color-lighter);
  border: 1px solid var(--el-border-color-lighter);
  font-size: 12.5px;
  line-height: 1.55;
  white-space: pre;
  overflow: auto;
}
.step-error {
  color: var(--el-color-danger);
  font-size: 12px;
  margin-right: 12px;
}
.proxies {
  width: 100%;
}
.proxy-row {
  gap: 6px;
  margin-bottom: 6px;
  align-items: center;
}
.proxy-row .arrow {
  flex: none;
  white-space: nowrap;
}
.proxy-path {
  flex: none;
  width: 140px;
}
.file-to {
  flex: none;
  width: 200px;
}
.file-add {
  gap: 4px;
}
.file-cands {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px;
  margin: 2px 0 4px;
}
.file-cand {
  cursor: pointer;
}
</style>
<style>
.el-dialog.project-wizard .el-dialog__body { display: flex; flex-direction: column; height: min(600px, calc(100dvh - 240px)); overflow: hidden; }
</style>
