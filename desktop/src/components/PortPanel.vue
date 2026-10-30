<script lang="ts">
import { reactive } from 'vue'
import type { PortProbe, PortReport } from '../ferry'

// 各项目各目标的查看结果、本机测试结果（切换目标、切回「应用」再回来不用重新查）
const cache = reactive<Record<string, PortReport>>({})
const errors = reactive<Record<string, { msg: string; unknownHost: boolean }>>({})
const probes = reactive<Record<string, Record<number, PortProbe | 'running'>>>({})
</script>

<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { CopyDocument, Key, Refresh } from '@element-plus/icons-vue'
import { call, FerryError, type PortInfo } from '../ferry'
import { secretRequest, startTask, trustRequest, trustedTick } from '../state'
import LoadingBlock from './LoadingBlock.vue'
import GlassPane from './glass/GlassPane.vue'
import { glass } from '../glass/runtime'

const props = defineProps<{
  config: string
  target: string
  connected: boolean
  /** 应用的 systemd 单元 → 应用名（「谁在用」里显示应用名） */
  apps?: Record<string, string>
  /** 应用的附加端口里不宜对外开放的：端口 → 用途 */
  risky?: Record<number, string>
  /** 从「应用」点过来要看的端口：标出来、滚到那里 */
  focus?: number
}>()
const emit = defineEmits<{ loaded: [] }>()

const key = computed(() => `${props.config}|${props.target}`)
const report = computed(() => cache[key.value])
const error = computed(() => errors[key.value])
const probe = computed(() => probes[key.value] ?? {})
const loading = ref(false)

async function load() {
  const k = key.value
  loading.value = true
  try {
    cache[k] = await call<PortReport>(['port', props.target, '-c', props.config])
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

watch(
  key,
  () => {
    if (!cache[key.value]) load()
  },
  { immediate: true },
)
watch(trustedTick, () => errors[key.value] && load())

const fw = computed(() => report.value?.firewall)
const ports = computed<PortInfo[]>(() => report.value?.ports ?? [])
const portQuery = ref('')
const matchingPorts = computed(() => ports.value.filter((p) => JSON.stringify(p).toLowerCase().includes(portQuery.value.trim().toLowerCase())))
const guard = computed(() => report.value?.dockerGuard)

// ---------------------------------------------------------------- 外部能不能连

const isLocalHost = (h: string) => h.startsWith('127.') || h === '[::1]'
const localOnly = (p: PortInfo) => !!p.listen?.length && p.listen.every(isLocalHost)

type Reach = 'open' | 'blocked' | 'partial' | 'local' | 'internal' | 'docker' | 'guarded' | 'idle' | 'unknown'

function reach(p: PortInfo): Reach {
  if (!p.listen?.length) return 'idle'
  if (localOnly(p)) return 'local'
  if (p.internal) return 'internal'
  if (p.docker) return p.guard ? 'guarded' : 'docker'
  if (p.firewall === 'blocked') return 'blocked'
  if (p.firewall === 'partial') return 'partial'
  if (p.firewall === 'open' || !fw.value?.active) return 'open'
  return 'unknown'
}

const reachText: Record<Reach, string> = {
  open: '对外开放',
  blocked: '防火墙拦着',
  partial: '部分来源能连',
  local: '只有本机能连',
  internal: '只在内部网卡',
  docker: '对外开放',
  guarded: 'Ferry 拦着外部',
  idle: '没有程序在用',
  unknown: '看不出来',
}

function reachTip(p: PortInfo): string {
  switch (reach(p)) {
    case 'open':
      return fw.value?.active
        ? `防火墙放行了（${p.rule || '默认放行'}）：其他机器都能连。云服务器还要看云服务商的安全组`
        : '服务器没开防火墙：其他机器都能连。云服务器还要看云服务商的安全组'
    case 'blocked':
      return p.rule ? `防火墙拦着（${p.rule}）：其他机器连不上，服务器本机能连` : '防火墙没有放行它：其他机器连不上，服务器本机能连'
    case 'partial':
      return `防火墙只对部分来源放行（${p.rule}）`
    case 'local':
      return `只监听本机（${p.listen!.join('、')}）：不管防火墙怎么设，其他机器都连不上。要对外提供，得改程序的监听地址`
    case 'internal':
      return `只监听 ${p.iface} 上的 ${p.listen!.join('、')}：这是服务器内部的虚拟网卡（libvirt、Docker 等的内部网络），外部的机器连不到`
    case 'docker':
      return `Docker 把容器 ${p.container ?? ''} 的端口映射到了主机：不经过 firewalld / ufw 的规则，其他机器都能连。不想对外，点「关闭」在 Docker 的规则链里拦住外部访问，或者点「改映射」看怎么改成只映射到本机`
    case 'guarded':
      return `Ferry 在 Docker 的 DOCKER-USER 规则链里拦住了从 ${guard.value?.iface ?? '外网卡'} 进来的连接：其他机器连不上，服务器本机和同一台服务器上的容器照常能连。开机、Docker 重启后自动加回`
    case 'idle':
      if (p.guard) return '容器已经不在了，Ferry 拦它的规则还在：不需要了可以点「放行」去掉规则'
      return p.firewall === 'open' ? `防火墙放行了（${p.rule}），但现在没有程序在监听：不用了可以关掉` : '没有程序在监听'
    default:
      return fw.value?.why || '看不出防火墙对它的设置'
  }
}

/** 程序：应用名、Ferry 装的软件、系统服务、容器 */
function who(p: PortInfo): { name: string; sub: string } {
  if (p.docker) {
    const sub = p.manager === '1Panel' ? '1Panel 应用' : p.manager === 'compose' ? `compose ${p.composeService ?? ''}` : 'Docker 映射'
    return { name: `容器 ${p.container || '（已不在）'}`, sub }
  }
  const app = p.unit ? props.apps?.[p.unit] : undefined
  if (app) return { name: app, sub: '应用' }
  if (p.unit?.startsWith('ferry-') && p.ferry) return { name: p.unit.slice(6, -8), sub: 'Ferry 装的软件' }
  if (p.container) return { name: `容器 ${p.container}`, sub: p.process ?? '' }
  if (p.unit) return { name: p.process ?? p.unit, sub: p.unit }
  if (p.process) return { name: p.process, sub: p.pid ? `PID ${p.pid}` : '' }
  return { name: '', sub: '' }
}

function listenText(p: PortInfo): string {
  if (!p.listen?.length) return '—'
  if (p.listen.includes('*')) return '所有地址'
  return p.listen.join(' ') + (p.iface ? `（${p.iface}）` : '')
}

/** 「外部能不能连」下面那一行：起作用的规则，或说明 */
function ruleLine(p: PortInfo): string {
  const r = reach(p)
  if (r === 'local' || r === 'internal') return ''
  if (p.docker) return p.rule ? `防火墙规则 ${p.rule} 对 Docker 映射不起作用` : ''
  if (p.rule) return p.rule
  if (r === 'open' && !fw.value?.active) return '没开防火墙，不拦'
  return ''
}

// ---------------------------------------------------------------- 放行 / 关闭

const pkey = (p: PortInfo) => `${p.port}/${p.proto}`

// 只监听本机的不给放行：防火墙放行了外部也连不上。
// Docker 映射的端口不经过防火墙：关闭、放行改的是 DOCKER-USER 里 Ferry 加的规则
function canOpen(p: PortInfo): boolean {
  if (p.docker) return !!p.guard
  return !!fw.value?.manage && (p.firewall === 'blocked' || p.firewall === 'partial') && !localOnly(p)
}
function canClose(p: PortInfo): boolean {
  if (p.docker) return !!guard.value?.ok && !p.guard && !!p.listen?.length && !localOnly(p)
  return !!fw.value?.manage && p.firewall === 'open' && !p.ssh
}
/** 关不了的原因（显示「不能关」）；空 = 不显示 */
function closeWhy(p: PortInfo): string {
  if (p.docker) return !p.guard && !localOnly(p) && p.listen?.length && guard.value && !guard.value.ok ? (guard.value.why ?? '') : ''
  if (!fw.value?.manage || p.firewall !== 'open') return ''
  if (p.ssh) return 'SSH 端口：Ferry 用它连服务器，关掉就连不上了'
  return ''
}

async function setPort(p: PortInfo, action: 'open' | 'close') {
  const what = pkey(p)
  const label = action === 'open' ? '放行' : '关闭'
  const lines: string[] = []
  const w = who(p)
  if (p.docker) {
    const iface = guard.value?.iface ?? '外网卡'
    if (action === 'close') {
      lines.push(`在 Docker 的 DOCKER-USER 规则链里拦住从 ${iface} 进来、访问 ${p.port} 的连接（${w.name}）。`)
      lines.push('其他机器连不上；服务器本机、同一台服务器上的容器照常能连。不用重建容器，也不停服务。')
      lines.push('规则存在服务器上，开机、Docker 重启后由 ferry-docker-guard 服务自动加回。')
    } else {
      lines.push(`去掉 Ferry 在 DOCKER-USER 里拦 ${p.port} 的规则，之后其他机器又能连（${w.name}）。`)
      if (props.risky?.[p.port]) lines.push(`注意：${props.risky[p.port]}`)
    }
  } else if (action === 'open') {
    lines.push(`放行后，任何地址都能连 ${p.port} 端口${w.name ? `（${w.name}）` : ''}。`)
    if (props.risky?.[p.port]) lines.push(`注意：${props.risky[p.port]}`)
    if (localOnly(p)) lines.push('它只监听本机，放行了其他机器也连不上，要先改程序的监听地址。')
  } else {
    lines.push(`关闭后，其他机器（包括别的服务器上连它的程序）连不上 ${p.port} 端口；服务器本机不受影响。`)
    if (w.name && p.listen?.length) lines.push(`现在在用它的：${w.name}。`)
    if (p.rule && /^(服务|应用) /.test(p.rule)) lines.push(`它是「${p.rule}」放行的：会去掉这条规则，里面的端口一起关闭。`)
  }
  if (p.docker) {
    // 不改防火墙
  } else if (fw.value?.kind === 'firewalld') lines.push(`改 firewalld 区域 ${fw.value.zone} 的运行时和永久配置，马上生效，重启后也保持。`)
  else if (fw.value?.kind === 'ufw') lines.push('改 ufw 的规则，马上生效，重启后也保持。')
  try {
    await ElMessageBox.confirm(lines.join('\n'), `${label} ${props.target} 的端口 ${what}`, {
      type: action === 'close' ? 'warning' : 'info',
      confirmButtonText: label,
      customStyle: { whiteSpace: 'pre-line' },
    })
  } catch {
    return
  }
  const k = key.value
  startTask(`${label} ${props.target} 的端口 ${what}`, ['port', action, props.target, what, '-y', '-c', props.config], (t) => {
    if (t.status === 'ok' && t.data) cache[k] = t.data as PortReport
    else load()
    delete probes[k]?.[p.port]
  })
}

// ---------------------------------------------------------------- 打开防火墙（装了没开）

const enableOpen = ref(false)
const enableSel = ref<string[]>([])
const enableExtra = ref('')
const enableDocker = ref(true)
/** 现在外部能连的端口（打开后要不要继续放行，让用户勾） */
const candidates = computed(() => ports.value.filter((p) => !p.ssh && !p.docker && reach(p) === 'open'))
const sshList = computed(() => ports.value.filter((p) => p.ssh).map(pkey).join(' ') || '22/tcp')
const extraTokens = computed(() => enableExtra.value.split(/[\s,，、]+/).filter(Boolean))
const extraList = computed(() =>
  extraTokens.value
    .filter((t) => /^\d{1,5}(\/(tcp|udp))?$/.test(t) && +t.split('/')[0] >= 1 && +t.split('/')[0] <= 65535)
    .map((t) => (t.includes('/') ? t : t + '/tcp')),
)
const extraBad = computed(() => extraTokens.value.filter((t) => !extraList.value.includes(t.includes('/') ? t : t + '/tcp')))
const needDocker = computed(() => !!fw.value?.docker && fw.value.kind === 'firewalld')

function openEnable() {
  // 默认继续放行现在对外开放的端口，不宜对外的不勾
  enableSel.value = candidates.value.filter((p) => !props.risky?.[p.port]).map(pkey)
  enableExtra.value = ''
  enableDocker.value = true
  enableOpen.value = true
}

function doEnable() {
  const allow = [...new Set([...enableSel.value, ...extraList.value])]
  const args = ['port', 'enable', props.target, '-y', '-c', props.config]
  if (allow.length) args.push('--allow', allow.join(','))
  if (needDocker.value && enableDocker.value) args.push('--restart-docker')
  const k = key.value
  enableOpen.value = false
  startTask(`打开 ${props.target} 的防火墙`, args, (t) => {
    if (t.status === 'ok' && t.data) cache[k] = t.data as PortReport
    else load()
  })
}

// ---------------------------------------------------------------- 关闭防火墙（在运行）

const disableOpen = ref(false)
const disableDocker = ref(true)
/** 现在防火墙拦着、关掉后外部能连上的端口 */
const unblocked = computed(() => ports.value.filter((p) => reach(p) === 'blocked' || reach(p) === 'partial'))
/** Ferry 在 DOCKER-USER 里拦着的：关防火墙不影响 */
const guarded = computed(() => ports.value.filter((p) => reach(p) === 'guarded'))

function openDisable() {
  disableDocker.value = true
  disableOpen.value = true
}

function doDisable() {
  const args = ['port', 'disable', props.target, '-y', '-c', props.config]
  if (needDocker.value && disableDocker.value) args.push('--restart-docker')
  const k = key.value
  disableOpen.value = false
  startTask(`关闭 ${props.target} 的防火墙`, args, (t) => {
    if (t.status === 'ok' && t.data) cache[k] = t.data as PortReport
    else load()
    delete probes[k]
  })
}

// ---------------------------------------------------------------- 改映射（只映射到本机）

const remap = ref<PortInfo | null>(null)
const remapSpec = computed(() => {
  const p = remap.value
  if (!p) return { from: '', to: '' }
  const ctr = `${p.ctrPort || p.port}${p.proto === 'udp' ? '/udp' : ''}`
  return { from: `${p.port}:${ctr}`, to: `127.0.0.1:${p.port}:${ctr}` }
})
const composeCmd = computed(() => {
  const p = remap.value
  if (!p?.composeDir) return ''
  return `cd ${p.composeDir} && docker compose up -d ${p.composeService ?? ''}`.trim()
})

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制')
  } catch {
    ElMessage.error('复制失败，请手动选中复制')
  }
}

// ---------------------------------------------------------------- 从本机测试

/** 值得从本机测的端口：TCP、不是只监听本机的，或防火墙放行了的 */
const testable = (p: PortInfo) => p.proto === 'tcp' && !p.internal && (p.firewall === 'open' || (!!p.listen?.length && !localOnly(p)))
const testing = computed(() => Object.values(probe.value).some((v) => v === 'running'))

async function test(list: PortInfo[]) {
  if (!probes[key.value]) probes[key.value] = {}
  const pr = probes[key.value] // 经 reactive 取出来的才会更新界面（??= 返回的是原对象）
  const ports = list.filter(testable).map((p) => p.port)
  if (!ports.length) {
    ElMessage.info('没有要测试的端口（只监听本机的不用测）')
    return
  }
  for (const n of ports) pr[n] = 'running'
  try {
    const res = await call<PortProbe[]>(['port', 'check', props.target, ports.join(','), '-c', props.config])
    for (const r of res) pr[r.port] = r
  } catch (e) {
    for (const n of ports) delete pr[n]
    ElMessage.error((e as Error).message)
  }
}

function probeOf(p: PortInfo): PortProbe | 'running' | undefined {
  return p.proto === 'tcp' ? probe.value[p.port] : undefined
}

function probeTip(p: PortInfo, r: PortProbe): string {
  const re = reach(p)
  if (r.ok) {
    if (re === 'blocked') return `能连上（${r.ms} ms）。防火墙显示拦着它，可能你和服务器在同一个内网、走了别的规则，或者有别的程序放行了`
    return `从这台电脑能连上（${r.ms} ms）`
  }
  const how = r.why === 'refused' ? '服务器回绝了' : r.why === 'timeout' ? '一直没有回应' : r.msg ?? ''
  if (re === 'blocked') return `连不上（${how}）：防火墙拦着，这是正常的`
  if (re === 'idle' || !p.listen?.length) return `连不上（${how}）：没有程序在监听这个端口`
  if (r.why === 'timeout') return `连不上（${how}）：服务器上已经放行了，多半是云服务商的安全组没放行，或者中间还有别的防火墙`
  return `连不上（${how}）`
}

// ---------------------------------------------------------------- 概览、定位

/** 项目页上方的概览用：还没读到时为 null */
const counts = computed(() => {
  const r = report.value
  if (!r) return null
  const listening = ports.value.filter((p) => p.listen?.length)
  const open = listening.filter((p) => ['open', 'docker', 'partial'].includes(reach(p)))
  const blocked = listening.filter((p) => reach(p) === 'blocked' || reach(p) === 'guarded')
  const risky = open.filter((p) => props.risky?.[p.port]).length
  const docker = open.filter((p) => p.docker).length
  const idle = ports.value.filter((p) => !p.listen?.length && p.firewall === 'open').length
  const temp = ports.value.filter((p) => p.temp).length
  const why = [
    risky ? `${risky} 个不宜对外的端口开着` : '',
    docker ? `${docker} 个 Docker 端口对外开放` : '',
    idle ? `${idle} 个放行了没人用` : '',
    temp ? `${temp} 个重启后失效` : '',
  ]
    .filter(Boolean)
    .join('，')
  return { fw: r.firewall, listening: listening.length, open: open.length, blocked: blocked.length, attention: risky + docker + idle + temp, why }
})

defineExpose({ load, loading, error, counts, test: () => test(ports.value), testing })

const focused = ref<number>()
const box = ref<HTMLElement>()
async function showFocus() {
  if (!props.focus || !report.value) return
  focused.value = props.focus
  await nextTick()
  box.value?.querySelector('.row-focus')?.scrollIntoView({ block: 'center', behavior: 'smooth' })
  setTimeout(() => (focused.value = undefined), 2500)
}
watch(() => [props.focus, report.value], showFocus, { immediate: true })

const rowClass = ({ row }: { row: PortInfo }) => (row.port === focused.value ? 'row-focus' : '')

const fwName = computed(() => {
  const f = fw.value
  if (!f) return ''
  if (!f.kind) return '没有防火墙'
  return f.kind + (f.zone ? `（区域 ${f.zone}）` : '')
})
</script>

<template>
  <div ref="box" class="pp">
    <div class="fw-line" role="group" aria-label="端口操作">
      <GlassPane v-if="glass.enabled" class="cg-strip" :radius="14" :adaptive="false" aria-hidden="true" />
      <el-input v-model="portQuery" class="port-search" placeholder="搜索端口、进程或监听地址" clearable aria-label="搜索服务器端口" />
      <el-tooltip :show-after="HOVER_DELAY" content="从这台电脑连服务器上对外的端口，看实际能不能连上（不改动服务器）" placement="bottom">
        <el-button :loading="testing" :disabled="!report" @click="test(ports)">从本机测试</el-button>
      </el-tooltip>
      <template v-if="report && fw">
        <el-button v-if="fw.enable && !fw.active" type="primary" plain @click="openEnable">打开防火墙</el-button>
        <el-button v-if="fw.disable && fw.active" plain @click="openDisable">关闭防火墙</el-button>
      </template>
      <el-button :icon="Refresh" :loading="loading" circle aria-label="重新查看" @click="load" />
      <span class="spacer" />
      <template v-if="report && fw">
        <span class="fw-state" :class="fw.active ? 'on' : 'off'"><span class="dot" />{{ fw.active ? '防火墙在运行' : '防火墙没开' }}</span>
        <el-tooltip :show-after="HOVER_DELAY" :disabled="!fw.rules?.length" placement="bottom">
          <template #content>放行的规则：<br /><span v-for="r in fw.rules" :key="r" class="mono">{{ r }}<br /></span></template>
          <span class="fw-name" :title="fwName">{{ fwName }}</span>
        </el-tooltip>
      </template>
      <el-tooltip v-if="report && fw" :show-after="HOVER_DELAY" content="云服务器在云服务商那边还有一层「安全组」，Ferry 看不到。用「从本机测试」看这台电脑实际能不能连上" placement="bottom">
        <span class="muted small hint-sg">云服务器还有安全组</span>
      </el-tooltip>
    </div>
    <el-alert v-if="error" type="error" :closable="false" show-icon class="pp-err">
      <template #title>{{ error.unknownHost ? '还没有核对这台服务器的主机公钥' : '查看端口失败' }}</template>
      <div class="mono err">{{ error.msg }}</div>
      <el-button v-if="error.unknownHost" type="primary" size="small" :icon="Key" class="trust-btn" @click="trustRequest = { target, config }">
        核对并记录主机公钥
      </el-button>
    </el-alert>

    <LoadingBlock
      v-if="loading && !report && !error"
      :text="connected ? `正在查看 ${target} 的端口和防火墙（复用已有连接）…` : `正在连接 ${target}，查看端口和防火墙…`"
      :rows="6"
      min-height="220px"
      class="pp-loading"
    />
    <template v-else-if="report && fw">
      <div v-if="fw.why || report.notes?.length" class="fw-notes">
        <span v-if="fw.why" class="fw-why">{{ fw.why }}</span>
        <span v-for="n in report.notes ?? []" :key="n" class="fw-why">{{ n }}</span>
      </div>

      <div class="table-box">
        <el-table :data="matchingPorts" :row-class-name="rowClass" empty-text="没有匹配的端口">
          <el-table-column label="端口" width="110">
            <template #default="{ row }">
              <span class="mono port-num">{{ row.port }}</span><span class="muted proto">/{{ row.proto }}</span>
            </template>
          </el-table-column>
          <el-table-column label="谁在用" min-width="170">
            <template #default="{ row }">
              <template v-if="who(row).name">
                <span class="name">{{ who(row).name }}</span>
                <span class="sub">{{ who(row).sub }}</span>
              </template>
              <span v-else class="muted">—</span>
              <el-tag v-if="row.ssh" size="small" type="info" class="ml">SSH</el-tag>
              <el-tooltip v-if="risky?.[row.port]" :show-after="HOVER_DELAY" :content="risky[row.port]" placement="top">
                <el-tag size="small" type="warning" class="ml">不宜对外</el-tag>
              </el-tooltip>
            </template>
          </el-table-column>
          <el-table-column label="监听" min-width="110">
            <template #default="{ row }">
              <span class="mono" :class="{ muted: localOnly(row) || !row.listen?.length }">{{ listenText(row) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="外部能不能连" min-width="190">
            <template #default="{ row }">
              <el-tooltip :show-after="HOVER_DELAY" :content="reachTip(row)" placement="top">
                <span class="reach" :class="reach(row)"><span class="dot" />{{ reachText[reach(row)] }}</span>
              </el-tooltip>
              <el-tooltip v-if="row.temp" :show-after="HOVER_DELAY" content="这条放行规则没写进永久配置：重启服务器或防火墙后会失效。在这里关掉再放行一次，就会写进永久配置" placement="top">
                <el-tag size="small" type="warning" class="ml">临时</el-tag>
              </el-tooltip>
              <div v-if="ruleLine(row)" class="rule">{{ ruleLine(row) }}</div>
            </template>
          </el-table-column>
          <el-table-column label="从本机连" min-width="120">
            <template #default="{ row }">
              <template v-if="probeOf(row) === 'running'"><span class="muted">测试中…</span></template>
              <el-tooltip v-else-if="probeOf(row)" :show-after="HOVER_DELAY" :content="probeTip(row, probeOf(row) as PortProbe)" placement="top">
                <span v-if="(probeOf(row) as PortProbe).ok" class="probe ok">能连上</span>
                <span v-else class="probe bad">连不上</span>
              </el-tooltip>
              <span v-else class="muted">—</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="190" fixed="right">
            <template #default="{ row }">
              <el-button v-if="canOpen(row)" link type="primary" @click="setPort(row, 'open')">放行</el-button>
              <el-button v-if="canClose(row)" link type="danger" @click="setPort(row, 'close')">关闭</el-button>
              <el-tooltip v-else-if="closeWhy(row)" :show-after="HOVER_DELAY" :content="closeWhy(row)" placement="left">
                <span class="muted small">不能关</span>
              </el-tooltip>
              <el-tooltip v-if="row.docker && row.listen?.length && !localOnly(row)" :show-after="HOVER_DELAY" content="怎么把映射改成只到本机（从根上不对外）" placement="left">
                <el-button link type="primary" @click="remap = row">改映射</el-button>
              </el-tooltip>
              <el-button v-if="testable(row)" link type="primary" :disabled="probeOf(row) === 'running'" @click="test([row])">测试</el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <p v-if="!fw.manage && fw.active" class="muted small">Ferry 只能查看这台服务器的防火墙：{{ fw.why }}</p>
    </template>

    <el-dialog v-if="enableOpen && fw" :model-value="true" append-to-body :title="`打开 ${target} 的防火墙（${fw.kind}）`" width="620px" @close="enableOpen = false">
      <p>打开后，外部只能连下面放行的端口，其他端口外部就连不上了（服务器本机不受影响）。</p>
      <div class="en-sec">一定放行（SSH，Ferry 用它连服务器）：<span class="mono">{{ sshList }}</span></div>
      <div class="en-sec">现在对外开放的端口，勾上的继续放行：</div>
      <el-checkbox-group v-if="candidates.length" v-model="enableSel" class="en-list">
        <el-checkbox v-for="p in candidates" :key="pkey(p)" :value="pkey(p)">
          <span class="mono">{{ pkey(p) }}</span>
          <span class="sub">{{ who(p).name || '—' }}</span>
          <el-tooltip v-if="risky?.[p.port]" :show-after="HOVER_DELAY" :content="risky[p.port]" placement="top">
            <el-tag size="small" type="warning" class="ml">不宜对外</el-tag>
          </el-tooltip>
        </el-checkbox>
      </el-checkbox-group>
      <p v-else class="muted small">除了 SSH，现在没有别的端口对外开放。</p>
      <div class="en-sec">其他要放行的端口（可以不填）：</div>
      <el-input v-model="enableExtra" placeholder="如 51820/udp, 9000" />
      <p class="muted small">Ferry 只扫了 TCP 端口：用 UDP 的程序（WireGuard、DNS 等）要在这里加上。Docker 映射的端口不经过防火墙，不受影响。</p>
      <p v-if="extraBad.length" class="bad-text">看不懂：{{ extraBad.join(' ') }}（写 9000、9000/tcp 或 53/udp）</p>
      <el-alert v-if="needDocker" type="warning" :closable="false" show-icon class="en-docker">
        <template #title>Docker 正在运行</template>
        firewalld 打开时会动 Docker 的网络规则，容器的端口映射可能失效，要重启 Docker 才能恢复。
        <div><el-checkbox v-model="enableDocker">打开后重启 Docker（所有容器会重启一次）</el-checkbox></div>
      </el-alert>
      <p class="muted small">安全保护：打开后 Ferry 马上用新的连接试一次 SSH；连不上的话，3 分钟后服务器会自动关掉防火墙，恢复原样。</p>
      <template #footer>
        <el-button @click="enableOpen = false">取消</el-button>
        <el-button type="primary" :disabled="extraBad.length > 0 || (needDocker && !enableDocker)" @click="doEnable">打开防火墙</el-button>
      </template>
    </el-dialog>

    <el-dialog v-if="disableOpen && fw" :model-value="true" append-to-body :title="`关闭 ${target} 的防火墙（${fw.kind}）`" width="620px" @close="disableOpen = false">
      <p>关闭后防火墙不再拦任何端口：服务器上对外监听的端口，其他机器都能连。</p>
      <div class="en-sec">现在被防火墙拦着、关闭后外部能连上的端口：</div>
      <div v-if="unblocked.length" class="en-list">
        <div v-for="p in unblocked" :key="pkey(p)">
          <span class="mono">{{ pkey(p) }}</span>
          <span class="sub">{{ who(p).name || '—' }}</span>
          <el-tag v-if="reach(p) === 'partial'" size="small" type="info" class="ml">现在只放行部分来源</el-tag>
          <el-tooltip v-if="risky?.[p.port]" :show-after="HOVER_DELAY" :content="risky[p.port]" placement="top">
            <el-tag size="small" type="warning" class="ml">不宜对外</el-tag>
          </el-tooltip>
        </div>
      </div>
      <p v-else class="muted small">没有：现在对外监听的端口防火墙都放行了，关闭后外部能连的端口和现在一样。</p>
      <ul class="dis-notes">
        <li>已经放行的端口照旧能连；只监听本机、只在内部网卡上的端口，外部照样连不上。</li>
        <li v-if="fw.kind === 'firewalld'">开机也不再启动。放行规则留在 firewalld 的配置里，以后点「打开防火墙」时照旧生效。</li>
        <li v-else>开机也不再启用。ufw 的规则留着，以后点「打开防火墙」时照旧生效。</li>
        <li v-if="guarded.length">
          Ferry 在 Docker 规则链里拦着的端口（<span class="mono">{{ guarded.map(pkey).join(' ') }}</span>）不受影响，仍然拦着外部。
        </li>
        <li>云服务器在云服务商那边还有安全组：安全组没放行的端口，外部仍然连不上。</li>
      </ul>
      <el-alert v-if="needDocker" type="warning" :closable="false" show-icon class="en-docker">
        <template #title>Docker 正在运行</template>
        关闭 firewalld 时可能清掉 Docker 的网络规则，容器的端口映射会失效，要重启 Docker 才能恢复。
        <div><el-checkbox v-model="disableDocker">关闭后重启 Docker（所有容器会重启一次）</el-checkbox></div>
      </el-alert>
      <template #footer>
        <el-button @click="disableOpen = false">取消</el-button>
        <el-button type="danger" :disabled="needDocker && !disableDocker" @click="doDisable">关闭防火墙</el-button>
      </template>
    </el-dialog>

    <el-dialog v-if="remap" :model-value="true" append-to-body :title="`把容器 ${remap.container} 的 ${remap.port} 改成只映射到本机`" width="600px" @close="remap = null">
      <p>
        现在是 <span class="mono">{{ remapSpec.from }}</span>，监听所有地址，其他机器都能连。改成
        <span class="mono">{{ remapSpec.to }}</span> 后只有服务器本机能连（本机的程序、nginx 转发照常能用），也就不需要拦截规则。端口映射要重建容器才能改。
      </p>
      <template v-if="remap.manager === '1Panel'">
        <p><b>这是 1Panel 装的应用</b>，请在 1Panel 里改：打开「应用商店 → 已安装」，找到这个应用，在它的参数里取消「端口外部访问」，保存后 1Panel 会重建容器。</p>
        <p class="muted small">不同版本的 1Panel 位置可能不一样。不要直接改 1Panel 生成的 compose 文件，1Panel 下次操作时会覆盖掉。</p>
      </template>
      <template v-else-if="remap.manager === 'compose'">
        <p>
          <b>这是 docker compose 起的</b>（服务 <span class="mono">{{ remap.composeService }}</span>）。打开
          <span class="mono">{{ remap.composeFile || '它的 compose 文件' }}</span>，在这个服务的 <span class="mono">ports</span> 里改：
        </p>
        <pre class="mono snippet">services:
  {{ remap.composeService || '服务名' }}:
    ports:
      - "{{ remapSpec.to }}"   # 原来是 "{{ remapSpec.from }}"</pre>
        <p class="muted small">如果那里写的是变量（如 <span class="mono">${PORT}:3306</span>），就在前面加上 <span class="mono">127.0.0.1:</span>。改好后在服务器上执行（只重建这个服务）：</p>
        <div v-if="composeCmd" class="cmd">
          <span class="mono">{{ composeCmd }}</span>
          <el-button :icon="CopyDocument" link @click="copy(composeCmd)">复制</el-button>
        </div>
      </template>
      <template v-else>
        <p><b>这个容器是直接用 docker run 建的</b>（或认不出来源）。重建时把参数</p>
        <pre class="mono snippet">-p {{ remapSpec.from }}   改成   -p {{ remapSpec.to }}</pre>
        <p class="muted small">Ferry 不替你重建：要照搬原来的数据卷、环境变量、网络、重启策略，漏一项就可能丢数据或起不来。不方便重建的话，用「关闭」在 Docker 的规则链里拦外部访问就行。</p>
      </template>
      <p v-if="remap.guard" class="muted small">改好以后，Ferry 拦这个端口的规则就用不着了，可以点「放行」去掉。</p>
      <template #footer>
        <el-button v-if="canClose(remap)" @click="setPort(remap, 'close'); remap = null">先用「关闭」拦住</el-button>
        <el-button type="primary" @click="remap = null">知道了</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.pp {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.pp-loading {
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ferry-card-radius);
  padding: 14px 16px;
}
.pp-err :deep(.el-alert__content) {
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
.fw-line {
  display: flex;
  align-items: center;
  flex-wrap: nowrap;
  gap: 10px;
  min-height: 32px;
  white-space: nowrap;
  overflow-x: auto;
  padding: 3px;
  margin: -3px;
  position: sticky;
  top: -3px;
  z-index: 2;
  background: var(--el-bg-color-page);
  font-size: 13px;
}
.fw-line > * { flex-shrink: 0; }
.fw-line > .port-search { flex: 0 1 320px; min-width: 120px; }
.fw-line > .spacer { flex-shrink: 1; min-width: 0; }
.fw-line .el-button + .el-button { margin-left: 0; }
.fw-notes { display: flex; flex-wrap: wrap; gap: 6px 12px; font-size: 13px; }
@container workspace (max-width: 700px) { .hint-sg { display: none; } }
.fw-name {
  flex: 0 1 auto;
  min-width: 60px;
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  font-weight: 500;
  cursor: default;
}
.fw-why {
  color: var(--el-color-warning-dark-2);
}
.hint-sg {
  border-bottom: 1px dashed var(--el-border-color);
  cursor: default;
}
.fw-state,
.reach {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 1px 8px;
  border-radius: 10px;
  font-size: 12px;
  white-space: nowrap;
}
.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
}
.fw-state.on,
.reach.blocked,
.reach.guarded {
  background: var(--el-color-success-light-9);
  color: var(--el-color-success-dark-2);
}
.reach.internal {
  background: var(--el-fill-color);
  color: var(--el-text-color-secondary);
}
.en-sec {
  margin: 12px 0 6px;
  font-weight: 500;
}
.en-list {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  max-height: 220px;
  overflow-y: auto;
}
.en-docker {
  margin: 10px 0;
}
.dis-notes {
  margin: 12px 0 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.7;
  color: var(--el-text-color-regular);
}
.bad-text {
  color: var(--el-color-danger);
  font-size: 12px;
}
.fw-state.off,
.reach.open,
.reach.docker {
  background: var(--el-color-warning-light-9);
  color: var(--el-color-warning-dark-2);
}
.reach.partial {
  background: var(--el-color-primary-light-9);
  color: var(--el-color-primary);
}
.reach.local,
.reach.idle,
.reach.unknown {
  background: var(--el-fill-color);
  color: var(--el-text-color-secondary);
}
.port-num {
  font-weight: 600;
}
.proto {
  font-size: 12px;
}
.name {
  font-weight: 500;
}
.sub {
  margin-left: 6px;
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.rule {
  margin-top: 2px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.snippet {
  margin: 6px 0 10px;
  padding: 8px 10px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  white-space: pre;
  overflow-x: auto;
}
.cmd {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
}
.probe.ok {
  color: var(--el-color-success);
}
.probe.bad {
  color: var(--el-color-danger);
}
.table-box {
  /* 固定操作列的层级只在表格内生效，滚动时不能盖过吸顶工具栏。 */
  isolation: isolate;
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
.table-box :deep(.row-focus td.el-table__cell) {
  background: var(--el-color-primary-light-9);
  transition: background 0.3s;
}
</style>
