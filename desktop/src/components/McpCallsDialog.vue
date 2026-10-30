<script setup lang="ts">
// 编程软件经 MCP 的调用记录（设置 → 命令行与 MCP → 查看调用记录）：筛选、分页。
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { FolderOpened, Refresh } from '@element-plus/icons-vue'
import { call, time } from '../ferry'
import { reveal } from '../platform'

const props = defineProps<{ dataDir?: string }>()
const emit = defineEmits<{ close: [] }>()
const visible = ref(true)

interface CallEntry {
  time: string
  client?: string
  tool: string
  op?: string
  args?: Record<string, unknown>
  ok: boolean
  error?: string
  decision?: string
  ms: number
}

/** 改动结果那条记录在筛选里的「工具」（与 Go 的 mcpserver.ResultTool 一致） */
const RESULT_TOOL = '（结果）'

const TOOL_NAMES: Record<string, string> = {
  list_projects: '列出项目',
  app_status: '应用状态',
  app_logs: '应用日志',
  app_releases: '应用版本',
  deploy_history: '部署记录',
  server_software: '服务器软件',
  server_ports: '端口和防火墙',
  ssh_sessions: 'SSH 连接',
  server_check: '测试连接',
  port_check: '测试端口',
  preflight: '部署前检查',
  software_logs: '软件日志',
  run_command: '只读命令',
  build_log: '构建日志',
  detect_project: '识别项目',
  deploy_changes: '部署会带上线的改动',
  monitor_status: '服务器告警',
  backup_status: '数据备份',
  task_status: '查进展',
  deploy: '请求部署',
  app_control: '请求启停',
  rollback: '请求回滚',
  software_control: '请求启停软件',
  port_control: '请求放行 / 关闭端口',
  firewall_enable: '请求打开防火墙',
  ssh_disconnect: '请求断开 SSH',
  backup_run: '请求现在备份',
  create_project: '请求新建项目',
  [RESULT_TOOL]: '改动的结果',
}

const toolName = (t: string) => TOOL_NAMES[t] ?? t
const toolLabel = (c: CallEntry) => toolName(c.tool.endsWith(RESULT_TOOL) ? RESULT_TOOL : c.tool)

/** 参数里给人看的部分：项目、服务器、应用、命令…… */
function argsText(c: CallEntry) {
  const a = c.args ?? {}
  const parts: string[] = []
  const spec = a.spec as { project?: string } | undefined
  if (spec?.project) parts.push(`项目 ${spec.project}`)
  for (const k of ['project', 'target', 'app', 'apps', 'action', 'name', 'command', 'keyword', 'dir', 'id']) {
    const v = a[k]
    if (v === undefined || v === '' || (Array.isArray(v) && !v.length)) continue
    parts.push(Array.isArray(v) ? v.join('、') : String(v))
  }
  return parts.join(' · ')
}

function resultText(c: CallEntry) {
  if (c.decision) return c.ok ? `${c.decision} · 成功` : `${c.decision}${c.error ? ' · ' + c.error : ''}`
  return c.ok ? '成功' : c.error || '失败'
}

// ---------------------------------------------------------------- 筛选

const range = ref<[Date, Date] | null>(null)
const client = ref('')
const tools = ref<string[]>([])
const result = ref<'' | 'ok' | 'fail'>('')
const changes = ref(false)
const keyword = ref('')

const filtered = computed(() => !!(range.value || client.value || tools.value.length || result.value || changes.value || keyword.value.trim()))

function resetFilters() {
  range.value = null
  client.value = ''
  tools.value = []
  result.value = ''
  changes.value = false
  keyword.value = ''
}

const day = 24 * 3600 * 1000
const shortcuts = [
  { text: '最近 1 小时', value: () => [new Date(Date.now() - 3600 * 1000), new Date()] },
  { text: '今天', value: () => [new Date(new Date().setHours(0, 0, 0, 0)), new Date()] },
  { text: '最近 7 天', value: () => [new Date(Date.now() - 7 * day), new Date()] },
  { text: '最近 30 天', value: () => [new Date(Date.now() - 30 * day), new Date()] },
]

// ---------------------------------------------------------------- 列表

const rows = ref<CallEntry[]>([])
const total = ref(0)
const allCount = ref(0)
const clients = ref<string[]>([])
const toolOptions = ref<string[]>([])
const loading = ref(false)
const error = ref('')
const page = ref(1)
const pageSize = ref(50)

function args(): string[] {
  const a = ['mcp-log', '--offset', String((page.value - 1) * pageSize.value), '--limit', String(pageSize.value)]
  if (range.value) a.push('--since', range.value[0].toISOString(), '--until', range.value[1].toISOString())
  if (client.value) a.push('--client', client.value)
  if (tools.value.length) a.push('--tool', tools.value.join(','))
  if (result.value) a.push('--result', result.value)
  if (changes.value) a.push('--changes')
  if (keyword.value.trim()) a.push('--q', keyword.value.trim())
  return a
}

let seq = 0
async function load() {
  const my = ++seq
  loading.value = true
  try {
    const r = await call<{ items: CallEntry[]; total: number; count: number; clients: string[]; tools: string[] }>(args())
    if (my !== seq) return
    rows.value = r.items
    total.value = r.total
    allCount.value = r.count
    clients.value = r.clients
    toolOptions.value = r.tools
    error.value = ''
    // 记录被清理后当前页可能已经没有了：回到最后一页
    const last = Math.max(1, Math.ceil(r.total / pageSize.value))
    if (page.value > last) page.value = last
  } catch (e) {
    if (my !== seq) return
    error.value = (e as Error).message
    rows.value = []
    total.value = 0
  } finally {
    if (my === seq) loading.value = false
  }
}
onMounted(load)

// 筛选条件变了回到第一页；关键字停下输入再查
let timer: ReturnType<typeof setTimeout> | undefined
watch([range, client, tools, result, changes], () => (page.value === 1 ? load() : (page.value = 1)), { deep: true })
watch(keyword, () => {
  clearTimeout(timer)
  timer = setTimeout(() => (page.value === 1 ? load() : (page.value = 1)), 300)
})
watch([page, pageSize], load)

const logDir = computed(() => (props.dataDir ? `${props.dataDir}${props.dataDir.includes('\\') ? '\\' : '/'}mcp` : ''))

async function openDir() {
  try {
    await reveal(logDir.value)
  } catch {
    ElMessage.info('还没有调用记录')
  }
}

const pretty = (v: unknown) => JSON.stringify(v, null, 2)
</script>

<template>
  <el-dialog v-model="visible" title="MCP 调用记录" width="90%" top="4vh" append-to-body @closed="emit('close')">
    <div class="filters">
      <el-date-picker
        v-model="range"
        type="datetimerange"
        size="small"
        :shortcuts="shortcuts"
        range-separator="到"
        start-placeholder="开始时间"
        end-placeholder="结束时间"
        class="range"
      />
      <el-select v-model="client" size="small" clearable placeholder="全部编程软件" class="client">
        <el-option v-for="c in clients" :key="c" :value="c" :label="c" />
      </el-select>
      <el-select v-model="tools" size="small" multiple collapse-tags collapse-tags-tooltip clearable placeholder="全部操作" class="tools">
        <el-option v-for="t in toolOptions" :key="t" :value="t" :label="toolName(t)" />
      </el-select>
      <el-radio-group v-model="result" size="small">
        <el-radio-button value="">全部</el-radio-button>
        <el-radio-button value="ok">成功</el-radio-button>
        <el-radio-button value="fail">失败</el-radio-button>
      </el-radio-group>
      <el-checkbox v-model="changes" size="small">只看改动服务器的</el-checkbox>
      <el-input v-model="keyword" size="small" clearable placeholder="搜索项目、服务器、应用、命令、错误…" class="keyword" />
      <el-button size="small" :disabled="!filtered" @click="resetFilters">清空筛选</el-button>
      <span class="spacer" />
      <el-button size="small" :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
    </div>
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="mb" />
    <el-table
      v-loading="loading"
      :data="rows"
      size="small"
      border
      height="clamp(160px, calc(100dvh - 300px), 62vh)"
      :empty-text="filtered ? '没有符合筛选的记录' : '还没有编程软件调用过'"
    >
      <el-table-column type="expand" width="32">
        <template #default="{ row }">
          <div class="detail">
            <div v-if="row.op" class="muted small">改动 ID：<span class="mono">{{ row.op }}</span></div>
            <div v-if="row.decision" class="muted small">确认：{{ row.decision }}</div>
            <div v-if="row.error" class="fail small">错误：{{ row.error }}</div>
            <div class="muted small">耗时 {{ row.ms }} ms</div>
            <pre v-if="row.args" class="mono args">{{ pretty(row.args) }}</pre>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="时间" width="160">
        <template #default="{ row }">{{ time(row.time) }}</template>
      </el-table-column>
      <el-table-column label="编程软件" width="170" show-overflow-tooltip>
        <template #default="{ row }">{{ row.client || '-' }}</template>
      </el-table-column>
      <el-table-column label="做了什么" width="120">
        <template #default="{ row }">{{ toolLabel(row) }}</template>
      </el-table-column>
      <el-table-column label="内容" min-width="220" show-overflow-tooltip>
        <template #default="{ row }">{{ argsText(row) }}</template>
      </el-table-column>
      <el-table-column label="结果" min-width="200" show-overflow-tooltip>
        <template #default="{ row }">
          <span :class="row.ok ? '' : 'fail'">{{ resultText(row) }}</span>
        </template>
      </el-table-column>
    </el-table>
    <div class="foot">
      <span class="muted small">
        <template v-if="filtered">符合筛选 {{ total }} 条，</template>共 {{ allCount }} 条记录。点行首的箭头看完整参数。
        <template v-if="logDir">
          记录文件在 <span class="mono">{{ logDir }}</span>
          <el-button link :icon="FolderOpened" @click="openDir" />
        </template>
      </span>
      <span class="spacer" />
      <el-pagination
        v-model:current-page="page"
        v-model:page-size="pageSize"
        :total="total"
        :page-sizes="[20, 50, 100, 200]"
        layout="sizes, prev, pager, next"
        size="small"
      />
    </div>
  </el-dialog>
</template>

<style scoped>
.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
}
.filters .range {
  width: 340px;
  flex: none;
}
.filters .client {
  width: 170px;
}
.filters .tools {
  width: 190px;
}
.filters .keyword {
  width: 240px;
}
.spacer {
  flex: 1;
}
.mb {
  margin-bottom: 8px;
}
.small {
  font-size: 12px;
}
.fail {
  color: var(--el-color-danger);
}
.detail {
  padding: 4px 16px 8px 48px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.args {
  margin: 6px 0 0;
  padding: 8px 10px;
  font-size: 12px;
  background: var(--el-fill-color-light);
  border-radius: 6px;
  white-space: pre-wrap;
  word-break: break-all;
}
.foot {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 10px;
  flex-wrap: wrap;
}
.foot > span { min-width: 0; overflow-wrap: anywhere; }
</style>
