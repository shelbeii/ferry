<script setup lang="ts">
// 设置 → 命令行与 MCP：把 ferry 加进 PATH（Windows），以及编程软件接入 Ferry MCP 服务的配置。
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { CopyDocument, Document, Tools } from '@element-plus/icons-vue'
import { call, type Inspect } from '../ferry'
import { projects } from '../state'
import McpCallsDialog from './McpCallsDialog.vue'
import McpToolsDialog from './McpToolsDialog.vue'
import GlassSwitch from './glass/GlassSwitch.vue'
import GlassToggle from './glass/GlassToggle.vue'

const props = defineProps<{ dataDir?: string; mcp: { allowChanges: boolean; targets: string[] | null } }>()
const emit = defineEmits<{ goto: [tab: string] }>()

// ---------------------------------------------------------------- 允许改动的目标

interface TargetRow {
  key: string // 项目/目标（项目是 ferry.yaml 里的 project）
  project: string
  target: string
  host: string
  blocked: string // 不能选的原因（生产服务器……）
}

const scope = ref<'all' | 'selected'>(props.mcp.targets?.length ? 'selected' : 'all')
const rows = ref<TargetRow[]>([])
const rowsError = ref('')
const loadingRows = ref(false)

async function loadTargets() {
  if (rows.value.length || loadingRows.value) return
  loadingRows.value = true
  const out: TargetRow[] = []
  const errs: string[] = []
  for (const p of projects.value) {
    try {
      const info = await call<Inspect>(['inspect', '-c', p.path])
      for (const t of info.targets ?? []) {
        out.push({
          key: `${info.project}/${t.name}`,
          project: p.name || info.project,
          target: t.name,
          host: t.host,
          blocked: t.production ? '生产服务器' : t.aiChanges === false ? 'aiChanges: false' : '',
        })
      }
    } catch (e) {
      errs.push(`${p.name}：${(e as Error).message}`)
    }
  }
  rows.value = out
  rowsError.value = errs.join('；')
  loadingRows.value = false
}

watch(
  () => [props.mcp.allowChanges, scope.value] as const,
  ([on, sc]) => {
    if (on && sc === 'selected') loadTargets()
    if (sc === 'all') props.mcp.targets = []
  },
  { immediate: true },
)

function toggleTarget(key: string, on: boolean) {
  const list = new Set(props.mcp.targets ?? [])
  if (on) list.add(key)
  else list.delete(key)
  props.mcp.targets = [...list]
}

interface PathStatus {
  supported: boolean
  exe: string
  dir: string
  inPath: boolean
}

const status = ref<PathStatus>()
const error = ref('')
const busy = ref(false)

async function load() {
  try {
    status.value = await call<PathStatus>(['path', 'status'])
    error.value = ''
  } catch (e) {
    error.value = (e as Error).message
  }
}
onMounted(load)

async function setPath(add: boolean) {
  busy.value = true
  try {
    status.value = await call<PathStatus>(['path', add ? 'add' : 'remove'])
    ElMessage.success(add ? '已加入 PATH：重新打开命令行窗口后可以直接输入 ferry' : '已从 PATH 移除')
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    busy.value = false
  }
}

async function copy(text: string, what: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success(`${what}已复制`)
  } catch {
    ElMessage.error('复制失败')
  }
}

// ---------------------------------------------------------------- MCP

const exe = computed(() => status.value?.exe ?? 'ferry')
const tool = ref('claude')

const json = (v: unknown) => JSON.stringify(v, null, 2)
// TOML 的单引号字符串里反斜杠不转义
const tomlStr = (s: string) => (s.includes("'") ? JSON.stringify(s) : `'${s}'`)
const shellArg = (s: string) => (/[\s"]/.test(s) ? `"${s}"` : s)

const tools = computed(() => {
  const e = exe.value
  const servers = { mcpServers: { ferry: { command: e, args: ['mcp'] } } }
  return [
    {
      id: 'claude',
      label: 'Claude Code',
      where: '在终端执行（--scope user 对所有项目生效；去掉它只对当前项目生效）',
      lang: 'shell',
      text: `claude mcp add ferry --scope user -- ${shellArg(e)} mcp`,
    },
    {
      id: 'cursor',
      label: 'Cursor',
      where: '写进 ~/.cursor/mcp.json（所有项目）或项目里的 .cursor/mcp.json；已有 mcpServers 时只加 ferry 这一项',
      lang: 'json',
      text: json(servers),
    },
    {
      id: 'vscode',
      label: 'VS Code',
      where: '写进项目里的 .vscode/mcp.json（GitHub Copilot 的代理模式使用），或命令面板「MCP: Add Server」',
      lang: 'json',
      text: json({ servers: { ferry: { type: 'stdio', command: e, args: ['mcp'] } } }),
    },
    {
      id: 'codex',
      label: 'Codex',
      where: '写进 ~/.codex/config.toml',
      lang: 'toml',
      text: `[mcp_servers.ferry]\ncommand = ${tomlStr(e)}\nargs = ["mcp"]`,
    },
    {
      id: 'other',
      label: '其他',
      where: 'Claude Desktop、Windsurf、Cline、Trae、通义灵码等大多用这种格式，写进它们的 MCP 配置文件',
      lang: 'json',
      text: json(servers),
    },
  ]
})
const current = computed(() => tools.value.find((t) => t.id === tool.value)!)

// ---------------------------------------------------------------- 调用记录（点按钮在单独的窗口里看）

const showCalls = ref(false)
const showTools = ref(false)
const callCount = ref<number>()

async function loadCallCount() {
  try {
    callCount.value = (await call<{ count: number }>(['mcp-log', '--limit', '1'])).count
  } catch {
    callCount.value = undefined
  }
}
onMounted(loadCallCount)
</script>

<template>
  <div>
    <h4 class="first">命令行</h4>
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="mb" />
    <el-descriptions v-if="status" :column="1" border size="small" label-width="96px">
      <el-descriptions-item label="ferry 程序">
        <span class="mono">{{ status.exe }}</span>
        <el-button link :icon="CopyDocument" class="ml" @click="copy(status.exe, '路径')" />
      </el-descriptions-item>
      <el-descriptions-item label="PATH">
        <template v-if="status.supported">
          <el-tag v-if="status.inPath" type="success" size="small" disable-transitions>已加入</el-tag>
          <el-tag v-else type="info" size="small" disable-transitions>未加入</el-tag>
          <el-button v-if="status.inPath" size="small" class="ml" :loading="busy" @click="setPath(false)">移除</el-button>
          <el-button v-else size="small" type="primary" class="ml" :loading="busy" @click="setPath(true)">加入 PATH</el-button>
        </template>
        <span v-else class="muted">这个系统上请自己把 <span class="mono">{{ status.dir }}</span> 加进 PATH</span>
      </el-descriptions-item>
    </el-descriptions>
    <p class="muted small">
      加入 PATH 后，在 cmd、PowerShell 里可以直接输入 <span class="mono">ferry status 目标</span> 这样的命令（只改当前用户的设置，重新打开的命令行窗口才生效）。命令行用的数据（已信任的服务器、密钥、设置）和桌面版是同一份。
    </p>

    <h4>MCP：让编程软件使用 Ferry</h4>
    <p class="muted small">
      Claude Code、Cursor 等编程软件接入后，它的 AI 可以查看 Ferry 里的项目、服务器上应用的状态、日志、版本、部署记录、已装的软件、端口和登录用户，帮你排查线上问题。
      返回的内容会先去掉密码、密钥等敏感信息。不能执行任意命令。
    </p>
    <div class="switch-row">
      <GlassSwitch v-model="props.mcp.allowChanges" aria-label="允许编程软件请求改动" />
      <span>允许编程软件请求部署、启停、回滚等改动</span>
    </div>
    <p class="muted small">
      打开后，编程软件可以请求部署、启停、回滚、启停软件、放行 / 关闭端口、打开防火墙、断开 SSH 连接，但每次都会在 Ferry 里弹出确认，你点「同意执行」才做，由 Ferry 执行（任务面板里能看到、能停止）；5 分钟没确认自动拒绝，Ferry 没打开时一律不做。
      生产服务器（配置里 <span class="mono">production: true</span>）和设了 <span class="mono">aiChanges: false</span> 的服务器始终不允许。改完点「保存」生效。
    </p>
    <div v-if="props.mcp.allowChanges" class="scope">
      <el-radio-group v-model="scope" size="small">
        <el-radio value="all">全部非生产服务器</el-radio>
        <el-radio value="selected">只允许下面打开的服务器</el-radio>
      </el-radio-group>
      <div v-if="scope === 'selected'" class="targets">
        <div v-if="loadingRows" class="muted small">正在读取项目…</div>
        <div v-else-if="!rows.length" class="muted small">没有可选的服务器</div>
        <div v-for="r in rows" :key="r.key" class="target-row">
          <GlassToggle
            :model-value="!r.blocked && (props.mcp.targets ?? []).includes(r.key)"
            :disabled="!!r.blocked"
            @update:model-value="(v: boolean) => toggleTarget(r.key, v)"
          >
            {{ r.project }} / {{ r.target }}
          </GlassToggle>
          <span class="muted mono small">{{ r.host }}</span>
          <el-tag v-if="r.blocked" size="small" type="info" disable-transitions>{{ r.blocked }}，不允许</el-tag>
        </div>
        <div v-if="rowsError" class="muted small">{{ rowsError }}</div>
        <div v-if="scope === 'selected' && !(props.mcp.targets ?? []).length && rows.length" class="warn small">
          一个都没打开时等于全部非生产服务器
        </div>
      </div>
    </div>
    <el-radio-group v-model="tool" size="small" class="mb">
      <el-radio-button v-for="t in tools" :key="t.id" :value="t.id">{{ t.label }}</el-radio-button>
    </el-radio-group>
    <div class="muted small where">{{ current.where }}</div>
    <div class="snippet">
      <pre class="mono">{{ current.text }}</pre>
      <el-button class="copy" size="small" :icon="CopyDocument" @click="copy(current.text, '配置')">复制</el-button>
    </div>
    <p class="muted small">
      配置里用的是 ferry 程序的完整路径，不加入 PATH 也能用。Ferry 升级后路径不变，不用重新配置；换了安装位置要改一下。
      编程软件在项目目录里打开时，默认查看这个目录的 ferry.yaml；也可以直接说项目名（Ferry 项目列表里的名称）。
    </p>
    <h4>调用记录</h4>
    <p class="muted small">
      编程软件每次调用 Ferry 的工具、参数、结果，以及改动是否同意，都记在本机。
      保留多少天、最多占用多少，在<el-button link type="primary" class="inline-link" @click="emit('goto', 'storage')">「存储」</el-button>里设置。
    </p>
    <div class="calls-row">
      <el-button :icon="Document" @click="showCalls = true">查看调用记录</el-button>
      <el-button :icon="Tools" class="no-ml" @click="showTools = true">MCP 工具列表</el-button>
      <span v-if="callCount !== undefined" class="muted small">{{ callCount ? `现有 ${callCount} 条调用记录` : '还没有编程软件调用过' }}</span>
    </div>
    <McpCallsDialog v-if="showCalls" :data-dir="dataDir" @close="(showCalls = false), loadCallCount()" />
    <McpToolsDialog v-if="showTools" :allow-changes="props.mcp.allowChanges" @close="showTools = false" />
  </div>
</template>

<style scoped>
h4 {
  margin: 18px 0 8px;
}
h4.first {
  margin-top: 4px;
}
.mb {
  margin-bottom: 8px;
}
.ml {
  margin-left: 8px;
}
.small {
  font-size: 12px;
}
.where {
  margin-bottom: 6px;
}
.calls-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.no-ml {
  margin-left: 0;
}
.inline-link {
  vertical-align: baseline;
  padding: 0;
  height: auto;
  font-size: inherit;
}
.scope {
  margin: 0 0 10px 2px;
}
.targets {
  margin-top: 6px;
  padding: 6px 10px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  max-height: 200px;
  overflow: auto;
}
.target-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.warn {
  color: var(--el-color-warning);
}
.switch-row {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 4px 0 2px;
}
.snippet {
  position: relative;
  border: 1px solid var(--el-border-color);
  border-radius: 6px;
  background: var(--el-fill-color-light);
}
.snippet pre {
  margin: 0;
  padding: 10px 12px;
  padding-right: 80px;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
.snippet .copy {
  position: absolute;
  top: 8px;
  right: 8px;
}
</style>
