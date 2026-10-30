<script setup lang="ts">
import LoadingBlock from './LoadingBlock.vue'
import { computed, defineAsyncComponent, onMounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { CopyDocument, Delete, FolderOpened, Link, Plus, View } from '@element-plus/icons-vue'
import { bytes, call, pkgOrigin, time, type AppSettings, type DataDirInfo, type JdkList, type SecretEntry, type SwPackage, type SwTemplates } from '../ferry'
import AiModels from './AiModels.vue'
import NotifyChannels from './NotifyChannels.vue'
import DnsKeys from './DnsKeys.vue'
import CliMcp from './CliMcp.vue'
import { appSettings, applyTheme, dropTasks, loadSettings, saveSettings, settingsInfo, settingsRequest, startTask, tasks } from '../state'
import { appInfo, isTauri, openUrl, pickDir, pickFile, reveal } from '../platform'
import { glass, GLASS_TRANSPARENCY_DEFAULT } from '../glass/runtime'
import GlassToggle from './glass/GlassToggle.vue'

// 用到时才加载（编辑器、终端、AI 助手和各个对话框不放进启动时加载的代码里）
const TemplateEditor = defineAsyncComponent(() => import('./TemplateEditor.vue'))
const CredentialDialog = defineAsyncComponent(() => import('./CredentialDialog.vue'))

const emit = defineEmits<{ close: [] }>()

const visible = ref(true)
const tab = ref(settingsRequest.value || 'storage')
settingsRequest.value = ''
const settingsQuery = ref('')
const sections = [
  { name: 'storage', label: '存储', group: '常规', description: '管理数据位置、磁盘占用和记录保留策略。', keywords: '目录 日志 清理 任务' },
  { name: 'connect', label: '连接', group: '常规', description: '设置连接、上传和自动刷新行为。', keywords: 'SSH 超时 密钥 端口' },
  { name: 'secrets', label: '凭据', group: '常规', description: '查看和管理保存在本机系统凭据库中的密码与密钥。', keywords: '密码 key token' },
  { name: 'ui', label: '界面', group: '常规', description: '调整主题、任务展示与通知体验。', keywords: '主题 深色 通知 液态玻璃 玻璃 动画' },
  { name: 'build', label: '构建', group: '部署与运行', description: '为构建程序配置环境变量与工具路径。', keywords: 'JAVA_HOME PATH Maven' },
  { name: 'deploy', label: '部署', group: '部署与运行', description: '配置团队身份、部署默认行为与证书密钥。', keywords: '团队 DNS HTTPS' },
  { name: 'notify', label: '通知', group: '部署与运行', description: '将部署和回滚结果发送到团队使用的通知渠道。', keywords: '企业微信 钉钉 飞书 机器人' },
  { name: 'runtime', label: '运行环境', group: '部署与运行', description: '管理 JDK、nginx、软件安装包与安装模板。', keywords: 'Java 安装包 模板 nginx' },
  { name: 'ai', label: 'AI', group: '集成', description: '管理助手使用的模型、连接参数和 API Key。', keywords: '模型 服务商' },
  { name: 'cli', label: '命令行与 MCP', group: '集成', description: '接入编程工具，管理可用能力与调用权限。', keywords: 'Claude Cursor PATH 工具 权限' },
  { name: 'about', label: '关于', group: '应用', description: '版本信息与运行环境，排查问题时可复制完整诊断信息。', keywords: '版本 更新' },
]
const navigationGroups = computed(() => ['常规', '部署与运行', '集成', '应用'].map((label) => ({ label, items: sections.filter((s) => s.group === label && `${s.label} ${s.description} ${s.keywords}`.toLowerCase().includes(settingsQuery.value.trim().toLowerCase())) })).filter((g) => g.items.length))
const currentSection = computed(() => sections.find((s) => s.name === tab.value) ?? sections[0])
const credentialQuery = ref('')
const runtimeView = ref('jdk')
const matchingSecrets = computed(() => secrets.value.filter((s) => `${s.label} ${secretKind(s.kind)}`.toLowerCase().includes(credentialQuery.value.trim().toLowerCase())))
const saving = ref(false)
const initializationError = ref('')
const form = ref<AppSettings>()
const envRows = ref<{ key: string; value: string }[]>([])
const pathRows = ref<string[]>([])
const secrets = ref<SecretEntry[]>([])
const secretsError = ref('')
const viewedSecret = ref<SecretEntry>()
watch([visible, tab], () => {
  if (!visible.value || tab.value !== 'secrets') viewedSecret.value = undefined
})

const info = computed(() => settingsInfo.value)
const dataDir = computed<DataDirInfo | undefined>(() => info.value?.dataDir)
const locked = computed(() => dataDir.value?.mode === 'env')
const modeText: Record<string, string> = {
  custom: '自定义位置',
  env: '由环境变量 FERRY_HOME 指定',
  install: '安装目录（默认）',
  user: '用户目录（安装目录不可写）',
}

async function initialize() {
  initializationError.value = ''
  try {
    await loadSettings()
    resetForm()
  } catch (e) {
    initializationError.value = (e as Error).message
    return
  }
  loadSecrets()
  loadAbout()
  loadJdks()
  loadSwLib()
  loadTemplates()
  loadAiUsage()
  loadTaskUsage()
  loadMcpUsage()
}
onMounted(initialize)

// ---------------------------------------------------------------- AI 助手

const aiModels = ref<InstanceType<typeof AiModels>>()
const notifyChannels = ref<InstanceType<typeof NotifyChannels>>()
const dnsKeys = ref<InstanceType<typeof DnsKeys>>()

const aiUsage = ref<{ count: number; bytes: number }>()
const aiCleaning = ref(false)

async function loadAiUsage() {
  try {
    aiUsage.value = await call<{ count: number; bytes: number }>(['ai-history', 'list'])
  } catch {
    aiUsage.value = undefined
  }
}

async function cleanAi(all: boolean) {
  if (!form.value) return
  try {
    if (all) {
      await ElMessageBox.confirm(`删除全部 ${aiUsage.value?.count ?? 0} 个 AI 助手对话？删除后不能恢复。`, '删除对话记录', { type: 'warning' })
    }
  } catch {
    return
  }
  aiCleaning.value = true
  try {
    const args = all
      ? ['ai-history', 'clean', '--all']
      : ['ai-history', 'clean', '--days', String(form.value.ai.keepDays), '--max-mb', String(form.value.ai.maxMB)]
    const r = await call<{ removed: number; count: number; bytes: number }>(args)
    aiUsage.value = r
    ElMessage.success(r.removed ? `删除了 ${r.removed} 个对话` : '没有要清理的对话')
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    aiCleaning.value = false
  }
}

// ---------------------------------------------------------------- 任务记录

const taskUsage = ref<{ count: number; bytes: number }>()
const taskCleaning = ref(false)

async function loadTaskUsage() {
  try {
    taskUsage.value = await call<{ count: number; bytes: number }>(['task-history', 'list'])
  } catch {
    taskUsage.value = undefined
  }
}

async function cleanTasks(all: boolean) {
  if (!form.value) return
  try {
    if (all) {
      await ElMessageBox.confirm(`删除全部 ${taskUsage.value?.count ?? 0} 个任务记录？删除后不能恢复，进行中的任务不受影响。`, '删除任务记录', { type: 'warning' })
    }
  } catch {
    return
  }
  taskCleaning.value = true
  try {
    const ui = form.value.ui
    const args = all
      ? ['task-history', 'clean', '--all']
      : ['task-history', 'clean', '--days', String(ui.taskKeepDays), '--max-mb', String(ui.taskMaxMB), '--keep', String(ui.taskKeep)]
    const r = await call<{ removed: number; ids?: string[]; count: number; bytes: number }>(args)
    taskUsage.value = r
    // 任务面板里也去掉
    dropTasks(all ? tasks.map((t) => t.hid) : (r.ids ?? []))
    ElMessage.success(r.removed ? `删除了 ${r.removed} 个任务记录` : '没有要清理的任务记录')
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    taskCleaning.value = false
  }
}

// ---------------------------------------------------------------- MCP 调用记录

const mcpUsage = ref<{ count: number; bytes: number }>()
const mcpCleaning = ref(false)

async function loadMcpUsage() {
  try {
    mcpUsage.value = await call<{ count: number; bytes: number }>(['mcp-log', '--limit', '1'])
  } catch {
    mcpUsage.value = undefined
  }
}

async function cleanMcpLog(all: boolean) {
  if (!form.value) return
  try {
    if (all) {
      await ElMessageBox.confirm(`删除全部 ${mcpUsage.value?.count ?? 0} 条 MCP 调用记录？删除后不能恢复。`, '删除调用记录', { type: 'warning' })
    }
  } catch {
    return
  }
  mcpCleaning.value = true
  try {
    const m = form.value.mcp
    const args = all ? ['mcp-log', 'clean', '--all'] : ['mcp-log', 'clean', '--days', String(m.logKeepDays), '--max-mb', String(m.logMaxMB)]
    const r = await call<{ removed: number; count: number; bytes: number }>(args)
    mcpUsage.value = r
    ElMessage.success(r.removed ? `删除了 ${r.removed} 条调用记录` : '没有要清理的调用记录')
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    mcpCleaning.value = false
  }
}

// ---------------------------------------------------------------- 运行环境（Ferry 提供的 JDK）

const jdks = ref<JdkList>()
const jdkError = ref('')
const jdkJava = ref(17)
const jdkArch = ref<'x64' | 'aarch64'>('x64')
const jdkBusy = ref(false)

const secretKind = (k: string) => ({ password: '登录密码', passphrase: '私钥密码', software: '软件密码', ai: 'AI 模型', notify: '部署通知', dns: 'DNS 密钥', 'ai-input': 'AI 助手填写' })[k] ?? k

async function loadJdks() {
  try {
    jdks.value = await call<JdkList>(['runtime', 'list'])
    jdkError.value = ''
  } catch (e) {
    jdkError.value = (e as Error).message
  }
}

async function importJdk() {
  const f = await pickFile('选择 Linux 版 JDK 的压缩包（.tar.gz）', 'JDK 压缩包', ['gz', 'tgz'])
  if (!f) return
  jdkBusy.value = true
  try {
    const m = await call<{ id: string }>(['runtime', 'import', f])
    ElMessage.success(`已导入 ${m.id}`)
    loadJdks()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    jdkBusy.value = false
  }
}

function downloadJdk() {
  startTask(`下载 JDK ${jdkJava.value}（linux/${jdkArch.value}）`, ['runtime', 'download', '--java', String(jdkJava.value), '--arch', jdkArch.value], () => loadJdks())
}

async function removeJdk(id: string) {
  try {
    await ElMessageBox.confirm(`从本机删除 ${id}？服务器上已安装的不受影响，以后需要时会重新下载。`, '删除 JDK', { type: 'warning' })
  } catch {
    return
  }
  try {
    await call(['runtime', 'remove', id])
    loadJdks()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// nginx（前端站点 static.port 用）
const ngxSeries = ref('stable')
const rt = computed(() => form.value?.runtime)
const NGINX_SOURCE = 'https://jirutka.github.io/nginx-binaries'
const ngxArch = ref<'x86_64' | 'aarch64'>('x86_64')
const ngxBusy = ref(false)
const isStable = (v: string) => Number(v.split('.')[1]) % 2 === 0

function downloadNginx() {
  const what = ngxSeries.value === 'stable' ? '最新稳定版' : ngxSeries.value
  startTask(`下载 nginx ${what}（linux/${ngxArch.value}）`, ['runtime', 'download', '--nginx', ngxSeries.value, '--arch', ngxArch.value], () => loadJdks())
}

async function importNginx() {
  const f = await pickFile('选择 Linux 版 nginx 程序（如 nginx-1.30.5-x86_64-linux）', 'nginx 程序', ['*'])
  if (!f) return
  ngxBusy.value = true
  try {
    const m = await call<{ id: string }>(['runtime', 'import', '--nginx', f])
    ElMessage.success(`已导入 ${m.id}`)
    loadJdks()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    ngxBusy.value = false
  }
}

// 软件模板
const tpl = ref<SwTemplates>()
const editing = ref<{ title: string; content: string } | null>(null)

async function loadTemplates() {
  try {
    tpl.value = await call<SwTemplates>(['software', 'templates'])
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

const TEMPLATE_SKELETON = `id: my-app
name: 我的服务
category: 自定义
desc: 说明
params:
  - {key: port, label: 端口, type: port, default: "8080"}
  - {key: password, label: 密码, type: password}
variants:
  - label: 导入的程序
    user: myapp
    package: {format: binary, into: my-app}
    service:
      exec: \${dir}/my-app --port \${port}
      env: {APP_PASSWORD: "\${password}"}
      ready: {port: "\${port}"}
    notes:
      - 地址：\${host}:\${port}
`

function newTemplate() {
  editing.value = { title: '新建软件模板', content: TEMPLATE_SKELETON }
}

async function editTemplate(id: string, copy: boolean) {
  try {
    const r = await call<{ content: string }>(['software', 'template', 'show', id])
    editing.value = { title: copy ? `复制内置模板 ${id}（保存后代替内置的；也可以改 id 另存一个）` : `编辑模板 ${id}`, content: r.content }
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

async function deleteTemplate(id: string) {
  try {
    await ElMessageBox.confirm(`删除自己的模板 ${id}？已经装好的软件不受影响。`, '删除模板', { type: 'warning' })
    await call(['software', 'template', 'delete', id])
    loadTemplates()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error((e as Error).message)
  }
}

async function importTemplate() {
  const f = await pickFile('选择软件模板（.yaml）', '软件模板', ['yaml', 'yml'])
  if (!f) return
  try {
    const t = await call<{ id: string; name: string }>(['software', 'template', 'import', f])
    ElMessage.success(`已导入模板 ${t.id}（${t.name}）`)
    loadTemplates()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// 软件安装包库
const swLib = ref<SwPackage[]>([])
const swBusy = ref(false)
const tplName = (id?: string) => (id && tpl.value?.templates.find((t) => t.id === id)?.name) || id

async function loadSwLib() {
  try {
    swLib.value = (await call<typeof swLib.value | null>(['software', 'lib', 'list'])) ?? []
  } catch {
    swLib.value = []
  }
}

async function importSoftware() {
  const f = await pickFile('选择安装包（官方的文件名不要改；自己的程序、压缩包都可以）', '安装包', ['*'])
  if (!f) return
  swBusy.value = true
  try {
    const m = await call<SwPackage>(['software', 'lib', 'import', f])
    ElMessage.success(m.template ? `已导入 ${tplName(m.template)} ${m.version}` : `已导入 ${m.file}（${m.desc}）`)
    loadSwLib()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    swBusy.value = false
  }
}

async function removeSoftware(id: string) {
  try {
    await ElMessageBox.confirm(`从本机删除 ${id}？服务器上已安装的不受影响。`, '删除安装包', { type: 'warning' })
    await call(['software', 'lib', 'remove', id])
    loadSwLib()
  } catch (e) {
    if (e !== 'cancel') ElMessage.error((e as Error).message)
  }
}

async function removeNginx(id: string) {
  try {
    await ElMessageBox.confirm(`从本机删除 ${id}？服务器上已安装的不受影响，以后需要时会重新下载。`, '删除 nginx', { type: 'warning' })
  } catch {
    return
  }
  try {
    await call(['runtime', 'remove', id])
    loadJdks()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

async function chooseJdkDir() {
  const d = await pickDir('选择存放 JDK 的目录')
  if (d && form.value) form.value.runtime.cacheDir = d
}

// ---------------------------------------------------------------- 关于

const REPO = 'https://github.com/shelbeii/ferry'

interface CliVersion {
  version: string
  proto: number
  go: string
  os: string
  arch: string
  agents: { arch: string; sha256: string; size?: string }[]
}

const app = ref<{ name: string; version: string; tauri: string }>()
const cli = ref<CliVersion>()
const aboutError = ref('')

async function loadAbout() {
  try {
    app.value = await appInfo()
    cli.value = await call<CliVersion>(['version'])
  } catch (e) {
    aboutError.value = (e as Error).message
  }
}

function aboutText(): string {
  const lines = [
    `Ferry 桌面版 ${app.value?.version ?? ''}（Tauri ${app.value?.tauri ?? ''}）`,
    `ferry 命令行 ${cli.value?.version ?? ''}，agent 协议 ${cli.value?.proto ?? ''}，${cli.value?.go ?? ''}，${cli.value?.os ?? ''}/${cli.value?.arch ?? ''}`,
    ...(cli.value?.agents ?? []).map((a) => `agent linux/${a.arch} ${a.sha256}`),
    `数据目录 ${dataDir.value?.dir ?? ''}（${modeText[dataDir.value?.mode ?? 'install']}）`,
    `系统 ${navigator.userAgent}`,
  ]
  return lines.join('\n')
}

async function copyAbout() {
  try {
    await navigator.clipboard.writeText(aboutText())
    ElMessage.success('版本信息已复制')
  } catch {
    ElMessage.error('复制失败')
  }
}

function resetForm() {
  // 响应式对象不能 structuredClone：转成普通对象再改
  const s = JSON.parse(JSON.stringify(appSettings.value)) as AppSettings
  s.mcp ??= { allowChanges: false, targets: [], logKeepDays: 30, logMaxMB: 20 } // 旧版本的设置里没有
  s.mcp.targets ??= []
  s.mcp.logKeepDays ||= 30
  s.mcp.logMaxMB ||= 20
  s.notify ??= { channels: [] }
  s.notify.channels ??= []
  s.team ??= { name: '' }
  form.value = s
  envRows.value = Object.entries(s.build.env ?? {}).map(([key, value]) => ({ key, value }))
  pathRows.value = [...(s.build.path ?? [])]
}

async function loadSecrets() {
  secretsError.value = ''
  try {
    secrets.value = (await call<SecretEntry[] | null>(['secret', 'list'])) ?? []
  } catch (e) {
    secretsError.value = (e as Error).message
  }
}

// 选主题立即生效；没保存就关掉时回到原来的（保存后 appSettings 已经是新的）
watch(
  () => form.value?.ui.theme,
  (t, old) => {
    if (t && old) applyTheme(t)
  },
)
// 液态玻璃也一样：打开、关掉马上看到效果，没保存就关掉时恢复
watch(
  () => form.value?.ui.glass,
  (on, old) => {
    if (on !== undefined && old !== undefined) glass.enabled = on
  },
)
watch(
  () => form.value?.ui.glassTransparency,
  (value, old) => {
    if (value !== undefined && old !== undefined) glass.transparency = value
  },
)
function restoreTheme() {
  if (!appSettings.value) return
  applyTheme(appSettings.value.ui.theme)
  glass.enabled = appSettings.value.ui.glass !== false
  glass.transparency = appSettings.value.ui.glassTransparency ?? GLASS_TRANSPARENCY_DEFAULT
}

async function save() {
  if (!form.value) return
  saving.value = true
  try {
    const env: Record<string, string> = {}
    for (const r of envRows.value) if (r.key.trim()) env[r.key.trim()] = r.value
    const s: AppSettings = {
      ...form.value,
      build: { ...form.value.build, env, path: pathRows.value.map((p) => p.trim()).filter(Boolean) },
      desktop: appSettings.value!.desktop, // 项目列表以当前为准
    }
    await aiModels.value?.applyKeys()
    await notifyChannels.value?.applySecrets()
    await dnsKeys.value?.applyKeys()
    await saveSettings(s)
    ElMessage.success('设置已保存')
    visible.value = false
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

// ---------------------------------------------------------------- 数据目录

async function openDataDir() {
  if (!info.value) return
  if (!(await reveal(info.value.file))) ElMessage.info(info.value.dataDir.dir)
}

async function changeDir(reset: boolean) {
  let dst = dataDir.value?.defaultDir ?? ''
  if (!reset) {
    const picked = await pickDir('选择新的数据目录')
    if (!picked) return
    dst = picked
  }
  let move = true
  try {
    await ElMessageBox.confirm(
      `数据目录改到：${dst}\n\n是否把现有数据（密钥、主机公钥记录、构建日志、设置）一起搬过去？选「只改位置」则新位置从空开始，原数据保留在原处。`,
      reset ? '恢复默认位置' : '更改数据目录',
      { confirmButtonText: '搬过去', cancelButtonText: '只改位置', distinguishCancelAndClose: true, type: 'warning' },
    )
  } catch (action) {
    if (action !== 'cancel') return
    move = false
  }
  try {
    await call(reset ? ['datadir', 'reset', ...(move ? ['--move'] : [])] : ['datadir', 'set', dst, ...(move ? ['--move'] : [])])
    await loadSettings()
    resetForm()
    loadSecrets()
    ElMessage.success('数据目录已更改')
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---------------------------------------------------------------- 凭据

async function deleteSecret(e: SecretEntry) {
  try {
    await ElMessageBox.confirm(`删除已保存的${secretKind(e.kind)}：${e.label}？`, '删除凭据', { type: 'warning' })
  } catch {
    return
  }
  try {
    await call(['secret', 'delete', '--account', e.account])
    loadSecrets()
  } catch (err) {
    ElMessage.error((err as Error).message)
  }
}

// ---------------------------------------------------------------- 构建

async function addPathDir() {
  const d = await pickDir('选择要加到 PATH 的目录')
  if (d) pathRows.value.push(d)
}

async function pickKeyDir() {
  const d = await pickDir('选择新密钥的保存目录')
  if (d && form.value) form.value.connect.keyDir = d
}
</script>

<template>
  <el-dialog v-model="visible" title="设置" width="min(1120px, 94vw)" top="4vh" class="settings-dialog" @close="restoreTheme" @closed="emit('close')">
    <div v-if="form && info" class="settings-workspace">
      <aside class="settings-navigation" aria-label="设置分类">
        <el-input v-model="settingsQuery" placeholder="搜索设置" clearable size="small" aria-label="搜索设置分类" />
        <div v-for="group in navigationGroups" :key="group.label" class="settings-group"><span>{{ group.label }}</span><button v-for="item in group.items" :key="item.name" :class="{ active: tab === item.name }" :aria-current="tab === item.name ? 'page' : undefined" @click="tab = item.name">{{ item.label }}</button></div>
        <p v-if="!navigationGroups.length" class="muted small">没有匹配的设置分类</p>
      </aside>
      <section class="settings-content">
        <header class="settings-heading"><h2>{{ currentSection.label }}</h2><p>{{ currentSection.description }}</p></header>
    <!-- 各页用 lazy：第一次切到才渲染，打开设置时不用一下子把二十几页全建出来（会卡一下） -->
    <el-tabs v-model="tab" class="tabs">
      <!-- 存储 -->
      <el-tab-pane label="存储" name="storage" lazy>
        <h4>数据目录</h4>
        <p class="muted small">设置、项目列表、密钥、构建日志等都保存在这里；升级不会动它。</p>
        <div class="dir">
          <span class="mono">{{ dataDir?.dir }}</span>
          <el-tag size="small" :type="locked ? 'warning' : 'info'">{{ modeText[dataDir?.mode ?? 'install'] }}</el-tag>
        </div>
        <div class="row">
          <el-button :icon="FolderOpened" :disabled="!isTauri" @click="openDataDir">在资源管理器中显示</el-button>
          <el-button :disabled="locked" @click="changeDir(false)">更改位置…</el-button>
          <el-button :disabled="locked || dataDir?.mode === 'install' || dataDir?.mode === 'user'" @click="changeDir(true)">恢复默认位置</el-button>
        </div>
        <el-alert v-if="dataDir?.note" type="warning" :closable="false" show-icon class="note">
          <template #title>没有用安装目录（{{ dataDir.note }}），数据放在用户目录。可以把 Ferry 装到可写的目录，或用「更改位置」指定</template>
        </el-alert>
        <p class="muted small">更改位置后需要重启 Ferry。</p>

        <h4>记录的保留</h4>
        <p class="muted small">下面这些记录都只保存在本机的数据目录里，不会上传到别处。超过保留天数或最多占用时自动删除，从最旧的删起；也可以点「按上面的设置清理」马上清理。</p>

        <h4 class="sub">构建日志</h4>
        <p class="muted small">每次部署前在本机构建时的完整输出，在数据目录的 projects/&lt;项目&gt;/logs 里，构建失败时用来查原因。每次构建完清理一次。</p>
        <el-form label-width="110px">
          <el-form-item label="保留天数">
            <el-input-number v-model="form.logs.buildLogDays" :min="0" :max="3650" />
            <span class="muted small ml">0 = 不自动清理</span>
          </el-form-item>
        </el-form>

        <h4 class="sub">任务记录</h4>
        <p class="muted small">任务面板里的任务（部署、启停、回滚、安装软件等的过程和结果），在数据目录的 tasks 里，重启 Ferry 后还能看到。每次保存任务时清理一次。</p>
        <el-form label-width="110px">
          <el-form-item label="最多保留">
            <el-input-number v-model="form.ui.taskKeep" :min="5" :max="500" />
            <span class="muted small ml">个任务，超出时从最旧的删起</span>
          </el-form-item>
          <el-form-item label="保留天数">
            <el-input-number v-model="form.ui.taskKeepDays" :min="1" :max="3650" />
            <span class="muted small ml">超过这么多天的任务自动删除</span>
          </el-form-item>
          <el-form-item label="最多占用">
            <el-input-number v-model="form.ui.taskMaxMB" :min="10" :max="102400" :step="50" />
            <span class="muted small ml">MB，超出时从最旧的任务删起</span>
          </el-form-item>
          <el-form-item>
            <div class="usage">
              <span class="muted small">{{ taskUsage ? `现有 ${taskUsage.count} 个任务记录，占用 ${bytes(taskUsage.bytes)}` : '' }}</span>
              <el-button size="small" :loading="taskCleaning" @click="cleanTasks(false)">按上面的设置清理</el-button>
              <el-button size="small" type="danger" plain :disabled="!taskUsage?.count" @click="cleanTasks(true)">全部删除</el-button>
            </div>
          </el-form-item>
        </el-form>

        <h4 class="sub">AI 对话记录</h4>
        <p class="muted small">
          AI 助手的历史对话（你的问题、回答、在服务器上执行的命令和输出），在数据目录的 ai 里，可以在「历史对话」里接着聊。每次和助手对话时清理一次。
          改动服务器的记录（ai/audit.jsonl）不随对话清理。
        </p>
        <el-form label-width="110px">
          <el-form-item label="保留天数">
            <el-input-number v-model="form.ai.keepDays" :min="1" :max="3650" />
            <span class="muted small ml">多久没再用的对话自动删除</span>
          </el-form-item>
          <el-form-item label="最多占用">
            <el-input-number v-model="form.ai.maxMB" :min="10" :max="102400" :step="50" />
            <span class="muted small ml">MB，超出时从最旧的对话删起</span>
          </el-form-item>
          <el-form-item>
            <div class="usage">
              <span class="muted small">{{ aiUsage ? `现有 ${aiUsage.count} 个对话，占用 ${bytes(aiUsage.bytes)}` : '' }}</span>
              <el-button size="small" :loading="aiCleaning" @click="cleanAi(false)">按上面的设置清理</el-button>
              <el-button size="small" type="danger" plain :disabled="!aiUsage?.count" @click="cleanAi(true)">全部删除</el-button>
            </div>
          </el-form-item>
        </el-form>

        <h4 class="sub">MCP 调用记录</h4>
        <p class="muted small">
          Claude Code、Cursor 等编程软件每次通过 MCP 调用 Ferry 的记录（调用了什么、参数、结果、改动是否同意），在数据目录的 mcp/calls.jsonl 里，可以在「命令行与 MCP」里查看。
          编程软件调用时清理：超过最多占用马上清理，超过天数的最多一小时清理一次。
        </p>
        <el-form label-width="110px">
          <el-form-item label="保留天数">
            <el-input-number v-model="form.mcp.logKeepDays" :min="1" :max="3650" />
            <span class="muted small ml">超过这么多天的记录自动删除</span>
          </el-form-item>
          <el-form-item label="最多占用">
            <el-input-number v-model="form.mcp.logMaxMB" :min="1" :max="1024" :step="10" />
            <span class="muted small ml">MB，超出时从最旧的记录删起</span>
          </el-form-item>
          <el-form-item>
            <div class="usage">
              <span class="muted small">{{ mcpUsage ? `现有 ${mcpUsage.count} 条调用记录，占用 ${bytes(mcpUsage.bytes)}` : '' }}</span>
              <el-button size="small" :loading="mcpCleaning" @click="cleanMcpLog(false)">按上面的设置清理</el-button>
              <el-button size="small" type="danger" plain :disabled="!mcpUsage?.count" @click="cleanMcpLog(true)">全部删除</el-button>
            </div>
          </el-form-item>
        </el-form>
      </el-tab-pane>

      <!-- 连接 -->
      <el-tab-pane label="连接" name="connect" lazy>
        <el-form label-width="150px">
          <el-form-item label="连接超时">
            <el-input-number v-model="form.connect.timeoutSec" :min="3" :max="300" />
            <span class="muted small ml">秒</span>
          </el-form-item>
          <el-form-item label="上传并行连接数">
            <el-input-number v-model="form.connect.uploadStreams" :min="1" :max="32" />
            <span class="muted small ml">大文件用多条连接同时上传；网络限速时可以调大</span>
          </el-form-item>
          <el-form-item label="空闲断开">
            <el-input-number v-model="form.connect.idleMinutes" :min="1" :max="1440" />
            <span class="muted small ml">分钟没有操作后断开连接</span>
          </el-form-item>
          <el-form-item label="自动读取应用状态">
            <el-select v-model="form.connect.statusMinutes" style="width: 150px">
              <el-option v-for="m in [1, 2, 5, 10, 30]" :key="m" :value="m" :label="`每 ${m} 分钟`" />
              <el-option :value="-1" label="关闭" />
            </el-select>
            <span class="muted small ml">项目页连着服务器时多久读一次；关闭后只在点刷新、操作完成后读</span>
          </el-form-item>
          <el-form-item label="首次连接的主机公钥">
            <el-radio-group v-model="form.connect.acceptNewHostKey">
              <el-radio :value="false">必须核对后记录（推荐）</el-radio>
              <el-radio :value="true">自动记录</el-radio>
            </el-radio-group>
          </el-form-item>
          <el-alert
            v-if="form.connect.acceptNewHostKey"
            type="warning"
            :closable="false"
            show-icon
            title="自动记录时，第一次连接无法发现冒充的服务器。公钥变化仍然会拒绝连接。"
            class="mb"
          />
          <el-form-item label="新密钥保存位置">
            <el-input v-model="form.connect.keyDir" :placeholder="`默认：${info.keyDir}`">
              <template #append><el-button :icon="FolderOpened" @click="pickKeyDir" /></template>
            </el-input>
          </el-form-item>
        </el-form>
      </el-tab-pane>

      <!-- 凭据 -->
      <el-tab-pane label="凭据" name="secrets" lazy>
        <p class="muted small">系统凭据库里保存的密码和密钥。点击「查看」可单独查看内容。</p>
        <el-alert v-if="secretsError" type="error" :title="secretsError" :closable="false" show-icon />
        <el-input v-model="credentialQuery" placeholder="搜索账户或凭据类型" clearable aria-label="搜索已保存的凭据" class="credential-search" />
        <el-table :data="matchingSecrets" size="small" border empty-text="没有匹配的凭据">
          <el-table-column label="类型" width="100">
            <template #default="{ row }">{{ secretKind(row.kind) }}</template>
          </el-table-column>
          <el-table-column label="账户" min-width="210">
            <template #default="{ row }"><span class="mono">{{ row.label }}</span></template>
          </el-table-column>
          <el-table-column label="保存时间" width="160">
            <template #default="{ row }">{{ time(row.savedAt) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="112" fixed="right">
            <template #default="{ row }">
              <el-button :icon="View" link type="primary" :aria-label="`查看 ${row.label}`" @click="viewedSecret = row">查看</el-button>
              <el-button :icon="Delete" link type="danger" :aria-label="`删除 ${row.label}`" @click="deleteSecret(row)" />
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- 构建 -->
      <el-tab-pane label="构建" name="build" lazy>
        <p class="muted small">构建时找不到 mvn、java 等命令时，在这里补充环境变量和 PATH。</p>
        <h4>环境变量</h4>
        <div v-for="(r, i) in envRows" :key="i" class="kv">
          <el-input v-model="r.key" placeholder="名称，如 JAVA_HOME" class="k mono" />
          <el-input v-model="r.value" placeholder="值" class="mono" />
          <el-button :icon="Delete" link type="danger" @click="envRows.splice(i, 1)" />
        </div>
        <el-button :icon="Plus" size="small" @click="envRows.push({ key: '', value: '' })">添加变量</el-button>
        <h4>加到 PATH 最前面的目录</h4>
        <div v-for="(_, i) in pathRows" :key="'p' + i" class="kv">
          <el-input v-model="pathRows[i]" class="mono" placeholder="如 D:\Maven\bin" />
          <el-button :icon="Delete" link type="danger" @click="pathRows.splice(i, 1)" />
        </div>
        <el-button :icon="Plus" size="small" @click="addPathDir">添加目录</el-button>
        <h4>输出</h4>
        <GlassToggle v-model="form.build.verbose">在任务面板里实时显示构建输出</GlassToggle>
      </el-tab-pane>

      <!-- 部署 -->
      <el-tab-pane label="部署" name="deploy" lazy>
        <el-form label-width="96px" class="team">
          <el-form-item label="你的名字">
            <el-input v-model="form.team.name" placeholder="如 张三" maxlength="40" style="width: 220px" />
            <div class="muted small full">团队里显示：部署记录的操作人、「张三正在部署」、部署通知；服务器的 deployers 名单也按它认人。不填只显示 用户@电脑名</div>
          </el-form-item>
        </el-form>
        <p class="muted small">部署对话框里各选项的默认值。</p>
        <div class="col">
          <GlassToggle v-model="form.deploy.skipBuild">默认跳过构建</GlassToggle>
          <GlassToggle v-model="form.deploy.full">默认全量上传（不使用增量）</GlassToggle>
          <GlassToggle :model-value="!form.deploy.keepPerms" @update:model-value="(v: boolean) => form && (form.deploy.keepPerms = !v)">部署时自动修正权限</GlassToggle>
          <div class="muted small">运行用户访问不了要用的目录、文件时，部署前自动补上权限，并在任务里列出改了什么</div>
        </div>
        <h4>HTTPS 证书的 DNS 密钥</h4>
        <p class="muted small">
          站点配置了 <span class="mono">static.https.dns</span>（用 DNS 验证申请证书，不要求 80 端口能从外网访问、也能申请通配符证书）时用。
          存在系统凭据库；部署时带到服务器上存一份（只有 root 能读），服务器每天续期证书时用。只给改 DNS 记录的权限。
        </p>
        <DnsKeys v-if="form" ref="dnsKeys" />
      </el-tab-pane>

      <!-- 通知 -->
      <el-tab-pane label="通知" name="notify" lazy>
        <p class="muted small">
          部署、回滚结束后把结果发到群里：项目、服务器、应用、版本、提交、操作人，失败时附上原因。命令行、桌面版、编程软件经 MCP 的部署都会发。
          构建失败、上传失败（服务器没有改动）、取消的不发。
        </p>
        <NotifyChannels v-if="form" ref="notifyChannels" :notify="form.notify" />
        <p class="muted small">
          CI 里不用设置：环境变量 <span class="mono">FERRY_NOTIFY="dingtalk:https://…"</span>（方式写 wecom、dingtalk、feishu、webhook，多个用空格分开），
          加签密钥放 <span class="mono">FERRY_NOTIFY_SECRET</span>。
        </p>
      </el-tab-pane>

      <!-- 运行环境 -->
      <el-tab-pane label="运行环境" name="runtime" lazy>
        <el-radio-group v-model="runtimeView" class="runtime-navigation" aria-label="运行资源类型"><el-radio-button value="jdk">Java JDK</el-radio-button><el-radio-button value="nginx">nginx</el-radio-button><el-radio-button value="packages">安装包</el-radio-button><el-radio-button value="templates">软件模板</el-radio-button></el-radio-group>
        <section v-show="runtimeView === 'jdk'">
        <p class="muted small">应用配置了 <span class="mono">service.runtime: &#123;java: 17&#125;</span> 时用这里的 JDK，服务器上没有会自动上传。</p>
        <el-form v-if="form" label-width="110px">
          <el-form-item label="下载源">
            <el-input v-model="form.runtime.source" class="mono" placeholder="https://api.adoptium.net（默认：Eclipse Temurin 官方）" />
            <div class="muted small">
              可以换成镜像，如 <span class="mono">https://mirrors.tuna.tsinghua.edu.cn/Adoptium/&#123;java&#125;/jdk/&#123;arch&#125;/linux/</span>
            </div>
          </el-form-item>
          <el-form-item label="本机存放">
            <div class="row">
              <el-input v-model="form.runtime.cacheDir" class="mono" :placeholder="jdks?.dir ?? '数据目录下的 runtimes'" />
              <el-button :icon="FolderOpened" @click="chooseJdkDir">选择</el-button>
            </div>
            <div class="muted small">每个 JDK 约 190 MB</div>
          </el-form-item>
        </el-form>
        <div class="jdk-bar">
          <span class="muted small">本机已有的 JDK</span>
          <span class="spacer" />
          <el-select v-model="jdkJava" size="small" style="width: 100px">
            <el-option v-for="v in [8, 11, 17, 21, 25]" :key="v" :value="v" :label="`Java ${v}`" />
          </el-select>
          <el-select v-model="jdkArch" size="small" style="width: 150px">
            <el-option value="x64" label="x64（x86_64）" />
            <el-option value="aarch64" label="aarch64（arm64）" />
          </el-select>
          <el-button size="small" @click="downloadJdk">下载</el-button>
          <el-button size="small" :loading="jdkBusy" @click="importJdk">导入压缩包…</el-button>
        </div>
        <el-alert v-if="jdkError" type="error" :title="jdkError" :closable="false" show-icon />
        <el-table :data="jdks?.jdks ?? []" size="small" border empty-text="还没有：部署时按需下载；本机不能上网时可以先导入">
          <el-table-column label="JDK" min-width="210">
            <template #default="{ row }"><span class="mono">{{ row.id }}</span></template>
          </el-table-column>
          <el-table-column label="版本" width="130">
            <template #default="{ row }">Java {{ row.java }}（{{ row.version }}）</template>
          </el-table-column>
          <el-table-column prop="arch" label="架构" width="80" />
          <el-table-column label="大小" width="90" align="right">
            <template #default="{ row }">{{ bytes(row.size) }}</template>
          </el-table-column>
          <el-table-column label="" width="60" align="center">
            <template #default="{ row }">
              <el-button :icon="Delete" link type="danger" @click="removeJdk(row.id)" />
            </template>
          </el-table-column>
        </el-table>

        </section><section v-show="runtimeView === 'nginx'">
        <h4 class="rt-title">nginx（前端网站）</h4>
        <p class="muted small">前端应用设置了 <span class="mono">static.port</span> 时用这里的 nginx 运行网站，不影响服务器上已有的 nginx。</p>
        <el-form v-if="rt" label-width="110px">
          <el-form-item label="下载源">
            <el-input v-model="rt.nginxSource" class="mono" :placeholder="`${NGINX_SOURCE}（默认）`" />
            <div class="muted small">可以换成内网镜像（nginx-binaries 格式的目录）</div>
          </el-form-item>
        </el-form>
        <div class="jdk-bar">
          <span class="muted small">本机已有的 nginx</span>
          <span class="spacer" />
          <el-select v-model="ngxSeries" size="small" style="width: 130px" filterable allow-create>
            <el-option value="stable" label="最新稳定版" />
            <el-option v-for="v in ['1.30', '1.28', '1.26']" :key="v" :value="v" :label="v" />
          </el-select>
          <el-select v-model="ngxArch" size="small" style="width: 150px">
            <el-option value="x86_64" label="x86_64（amd64）" />
            <el-option value="aarch64" label="aarch64（arm64）" />
          </el-select>
          <el-button size="small" @click="downloadNginx">下载</el-button>
          <el-button size="small" :loading="ngxBusy" @click="importNginx">导入程序…</el-button>
        </div>
        <el-table :data="jdks?.nginx ?? []" size="small" border empty-text="还没有：部署时按需下载；本机不能上网时可以先导入">
          <el-table-column label="nginx" min-width="210">
            <template #default="{ row }"><span class="mono">{{ row.id }}</span></template>
          </el-table-column>
          <el-table-column label="版本" width="130">
            <template #default="{ row }">{{ row.version }}（{{ isStable(row.version) ? '稳定版' : '开发版' }}）</template>
          </el-table-column>
          <el-table-column prop="arch" label="架构" width="80" />
          <el-table-column label="大小" width="90" align="right">
            <template #default="{ row }">{{ bytes(row.size) }}</template>
          </el-table-column>
          <el-table-column label="" width="60" align="center">
            <template #default="{ row }">
              <el-button :icon="Delete" link type="danger" @click="removeNginx(row.id)" />
            </template>
          </el-table-column>
        </el-table>

        </section><section v-show="runtimeView === 'packages'">
        <h4 class="rt-title">软件安装包</h4>
        <p class="muted small">服务器「软件」里安装时用的安装包，本机没有会先下载。</p>
        <el-form v-if="rt" label-width="110px">
          <el-form-item label="GitHub 加速">
            <el-input v-model="rt!.githubMirror" class="mono" :placeholder="'不填 = 直接从 GitHub 下载；如 https://ghfast.top/'" />
            <div class="muted small">GitHub 下载慢时填加速地址；也可以下载好后点「导入安装包」</div>
          </el-form-item>
          <el-form-item label="MySQL 下载源">
            <el-input v-model="rt!.mysqlSource" class="mono" :placeholder="'https://cdn.mysql.com（默认）'" />
          </el-form-item>
        </el-form>
        <div class="jdk-bar">
          <span class="muted small">本机已有的安装包</span>
          <span class="spacer" />
          <el-button size="small" :loading="swBusy" @click="importSoftware">导入安装包…</el-button>
        </div>
        <el-table :data="swLib" size="small" border empty-text="还没有：安装时按需下载；自己的程序、压缩包也可以导入，在「自定义安装」里用">
          <el-table-column label="安装包" min-width="200">
            <template #default="{ row }">
              <div class="mono" :title="`SHA256 ${row.sha256}`">{{ row.file }}</div>
              <div class="muted small" :title="row.source">{{ pkgOrigin(row).text }}</div>
              <div v-if="pkgOrigin(row).path" class="muted small pkg-path">{{ pkgOrigin(row).path }}</div>
            </template>
          </el-table-column>
          <el-table-column label="类型" width="140">
            <template #default="{ row }">
              <span :class="{ danger: row.format === 'binary' && !row.arch && row.desc !== '脚本' }">{{ row.desc }}</span>
            </template>
          </el-table-column>
          <el-table-column label="软件" width="110">
            <template #default="{ row }">
              <span v-if="row.template">{{ tplName(row.template) }} {{ row.version }}</span>
              <span v-else class="muted" title="不是哪个模板的官方安装包：在「导入的程序」这类装法或自定义安装里选它">没认出</span>
            </template>
          </el-table-column>
          <el-table-column label="大小" width="80" align="right">
            <template #default="{ row }">{{ bytes(row.size) }}</template>
          </el-table-column>
          <el-table-column label="" width="50" align="center">
            <template #default="{ row }">
              <el-button :icon="Delete" link type="danger" @click="removeSoftware(row.id)" />
            </template>
          </el-table-column>
        </el-table>

        </section><section v-show="runtimeView === 'templates'">
        <h4 class="rt-title">软件模板</h4>
        <p class="muted small">软件池按这些模板安装；ID 和内置的一样时代替内置的。</p>
        <div class="jdk-bar">
          <span class="muted small">共 {{ tpl?.templates.length ?? 0 }} 个</span>
          <span class="spacer" />
          <el-button size="small" @click="newTemplate">新建</el-button>
          <el-button size="small" @click="importTemplate">导入模板文件…</el-button>
          <el-button v-if="isTauri && tpl" size="small" @click="reveal(tpl.dir)">打开模板目录</el-button>
        </div>
        <el-alert v-for="e in tpl?.errors ?? []" :key="e" type="warning" :title="e" :closable="false" show-icon />
        <el-table :data="tpl?.templates ?? []" size="small" border>
          <el-table-column label="模板" min-width="150">
            <template #default="{ row }">
              {{ row.name }} <span class="muted mono small">{{ row.id }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="category" label="分类" width="90" />
          <el-table-column label="来源" width="100">
            <template #default="{ row }">{{ row.builtin ? '内置' : row.origin === 'ai' ? 'AI 写的' : row.overrides ? '自己的（代替内置）' : '自己的' }}</template>
          </el-table-column>
          <el-table-column label="装法" min-width="200">
            <template #default="{ row }"><span class="small">{{ row.variants.map((v: { label: string }) => v.label).join('；') }}</span></template>
          </el-table-column>
          <el-table-column label="" width="150" align="center">
            <template #default="{ row }">
              <el-button v-if="row.builtin" link type="primary" size="small" @click="editTemplate(row.id, true)">复制成自己的</el-button>
              <template v-else>
                <el-button link type="primary" size="small" @click="editTemplate(row.id, false)">编辑</el-button>
                <el-button link type="danger" size="small" @click="deleteTemplate(row.id)">删除</el-button>
              </template>
            </template>
          </el-table-column>
        </el-table>
        <TemplateEditor v-if="editing" :title="editing.title" :content="editing.content" @close="editing = null" @saved="loadTemplates" />
        </section>
      </el-tab-pane>

      <!-- 界面 -->
      <el-tab-pane label="AI" name="ai" lazy>
        <p class="muted small">项目和连接里的「AI 助手」用这里的模型。服务器上的命令输出、日志会发送给这个模型服务；助手改动服务器前，每一步都要你确认。</p>
        <h4>模型</h4>
        <AiModels v-if="form" ref="aiModels" :ai="form.ai" />
        <p class="muted small">
          对话记录保留多久、最多占用多少，在<el-button link type="primary" class="inline-link" @click="tab = 'storage'">「存储」</el-button>里设置。
        </p>
      </el-tab-pane>

      <el-tab-pane label="命令行与 MCP" name="cli" lazy>
        <CliMcp v-if="form" :data-dir="dataDir?.dir" :mcp="form.mcp" @goto="(t) => (tab = t)" />
      </el-tab-pane>

      <el-tab-pane label="界面" name="ui" lazy>
        <el-form v-if="form" label-width="150px">
          <el-form-item label="主题">
            <el-radio-group v-model="form.ui.theme">
              <el-radio-button value="system">跟随系统</el-radio-button>
              <el-radio-button value="light">浅色</el-radio-button>
              <el-radio-button value="dark">深色</el-radio-button>
            </el-radio-group>
          </el-form-item>
          <el-form-item label="液态玻璃">
            <div>
              <GlassToggle v-model="form.ui.glass">项目页顶部的按钮、功能切换，以及 AI 助手的标题栏和输入框用玻璃材质</GlassToggle>
              <div class="muted small">半透明、会折射背后的内容，按下时有弹性动画；电脑较慢或觉得晃眼时可以关掉，关掉后是普通按钮</div>
            </div>
          </el-form-item>
          <el-form-item v-if="form.ui.glass" label="玻璃透明度">
            <div class="glass-transparency">
              <div class="glass-slider">
                <span class="muted small">磨砂</span>
                <el-slider v-model="form.ui.glassTransparency" :min="0" :max="100" :step="1" :format-tooltip="(v: number) => `${v}%`" aria-label="玻璃透明度" />
                <span class="muted small">通透</span>
                <el-button link type="primary" size="small" :disabled="form.ui.glassTransparency === GLASS_TRANSPARENCY_DEFAULT" @click="form.ui.glassTransparency = GLASS_TRANSPARENCY_DEFAULT">恢复默认</el-button>
              </div>
              <div class="muted small">越往右越透明：列表从工具栏下面滑过时看得越清楚，拖到最右边不再磨砂，能直接看清后面的字；越往左磨砂越重、越不透，玻璃上的字更好认。拖动时马上能看到效果，默认 {{ GLASS_TRANSPARENCY_DEFAULT }}%</div>
            </div>
          </el-form-item>
          <el-form-item label="关闭窗口">
            <GlassToggle v-model="form.ui.confirmClose">有任务进行中或终端连着时先确认</GlassToggle>
          </el-form-item>
          <el-form-item label="系统通知">
            <div>
              <GlassToggle v-model="form.ui.notify">Ferry 不在前台时，用系统通知告诉我</GlassToggle>
              <div class="muted small">部署、启停等用时 10 秒以上的任务结束时，以及编程软件请求改动服务器、等你确认时；任务栏图标也会闪</div>
            </div>
          </el-form-item>
        </el-form>
        <p class="muted small">
          任务面板里的任务记录保留多少，在<el-button link type="primary" class="inline-link" @click="tab = 'storage'">「存储」</el-button>里设置。
        </p>
      </el-tab-pane>
      <!-- 关于 -->
      <el-tab-pane label="关于" name="about" lazy>
        <div class="about-head">
          <img src="/logo.png" alt="" class="about-logo" />
          <div>
            <div class="about-name">Ferry</div>
            <div class="muted">后端与前端部署工具 · 桌面版 {{ app?.version ?? '…' }}</div>
          </div>
        </div>
        <el-alert v-if="aboutError" type="error" :title="aboutError" :closable="false" show-icon class="mb" />
        <el-descriptions :column="1" border size="small" class="about-table">
          <el-descriptions-item label="桌面版">{{ app?.version ?? '…' }}<span class="muted ml">Tauri {{ app?.tauri ?? '' }}</span></el-descriptions-item>
          <el-descriptions-item label="ferry 命令行">
            <span class="mono">{{ cli?.version ?? '…' }}</span>
            <span class="muted ml">内置在桌面版中，与它同时发布</span>
          </el-descriptions-item>
          <el-descriptions-item label="agent 协议">{{ cli?.proto ?? '…' }}</el-descriptions-item>
          <el-descriptions-item label="服务器端 agent">
            <div v-for="a in cli?.agents ?? []" :key="a.arch" class="mono">
              linux/{{ a.arch }}　{{ a.sha256 }}<span v-if="a.size" class="muted">　{{ a.size }}</span>
            </div>
          </el-descriptions-item>
          <el-descriptions-item label="运行环境">{{ cli ? `${cli.go} · ${cli.os}/${cli.arch}` : '…' }}</el-descriptions-item>
          <el-descriptions-item label="数据目录"><span class="mono">{{ dataDir?.dir }}</span></el-descriptions-item>
          <el-descriptions-item label="设置文件"><span class="mono">{{ info?.file }}</span></el-descriptions-item>
        </el-descriptions>
        <div class="row about-actions">
          <el-button :icon="CopyDocument" @click="copyAbout">复制版本信息</el-button>
          <el-button :icon="Link" @click="openUrl(REPO)">项目主页</el-button>
        </div>
      </el-tab-pane>
    </el-tabs>
      </section>
    </div>
    <el-alert v-else-if="initializationError" type="error" title="设置读取失败" :closable="false" show-icon><p>{{ initializationError }}</p><el-button type="primary" size="small" @click="initialize">重新读取设置</el-button></el-alert>
    <LoadingBlock v-else text="正在读取设置…" :rows="9" min-height="320px" />
    <template #footer>
      <!-- 「关于」页没有可保存的内容 -->
      <el-button v-if="tab === 'about'" @click="visible = false">关闭</el-button>
      <template v-else>
        <span class="muted small foot" :title="info?.file">配置选项点「保存」生效；测试、查看和清理操作即时执行。</span>
        <el-button @click="visible = false">取消</el-button>
        <el-button type="primary" :disabled="!form" :loading="saving" @click="save">保存</el-button>
      </template>
    </template>
  </el-dialog>
  <CredentialDialog v-if="viewedSecret && visible" :key="viewedSecret.account" :entry="viewedSecret" @close="viewedSecret = undefined" />
</template>

<style scoped>
.team .full {
  width: 100%;
  line-height: 1.5;
  margin-top: 4px;
}
/* 固定高度（不超过窗口），内容多时只在右侧内容区滚动，不把整个窗口撑出滚动条 */
.tabs {
  min-width: 0;
}
.tabs > :deep(.el-tabs__header) { display: none; }
.tabs :deep(.el-tabs__content) {
  overflow: visible;
}
.settings-workspace { display: grid; grid-template-columns: 174px minmax(0, 1fr); height: min(620px, calc(90dvh - 220px)); min-height: 260px; gap: 28px; }
.settings-navigation { overflow-y: auto; padding-right: 16px; border-right: 1px solid var(--ferry-stroke); }
.settings-group { display: flex; flex-direction: column; gap: 3px; margin-top: 22px; }
.settings-group > span { font-size: 10px; letter-spacing: 1px; color: var(--el-text-color-placeholder); padding: 0 10px 7px; }
.settings-group button { text-align: left; padding: 10px 12px; font-size: 13px; border: 0; border-radius: 7px; color: var(--el-text-color-regular); background: transparent; cursor: pointer; }
.settings-group button:hover { background: var(--el-fill-color-light); }
.settings-group button.active { background: var(--el-color-primary-light-9); color: var(--el-color-primary); font-weight: 600; }
.settings-content { min-width: 0; overflow-y: auto; padding-right: 8px; overflow-wrap: anywhere; }
.settings-content :deep(.el-form-item__content) { min-width: 0; }
.settings-heading h2 { font-size: 24px; margin: 0 0 8px; letter-spacing: -.6px; }
.settings-heading p { color: var(--el-text-color-secondary); margin: 0 0 26px; line-height: 1.7; font-size: 13px; }
.credential-search { max-width: 320px; margin-bottom: 16px; }
.runtime-navigation { margin-bottom: 22px; }
@media (max-width: 900px) { .settings-workspace { grid-template-columns: 130px minmax(0, 1fr); gap: 16px; } .foot { display: none; } }
h4 {
  margin: 4px 0 8px;
}
h4.sub {
  margin-top: 16px;
  padding-top: 12px;
  border-top: 1px solid var(--el-border-color-lighter);
  font-size: 14px;
}
.usage {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}
.usage .muted {
  margin-right: 4px;
}
.inline-link {
  vertical-align: baseline;
  padding: 0;
  height: auto;
  font-size: inherit;
}
.small {
  font-size: 12px;
  line-height: 1.6;
}
.dir {
  display: flex;
  align-items: center;
  gap: 8px;
  background: var(--el-fill-color-light);
  border-radius: 6px;
  padding: 8px 10px;
  margin-bottom: 8px;
  word-break: break-all;
}
.row {
  display: flex;
  gap: 8px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.kv {
  display: flex;
  gap: 8px;
  align-items: center;
  margin-bottom: 6px;
}
.kv .k {
  width: 220px;
  flex-shrink: 0;
}
.col {
  display: flex;
  flex-direction: column;
}
.ml {
  margin-left: 8px;
}
.mb {
  margin-bottom: 12px;
}
.about-head {
  display: flex;
  align-items: center;
  gap: 14px;
  margin-bottom: 14px;
}
.about-logo {
  width: 52px;
  height: 52px;
}
.about-name {
  font-size: 22px;
  font-weight: 600;
}
.about-table {
  margin-bottom: 12px;
}
.about-table :deep(.el-descriptions__table) { table-layout: fixed; }
.about-table :deep(.el-descriptions__content) { overflow-wrap: anywhere; }
.about-table :deep(.el-descriptions__label) {
  width: 120px;
}
.about-actions {
  margin-top: 4px;
}
.foot {
  margin-right: 12px;
}
.jdk-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 4px 0 8px;
  flex-wrap: wrap;
}
.jdk-bar .spacer {
  flex: 1;
}
.pkg-path {
  word-break: break-all;
}
.danger {
  color: var(--el-color-danger);
}
.rt-title {
  margin: 20px 0 4px;
  font-size: 14px;
}
.glass-transparency {
  width: 100%;
}
.glass-slider {
  display: flex;
  align-items: center;
  gap: 12px;
  max-width: 460px;
}
.glass-slider .el-slider {
  flex: 1;
}
</style>
