<script lang="ts">
import { reactive } from 'vue'
import type { SoftwareReport } from '../ferry'

// 各项目各目标的查找结果（切换目标、切回「应用」再回来不用重新查）
const cache = reactive<Record<string, SoftwareReport>>({})
const errors = reactive<Record<string, { msg: string; unknownHost: boolean }>>({})
</script>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { CopyDocument, Key, Plus, Refresh, Grid, List } from '@element-plus/icons-vue'
import { call, FerryError, time, type KeptSoftware, type Software, type SoftwareResult } from '../ferry'
import { secretRequest, startTask, trustRequest, trustedTick, type Task } from '../state'
import LoadingBlock from './LoadingBlock.vue'
import GlassPane from './glass/GlassPane.vue'
import { glass } from '../glass/runtime'
import InstallSoftwareDialog from './InstallSoftwareDialog.vue'
import SoftwareLogsDialog from './SoftwareLogsDialog.vue'
import SoftwareConfigDialog from './SoftwareConfigDialog.vue'

// mode：卡片还是列表（项目页工具栏上切换）
const props = defineProps<{ config: string; target: string; host: string; connected: boolean }>()
const mode = defineModel<'card' | 'list'>('mode', { default: 'card' })
const emit = defineEmits<{ loaded: []; goPort: [port: number]; backup: [] }>()

const key = computed(() => `${props.config}|${props.target}`)
const report = computed(() => cache[key.value])
const error = computed(() => errors[key.value])
const loading = ref(false)

async function load() {
  const k = key.value
  const t = props.target
  loading.value = true
  try {
    cache[k] = await call<SoftwareReport>(['software', t, '-c', props.config])
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

// ---------------------------------------------------------------- 安装、启停、卸载（Ferry 装的软件）

const installOpen = ref(false)
const configOf = ref<Software>()
watch(key, () => { configOf.value = undefined })
const logsName = ref('')
const logsOf = ref<{ title: string; unit?: string; container?: string } | null>(null)
const result = ref<SoftwareResult | null>(null)
const pwShow = ref<{ name: string; account?: string; password: string } | null>(null)
const removing = ref<{ row: Software; purge: boolean } | null>(null)

function openInstall() {
  installOpen.value = true
}

function onInstalled(t: Task) {
  if (t.status === 'ok' && t.data) result.value = t.data as SoftwareResult
  load()
}

const controlLabels = { start: '启动', stop: '停止', restart: '重启' }
type ControlAction = keyof typeof controlLabels

async function confirmControl(row: Software, action: ControlAction, target: string): Promise<boolean> {
  const label = controlLabels[action]
  const name = row.instance || row.name
  const what = row.via === 'docker' ? `容器 ${row.container}` : row.unit ? `系统服务 ${row.unit}` : ''
  const warn: string[] = []
  if (action !== 'start') {
    if (row.hosting) warn.push(`${label}此 nginx 实例会影响它托管的所有站点。`)
    if (row.kind === 'docker') warn.push(`${label} Docker 会让这台服务器上的全部容器${action === 'stop' ? '停止' : '重启'}。`)
    if (row.kind === '1panel') warn.push(`${label}期间 1Panel 面板打不开。`)
    if (row.manager === '1Panel') warn.push('这是 1Panel 装的，1Panel 里会看到状态变化。')
    if (row.listen?.length) warn.push(`连着它的程序（端口 ${row.listen.map((l) => l.slice(l.lastIndexOf(':') + 1)).join('、')}）会断开。`)
  }
  try {
    await ElMessageBox.confirm(`${label} ${target} 上的 ${name}${what ? `（${what}）` : ''}？${warn.join('')}`, `${label} ${name}`, {
      type: action === 'start' ? 'info' : 'warning',
      confirmButtonText: label,
      cancelButtonText: '取消',
    })
    return true
  } catch {
    return false
  }
}

async function control(row: Software, action: ControlAction) {
  const target = props.target, config = props.config, instance = row.instance!
  if (!await confirmControl(row, action, target)) return
  startTask(`${controlLabels[action]} ${target} 上的 ${instance}`, ['software', action, target, instance, '-c', config], () => load())
}

// ---------------------------------------------------------------- 不是 Ferry 装的：按系统服务或容器启停

/** 能不能启停：系统服务（不是 Ferry 部署的应用）、Docker 容器 */
function controllable(row: Software): boolean {
  if (row.via === 'docker') return !!row.container && report.value?.docker === 'ok'
  if (row.via === 'systemd') return !!row.unit && row.manager !== 'Ferry'
  return false
}

function readonlyWhy(row: Software): string {
  if (row.via === 'process') return '直接用命令启动的进程（不是系统服务，也不在容器里）：Ferry 没法再把它启动起来，所以不提供启停'
  if (row.manager === 'Ferry') return '这是 Ferry 部署的应用使用的：请在「应用」里启停、看日志'
  if (row.via === 'docker') return '读不到容器列表（服务器上没有 docker 命令或 Docker 没有运行），不能启停'
  return '只能查看'
}

async function controlOther(row: Software, action: ControlAction) {
  const label = controlLabels[action]
  const target = props.target, config = props.config
  const what = row.via === 'docker' ? `容器 ${row.container}` : `系统服务 ${row.unit}`
  const args = ['software', action, target, ...(row.via === 'docker' ? ['--container', row.container!] : ['--unit', row.unit!]), '-y', '-c', config]
  if (!await confirmControl(row, action, target)) return
  startTask(`${label} ${target} 上的 ${row.name}（${what}）`, args, () => load())
}

async function showPassword(row: Software) {
  try {
    const r = await call<{ password: string }>(['software', 'password', props.target, row.instance!, '-c', props.config])
    pwShow.value = { name: row.instance!, account: row.account, password: r.password }
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---------------------------------------------------------------- 升级、设置备份、恢复保留了数据的

const dbKinds = ['mysql', 'mariadb', 'postgresql']
const canBackup = (row: Software) => !!row.instance && dbKinds.includes(row.kind ?? '')

const upgrading = ref<{ row: Software; version: string; latest: string; latestError: string; loading: boolean; major: boolean } | null>(null)
async function openUpgrade(row: Software) {
  upgrading.value = { row, version: '', latest: '', latestError: '', loading: true, major: false }
  const u = upgrading.value // 改响应式的那份，界面才会跟着变
  try {
    u.latest = (await call<{ version: string }>(['software', 'latest', row.kind!])).version
  } catch (e) {
    u.latestError = (e as Error).message
  } finally {
    u.loading = false
  }
}
const upTo = computed(() => upgrading.value?.version.trim() || upgrading.value?.latest || '')
const majorOf = (v?: string) => (v ?? '').replace(/^v/i, '').split('.')[0]
const upMajor = computed(() => !!upgrading.value?.row.version && !!upTo.value && majorOf(upgrading.value.row.version) !== majorOf(upTo.value))
function upgrade() {
  const u = upgrading.value
  if (!u || !upTo.value) return
  const args = ['software', 'upgrade', props.target, u.row.instance!, '--version', upTo.value, ...(u.major ? ['--major'] : []), '-c', props.config]
  startTask(`把 ${props.target} 上的 ${u.row.instance} 升级到 ${upTo.value}`, args, () => load())
  upgrading.value = null
}

const backingUp = ref<{ row: Software; database: string; busy: boolean } | null>(null)
async function setBackup() {
  const b = backingUp.value
  if (!b) return
  b.busy = true
  try {
    const db = b.database.trim()
    const r = await call<{ backup: string }>(['software', 'backup', props.target, b.row.instance!, ...(db ? ['--database', db] : []), '-c', props.config])
    ElMessage.success(`已在配置里加上数据备份 ${r.backup}：在「数据备份」里打开后每天备份`)
    backingUp.value = null
    emit('backup')
  } catch (e) {
    ElMessage.error((e as Error).message)
    b.busy = false
  }
}

function backupFromResult() {
  const r = result.value
  if (!r) return
  backingUp.value = { row: { name: r.title || r.kind, instance: r.name, kind: r.kind, state: 'running', via: 'systemd' } as Software, database: '', busy: false }
  result.value = null
}

function restore(k: KeptSoftware) {
  startTask(`恢复 ${props.target} 上的 ${k.name}`, ['software', 'restore', props.target, k.name, '-c', props.config], () => load())
}
async function dropKept(k: KeptSoftware) {
  try {
    await ElMessageBox.confirm(`删掉 ${k.name} 保留的安装目录 ${k.dir}？里面的数据都会删掉，不能恢复。`, `删除 ${k.name} 的数据`, { type: 'warning', confirmButtonText: '删除' })
  } catch {
    return
  }
  startTask(`删除 ${props.target} 上 ${k.name} 保留的数据`, ['software', 'remove', props.target, k.name, '--purge', '-y', '-c', props.config], () => load())
}
// 最近一次安装没成功、也没有装上：说明原因（连接断开时本机没拿到结果）
const lastFailed = computed(() => {
  const l = report.value?.lastInstall
  if (!l || l.ok || allItems.value.some((i) => i.instance === l.name)) return undefined
  return Date.now() - new Date(l.time).getTime() < 3 * 24 * 3600 * 1000 ? l : undefined
})

function remove() {
  const r = removing.value
  if (!r) return
  const args = ['software', 'remove', props.target, r.row.instance!, '-y', ...(r.purge ? ['--purge'] : []), '-c', props.config]
  startTask(`卸载 ${props.target} 上的 ${r.row.instance}`, args, () => load())
  removing.value = null
}

async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制')
  } catch {
    ElMessage.error('复制失败，请手动选中复制')
  }
}

const withHost = (s: string) => s.replaceAll('<服务器地址>', props.host)

// 其他机器能连的：到「端口」里看防火墙放没放行
const publicPorts = computed(() => (result.value?.public ? [...new Set(result.value.openPorts ?? [])].filter((p) => p > 0) : []))
function goPort(p: number) {
  result.value = null
  emit('goPort', p)
}

const softwareQuery = ref('')
const allItems = computed<Software[]>(() => report.value?.items ?? [])
const items = computed(() => allItems.value.filter((i) => JSON.stringify(i).toLowerCase().includes(softwareQuery.value.trim().toLowerCase())))
const running = computed(() => allItems.value.filter((i) => i.state === 'running').length)
// 项目页上方的概览用：还没读到时为 null
const counts = computed(() => (report.value ? { total: allItems.value.length, running: running.value } : null))

defineExpose({ load, loading, error, openInstall, counts })

const dockerNote = computed(() => {
  const d = report.value?.docker
  if (d === undefined || d === 'ok') return ''
  if (d === '') return '服务器上没有 Docker'
  return d
})

const viaText: Record<Software['via'], string> = { systemd: '系统服务', docker: 'Docker 容器', process: '直接运行' }

function viaTip(s: Software): string {
  switch (s.via) {
    case 'systemd':
      return `systemd 系统服务 ${s.unit}：开机自动启动（如已启用），用 systemctl 管理`
    case 'docker':
      return `Docker 容器 ${s.container}，镜像 ${s.image ?? '未知'}`
    default:
      return '不是系统服务也不在容器里：可能是手动用命令（nohup、脚本）启动的，服务器重启后不会自动启动'
  }
}

function managerTip(m: string): string {
  if (m === '1Panel') return '由 1Panel 应用商店安装（Docker 容器），在 1Panel 里管理'
  if (m === '宝塔面板') return '装在宝塔面板的目录（/www/server）下，由宝塔面板管理'
  if (m === 'Ferry') return '由 Ferry 部署的应用使用（Ferry 生成的系统服务）'
  return `由 ${m} 启动`
}

function stateTip(s: Software): string {
  const parts: string[] = []
  if (s.since) parts.push(`启动于 ${time(s.since)}`)
  if (s.status) parts.push(`容器状态：${s.status}`)
  if (s.state === 'stopped' && s.via === 'systemd') parts.push('服务装了，但现在没有运行')
  return parts.join('；') || (s.state === 'running' ? '运行中' : '已停止')
}

const portOf = (l: string) => l.slice(l.lastIndexOf(':') + 1)
const isLocal = (l: string) => l.startsWith('127.') || l.startsWith('[::1]')
function listenTip(l: string): string {
  const host = l.slice(0, l.lastIndexOf(':'))
  if (host === '*') return `监听所有地址（${l}），外部可以访问（取决于防火墙）`
  if (isLocal(l)) return `只监听本机（${l}）：外部访问不到`
  return `监听 ${l}`
}
</script>

<template>
  <div class="sw">
    <div class="software-toolbar" role="group" aria-label="软件操作">
      <GlassPane v-if="glass.enabled" class="cg-strip" :radius="14" :adaptive="false" aria-hidden="true" />
      <el-input v-model="softwareQuery" class="software-search" placeholder="搜索软件、版本或端口" clearable aria-label="搜索服务器软件" />
      <el-button type="primary" :icon="Plus" @click="openInstall">安装软件</el-button>
      <el-button :icon="Refresh" :loading="loading" circle aria-label="重新查看" @click="load" />
      <el-radio-group v-model="mode" class="view-switch" aria-label="显示方式">
        <el-tooltip :show-after="HOVER_DELAY" content="卡片" placement="bottom"><el-radio-button value="card" aria-label="卡片"><el-icon><Grid /></el-icon></el-radio-button></el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" content="列表" placement="bottom"><el-radio-button value="list" aria-label="列表"><el-icon><List /></el-icon></el-radio-button></el-tooltip>
      </el-radio-group>
    </div>
    <el-alert v-if="error" type="error" :closable="false" show-icon class="sw-err">
      <template #title>{{ error.unknownHost ? '还没有核对这台服务器的主机公钥' : '查看软件失败' }}</template>
      <div class="mono err">{{ error.msg }}</div>
      <el-button v-if="error.unknownHost" type="primary" size="small" :icon="Key" class="trust-btn" @click="trustRequest = { target, config }">
        核对并记录主机公钥
      </el-button>
    </el-alert>

    <LoadingBlock
      v-if="loading && !report && !error"
      :text="connected ? `正在查看 ${target} 上已有的软件（复用已有连接）…` : `正在连接 ${target}，查看已有的软件…`"
      :rows="6"
      min-height="220px"
      class="sw-loading"
    />
    <template v-else-if="report">
      <!-- 找到几个、几个在运行已经在上方的概览里：这里只在有说明（没装 Docker、读取不了等）时显示 -->
      <div v-if="dockerNote || report.notes?.length" class="sw-head muted">
        <span v-if="dockerNote" class="docker-note" :class="{ warn: report.docker }">{{ dockerNote }}</span>
        <span v-for="n in report.notes ?? []" :key="n" class="docker-note warn">{{ n }}</span>
      </div>
      <el-alert v-if="lastFailed" type="warning" :closable="false" show-icon>
        <template #title>{{ time(lastFailed.time).slice(5, 16) }} 在这台服务器上安装 {{ lastFailed.name }} 没成功：{{ lastFailed.msg }}</template>
      </el-alert>
      <div v-if="report.kept?.length" class="kept">
        <div class="muted small">卸载时保留了数据的软件：可以恢复（照原样启动），不需要了就删掉数据</div>
        <div v-for="k in report.kept" :key="k.name" class="kept-row">
          <b>{{ k.name }}</b>
          <span class="muted">{{ [k.title, k.version].filter(Boolean).join(' ') }}</span>
          <span class="mono small">{{ k.dir }}</span>
          <span class="muted small">{{ time(k.removedAt).slice(5, 16) }} 卸载</span>
          <span class="spacer" />
          <el-button link type="primary" size="small" @click="restore(k)">恢复</el-button>
          <el-button link type="danger" size="small" @click="dropKept(k)">删除数据</el-button>
        </div>
      </div>
      <!-- 卡片 -->
      <div v-if="mode === 'card'" class="cards">
        <article v-for="row in items" :key="(row.unit ?? row.container ?? '') + row.name + (row.pid ?? '')" class="card">
          <div class="card-top">
            <div class="card-name">
              <span class="name">{{ row.name }}</span>
              <el-tooltip :show-after="HOVER_DELAY" v-if="row.manager" :content="managerTip(row.manager)" placement="top">
                <el-tag size="small" type="info" class="ml">{{ row.manager }}</el-tag>
              </el-tooltip>
            </div>
            <el-tooltip :show-after="HOVER_DELAY" :content="stateTip(row)" placement="top">
              <span class="state" :class="row.state === 'running' ? 'ok' : 'off'"><span class="dot" />{{ row.state === 'running' ? '运行中' : '已停止' }}</span>
            </el-tooltip>
          </div>
          <dl class="card-fields">
            <div>
              <dt>版本</dt>
              <dd class="mono ellipsis" :title="row.version">{{ row.version || '—' }}</dd>
            </div>
            <div>
              <dt>PID</dt>
              <dd class="mono">{{ row.pid || '—' }}</dd>
            </div>
            <div>
              <dt>端口</dt>
              <dd class="mono">
                <el-tooltip :show-after="HOVER_DELAY" v-for="l in row.listen ?? []" :key="l" :content="listenTip(l)" placement="top">
                  <span class="port" :class="{ local: isLocal(l) }">{{ portOf(l) }}</span>
                </el-tooltip>
                <el-tooltip :show-after="HOVER_DELAY" v-if="row.inner?.length && !row.listen?.length" placement="top">
                  <template #content>容器里监听 {{ row.inner.join('、') }}，没有映射到主机：只有同一个 Docker 网络里的容器能访问</template>
                  <span class="muted">容器内 {{ row.inner.map(portOf).join(' ') }}</span>
                </el-tooltip>
                <span v-if="!row.listen?.length && !row.inner?.length" class="muted">—</span>
              </dd>
            </div>
            <div>
              <dt>启动于</dt>
              <dd>{{ time(row.since).slice(5, 16) || '—' }}</dd>
            </div>
            <div class="wide">
              <dt>运行方式</dt>
              <el-tooltip :show-after="HOVER_DELAY" :content="viaTip(row)" placement="top">
                <dd class="ellipsis">
                  {{ viaText[row.via as Software['via']] }}
                  <span v-if="row.unit" class="mono sub">{{ row.unit }}</span>
                  <span v-else-if="row.container" class="mono sub">{{ row.container }}</span>
                </dd>
              </el-tooltip>
            </div>
            <div class="wide">
              <dt>程序 / 镜像</dt>
              <dd class="mono ellipsis" :title="row.image ?? row.path ?? row.dir ?? ''">{{ row.image ?? row.path ?? row.dir ?? '—' }}</dd>
            </div>
          </dl>
          <div class="card-foot">
            <el-button link type="primary" size="small" @click="configOf = row">配置</el-button>
            <template v-if="row.instance">
              <el-button v-if="row.state !== 'running'" link type="primary" size="small" @click="control(row, 'start')">启动</el-button>
              <template v-else>
                <el-button link type="primary" size="small" @click="control(row, 'restart')">重启</el-button>
                <el-button link type="primary" size="small" @click="control(row, 'stop')">停止</el-button>
              </template>
              <el-button link type="primary" size="small" @click="logsName = row.instance">日志</el-button>
              <el-button link type="primary" size="small" @click="showPassword(row)">密码</el-button>
              <el-button v-if="row.upgradable" link type="primary" size="small" @click="openUpgrade(row)">升级</el-button>
              <el-button v-if="canBackup(row)" link type="primary" size="small" @click="backingUp = { row, database: '', busy: false }">备份</el-button>
              <span class="spacer" />
              <el-button link type="danger" size="small" @click="removing = { row, purge: false }">卸载</el-button>
            </template>
            <template v-else-if="controllable(row)">
              <el-button v-if="row.state !== 'running'" link type="primary" size="small" @click="controlOther(row, 'start')">启动</el-button>
              <template v-else>
                <el-button link type="primary" size="small" @click="controlOther(row, 'restart')">重启</el-button>
                <el-button link type="primary" size="small" @click="controlOther(row, 'stop')">停止</el-button>
              </template>
              <el-button
                link
                type="primary"
                size="small"
                @click="logsOf = { title: row.name, unit: row.via === 'systemd' ? row.unit : undefined, container: row.via === 'docker' ? row.container : undefined }"
              >
                日志
              </el-button>
            </template>
            <el-tooltip :show-after="HOVER_DELAY" v-else :content="readonlyWhy(row)" placement="top">
              <span class="muted small">只读</span>
            </el-tooltip>
          </div>
        </article>
        <el-empty v-if="!items.length" :description="softwareQuery.trim() ? '没有匹配的软件，请调整搜索条件' : '没有找到认得的软件（数据库、缓存、Nacos、Nginx、Docker 容器等）'" :image-size="60" class="cards-empty" />
      </div>
      <div v-else class="table-box">
      <el-table :data="items" empty-text="没有找到认得的软件（数据库、缓存、Nacos、Nginx、Docker 容器等）">
        <el-table-column label="软件" min-width="150">
          <template #default="{ row }">
            <span class="name">{{ row.name }}</span>
            <el-tooltip :show-after="HOVER_DELAY" v-if="row.manager" :content="managerTip(row.manager)" placement="top">
              <el-tag size="small" type="info" class="ml">{{ row.manager }}</el-tag>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="版本" min-width="110">
          <template #default="{ row }"><span class="mono">{{ row.version ?? '' }}</span></template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tooltip :show-after="HOVER_DELAY" :content="stateTip(row)" placement="top">
              <span class="state" :class="row.state === 'running' ? 'ok' : 'off'"><span class="dot" />{{ row.state === 'running' ? '运行中' : '已停止' }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="运行方式" min-width="200">
          <template #default="{ row }">
            <el-tooltip :show-after="HOVER_DELAY" :content="viaTip(row)" placement="top">
              <span>
                {{ viaText[row.via as Software['via']] }}
                <span v-if="row.unit" class="mono sub">{{ row.unit }}</span>
                <span v-else-if="row.container" class="mono sub">{{ row.container }}</span>
              </span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="端口" min-width="120">
          <template #default="{ row }">
            <el-tooltip :show-after="HOVER_DELAY" v-for="l in row.listen ?? []" :key="l" :content="listenTip(l)" placement="top">
              <span class="port mono" :class="{ local: isLocal(l) }">{{ portOf(l) }}</span>
            </el-tooltip>
            <el-tooltip :show-after="HOVER_DELAY" v-if="row.inner?.length && !row.listen?.length" placement="top">
              <template #content>容器里监听 {{ row.inner.join('、') }}，没有映射到主机：只有同一个 Docker 网络里的容器能访问</template>
              <span class="muted inner">容器内 {{ row.inner.map(portOf).join(' ') }}</span>
            </el-tooltip>
            <el-tooltip :show-after="HOVER_DELAY"
              v-if="row.state === 'running' && !row.listen?.length && !row.inner?.length"
              content="没有监听网络端口：可能只通过本机的 socket 文件通信（如 1Panel 的 agent），或者还在启动"
              placement="top"
            >
              <span class="muted inner">无</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="PID" width="80">
          <template #default="{ row }">{{ row.pid || '' }}</template>
        </el-table-column>
        <el-table-column label="启动时间" width="170">
          <template #default="{ row }">{{ time(row.since) }}</template>
        </el-table-column>
        <el-table-column label="程序 / 镜像" min-width="220" :show-overflow-tooltip="{ showAfter: HOVER_DELAY }">
          <template #default="{ row }"><span class="mono">{{ row.image ?? row.path ?? row.dir ?? '' }}</span></template>
        </el-table-column>
        <el-table-column label="操作" width="350" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="configOf = row">配置</el-button>
            <template v-if="row.instance">
              <el-button v-if="row.state !== 'running'" link type="primary" @click="control(row, 'start')">启动</el-button>
              <template v-else>
                <el-button link type="primary" @click="control(row, 'restart')">重启</el-button>
                <el-button link type="primary" @click="control(row, 'stop')">停止</el-button>
              </template>
              <el-button link type="primary" @click="logsName = row.instance">日志</el-button>
              <el-button link type="primary" @click="showPassword(row)">密码</el-button>
              <el-button v-if="row.upgradable" link type="primary" @click="openUpgrade(row)">升级</el-button>
              <el-button v-if="canBackup(row)" link type="primary" @click="backingUp = { row, database: '', busy: false }">备份</el-button>
              <el-button link type="danger" @click="removing = { row, purge: false }">卸载</el-button>
            </template>
            <template v-else-if="controllable(row)">
              <el-button v-if="row.state !== 'running'" link type="primary" @click="controlOther(row, 'start')">启动</el-button>
              <template v-else>
                <el-button link type="primary" @click="controlOther(row, 'restart')">重启</el-button>
                <el-button link type="primary" @click="controlOther(row, 'stop')">停止</el-button>
              </template>
              <el-button link type="primary" @click="logsOf = { title: row.name, unit: row.via === 'systemd' ? row.unit : undefined, container: row.via === 'docker' ? row.container : undefined }">
                日志
              </el-button>
            </template>
            <el-tooltip :show-after="HOVER_DELAY" v-else :content="readonlyWhy(row)" placement="left">
              <span class="muted small">只读</span>
            </el-tooltip>
          </template>
        </el-table-column>
      </el-table>
      </div>
    </template>

    <InstallSoftwareDialog v-if="installOpen" :config="config" :target="target" :report="report" :done="onInstalled" @close="installOpen = false" />
    <SoftwareConfigDialog v-if="configOf" :key="key" :config="config" :target="target" :software="configOf" @close="configOf = undefined" @changed="load" />
    <SoftwareLogsDialog v-if="logsName" :config="config" :target="target" :name="logsName" @close="logsName = ''" />
    <SoftwareLogsDialog
      v-if="logsOf"
      :config="config"
      :target="target"
      :name="logsOf.title"
      :unit="logsOf.unit"
      :container="logsOf.container"
      @close="logsOf = null"
    />

    <el-dialog v-if="result" :model-value="true" append-to-body title="安装完成" width="max-content" class="software-info-dialog" @close="result = null">
      <p>
        {{ result.target }} 上已安装 <b>{{ result.name }}</b>（{{ [result.title || result.kind, result.version].filter(Boolean).join(' ') }}），系统服务 <span class="mono">{{ result.unit }}</span>，安装目录
        <span class="mono">{{ result.dir }}</span>。
      </p>
      <div v-if="result.account" class="pw">
        <span>账号：</span>
        <span class="mono pw-val">{{ result.account }}</span>
        <el-button :icon="CopyDocument" link @click="copy(result.account ?? '')">复制</el-button>
      </div>
      <div v-if="result.password" class="pw">
        <span>密码：</span>
        <span class="mono pw-val">{{ result.password }}</span>
        <el-button :icon="CopyDocument" link @click="copy(result.password ?? '')">复制</el-button>
      </div>
      <p v-if="result.password" class="muted small">{{ result.saved ? '已存进本机的系统凭据库，以后点这一行的「密码」还能看到。' : '没能存进系统凭据库：请现在记下来。' }}</p>
      <ul class="notes">
        <li v-for="n in result.notes ?? []" :key="n">{{ withHost(n) }}</li>
      </ul>
      <p v-if="publicPorts.length" class="muted small">
        {{ result.public === 'chosen' ? '选了允许其他机器连接' : '这个软件监听所有地址' }}：其他机器要连的话，服务器开着防火墙时还要放行端口
        {{ publicPorts.join('、') }}{{ result.openUDP?.length ? `，UDP 端口 ${result.openUDP.join('、')}` : '' }}；只给同一台服务器上的程序用就不用放行{{ result.public === 'always' ? '，不放行反而更安全' : '' }}。
      </p>
      <template #footer>
        <el-button v-if="publicPorts.length" @click="goPort(publicPorts[0])">到「端口」里看看</el-button>
        <el-button v-if="dbKinds.includes(result.kind)" @click="backupFromResult">设置数据备份</el-button>
        <el-button type="primary" @click="result = null">知道了</el-button>
      </template>
    </el-dialog>

    <el-dialog v-if="pwShow" :model-value="true" append-to-body :title="`${pwShow.name} 的${pwShow.account ? '账号和' : ''}密码`" width="max-content" class="software-info-dialog" @close="pwShow = null">
      <div v-if="pwShow.account" class="pw">
        <span>账号：</span>
        <span class="mono pw-val">{{ pwShow.account }}</span>
        <el-button :icon="CopyDocument" link @click="copy(pwShow.account ?? '')">复制</el-button>
      </div>
      <div class="pw">
        <span v-if="pwShow.account">密码：</span>
        <span class="mono pw-val">{{ pwShow.password }}</span>
        <el-button :icon="CopyDocument" link @click="copy(pwShow.password)">复制</el-button>
      </div>
      <p class="muted small">安装时保存在本机的系统凭据库里（设置 → 凭据）。如果在软件里改过密码，这里不会跟着变。</p>
    </el-dialog>

    <el-dialog v-if="removing" :model-value="true" append-to-body :title="`卸载 ${removing.row.instance}`" width="480px" @close="removing = null">
      <p>停止并删除系统服务 <span class="mono">{{ removing.row.unit }}</span>。</p>
      <el-checkbox v-model="removing.purge">同时删除安装目录 <span class="mono">{{ removing.row.dir }}</span>（里面的数据都会删掉，不能恢复）</el-checkbox>
      <p v-if="!removing.purge" class="muted small">不勾选时保留安装目录和数据（密码也留着）：以后可以在这里恢复，或再删掉数据。</p>
      <template #footer>
        <el-button @click="removing = null">取消</el-button>
        <el-button type="danger" @click="remove">{{ removing.purge ? '卸载并删除数据' : '卸载' }}</el-button>
      </template>
    </el-dialog>

    <el-dialog v-if="upgrading" :model-value="true" append-to-body :title="`升级 ${upgrading.row.instance}`" width="520px" @close="upgrading = null">
      <p>现在是 <b>{{ upgrading.row.version || '（不知道版本）' }}</b>。换掉程序，配置和数据带到新版本；新版本起不来时自动换回现在的版本。</p>
      <el-form label-width="90px">
        <el-form-item label="升到">
          <el-input v-model="upgrading.version" class="mono" :placeholder="upgrading.loading ? '正在查最新版本…' : upgrading.latest || '写要升到的版本'" />
          <div class="muted small">
            <template v-if="upgrading.latest">最新 {{ upgrading.latest }}，不填就升到它</template>
            <template v-else-if="!upgrading.loading">查不到最新版本：写上版本（没有下载地址的软件，先在「安装软件」里导入新版本的安装包）</template>
          </div>
        </el-form-item>
      </el-form>
      <el-alert v-if="upMajor" type="warning" :closable="false" show-icon>
        <template #title>从 {{ upgrading.row.version }} 升到 {{ upTo }} 是跨大版本升级：配置、数据可能不兼容</template>
        <el-checkbox v-model="upgrading.major">我看过新版本的升级说明，已经备份</el-checkbox>
      </el-alert>
      <p v-if="canBackup(upgrading.row)" class="muted small">数据库升级前建议先在「数据备份」里备份一次：新版本启动时会升级数据，换回旧版本不一定能用新数据。</p>
      <template #footer>
        <el-button @click="upgrading = null">取消</el-button>
        <el-button type="primary" :disabled="!upTo || upTo === upgrading.row.version || (upMajor && !upgrading.major)" @click="upgrade">升级</el-button>
      </template>
    </el-dialog>

    <el-dialog v-if="backingUp" :model-value="true" append-to-body :title="`给 ${backingUp.row.instance} 设置数据备份`" width="520px" @close="backingUp = null">
      <p>在服务器上写好备份用的密码文件（只有 root 能读），在项目配置的 backups 里加上一项。之后在「数据备份」里打开，每天 3 点备份、保留 7 份，也可以随时「现在备份」。</p>
      <el-form label-width="90px">
        <el-form-item label="备份哪个库">
          <el-input v-model="backingUp.database" class="mono" placeholder="不填 = 全部库" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="backingUp = null">取消</el-button>
        <el-button type="primary" :loading="backingUp.busy" @click="setBackup">设置备份</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
:global(.el-dialog.software-info-dialog) {
  min-width: min(460px, calc(100vw - 40px));
  max-width: min(960px, calc(100vw - 40px));
}
:global(.software-info-dialog .el-dialog__body) {
  overflow-wrap: anywhere;
}
.kept {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 12px;
  border: 1px dashed var(--el-border-color);
  border-radius: 10px;
}
.kept-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.kept-row .spacer {
  flex: 1;
}
.sw {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.software-toolbar { display: flex; align-items: center; flex-wrap: nowrap; gap: 10px; min-height: 32px; overflow-x: auto; padding: 3px; margin: -3px; position: sticky; top: -3px; z-index: 2; background: var(--el-bg-color-page); }
.software-toolbar > * { flex-shrink: 0; }
.software-toolbar > .software-search { flex: 0 1 300px; min-width: 120px; }
.software-toolbar .el-button + .el-button { margin-left: 0; }
.software-toolbar .el-radio-group { flex-wrap: nowrap; }
.view-switch :deep(.el-radio-button__inner) { padding: 8px 10px; }
.sw-head {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  font-size: 13px;
}
.spacer {
  flex: 1;
}
.hint {
  font-size: 12px;
}
.docker-note.warn {
  color: var(--el-color-warning);
}
.name {
  font-weight: 500;
}
.sub {
  margin-left: 4px;
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.ml {
  margin-left: 6px;
}
.port {
  display: inline-block;
  margin-right: 6px;
}
.port.local {
  color: var(--el-text-color-secondary);
  border-bottom: 1px dashed var(--el-border-color);
}
.inner {
  font-size: 12px;
}
.err {
  white-space: pre-wrap;
  margin: 4px 0 0;
}
.trust-btn {
  margin-top: 8px;
}
.small {
  font-size: 12px;
}
.pw {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 10px 0 4px;
}
.pw-val {
  min-width: 0;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
  font-size: 15px;
  padding: 2px 8px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  user-select: all;
}
.pw > :not(.pw-val) {
  flex-shrink: 0;
}
.notes {
  margin: 8px 0 0;
  padding-left: 18px;
  line-height: 1.8;
}
.sw-loading {
  background: var(--el-bg-color);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: var(--ferry-card-radius);
  padding: 14px 16px;
}
.sw-err :deep(.el-alert__content) {
  width: 100%;
}
/* ---- 状态：小圆点 + 文字（和应用的一样） */
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
.state.off {
  background: var(--el-fill-color);
  color: var(--el-text-color-secondary);
}
.state.off .dot {
  background: var(--el-text-color-placeholder);
}
/* ---- 卡片 */
.cards {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(250px, 1fr));
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
}
.card-top {
  display: flex;
  align-items: center;
  gap: 8px;
}
.card-name {
  flex: 1;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.card .name {
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
.card-fields .sub {
  font-size: 12px;
}
.ellipsis {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* 底部一栏贴着卡片底边：同一行的卡片里操作按钮在一条线上 */
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
.card-fields .port.local {
  color: inherit;
}
.card-foot .el-button + .el-button {
  margin-left: 0;
}
/* ---- 列表 */
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
</style>
