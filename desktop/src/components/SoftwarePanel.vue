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
import { call, FerryError, time, type Software, type SoftwareResult } from '../ferry'
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
const emit = defineEmits<{ loaded: []; goPort: [port: number] }>()

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
const pwShow = ref<{ name: string; password: string } | null>(null)
const removing = ref<{ row: Software; purge: boolean } | null>(null)

function openInstall() {
  installOpen.value = true
}

function onInstalled(t: Task) {
  if (t.status === 'ok' && t.data) result.value = t.data as SoftwareResult
  load()
}

async function control(row: Software, action: 'start' | 'stop' | 'restart') {
  const label = { start: '启动', stop: '停止', restart: '重启' }[action]
	const target=props.target,config=props.config
	if(row.hosting && action!=='start'){try{await ElMessageBox.confirm(`${label}此 nginx 实例会影响它托管的所有站点。确定继续？`,`${label} nginx`,{type:'warning',confirmButtonText:label})}catch{return}}
	startTask(`${label} ${target} 上的 ${row.instance}`, ['software', action, target, row.instance!, '-c', config], () => load())
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

async function controlOther(row: Software, action: 'start' | 'stop' | 'restart') {
  const label = { start: '启动', stop: '停止', restart: '重启' }[action]
  const what = row.via === 'docker' ? `容器 ${row.container}` : `系统服务 ${row.unit}`
  if (action !== 'start') {
    const warn: string[] = []
    if (row.kind === 'docker') warn.push(`${label} Docker 会让这台服务器上的全部容器${action === 'stop' ? '停止' : '重启'}。`)
    if (row.kind === '1panel') warn.push(`${label}期间 1Panel 面板打不开。`)
    if (row.manager === '1Panel') warn.push('这是 1Panel 装的，1Panel 里会看到状态变化。')
    if (row.listen?.length) warn.push(`连着它的程序（端口 ${row.listen.map((l) => l.slice(l.lastIndexOf(':') + 1)).join('、')}）会断开。`)
    try {
      await ElMessageBox.confirm(`${label} ${props.target} 上的 ${row.name}（${what}）？${warn.join('')}`, `${label} ${row.name}`, {
        type: 'warning',
        confirmButtonText: label,
      })
    } catch {
      return
    }
  }
  const args = ['software', action, props.target, ...(row.via === 'docker' ? ['--container', row.container!] : ['--unit', row.unit!]), '-y', '-c', props.config]
  startTask(`${label} ${props.target} 上的 ${row.name}（${what}）`, args, () => load())
}

async function showPassword(row: Software) {
  try {
    const r = await call<{ password: string }>(['software', 'password', props.target, row.instance!, '-c', props.config])
    pwShow.value = { name: row.instance!, password: r.password }
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

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

// 装的时候选了允许其他机器连接：到「端口」里看防火墙放没放行
const publicPorts = computed(() => (result.value?.public ? [...new Set(Object.values(result.value.ports ?? {}))].filter((p) => p > 0) : []))
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
        <el-table-column label="操作" width="270" fixed="right">
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

    <el-dialog v-if="result" :model-value="true" append-to-body title="安装完成" width="560px" @close="result = null">
      <p>
        {{ result.target }} 上已安装 <b>{{ result.name }}</b>（{{ [result.title || result.kind, result.version].filter(Boolean).join(' ') }}），系统服务 <span class="mono">{{ result.unit }}</span>，安装目录
        <span class="mono">{{ result.dir }}</span>。
      </p>
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
        选了允许其他机器连接：服务器开着防火墙的话，还要放行端口 {{ publicPorts.join('、') }}；只给同一台服务器上的程序用就不用放行。
      </p>
      <template #footer>
        <el-button v-if="publicPorts.length" @click="goPort(publicPorts[0])">到「端口」里看看</el-button>
        <el-button type="primary" @click="result = null">知道了</el-button>
      </template>
    </el-dialog>

    <el-dialog v-if="pwShow" :model-value="true" append-to-body :title="`${pwShow.name} 的密码`" width="460px" @close="pwShow = null">
      <div class="pw">
        <span class="mono pw-val">{{ pwShow.password }}</span>
        <el-button :icon="CopyDocument" link @click="copy(pwShow.password)">复制</el-button>
      </div>
      <p class="muted small">安装时保存在本机的系统凭据库里（设置 → 凭据）。如果在软件里改过密码，这里不会跟着变。</p>
    </el-dialog>

    <el-dialog v-if="removing" :model-value="true" append-to-body :title="`卸载 ${removing.row.instance}`" width="480px" @close="removing = null">
      <p>停止并删除系统服务 <span class="mono">{{ removing.row.unit }}</span>。</p>
      <el-checkbox v-model="removing.purge">同时删除安装目录 <span class="mono">{{ removing.row.dir }}</span>（里面的数据都会删掉，不能恢复）</el-checkbox>
      <p v-if="!removing.purge" class="muted small">不勾选时保留安装目录和数据，不需要了可以以后手动删除。</p>
      <template #footer>
        <el-button @click="removing = null">取消</el-button>
        <el-button type="danger" @click="remove">{{ removing.purge ? '卸载并删除数据' : '卸载' }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
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
  font-size: 15px;
  padding: 2px 8px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  user-select: all;
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
