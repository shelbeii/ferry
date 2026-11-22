<script lang="ts">
import { reactive } from 'vue'
import type { InstalledRuntime as RuntimeRow } from '../ferry'

// 安装和卸载任务可能跨过页签切换才结束，通知当前打开的清单重新读取。
const inventoryRevisions = reactive<Record<string, number>>({})

// 各项目、各服务器上次读到的运行环境清单：切换页签面板会重新挂载，切回来先显示它，
// 间隔没到（1 分钟）就不再去读服务器
const cache = reactive<Record<string, { rows: RuntimeRow[]; at: number }>>({})
const freshMs = 60_000
</script>

<script setup lang="ts">
import { computed, defineAsyncComponent, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Grid, Key, List, Plus, Refresh } from '@element-plus/icons-vue'
import { FerryError, type InstalledRuntime } from '../ferry'
import { glass } from '../glass/runtime'
import { useInteractionScope } from '../interactionScope'
import { useManualRefresh } from '../manualRefresh'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
import type { RowAction } from '../rowActions'
import { secretRequest, startTask, trustRequest, trustedTick } from '../state'
import GlassPane from './glass/GlassPane.vue'
import InstallRuntimeDialog from './InstallRuntimeDialog.vue'
import LoadingBlock from './LoadingBlock.vue'
import RowActions from './RowActions.vue'

const FilesDrawer = defineAsyncComponent(() => import('./FilesDrawer.vue'))

const props = defineProps<{ config: string; target: string; connected: boolean }>()
const emit = defineEmits<{ loaded: [] }>()
const mode = defineModel<'card' | 'list'>('mode', { default: 'card' })
const key = computed(() => `${props.config}|${props.target}`)
const captureContext = useInteractionScope(() => key.value)
const reads = useReadRequests()
const loading = computed(() => !!reads.loading.value[key.value])
const manual = useManualRefresh(() => key.value)
const refreshing = manual.refreshing
const rows = computed(() => cache[key.value]?.rows)
const error = ref<{ message: string; unknownHost: boolean }>()
const installOpen = ref(false)
const removing = ref<InstalledRuntime>()
const fileLocation = ref<InstalledRuntime>()
const filesOpen = ref(false)
const query = ref('')
const kindFilter = ref('all')
const names = { jdk: 'JDK', node: 'Node.js', python: 'Python', dotnet: '.NET' } satisfies Record<InstalledRuntime['kind'], string>
const components = { aspnetcore: 'ASP.NET Core Runtime', runtime: '.NET Runtime', sdk: '.NET SDK' }

function runtimeType(row: InstalledRuntime) {
  return row.component ? components[row.component] : names[row.kind]
}

const counts = computed(() => {
  const installed = rows.value
  if (!installed) return undefined
  return { total: installed.length, shared: installed.filter((row) => row.shared).length }
})

const shown = computed(() => {
  const term = query.value.trim().toLowerCase()
  return (rows.value ?? []).filter((row) => (kindFilter.value === 'all' || row.kind === kindFilter.value) &&
    [runtimeType(row), row.id, row.version, row.arch, row.path].some((value) => value?.toLowerCase().includes(term)))
})

async function load(_background = false, signal?: AbortSignal) {
  const { config, target } = props
  const k = key.value
  const current = captureContext()
  try {
    await reads.run(k, async (s) => {
      const result = await readCall<InstalledRuntime[]>(['software', 'runtime', 'list', target, '-c', config], s)
      if (!current()) return
      cache[k] = { rows: result, at: Date.now() }
      error.value = undefined
    }, { signal })
  } catch (e) {
    if (!current() || e instanceof ReadCanceled || signal?.aborted) return
    const err = e as Error
    error.value = { message: err.message, unknownHost: err instanceof FerryError && err.unknownHost }
    if (err instanceof FerryError && err.needSecret && err.detail) {
      secretRequest.value = { detail: err.detail, config, retry: () => { if (current()) void load() } }
    }
  } finally {
    if (current()) emit('loaded')
  }
}

watch([key, () => inventoryRevisions[key.value]], ([nextKey, revision], previous) => {
  const [previousKey, previousRevision] = previous ?? []
  if (previousKey) reads.cancel(previousKey)
  if (nextKey !== previousKey) {
    error.value = undefined
    installOpen.value = false
    removing.value = undefined
    fileLocation.value = undefined
    filesOpen.value = false
  }
  // 装/卸刚结束（revision 变了）或者上次读的已经过期，才去读服务器
  const changed = revision !== previousRevision
  const cached = cache[nextKey]
  if (changed || !cached || Date.now() - cached.at >= freshMs) void load()
}, { immediate: true })
watch(trustedTick, () => { if (error.value?.unknownHost) void load() })

function refresh() {
  return manual.run('运行环境', () => load(), () => !!rows.value && !error.value)
}

function install(name: string, version: string, args: string[]) {
  if (!installOpen.value) return
  const { config, target } = props
  const k = key.value
  installOpen.value = false
  startTask(`在 ${target} 安装 ${name} ${version}`, [...args, '-c', config], (task) => {
    if (task.status !== 'ok') return
    inventoryRevisions[k] = (inventoryRevisions[k] ?? 0) + 1
    ElMessage.success(`${target} 的 ${name} 已就绪`)
  })
}

function runtimeName(row: InstalledRuntime) {
  return `${runtimeType(row)} ${row.version || row.java}`
}

function rowActions(row: InstalledRuntime): RowAction[] {
  return [
    { id: 'location', label: '查看位置', title: row.home, run: () => { fileLocation.value = row; filesOpen.value = true } },
    { id: 'copy', label: '复制路径', run: () => copyPath(row.path) },
    { id: 'remove', label: '卸载', danger: true, run: () => { removing.value = row } },
  ]
}

function remove() {
  const row = removing.value
  if (!row) return
  const { config, target } = props
  const k = key.value
  removing.value = undefined
  startTask(`卸载 ${target} 上的 ${runtimeName(row)}`, ['software', 'runtime', 'remove', target, row.path, '--yes', '-c', config], (task) => {
    inventoryRevisions[k] = (inventoryRevisions[k] ?? 0) + 1
    if (task.status === 'ok') ElMessage.success(`已卸载 ${runtimeName(row)}`)
  })
}

async function copyPath(path: string) {
  try {
    await navigator.clipboard.writeText(path)
    ElMessage.success('已复制')
  } catch {
    ElMessage.error('复制失败，请手动选中复制')
  }
}

defineExpose({ load, loading, refreshing, error, counts })
</script>

<template>
  <div class="runtime-panel" :class="{ refreshing }">
    <div class="software-toolbar" role="group" aria-label="运行环境操作">
      <GlassPane v-if="glass.enabled" class="cg-strip" :radius="14" :adaptive="false" aria-hidden="true" />
      <el-input v-model="query" class="software-search" placeholder="搜索类型、版本或路径" clearable aria-label="搜索运行环境" />
      <el-select v-model="kindFilter" class="runtime-filter" aria-label="运行环境类型">
        <el-option label="全部类型" value="all" />
        <el-option v-for="(name, kind) in names" :key="kind" :label="name" :value="kind" />
      </el-select>
      <el-button type="primary" :icon="Plus" @click="installOpen = true">安装运行环境</el-button>
      <el-tooltip :show-after="HOVER_DELAY" content="只刷新运行环境" placement="bottom">
        <el-button :icon="Refresh" :loading="loading || refreshing" circle aria-label="刷新运行环境" @click="refresh" />
      </el-tooltip>
      <el-radio-group v-model="mode" class="view-switch" aria-label="运行环境显示方式">
        <el-tooltip :show-after="HOVER_DELAY" content="卡片" placement="bottom"><el-radio-button value="card" aria-label="卡片"><el-icon><Grid /></el-icon></el-radio-button></el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" content="列表" placement="bottom"><el-radio-button value="list" aria-label="列表"><el-icon><List /></el-icon></el-radio-button></el-tooltip>
      </el-radio-group>
    </div>

    <el-alert v-if="error" type="error" :closable="false" show-icon>
      <template #title>{{ error.unknownHost ? '还没有核对这台服务器的主机公钥' : '读取运行环境失败' }}</template>
      <div class="mono runtime-error">{{ error.message }}</div>
      <el-button v-if="error.unknownHost" type="primary" size="small" :icon="Key" @click="trustRequest = { target, config }">
        核对并记录主机公钥
      </el-button>
    </el-alert>

    <LoadingBlock v-if="loading && !rows && !error" :text="connected ? '正在读取运行环境…' : `正在连接 ${target}，查看运行环境…`" />
    <template v-else-if="rows">
      <el-empty v-if="!shown.length" :description="query.trim() || kindFilter !== 'all' ? '没有匹配的运行环境' : '还没有 Ferry 管理的运行环境'" :image-size="64" />
      <div v-else-if="mode === 'card'" class="cards" aria-label="运行环境卡片">
        <article v-for="row in shown" :key="row.path" class="card">
          <div class="card-top">
            <strong class="card-name" :title="runtimeName(row)">{{ runtimeName(row) }}</strong>
            <el-tag v-if="row.unavailable" size="small" type="warning">程序不可用</el-tag>
            <el-tag v-else-if="row.shared" size="small" type="info">共享 Java {{ row.java }}</el-tag>
            <el-tag v-else-if="row.custom" size="small" type="info">自定义目录</el-tag>
          </div>
          <dl class="card-fields">
            <div><dt>架构</dt><dd>{{ row.arch }}</dd></div>
            <div class="wide"><dt>程序路径</dt><dd class="mono ellipsis" :title="row.path">{{ row.path }}</dd></div>
          </dl>
          <div class="card-foot">
            <RowActions :actions="rowActions(row)" :label="runtimeName(row)" :inline-count="0" />
          </div>
        </article>
      </div>
      <div v-else class="table-box">
        <el-table :data="shown" row-key="path" size="small" aria-label="运行环境列表">
          <el-table-column label="运行环境" min-width="165"><template #default="{ row }">{{ runtimeType(row) }}</template></el-table-column>
          <el-table-column label="版本" min-width="140"><template #default="{ row }">{{ row.version || row.java }}</template></el-table-column>
          <el-table-column prop="arch" label="架构" width="95" />
          <el-table-column label="安装位置" width="115"><template #default="{ row }"><span v-if="row.unavailable">程序不可用</span><span v-else-if="row.shared" class="mono">java-{{ row.java }}</span><span v-else>{{ row.custom ? '自定义目录' : '公共目录' }}</span></template></el-table-column>
          <el-table-column prop="path" label="程序路径" min-width="210" show-overflow-tooltip />
          <el-table-column label="操作" width="100" fixed="right"><template #default="{ row }"><RowActions :actions="rowActions(row)" :label="runtimeName(row)" :inline-count="0" /></template></el-table-column>
        </el-table>
      </div>
    </template>

    <InstallRuntimeDialog v-if="installOpen" :config="config" :target="target" @close="installOpen = false" @install="install" />
    <FilesDrawer v-if="fileLocation" v-model="filesOpen" :config="config" :target="target" :title="`${runtimeName(fileLocation)} · ${target}`" :path="fileLocation.home" />
    <el-dialog v-if="removing" :model-value="true" append-to-body :title="`卸载 ${runtimeName(removing)}`" width="min(520px, 90vw)" @close="removing = undefined">
      <p>将从 {{ target }} 删除以下安装目录{{ removing.kind === 'jdk' ? '及指向它的 Ferry 共享或锁定版本入口' : '' }}。</p>
      <div class="mono runtime-error">{{ removing.home }}</div>
      <p v-if="removing.shared">此版本提供共享入口 <code>java-{{ removing.java }}</code>。</p>
      <template #footer>
        <el-button @click="removing = undefined">取消</el-button>
        <el-button type="danger" @click="remove">卸载</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped src="./serverInventory.css"></style>

<style scoped>
.runtime-panel { display: flex; flex-direction: column; gap: 10px; }
.runtime-panel.refreshing > :not(.software-toolbar) { opacity: 0.4; transition: opacity 0.2s; }
.runtime-error { overflow-wrap: anywhere; }
.runtime-error + .el-button { margin-top: 6px; }
.runtime-filter { width: 115px; }
.card-foot { justify-content: flex-end; }
.table-box { min-width: 0; }
</style>
