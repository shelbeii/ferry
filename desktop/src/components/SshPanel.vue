<script lang="ts">
import { reactive } from 'vue'
import type { SSHReport } from '../ferry'

// 各项目、各连接上次查看的结果（切回来时先显示它，同时重新查）
const cache = reactive<Record<string, SSHReport>>({})
// 上次读到的时间：刚打开过（15 秒内）就不再去读服务器，登录用户变得快，不缓存太久
const loadedAt: Record<string, number> = {}
const freshMs = 15_000
const errors = reactive<Record<string, { msg: string; unknownHost: boolean }>>({})
</script>

<script setup lang="ts">
// 服务器上连着的 SSH：谁、归属来源（本机 Ferry / 本机其他软件 / 同 IP 其他设备 / 外部）、认证方式、连了多久、在干什么；可以断开（当前连接除外）。
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { InfoFilled, Key, Refresh, SwitchButton } from '@element-plus/icons-vue'
import { call, FerryError, type SSHSession } from '../ferry'
import { secretRequest, trustRequest, trustedTick } from '../state'
import LoadingBlock from './LoadingBlock.vue'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
import { useManualRefresh } from '../manualRefresh'
import { useInteractionScope } from '../interactionScope'
import {
  buildKickWarningLines,
  filterSessions,
  formatSessionActivity,
  formatSessionAuth,
  originBadge,
  sessionShortDesc,
  summarizeSessions,
  type SessionFilter,
} from '../sshSessions'

const props = defineProps<{
  config: string
  target: string
  connected: boolean
  /** 自己带工具栏（「新建连接」的抽屉里；项目页的按钮在项目页的工具栏上） */
  bar?: boolean
}>()
const emit = defineEmits<{ loaded: [] }>()

const key = computed(() => `${props.config}|${props.target}`)
const captureContext = useInteractionScope(() => key.value)
const report = computed(() => cache[key.value])
const error = computed(() => errors[key.value])
const sessions = computed<SSHSession[]>(() => report.value?.sessions ?? [])
const sessionQuery = defineModel<string>('query', { default: '' })
const sessionFilter = defineModel<SessionFilter>('filter', { default: 'all' })
const matchingSessions = computed(() => filterSessions(sessions.value, sessionFilter.value, sessionQuery.value))
const shownCount = computed(() => matchingSessions.value.length)
const reads = useReadRequests()
const loading = computed(() => !!reads.loading.value[key.value])
onBeforeUnmount(reads.cancelAll)
const kicking = ref(false)
const selected = ref<SSHSession[]>([])

async function load(_background = false, signal?: AbortSignal) {
  const k = key.value
  const config = props.config
  const target = props.target
  try {
    await reads.run(k, async (s) => {
      cache[k] = await readCall<typeof cache[string]>(['who', target, '-c', config], s)
      loadedAt[k] = Date.now()
      delete errors[k]
    }, { signal })
  } catch (e) {
    if (e instanceof ReadCanceled || signal?.aborted) return
    const err = e as FerryError
    errors[k] = { msg: err.message, unknownHost: err instanceof FerryError && err.unknownHost }
    if (err instanceof FerryError && err.needSecret && err.detail) {
      secretRequest.value = { detail: err.detail, config, retry: () => { if (key.value === k) load() } }
    }
  } finally {
    if (key.value === k) emit('loaded')
  }
}

// 连着的人随时在变：打开了就查，但刚查过（15 秒内）先显示上次的结果
watch(
  key,
  (_, previous) => {
    if (previous) reads.cancel(previous)
    selected.value = []
    if (Date.now() - (loadedAt[key.value] ?? 0) >= freshMs) load()
  },
  { immediate: true },
)
watch(trustedTick, () => errors[key.value] && load())

const counts = computed(() => summarizeSessions(report.value))

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

function fromSub(s: SSHSession): string {
  if (s.origin === 'local-proc' && s.localPid) return `本机 PID ${s.localPid}`
  if (s.origin === 'same-ip') return '同 IP · 非本机端口'
  if (s.origin === 'nat-same-ip' && report.value?.natPortTranslated) return '同出口 IP · 经 NAT 转换'
  return ''
}

function why(s: SSHSession): string {
  if (s.self) return '这是本机 Ferry 当前页面正在用的连接（终端、文件、这个列表都经它），不能在这里断开'
  if (!s.kick) return `登录用户 ${report.value?.me} 不是 root，也不能免密 sudo：只能断开 ${report.value?.me} 自己的连接`
  return ''
}

function canBulkSelect(r: SSHSession): boolean {
  // 本机 Ferry 连接池里的其他连接不参与全选批量断开，避免误踢自己；仍可在行末手动断开
  return r.kick && !r.self && r.origin !== 'ferry-pool'
}

async function kick(rows: SSHSession[]) {
  if (kicking.value) return
  const current = captureContext()
  const config = props.config, target = props.target, k = key.value
  rows = rows.filter((r) => r.kick && !r.self)
  if (!rows.length) return
  const what = rows.length === 1 ? sessionShortDesc(rows[0]) : `这 ${rows.length} 个连接`
  const lines = buildKickWarningLines(rows)
  try {
    await ElMessageBox.confirm(lines.join('\n').trim(), `断开 ${what}`, {
      type: 'warning',
      confirmButtonText: '断开',
      customStyle: { whiteSpace: 'pre-line' },
    })
  } catch {
    return
  }
  if (!current()) return
  reads.cancel(k)
  kicking.value = true
  try {
    cache[k] = await call<typeof cache[string]>(['who', 'kick', target, ...rows.map((r) => String(r.pid)), '-y', '-c', config])
    loadedAt[k] = Date.now()
    if (!current()) return
    ElMessage.success(rows.length === 1 ? '已断开' : `已断开 ${rows.length} 个连接`)
  } catch (e) {
    if (!current()) return
    ElMessage.error({ message: (e as Error).message, duration: 6000 })
    load()
  } finally {
    if (current()) { kicking.value = false; selected.value = [] }
  }
}
watch(key, () => { kicking.value = false })

const kickSelected = () => kick(selected.value)

// 点刷新按钮：读取期间按钮转圈、内容变淡，读完提示（见 manualRefresh.ts）
const manual = useManualRefresh(() => key.value)
const refreshing = manual.refreshing
const refresh = () => manual.run('登录用户', () => load(), () => !error.value)
defineExpose({ load, refresh, refreshing, loading, error, counts, shownCount, selected, kicking, kickSelected })
</script>

<template>
  <div class="sp" :class="{ refreshing }">
    <div v-if="bar" class="sp-bar">
      <el-input v-model="sessionQuery" class="session-search" placeholder="搜索用户、归属、IP、密钥备注或命令" clearable aria-label="搜索登录会话" />
      <el-radio-group v-model="sessionFilter" size="small" aria-label="按来源筛选">
        <el-radio-button value="all">全部</el-radio-button>
        <el-radio-button value="others">其他设备</el-radio-button>
        <el-radio-button value="local">仅本机</el-radio-button>
        <el-radio-button value="non-ferry">隐藏 Ferry</el-radio-button>
      </el-radio-group>
      <el-button type="danger" plain :icon="SwitchButton" :disabled="!selected.length" :loading="kicking" @click="kickSelected">
        断开{{ selected.length ? `（${selected.length}）` : '所选' }}
      </el-button>
      <el-tooltip :show-after="HOVER_DELAY" content="只重新读这一页的登录用户" placement="bottom"><el-button :icon="Refresh" :loading="loading || refreshing" circle aria-label="刷新登录用户" @click="refresh()" /></el-tooltip>
      <span class="spacer" />
      <span v-if="counts" class="muted">
        {{ counts.total }} 个连接（本机 {{ counts.localCount }} · 其他 {{ counts.otherDeviceCount }}）· {{ counts.ips }} 个来源 IP
      </span>
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
      <div v-if="report.pending || report.notes?.length || (report.natPortTranslated && (counts?.natSameIPCount ?? 0) > 0)" class="sp-notes">
        <el-tooltip
          v-if="report.pending"
          :show-after="HOVER_DELAY"
          content="正常登录一两秒就过去了。一直有很多，多半是有人在试密码：可以只允许密钥登录、改 SSH 端口，或用防火墙限制来源"
          placement="bottom"
        >
          <span class="pending">另外有 {{ report.pending }} 个连接正在登录（还没通过验证）</span>
        </el-tooltip>
        <span v-if="report.natPortTranslated && (counts?.natSameIPCount ?? 0) > 0" class="muted small">
          当前连接经过了 NAT / 代理端口转换：同出口 IP 的非 Ferry 连接无法仅按本机端口区分设备，可结合认证密钥备注与终端内容判断
        </span>
        <span v-for="n in report.notes ?? []" :key="n" class="muted small">{{ n }}</span>
      </div>

      <div class="table-box">
        <el-table
          :data="matchingSessions"
          row-key="pid"
          :empty-text="sessionQuery.trim() || sessionFilter !== 'all' ? '没有匹配的 SSH 连接' : '没有已登录的 SSH 连接'"
          @selection-change="(rows: SSHSession[]) => (selected = rows)"
        >
          <el-table-column type="selection" width="44" :selectable="canBulkSelect" />
          <el-table-column label="用户与归属" min-width="210">
            <template #default="{ row }">
              <div class="user-line">
                <span class="name">{{ row.user }}</span>
                <el-tooltip v-if="originBadge(row)" :show-after="HOVER_DELAY" :content="originBadge(row)!.tip" placement="top">
                  <el-tag size="small" :type="originBadge(row)!.type" effect="plain" class="origin-tag">
                    {{ originBadge(row)!.text }}
                  </el-tag>
                </el-tooltip>
              </div>
              <div class="sub meta-line">
                <span class="mono">PID {{ row.pid }}</span>
                <template v-if="formatSessionAuth(row)">
                  <span class="dot">·</span>
                  <el-tooltip :show-after="HOVER_DELAY" :content="formatSessionAuth(row)!.tip" placement="top">
                    <span class="auth-chip">{{ formatSessionAuth(row)!.text }}</span>
                  </el-tooltip>
                </template>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="来源" min-width="155">
            <template #default="{ row }">
              <template v-if="row.from">
                <div>
                  <span class="mono">{{ row.from }}</span><span v-if="row.fromPort" class="muted mono port">:{{ row.fromPort }}</span>
                </div>
                <div v-if="fromSub(row)" class="sub">{{ fromSub(row) }}</div>
              </template>
              <el-tooltip v-else :show-after="HOVER_DELAY" content="登录用户不是 root、也不能免密 sudo，看不到别的用户的连接从哪里来" placement="top">
                <span class="muted">看不到</span>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="连了多久" width="102">
            <template #default="{ row }">
              <el-tooltip :show-after="HOVER_DELAY" :content="`${fullTime(row.since)} 连上`" placement="top">
                <span>{{ age(row) }}</span>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="类型" width="136">
            <template #default="{ row }">
              <div class="kind-line">
                <el-tooltip :show-after="HOVER_DELAY" :content="kindTip[row.kind as SSHSession['kind']]" placement="top">
                  <span class="kind" :class="row.kind">{{ kindText[row.kind as SSHSession['kind']] }}</span>
                </el-tooltip>
                <span v-if="row.ttys?.length" class="muted mono small ml">{{ row.ttys.join(',') }}</span>
              </div>
              <div v-if="row.forwards?.length" class="sub">
                <el-tooltip :show-after="HOVER_DELAY" :content="`活跃端口转发：${row.forwards.join('、')}`" placement="top">
                  <span class="fw-chip">含端口转发</span>
                </el-tooltip>
              </div>
            </template>
          </el-table-column>
          <el-table-column label="在干什么" min-width="230">
            <template #default="{ row }">
              <div class="act-line">
                <span v-if="formatSessionActivity(row).summary" class="act-text" :class="{ mono: !formatSessionActivity(row).hasFriendly }" :title="formatSessionActivity(row).raw || formatSessionActivity(row).summary">
                  {{ formatSessionActivity(row).summary }}
                </span>
                <span v-else class="muted">—</span>
                <el-popover placement="left" :width="380" trigger="click">
                  <template #reference>
                    <button type="button" class="detail-btn" aria-label="查看连接详情" title="查看原始命令与认证详情">
                      <el-icon><InfoFilled /></el-icon>
                    </button>
                  </template>
                  <div class="detail-pop">
                    <div class="detail-title">{{ row.user }} · sshd PID {{ row.pid }}</div>
                    <div class="detail-row"><span>连入时间</span><span class="mono">{{ fullTime(row.since) }}</span></div>
                    <div v-if="row.from" class="detail-row"><span>来源地址</span><span class="mono">{{ row.from }}{{ row.fromPort ? `:${row.fromPort}` : '' }}</span></div>
                    <div v-if="row.originDetail || row.originLabel" class="detail-row"><span>归属判定</span><span>{{ row.originDetail || row.originLabel }}</span></div>
                    <div v-if="row.localProcess || row.localPid" class="detail-row"><span>本机进程</span><span class="mono">{{ row.localProcess || '未知' }}{{ row.localPid ? ` (PID ${row.localPid})` : '' }}</span></div>
                    <div v-if="formatSessionAuth(row)" class="detail-row"><span>登录认证</span><span>{{ formatSessionAuth(row)!.text }}</span></div>
                    <div v-if="row.authKey" class="detail-row"><span>公钥指纹</span><span class="mono break-all">{{ row.authKey }}</span></div>
                    <div v-if="row.forwards?.length" class="detail-row"><span>端口转发</span><span class="mono break-all">{{ row.forwards.join('、') }}</span></div>
                    <div v-if="row.cmds?.length" class="detail-block">
                      <div class="detail-sub">原始进程命令</div>
                      <pre class="detail-pre">{{ row.cmds.join('\n') }}</pre>
                    </div>
                  </div>
                </el-popover>
              </div>
              <div v-if="idleText(row)" class="sub" :class="{ active: row.idle < 60 }">{{ idleText(row) }}</div>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="90" fixed="right">
            <template #default="{ row }">
              <el-button
                v-if="row.kick && !row.self"
                link
                :type="row.origin === 'ferry-pool' ? 'warning' : 'danger'"
                :disabled="kicking"
                @click="kick([row])"
              >
                断开
              </el-button>
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
/* 点刷新时内容变淡（工具栏不变），读完恢复 */
.sp.refreshing > :not(.sp-bar) {
  opacity: 0.4;
  transition: opacity 0.2s;
}
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
.sp-bar > .session-search { flex: 0 1 280px; min-width: 140px; }
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
.user-line {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}
.name {
  font-weight: 500;
}
.origin-tag {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.meta-line {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px;
}
.dot {
  opacity: 0.55;
}
.auth-chip {
  border-bottom: 1px dashed var(--el-border-color);
  cursor: default;
}
.port {
  font-size: 12px;
}
.kind-line {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
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
.fw-chip {
  color: var(--el-color-primary);
  font-size: 11.5px;
}
.act-line {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.act-text {
  font-size: 12.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  flex: 1 1 auto;
  min-width: 0;
}
.detail-btn {
  border: none;
  background: transparent;
  color: var(--el-text-color-placeholder);
  cursor: pointer;
  padding: 2px;
  display: inline-flex;
  align-items: center;
  flex-shrink: 0;
  border-radius: 4px;
}
.detail-btn:hover {
  color: var(--el-color-primary);
  background: var(--el-fill-color-light);
}
.detail-pop {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: 12.5px;
}
.detail-title {
  font-weight: 600;
  margin-bottom: 2px;
}
.detail-row {
  display: grid;
  grid-template-columns: 64px 1fr;
  gap: 8px;
  align-items: start;
}
.detail-row > span:first-child {
  color: var(--el-text-color-secondary);
}
.detail-block {
  margin-top: 2px;
}
.detail-sub {
  color: var(--el-text-color-secondary);
  margin-bottom: 4px;
}
.detail-pre {
  margin: 0;
  padding: 6px 8px;
  border-radius: 6px;
  background: var(--el-fill-color-lighter);
  font-family: var(--ferry-mono, monospace);
  font-size: 11.5px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 160px;
  overflow: auto;
}
.break-all {
  word-break: break-all;
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
