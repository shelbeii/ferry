<script lang="ts">
import { reactive } from 'vue'
import type { SSHReport } from '../ferry'

// 各项目、各连接上次查看的结果（切回来时先显示它，同时重新查）
const cache = reactive<Record<string, SSHReport>>({})
const errors = reactive<Record<string, { msg: string; unknownHost: boolean }>>({})
</script>

<script setup lang="ts">
// 服务器上连着的 SSH：谁、从哪里、什么时候连上、在干什么；可以断开（本机 Ferry 自己的连接除外）。
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Key, Refresh, SwitchButton } from '@element-plus/icons-vue'
import { call, FerryError, type SSHSession } from '../ferry'
import { secretRequest, trustRequest, trustedTick } from '../state'
import LoadingBlock from './LoadingBlock.vue'

const props = defineProps<{
  config: string
  target: string
  connected: boolean
  /** 自己带工具栏（「新建连接」的抽屉里；项目页的按钮在项目页的工具栏上） */
  bar?: boolean
}>()
const emit = defineEmits<{ loaded: [] }>()

const key = computed(() => `${props.config}|${props.target}`)
const report = computed(() => cache[key.value])
const error = computed(() => errors[key.value])
const sessions = computed<SSHSession[]>(() => report.value?.sessions ?? [])
const sessionQuery = defineModel<string>('query', { default: '' })
const matchingSessions = computed(() => sessions.value.filter((s) => `${s.user} ${s.from ?? ''} ${s.kind} ${(s.cmds ?? []).join(' ')}`.toLowerCase().includes(sessionQuery.value.trim().toLowerCase())))
const loading = ref(false)
const kicking = ref(false)
const selected = ref<SSHSession[]>([])

async function load() {
  const k = key.value
  loading.value = true
  try {
    cache[k] = await call<typeof cache[string]>(['who', props.target, '-c', props.config])
    delete errors[k]
  } catch (e) {
    const err = e as FerryError
    errors[k] = { msg: err.message, unknownHost: err instanceof FerryError && err.unknownHost }
    if (err instanceof FerryError && err.needSecret && err.detail) {
      secretRequest.value = { detail: err.detail, config: props.config, retry: load }
    }
  } finally {
    loading.value = false
    emit('loaded')
  }
}

// 连着的人随时在变：每次打开都重新查（先显示上次的结果）
watch(
  key,
  () => {
    selected.value = []
    load()
  },
  { immediate: true },
)
watch(trustedTick, () => errors[key.value] && load())

const counts = computed(() => {
  if (!report.value) return undefined
  const s = sessions.value
  return {
    total: s.length,
    users: new Set(s.map((x) => x.user)).size,
    ips: new Set(s.filter((x) => x.from).map((x) => x.from)).size,
    terminals: s.filter((x) => x.kind === 'terminal').length,
    pending: report.value.pending ?? 0,
  }
})

const kindText: Record<SSHSession['kind'], string> = {
  terminal: '终端',
  sftp: '传文件',
  ferry: 'Ferry',
  exec: '执行命令',
  idle: '空闲',
}
const kindTip: Record<SSHSession['kind'], string> = {
  terminal: '开着终端（命令行），如 Xshell、PuTTY、ssh 命令，或 Ferry 的终端',
  sftp: '在传文件（SFTP），如 FileZilla、WinSCP、Xftp，或 Ferry 的「文件」',
  ferry: 'Ferry 在操作这台服务器（查看状态、部署、看日志等）',
  exec: '在执行命令，没有开终端：如脚本、自动化工具',
  idle: '连着，但没有在运行的程序：可能是空闲的连接，或只用来做端口转发',
}

/** 秒数 → 3 分钟、2 小时、5 天 */
function span(sec: number): string {
  if (sec < 60) return '不到 1 分钟'
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时`
  return `${Math.floor(sec / 86400)} 天`
}

function fullTime(unix: number): string {
  const d = new Date(unix * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

const age = (s: SSHSession) => span(Math.max((report.value?.now ?? s.since) - s.since, 0))

function idleText(s: SSHSession): string {
  if (s.idle < 0) return ''
  return s.idle < 60 ? '正在使用' : `${span(s.idle)}没输入`
}

function why(s: SSHSession): string {
  if (s.self) return '这是本机 Ferry 正在用的连接（终端、文件、这个列表都经它），不能在这里断开'
  if (!s.kick) return `登录用户 ${report.value?.me} 不是 root，也不能免密 sudo：只能断开 ${report.value?.me} 自己的连接`
  return ''
}

function desc(s: SSHSession): string {
  return `${s.user}${s.from ? `（来自 ${s.from}）` : ''}的${kindText[s.kind] === '空闲' ? '连接' : kindText[s.kind]}`
}

async function kick(rows: SSHSession[]) {
  rows = rows.filter((r) => r.kick && !r.self)
  if (!rows.length) return
  const what = rows.length === 1 ? desc(rows[0]) : `这 ${rows.length} 个连接`
  const lines = rows.length > 1 ? rows.map((r) => `· ${desc(r)}`) : []
  lines.push('', '里面正在运行的命令会被结束。对方可以重新连上：不想让它再连，要改密码、去掉它的公钥，或用防火墙限制来源。')
  try {
    await ElMessageBox.confirm(lines.join('\n').trim(), `断开 ${what}`, {
      type: 'warning',
      confirmButtonText: '断开',
      customStyle: { whiteSpace: 'pre-line' },
    })
  } catch {
    return
  }
  const k = key.value
  kicking.value = true
  try {
    cache[k] = await call<typeof cache[string]>(['who', 'kick', props.target, ...rows.map((r) => String(r.pid)), '-y', '-c', props.config])
    ElMessage.success(rows.length === 1 ? '已断开' : `已断开 ${rows.length} 个连接`)
  } catch (e) {
    ElMessage.error({ message: (e as Error).message, duration: 6000 })
    load()
  } finally {
    kicking.value = false
    selected.value = []
  }
}

const kickSelected = () => kick(selected.value)

defineExpose({ load, loading, error, counts, selected, kicking, kickSelected })
</script>

<template>
  <div class="sp">
    <div v-if="bar" class="sp-bar">
      <el-input v-model="sessionQuery" class="session-search" placeholder="搜索用户、来源 IP 或命令" clearable aria-label="搜索登录会话" />
      <el-button type="danger" plain :icon="SwitchButton" :disabled="!selected.length" :loading="kicking" @click="kickSelected">
        断开{{ selected.length ? `（${selected.length}）` : '所选' }}
      </el-button>
      <el-button :icon="Refresh" :loading="loading" circle aria-label="重新查看" @click="load" />
      <span class="spacer" />
      <span v-if="counts" class="muted">{{ counts.total }} 个连接 · {{ counts.users }} 个用户 · {{ counts.ips }} 个来源 IP</span>
    </div>

    <el-alert v-if="error" type="error" :closable="false" show-icon class="sp-err">
      <template #title>{{ error.unknownHost ? '还没有核对这台服务器的主机公钥' : '查看登录用户失败' }}</template>
      <div class="mono err">{{ error.msg }}</div>
      <el-button v-if="error.unknownHost" type="primary" size="small" :icon="Key" class="trust-btn" @click="trustRequest = { target, config }">
        核对并记录主机公钥
      </el-button>
    </el-alert>

    <LoadingBlock
      v-if="loading && !report && !error"
      :text="connected ? `正在查看 ${target} 上连着的 SSH（复用已有连接）…` : `正在连接 ${target}，查看连着的 SSH…`"
      :rows="4"
      min-height="180px"
      class="sp-loading"
    />
    <template v-else-if="report">
      <div v-if="report.pending || report.notes?.length" class="sp-notes">
        <el-tooltip
          v-if="report.pending"
          :show-after="HOVER_DELAY"
          content="正常登录一两秒就过去了。一直有很多，多半是有人在试密码：可以只允许密钥登录、改 SSH 端口，或用防火墙限制来源"
          placement="bottom"
        >
          <span class="pending">另外有 {{ report.pending }} 个连接正在登录（还没通过验证）</span>
        </el-tooltip>
        <span v-for="n in report.notes ?? []" :key="n" class="muted small">{{ n }}</span>
      </div>

      <div class="table-box">
        <el-table
          :data="matchingSessions"
          row-key="pid"
          :empty-text="sessionQuery.trim() ? '没有匹配的 SSH 连接' : '没有已登录的 SSH 连接'"
          @selection-change="(rows: SSHSession[]) => (selected = rows)"
        >
          <el-table-column type="selection" width="44" :selectable="(r: SSHSession) => r.kick && !r.self" />
          <el-table-column label="用户" min-width="120">
            <template #default="{ row }">
              <span class="name">{{ row.user }}</span>
              <el-tooltip v-if="row.self" :show-after="HOVER_DELAY" content="本机 Ferry 正在用的连接（终端、文件、这个列表都经它）" placement="top">
                <el-tag size="small" type="success" effect="plain" class="ml">本机 Ferry</el-tag>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="来源" min-width="150">
            <template #default="{ row }">
              <template v-if="row.from">
                <span class="mono">{{ row.from }}</span><span v-if="row.fromPort" class="muted mono port">:{{ row.fromPort }}</span>
              </template>
              <el-tooltip v-else :show-after="HOVER_DELAY" content="登录用户不是 root、也不能免密 sudo，看不到别的用户的连接从哪里来" placement="top">
                <span class="muted">看不到</span>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="连了多久" width="110">
            <template #default="{ row }">
              <el-tooltip :show-after="HOVER_DELAY" :content="`${fullTime(row.since)} 连上`" placement="top">
                <span>{{ age(row) }}</span>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="类型" width="130">
            <template #default="{ row }">
              <el-tooltip :show-after="HOVER_DELAY" :content="kindTip[row.kind as SSHSession['kind']]" placement="top">
                <span class="kind" :class="row.kind">{{ kindText[row.kind as SSHSession['kind']] }}</span>
              </el-tooltip>
              <span v-if="row.ttys?.length" class="muted mono small ml">{{ row.ttys.join(',') }}</span>
            </template>
          </el-table-column>
          <el-table-column label="在干什么" min-width="200">
            <template #default="{ row }">
              <div v-if="row.cmds?.length" class="mono cmds" :title="row.cmds.join('\n')">{{ row.cmds.join('；') }}</div>
              <span v-else class="muted">—</span>
              <div v-if="idleText(row)" class="sub" :class="{ active: row.idle < 60 }">{{ idleText(row) }}</div>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="90" fixed="right">
            <template #default="{ row }">
              <el-button v-if="row.kick && !row.self" link type="danger" :disabled="kicking" @click="kick([row])">断开</el-button>
              <el-tooltip v-else :show-after="HOVER_DELAY" :content="why(row)" placement="left">
                <span class="muted small">不能断开</span>
              </el-tooltip>
            </template>
          </el-table-column>
        </el-table>
      </div>
    </template>
  </div>
</template>

<style scoped>
.sp {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.sp-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  overflow-x: auto;
  white-space: nowrap;
  padding: 3px;
  margin: -3px;
  font-size: 13px;
}
.sp-bar > * { flex-shrink: 0; }
.sp-bar > .session-search { flex: 0 1 320px; min-width: 120px; }
.sp-bar > .spacer { flex-shrink: 1; min-width: 0; }
.sp-bar .el-button + .el-button {
  margin-left: 0;
}
.sp-loading {
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ferry-card-radius);
  padding: 14px 16px;
}
.sp-err :deep(.el-alert__content) {
  width: 100%;
}
.err {
  white-space: pre-wrap;
  margin: 4px 0 0;
}
.trust-btn {
  margin-top: 8px;
}
.spacer {
  flex: 1;
}
.small {
  font-size: 12px;
}
.ml {
  margin-left: 6px;
}
.sp-notes {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 14px;
  font-size: 13px;
}
.pending {
  color: var(--el-color-warning-dark-2);
  border-bottom: 1px dashed var(--el-color-warning-light-5);
  cursor: default;
}
.name {
  font-weight: 500;
}
.port {
  font-size: 12px;
}
.kind {
  display: inline-block;
  padding: 1px 8px;
  border-radius: 10px;
  font-size: 12px;
  background: var(--el-fill-color);
  color: var(--el-text-color-secondary);
}
.kind.terminal {
  background: var(--el-color-primary-light-9);
  color: var(--el-color-primary);
}
.kind.sftp {
  background: var(--el-color-success-light-9);
  color: var(--el-color-success-dark-2);
}
.cmds {
  font-size: 12.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sub {
  margin-top: 2px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.sub.active {
  color: var(--el-color-success);
}
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
</style>
