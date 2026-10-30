<script lang="ts">
import { reactive } from 'vue'
import type { ServerInfo } from '../ferry'

// 切换目标、项目时保留上次采样；服务器时间仅用于显示，本机时间用于控制刷新间隔。
const cache = reactive<Record<string, { report: ServerInfo; at: number }>>({})
const errors = reactive<Record<string, { msg: string; unknownHost: boolean }>>({})
const attempts: Record<string, number> = {}
const observations = reactive<Record<string, { at: number; cpu: number | null; memory: number | null }[]>>({})
</script>

<script setup lang="ts">
import { computed, onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, watch } from 'vue'
import { Key, Refresh } from '@element-plus/icons-vue'
import { ferry, FerryError, type Running } from '../ferry'
import { appSettings, secretRequest, trustRequest, trustedTick } from '../state'
import { capacity, percentText, serverInfoCards, uptimeText, usageClass, usagePercent, visibleMounts } from '../serverinfo'
import LoadingBlock from './LoadingBlock.vue'
import ResourceTrend from './ResourceTrend.vue'

const props = defineProps<{ config: string; target: string; connected: boolean; active: boolean; bar?: boolean }>()
const emit = defineEmits<{ loaded: [] }>()
const key = computed(() => `${props.config}|${props.target}`)
const report = computed(() => cache[key.value]?.report)
const error = computed(() => errors[key.value])
const cards = computed(() => serverInfoCards(report.value))
const loading = ref(false)
const shown = ref(true)
const temporary = ref(false)
const mounts = computed(() => visibleMounts(report.value?.mounts ?? [], temporary.value).sort((a, b) => (b.usage ?? -1) - (a.usage ?? -1)))
const mountQuery = ref('')
const matchingMounts = computed(() => mounts.value.filter((m) => `${m.path} ${m.device} ${m.fsType}`.toLowerCase().includes(mountQuery.value.trim().toLowerCase())))
const cpuPoints = computed(() => (observations[key.value] ?? []).map((p) => ({ at: p.at, value: p.cpu })))
const memoryPoints = computed(() => (observations[key.value] ?? []).map((p) => ({ at: p.at, value: p.memory })))
const memoryUsage = computed(() => report.value ? usagePercent(report.value.memory.used, report.value.memory.total) : undefined)
const swapUsage = computed(() => report.value ? usagePercent(report.value.swap.used, report.value.swap.total) : undefined)
const interval = computed(() => appSettings.value?.connect.statusMinutes ?? 1)
const readTime = computed(() => cache[key.value] ? new Date(cache[key.value].at).toLocaleTimeString('zh-CN', { hour12: false }) : '')
const autoTip = computed(() => interval.value < 0 ? '自动刷新已关闭' : !props.connected ? '连接已断开，自动刷新已暂停' : `每 ${interval.value} 分钟自动刷新`)
const serverTime = computed(() => report.value?.now ? new Date(report.value.now * 1000).toISOString().replace('T', ' ').replace('.000Z', ' UTC') : '—')

let running: Running<ServerInfo> | undefined
let serial = 0
let timer: ReturnType<typeof setInterval> | undefined
async function load(background = false) {
  if (background && loading.value) return
  running?.kill()
  const seq = ++serial
  const k = key.value
  const config = props.config
  const target = props.target
  attempts[k] = Date.now()
  loading.value = true
  const proc = ferry<ServerInfo>(['server-info', target, '-c', config, ...(background ? ['--background'] : [])])
  running = proc
  try {
    const result = await proc.done
    if (seq !== serial) return
    if (!result.ok || !result.data) throw new FerryError(result.error ?? '读取服务器信息失败', result.detail)
    cache[k] = { report: result.data, at: Date.now() }
    const r = result.data
    observations[k] = [...(observations[k] ?? []), { at: Date.now(), cpu: r.cpu.usage, memory: usagePercent(r.memory.used, r.memory.total) ?? null }].slice(-40)
    delete errors[k]
  } catch (e) {
    if (seq !== serial || background) return
    const err = e as FerryError
    errors[k] = { msg: err.message, unknownHost: err instanceof FerryError && err.unknownHost }
    if (err instanceof FerryError && err.needSecret && err.detail) {
      secretRequest.value = { detail: err.detail, config, retry: () => { if (key.value === k) load() } }
    }
  } finally {
    if (seq === serial) {
      running = undefined
      loading.value = false
      emit('loaded')
    }
  }
}

watch(key, () => { temporary.value = false; load() }, { immediate: true })
watch(trustedTick, () => errors[key.value] && props.active && load())
onMounted(() => {
  timer = setInterval(() => {
    if (!shown.value || !props.active || !props.connected || document.hidden || interval.value < 0 || loading.value || error.value) return
    if (Date.now() - Math.max(cache[key.value]?.at ?? 0, attempts[key.value] ?? 0) >= interval.value * 60_000) load(true)
  }, 5000)
})
onActivated(() => { shown.value = true })
onDeactivated(() => { shown.value = false })
onBeforeUnmount(() => {
  clearInterval(timer)
  serial++
  running?.kill()
})
defineExpose({ load, loading, error, report, cards, readTime, autoTip })
</script>

<template>
  <div class="server-info">
    <div v-if="bar" class="panel-bar">
      <el-button :icon="Refresh" :loading="loading" circle aria-label="刷新服务器信息" @click="load()" />
      <span class="muted small">{{ readTime ? `上次采样 ${readTime} · ` : '' }}{{ autoTip }}</span>
    </div>
    <el-alert v-if="error" type="error" :closable="false" show-icon>
      <template #title>{{ error.unknownHost ? '还没有核对这台服务器的主机公钥' : '读取服务器信息失败' }}</template>
      <div class="mono error-text">{{ error.msg }}</div>
      <div v-if="report" class="small">下面保留上次采样结果，请刷新后查看最新使用情况。</div>
      <el-button v-if="error.unknownHost" type="primary" size="small" :icon="Key" @click="trustRequest = { target, config }">核对并记录主机公钥</el-button>
    </el-alert>
    <LoadingBlock v-if="loading && !report && !error" :text="connected ? '正在采集系统、CPU、内存与挂载点信息（约需 1 秒）…' : `正在连接 ${target}，读取服务器信息…`" :rows="6" min-height="280px" />
    <template v-if="report">
      <div v-if="bar" class="overview">
        <div v-for="c in cards" :key="c.label" class="overview-card">
          <span class="muted small">{{ c.label }}</span>
          <strong :class="c.cls">{{ c.num }}</strong>
          <span class="muted small" :title="c.title">{{ c.sub }}</span>
        </div>
      </div>
      <el-alert v-if="report.notes?.length" type="warning" :closable="false" show-icon :title="report.notes.join('；')" />

      <div class="resource-charts"><ResourceTrend title="CPU 使用趋势" :points="cpuPoints" :interval="interval" /><ResourceTrend title="内存使用趋势" :points="memoryPoints" :interval="interval" accent="var(--el-color-success)" /></div>
      <details class="section system-details">
        <summary><span><strong>{{ report.hostname || '系统与硬件' }}</strong><span class="muted small">{{ report.os }} · {{ report.arch }}</span></span><span class="muted small">已运行 {{ uptimeText(report.uptime) }} · 系统详情</span></summary>
        <div class="system-grid">
          <div class="detail"><span>主机名</span><b class="mono">{{ report.hostname || '—' }}</b></div>
          <div class="detail"><span>操作系统</span><b>{{ report.os || 'Linux（发行版未识别）' }}</b></div>
          <div class="detail"><span>内核版本</span><b class="mono">{{ report.kernel }}</b></div>
          <div class="detail"><span>处理器架构</span><b class="mono">{{ report.arch || '—' }}</b></div>
          <div class="detail cpu-model"><span>CPU 型号</span><b>{{ report.cpu.model || '未读取到型号' }}</b></div>
          <div class="detail"><span>逻辑 CPU</span><b>{{ report.cpu.cores || '—' }}</b></div>
          <div class="detail"><span>虚拟化</span><b>{{ report.virtualization === 'none' ? '物理机' : report.virtualization || '未识别' }}</b></div>
          <div class="detail"><span>服务器时间</span><b class="mono">{{ serverTime }}</b></div>
          <div class="detail"><span>服务器时区</span><b class="mono">{{ report.timezone || '—' }}</b></div>
        </div>
        <div class="load-row">
          <span class="muted small">平均负载</span>
          <span v-for="(label, i) in ['1 分钟', '5 分钟', '15 分钟']" :key="label" class="load-chip"><span class="muted small">{{ label }}</span><b>{{ report.cpu.load?.[i]?.toFixed(2) ?? '—' }}</b></span>
          <span class="muted small load-note">负载包含正在运行或等待资源的任务，需结合 {{ report.cpu.cores || '可用' }} 个逻辑 CPU 判断。</span>
        </div>
      </details>

      <div class="memory-grid">
        <section class="section">
          <div class="section-head"><h3>内存</h3><b :class="usageClass(memoryUsage)">{{ percentText(memoryUsage) }}</b></div>
          <template v-if="report.memory.total">
            <el-progress :percentage="memoryUsage ?? 0" :show-text="false" :stroke-width="8" :status="(memoryUsage ?? 0) >= 90 ? 'exception' : undefined" />
            <div class="memory-values">
              <div><span>总容量</span><b>{{ capacity(report.memory.total) }}</b></div>
              <div><span>实际占用</span><b>{{ capacity(report.memory.used) }}</b></div>
              <div><span>可用</span><b>{{ capacity(report.memory.available) }}</b></div>
              <div><span>缓存 / 缓冲</span><b>{{ capacity(report.memory.cached) }}</b></div>
            </div>
            <p class="muted small">实际占用 = 总容量 − 可用内存；可回收缓存包含在可用内存中。</p>
          </template>
          <p v-else class="muted">未读取到内存信息</p>
        </section>
        <section class="section">
          <div class="section-head"><h3>Swap 交换空间</h3><b :class="usageClass(swapUsage)">{{ report.memory.total && !report.swap.total ? '未启用' : percentText(swapUsage) }}</b></div>
          <template v-if="report.swap.total">
            <el-progress :percentage="swapUsage ?? 0" :show-text="false" :stroke-width="8" :status="(swapUsage ?? 0) >= 90 ? 'exception' : undefined" />
            <div class="memory-values">
              <div><span>总容量</span><b>{{ capacity(report.swap.total) }}</b></div>
              <div><span>已使用</span><b>{{ capacity(report.swap.used) }}</b></div>
              <div><span>剩余</span><b>{{ capacity(report.swap.free) }}</b></div>
            </div>
          </template>
          <p v-else class="muted">{{ report.memory.total ? '这台服务器没有启用 Swap。' : '未读取到交换空间信息' }}</p>
        </section>
      </div>

      <section class="section disks">
        <div class="section-head"><h3>磁盘与挂载点 <span class="muted small">{{ mounts.length }} 个</span></h3><el-checkbox v-model="temporary">显示临时 / 虚拟文件系统</el-checkbox></div>
        <div class="disk-tools"><el-input v-model="mountQuery" placeholder="筛选挂载点、设备或文件系统" clearable aria-label="筛选挂载点" /><span class="muted small">按容量使用率从高到低</span></div>
        <el-table :data="matchingMounts" empty-text="没有匹配的挂载点" row-key="path">
          <el-table-column label="挂载点" min-width="150" show-overflow-tooltip><template #default="{ row }"><b class="mono">{{ row.path }}</b></template></el-table-column>
          <el-table-column prop="device" label="设备 / 来源" min-width="150" show-overflow-tooltip />
          <el-table-column prop="fsType" label="文件系统" min-width="100" />
          <el-table-column label="总容量" width="115" align="right"><template #default="{ row }">{{ row.total ? capacity(row.total) : '—' }}</template></el-table-column>
          <el-table-column label="已使用" width="115" align="right"><template #default="{ row }">{{ row.total ? capacity(row.used) : '—' }}</template></el-table-column>
          <el-table-column label="可用" width="115" align="right"><template #default="{ row }">{{ row.total ? capacity(row.available) : '—' }}</template></el-table-column>
          <el-table-column label="使用率" min-width="150">
            <template #default="{ row }">
              <div class="disk-usage"><el-progress v-if="row.usage != null" :percentage="Math.min(100, row.usage)" :show-text="false" :stroke-width="6" :status="row.usage >= 90 ? 'exception' : row.usage >= 80 ? 'warning' : undefined" /><b :class="usageClass(row.usage)">{{ percentText(row.usage) }}</b></div>
            </template>
          </el-table-column>
          <el-table-column label="inode 使用率" width="125" align="right">
            <template #default="{ row }"><el-tooltip :show-after="HOVER_DELAY" :content="row.inodes ? `已用 ${row.inodesUsed.toLocaleString()} / 总计 ${row.inodes.toLocaleString()} 个 inode` : '这个文件系统不提供 inode 统计'" placement="top"><span :class="usageClass(row.inodesUsage)">{{ percentText(row.inodesUsage) }}</span></el-tooltip></template>
          </el-table-column>
        </el-table>
        <p class="muted small">各挂载点分别统计；同一设备可能挂载在多处。可用容量不含文件系统保留空间，使用率按系统 df 的结果显示。</p>
      </section>

      <section class="section">
        <div class="section-head"><h3>网卡地址</h3></div>
        <el-table :data="report.addresses" empty-text="未读取到网卡地址">
          <el-table-column prop="interface" label="网卡" min-width="140" />
          <el-table-column label="协议" width="100"><template #default="{ row }">{{ row.family === 'inet6' ? 'IPv6' : 'IPv4' }}</template></el-table-column>
          <el-table-column prop="address" label="地址 / 前缀长度" min-width="240" class-name="mono" />
        </el-table>
      </section>
    </template>
  </div>
</template>

<style scoped>
.server-info { container-type: inline-size; }
.resource-charts { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
.system-details summary { display: flex; align-items: center; justify-content: space-between; gap: 12px; cursor: pointer; list-style: none; }
.system-details summary::-webkit-details-marker { display: none; }
.system-details summary > span:first-child { display: flex; flex-wrap: wrap; gap: 10px; align-items: baseline; }
.system-details[open] summary { margin-bottom: 24px; border-bottom: 1px solid var(--ferry-stroke); padding-bottom: 16px; }
.disk-tools { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 12px; }
.disk-tools .el-input { max-width: 300px; }
@container (max-width: 680px) { .resource-charts, .memory-grid, .system-grid { grid-template-columns: minmax(0, 1fr) !important; } .section-head, .disk-tools, .system-details summary { flex-wrap: wrap; } }
.server-info { display: flex; flex-direction: column; gap: 16px; padding-bottom: 16px; }
.panel-bar, .section-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.section { background: var(--el-bg-color); border: 1px solid var(--el-border-color-lighter); border-radius: 12px; padding: 20px; min-width: 0; }
.section-head { margin-bottom: 18px; }
h3 { font-size: 15px; font-weight: 600; margin: 0; }
h3 .small { margin-left: 8px; font-weight: 400; }
.system-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px 32px; }
.detail { display: grid; grid-template-columns: 96px minmax(0, 1fr); align-items: baseline; gap: 12px; font-size: 13px; }
.detail > span, .memory-values span { color: var(--el-text-color-secondary); }
b { font-weight: 500; overflow-wrap: anywhere; }
.cpu-model { grid-column: 1 / -1; }
.load-row { display: flex; flex-wrap: wrap; align-items: center; gap: 14px; margin-top: 22px; padding-top: 16px; border-top: 1px solid var(--el-border-color-lighter); }
.load-chip { display: inline-flex; gap: 10px; align-items: center; padding: 6px 12px; border-radius: 6px; background: var(--el-fill-color-light); }
.load-note { flex: 1; min-width: 220px; line-height: 1.7; }
.memory-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
.memory-values { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px; margin-top: 20px; font-size: 13px; }
.memory-values > div { display: flex; flex-direction: column; gap: 6px; }
.memory-values b { font-size: 16px; font-variant-numeric: tabular-nums; }
p { line-height: 1.7; margin: 16px 0 0; }
.disk-usage { display: flex; align-items: center; gap: 10px; }
.disk-usage .el-progress { flex: 1; min-width: 40px; }
.disk-usage b { min-width: 52px; text-align: right; font-size: 12px; }
.error-text { white-space: pre-wrap; overflow-wrap: anywhere; }
.ok { color: var(--el-color-success); }
.warn { color: var(--el-color-warning); }
.bad { color: var(--el-color-danger); }
.off { color: var(--el-text-color-placeholder); }
.overview { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }
.overview-card { display: flex; flex-direction: column; gap: 10px; padding: 16px; border: 1px solid var(--el-border-color-lighter); border-radius: 10px; }
.overview-card strong { font-size: 25px; font-weight: 500; }
.overview-card > span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
