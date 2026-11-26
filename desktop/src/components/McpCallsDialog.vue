<script setup lang="ts">
// 编程软件经 MCP 的调用记录（设置 → 命令行与 MCP → 查看调用记录）：筛选、分页。
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { FolderOpened, Refresh } from '@element-plus/icons-vue'
import { call, time } from '../ferry'
import { reveal } from '../platform'
import GlassButton from './glass/GlassButton.vue'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'

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
const CLIENT_CONFIRM_TOOL = '（编程软件里确认）'

const TOOL_NAMES: Record<string, string> = {
  list_projects: '列出项目',
  app_status: '应用状态',
  app_secrets: '应用密钥状态',
  app_logs: '应用日志',
  app_log_runs: '历史运行日志',
  app_info: '应用详情',
  app_releases: '应用版本',
  deploy_history: '部署记录',
  audit_log: '审计记录',
  server_info: '服务器信息',
  server_software: '服务器软件',
  server_ports: '端口和防火墙',
  ssh_sessions: 'SSH 连接',
  server_check: '测试连接',
  port_check: '测试端口',
  preflight: '部署前检查',
  software_logs: '软件日志',
  software_templates: '软件模板库',
  template_guide: '软件模板格式',
  keep_scan: '保留文件扫描',
  files_list: '列目录',
  files_preview: '预览文件',
  files_size: '统计大小',
  files_access: '检查文件权限',
  files_download: '下载文件',
  run_command: '只读命令',
  build_log: '构建日志',
  detect_project: '识别项目',
  deploy_changes: '部署会带上线的改动',
  monitor_status: '服务器告警',
  backup_status: '数据备份',
  task_status: '查进展',
  task_cancel: '停止任务',
  config_schema: '配置项的说明',
  config_get: '查看配置',
  config_check: '检查配置',
  config_edit: '请求改配置',
  db_connections: '数据库连接',
  db_schema: '数据库结构',
  db_query: '数据库查询',
  db_execute: '请求改数据库',
  redis_query: 'Redis 查询',
  redis_execute: '请求改 Redis',
  deploy: '请求部署',
  app_control: '请求启停',
  rollback: '请求回滚',
  software_control: '请求启停软件',
  software_install: '请求装软件',
  software_upgrade: '请求升级软件',
  software_restore: '请求恢复软件',
  software_backup: '请求设置备份',
  save_software_template: '请求保存模板',
  port_control: '请求放行 / 关闭端口',
  firewall_enable: '请求打开防火墙',
  ssh_disconnect: '请求断开 SSH',
  backup_run: '请求现在备份',
  backup_fetch: '请求取回异地的备份',
  run_job: '请求现在运行定时任务',
  run_change_command: '请求执行命令',
  write_file: '请求写文件',
  files_upload: '请求上传文件',
  ask_secret: '请求填写密码',
  create_project: '请求新建项目',
  confirm_change: '两段式确认执行',
  [CLIENT_CONFIRM_TOOL]: '编程软件里确认',
  [RESULT_TOOL]: '改动的结果',
}

const toolName = (t: string) => TOOL_NAMES[t] ?? t
const toolLabel = (c: CallEntry) =>
  toolName(c.tool.endsWith(RESULT_TOOL) ? RESULT_TOOL : c.tool.endsWith(CLIENT_CONFIRM_TOOL) ? CLIENT_CONFIRM_TOOL : c.tool)

/** 参数里给人看的部分：项目、服务器、应用、命令…… */
function argsText(c: CallEntry) {
  const a = c.args ?? {}
  const parts: string[] = []
  const spec = a.spec as { project?: string } | undefined
  if (spec?.project) parts.push(`项目 ${spec.project}`)
  for (const k of ['project', 'target', 'app', 'apps', 'action', 'name', 'command', 'keyword', 'dir', 'id', 'path', 'query', 'scope']) {
    const v = a[k]
    if (v === undefined || v === '' || (Array.isArray(v) && !v.length)) continue
    parts.push(Array.isArray(v) ? v.join('、') : String(v))
  }
  return parts.join(' · ')
}

const channelBadge = (c: CallEntry) => {
  const ch = typeof c.args?.channel === 'string' ? c.args.channel : ''
  if (ch === 'mcp-app') return 'MCP 卡片'
  if (ch === 'elicitation') return 'MCP 表单'
  if (ch === 'two-step') return '两段式'
  if (ch === 'desktop') return '桌面弹窗'
  if (!ch && c.decision && c.tool.endsWith(RESULT_TOOL)) return '桌面弹窗'
  return ''
}

function filterByOp(op?: string) {
  if (!op) return
  keyword.value = op
}

function resultText(c: CallEntry) {
  if (c.decision) return c.ok ? `${c.decision} · 成功` : `${c.decision}${c.error ? ' · ' + c.error : ''}`
  return c.ok ? '成功' : c.error || '失败'
}

// ---------------------------------------------------------------- 筛选

const range = ref<[Date, Date] | null>(null)
const client = ref('')
const tools = ref<string[]>([])
const channel = ref('')
const result = ref<'' | 'ok' | 'fail'>('')
const changes = ref(false)
const groupByOp = ref(false)
const keyword = ref('')

const CHANNEL_OPTIONS = [
  { value: 'desktop', label: '桌面弹窗' },
  { value: 'mcp-app', label: 'MCP 卡片' },
  { value: 'elicitation', label: 'MCP 表单' },
  { value: 'two-step', label: '两段式' },
]

const filtered = computed(() => !!(range.value || client.value || tools.value.length || channel.value || result.value || changes.value || keyword.value.trim()))

function resetFilters() {
  range.value = null
  client.value = ''
  tools.value = []
  channel.value = ''
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

interface DisplayRow extends CallEntry {
  _key: string
  _isGroup?: boolean
  _stepCount?: number
  children?: DisplayRow[]
}

const rows = ref<CallEntry[]>([])
const total = ref(0)
const allCount = ref(0)
const clients = ref<string[]>([])
const toolOptions = ref<string[]>([])
const loading = ref(false)
const error = ref('')
const page = ref(1)
const pageSize = ref(50)

const displayRows = computed<DisplayRow[]>(() => {
  const flat: DisplayRow[] = rows.value.map((r, i) => ({ ...r, _key: `r-${i}-${r.time}-${r.tool}` }))
  if (!groupByOp.value) return flat

  const byOp = new Map<string, CallEntry[]>()
  for (const r of rows.value) {
    if (!r.op) continue
    const list = byOp.get(r.op) ?? []
    list.push(r)
    byOp.set(r.op, list)
  }

  const out: DisplayRow[] = []
  const emitted = new Set<string>()
  for (let i = 0; i < rows.value.length; i++) {
    const r = rows.value[i]
    if (!r.op) {
      out.push({ ...r, _key: `r-${i}-${r.time}-${r.tool}` })
      continue
    }
    const list = byOp.get(r.op) ?? []
    if (list.length < 2) {
      out.push({ ...r, _key: `r-${i}-${r.time}-${r.tool}` })
      continue
    }
    if (emitted.has(r.op)) continue
    emitted.add(r.op)
    // list 本身是新的在前：最新（通常是结果）作汇总行，展开后按时间正序显示每一步（请求 → 确认 → 结果）
    const latest = list[0]
    const resultRow = list.find((x) => x.tool.endsWith(RESULT_TOOL)) ?? latest
    const requestRow = [...list].reverse().find((x) => !x.tool.endsWith(RESULT_TOOL) && !x.tool.endsWith(CLIENT_CONFIRM_TOOL)) ?? latest
    const mergedArgs: Record<string, unknown> = { ...(requestRow.args ?? {}), ...(resultRow.args ?? {}) }
    if (!mergedArgs.channel) {
      const chRow = list.find((x) => typeof x.args?.channel === 'string' && x.args.channel)
      if (chRow?.args?.channel) mergedArgs.channel = chRow.args.channel
    }
    const orderedChildren: DisplayRow[] = [...list].reverse().map((c, idx) => ({
      ...c,
      _key: `op-${r.op}-c-${idx}-${c.time}-${c.tool}`,
    }))
    out.push({
      ...resultRow,
      client: resultRow.client || requestRow.client,
      tool: requestRow.tool,
      args: mergedArgs,
      _key: `op-${r.op}`,
      _isGroup: true,
      _stepCount: list.length,
      children: orderedChildren,
    })
  }
  return out
})

function args(): string[] {
  const a = ['mcp-log', '--offset', String((page.value - 1) * pageSize.value), '--limit', String(pageSize.value)]
  if (range.value) a.push('--since', range.value[0].toISOString(), '--until', range.value[1].toISOString())
  if (client.value) a.push('--client', client.value)
  if (tools.value.length) a.push('--tool', tools.value.join(','))
  if (channel.value) a.push('--channel', channel.value)
  if (result.value) a.push('--result', result.value)
  if (changes.value) a.push('--changes')
  if (keyword.value.trim()) a.push('--q', keyword.value.trim())
  return a
}

let seq = 0
const reads = useReadRequests()
async function load() {
  const my = ++seq
  reads.cancelAll()
  loading.value = true
  try {
    const r = await reads.run('calls', s => readCall<{ items: CallEntry[]; total: number; count: number; clients: string[]; tools: string[] }>(args(), s))
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
    if (my !== seq || e instanceof ReadCanceled) return
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
onBeforeUnmount(() => { seq++; clearTimeout(timer) })
watch([range, client, tools, channel, result, changes], () => (page.value === 1 ? load() : (page.value = 1)), { deep: true })
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
      <el-select v-model="channel" size="small" clearable placeholder="全部确认渠道" class="channel">
        <el-option v-for="o in CHANNEL_OPTIONS" :key="o.value" :value="o.value" :label="o.label" />
      </el-select>
      <el-radio-group v-model="result" size="small">
        <el-radio-button value="">全部</el-radio-button>
        <el-radio-button value="ok">成功</el-radio-button>
        <el-radio-button value="fail">失败</el-radio-button>
      </el-radio-group>
      <el-checkbox v-model="changes" size="small">只看改动服务器的</el-checkbox>
      <el-checkbox v-model="groupByOp" size="small">按改动聚合</el-checkbox>
      <el-input v-model="keyword" size="small" clearable placeholder="搜索项目、服务器、应用、命令、错误…" class="keyword" />
      <GlassButton size="small" :disabled="!filtered" @click="resetFilters">清空筛选</GlassButton>
      <span class="spacer" />
      <GlassButton size="small" :icon="Refresh" :loading="loading" @click="load">刷新</GlassButton>
    </div>
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="mb" />
    <el-table
      v-loading="loading"
      :data="displayRows"
      row-key="_key"
      :tree-props="{ children: 'children' }"
      size="small"
      border
      height="clamp(160px, calc(100dvh - 300px), 62vh)"
      :empty-text="filtered ? '没有符合筛选的记录' : '还没有编程软件调用过'"
    >
      <el-table-column type="expand" width="32">
        <template #default="{ row }">
          <div class="detail">
            <div v-if="row.op" class="muted small">
              改动 ID：<el-button link type="primary" size="small" class="mono op-link" @click="filterByOp(row.op)">{{ row.op }}</el-button>
              <span class="muted">（点击筛选此次改动的完整链路<template v-if="row._isGroup">；点时间列左侧箭头展开 {{ row._stepCount }} 步明细</template>）</span>
            </div>
            <div v-if="row.decision" class="muted small">确认：{{ row.decision }}</div>
            <div v-if="row.error" class="fail small">错误：{{ row.error }}</div>
            <div class="muted small">耗时 {{ row.ms }} ms</div>
            <pre v-if="row.args" class="mono args">{{ pretty(row.args) }}</pre>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="时间" width="185">
        <template #default="{ row }">{{ time(row.time) }}</template>
      </el-table-column>
      <el-table-column label="编程软件" width="170" show-overflow-tooltip>
        <template #default="{ row }">{{ row.client || '-' }}</template>
      </el-table-column>
      <el-table-column label="做了什么" width="190">
        <template #default="{ row }">
          <div class="tool-cell">
            <span>{{ toolLabel(row) }}</span>
            <el-tag v-if="row._isGroup" size="small" type="primary" effect="plain" disable-transitions>{{ row._stepCount }} 步</el-tag>
            <el-tag v-if="channelBadge(row)" size="small" type="info" effect="plain" disable-transitions>{{ channelBadge(row) }}</el-tag>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="内容" min-width="220" show-overflow-tooltip>
        <template #default="{ row }">
          <el-button v-if="row.op" link type="primary" size="small" class="mono op-chip" title="点击按改动 ID 聚合筛选完整链路" @click="filterByOp(row.op)">#{{ row.op.slice(0, 6) }}</el-button>
          <span>{{ argsText(row) }}</span>
        </template>
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
.filters .channel {
  width: 135px;
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
.tool-cell { display: flex; flex-wrap: wrap; align-items: center; gap: 4px; }
.op-chip { padding: 0; height: auto; margin-right: 4px; font-size: 11px; vertical-align: baseline; }
.op-link { padding: 0; height: auto; font-size: 12px; vertical-align: baseline; }
</style>
