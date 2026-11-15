<script setup lang="ts">
import LoadingBlock from './LoadingBlock.vue'
import { computed, onMounted, reactive, ref } from 'vue'
import { shortCommit, time, type HistoryEntry } from '../ferry'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
import { PAGE_SIZES } from '../pager'
import type { TableInstance } from 'element-plus'

const props = defineProps<{ config: string; target: string }>()
const emit = defineEmits<{ close: [] }>()

const visible = ref(true)
const error = ref('')
const rows = ref<HistoryEntry[]>([])
const reads = useReadRequests()
const loading = computed(() => reads.loading.value.history === true)
const actors = ref<string[]>([])
const apps = ref<string[]>([])
const table = ref<TableInstance>()
const page = ref(1)
const pageSize = ref(50)
const total = ref(0)
let snapshot: number | undefined
let applied: string[] = []

interface HistoryPage {
  items: HistoryEntry[]
  total: number
  snapshot: number
  actors?: string[]
  apps?: string[]
}

function emptyFilters() {
  return { app: '', actor: '', action: '', result: '', text: '', dates: null as [string, string] | null }
}
const filters = reactive(emptyFilters())

async function load() {
  reads.cancel('history')
  error.value = ''
  const args = [
    'history', props.target, '-c', props.config, '--page',
    '--limit', String(pageSize.value), '--offset', String((page.value - 1) * pageSize.value),
    ...(snapshot === undefined ? ['--facets'] : ['--snapshot', String(snapshot)]),
    ...applied,
  ]
  try {
    const result = await reads.run('history', (signal) => readCall<HistoryPage>(args, signal))
    snapshot = result.snapshot
    total.value = result.total
    rows.value = result.items
    if (result.actors) actors.value = result.actors
    if (result.apps) apps.value = result.apps
    table.value?.setScrollTop(0)
  } catch (e) {
    if (e instanceof ReadCanceled) return
    rows.value = []
    error.value = (e as Error).message
  }
}

function search() {
  const { app, actor, action, result, text, dates } = filters
  applied = []
  for (const [flag, value] of [
    ['--app', app], ['--actor', actor], ['--action', action], ['--result', result], ['--q', text],
    ['--since', dates?.[0]], ['--until', dates?.[1]],
  ]) {
    if (flag && value?.trim()) applied.push(flag, value.trim())
  }
  reload()
}

function reload() {
  page.value = 1
  snapshot = undefined
  total.value = 0
  rows.value = []
  void load()
}

function resetFilters() {
  Object.assign(filters, emptyFilters())
  search()
}

function changePage(value: number) {
  if (value === page.value) return
  page.value = value
  void load()
}

function changeSize(value: number) {
  if (value === pageSize.value) return
  pageSize.value = value
  page.value = 1
  void load()
}

onMounted(load)

const actionText: Record<string, string> = { deploy: '部署', rollback: '回滚', start: '启动', stop: '停止', restart: '重启', run: '运行', secrets: '更新应用密钥' }
const results = ['OK', 'FAILED', 'ROLLED_BACK', 'ROLLBACK_FAILED']

function resultType(r: string): 'success' | 'danger' | 'warning' | 'info' {
  if (r === 'OK') return 'success'
  if (r === 'FAILED' || r === 'ROLLBACK_FAILED') return 'danger'
  if (r === 'ROLLED_BACK') return 'warning'
  return 'info'
}
</script>

<template>
  <el-dialog v-model="visible" :title="`部署历史（${target}）`" width="1000px" class="history-dialog" append-to-body @close="reads.cancelAll" @closed="emit('close')">
    <div>
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon><el-button link type="primary" @click="reload">重新读取</el-button></el-alert>
      <form class="filters" @submit.prevent="search">
        <el-select v-model="filters.actor" size="small" filterable allow-create clearable placeholder="全部操作人" aria-label="操作人" style="width: 185px">
          <el-option v-for="a in actors" :key="a" :label="a" :value="a" />
        </el-select>
        <el-select v-model="filters.app" size="small" filterable allow-create clearable placeholder="全部应用" aria-label="应用" style="width: 150px">
          <el-option v-for="a in apps" :key="a" :label="a" :value="a" />
        </el-select>
        <el-select v-model="filters.action" size="small" clearable placeholder="全部操作" aria-label="操作" style="width: 108px">
          <el-option v-for="(label, value) in actionText" :key="value" :label="label" :value="value" />
        </el-select>
        <el-select v-model="filters.result" size="small" clearable placeholder="全部结果" aria-label="结果" style="width: 150px">
          <el-option v-for="result in results" :key="result" :label="result" :value="result" />
        </el-select>
        <el-date-picker v-model="filters.dates" type="daterange" value-format="YYYY-MM-DD" size="small" start-placeholder="开始日期" end-placeholder="结束日期" style="width: 238px" />
        <el-input v-model="filters.text" size="small" clearable placeholder="搜索版本、提交或说明" aria-label="搜索关键词" style="width: 200px" />
        <el-button native-type="submit" type="primary" size="small">查询</el-button>
        <el-button size="small" @click="resetFilters">重置</el-button>
      </form>
      <LoadingBlock v-if="loading" :text="`正在读取 ${target} 的部署历史…`" :rows="10" min-height="52vh" />
      <el-table v-else-if="!error" ref="table" :data="rows" size="small" border height="52vh" :empty-text="applied.length ? '没有符合条件的记录' : '还没有记录'">
        <el-table-column type="expand" width="36"><template #default="{ row }"><div class="history-detail"><p><span>完整版本</span><code>{{ row.release || '—' }}</code></p><p><span>提交</span><code>{{ row.commit || '—' }}</code></p><p><span>操作人</span>{{ row.actor || '—' }}</p><p v-if="row.msg"><span>说明</span>{{ row.msg }}</p></div></template></el-table-column>
        <el-table-column label="时间" width="165">
          <template #default="{ row }">{{ time(row.time) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="70">
          <template #default="{ row }">{{ actionText[row.action] ?? row.action }}</template>
        </el-table-column>
        <el-table-column prop="app" label="应用" width="120" />
        <el-table-column label="结果" width="130">
          <template #default="{ row }"><el-tag :type="resultType(row.result)" size="small">{{ row.result }}</el-tag></template>
        </el-table-column>
        <el-table-column label="提交" width="90">
          <template #default="{ row }"><span class="mono">{{ shortCommit(row.commit) }}</span></template>
        </el-table-column>
        <el-table-column prop="actor" label="操作人" min-width="140" show-overflow-tooltip />
        <!-- 说明：只有失败原因、「从某版本回滚」这类才有，成功的部署、启停没有；整列都空时不显示 -->
      </el-table>
      <el-pagination
        v-if="total > 0"
        :current-page="page"
        :page-size="pageSize"
        :page-sizes="PAGE_SIZES"
        :total="total"
        size="small"
        background
        layout="total, sizes, prev, pager, next"
        class="pager"
        @update:current-page="changePage"
        @update:page-size="changeSize"
      />
    </div>
  </el-dialog>
</template>

<style scoped>
.history-detail { padding: 4px 24px 12px; font-size: 12px; }
.history-detail p { display: flex; gap: 18px; overflow-wrap: anywhere; }
.history-detail p > span { min-width: 60px; color: var(--el-text-color-secondary); }
.pager { justify-content: flex-end; margin-top: 10px; }
.filters {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}
</style>
