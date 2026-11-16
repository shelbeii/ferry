<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import DbGrid from './DbGrid.vue'
import { dbCall, dbRun, exportTo, type DbConn, type DbResult, type DbTableInfo } from '../../db'

const props = defineProps<{ conn: DbConn; db: string; schema?: string; table: string }>()
const emit = defineEmits<{ query: [sql: string] }>()

const pageSize = 200
const offsets = ref([0])
const mode = ref<'rows' | 'columns'>('rows')
const result = shallowRef<DbResult>()
const info = shallowRef<DbTableInfo>()
const busy = ref(false)
const error = ref('')
let cancel: (() => void) | undefined

async function load(path = offsets.value) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  const target = { schema: props.schema, table: props.table }
  try {
    if (mode.value === 'columns') {
      info.value = await dbCall<DbTableInfo>(props.conn, 'table', props.db, target)
    } else {
      const r = dbRun<DbResult>(props.conn, 'rows', props.db, { ...target, offset: path.at(-1), limit: pageSize })
      cancel = r.cancel
      const data = await r.done
      if (data.error) throw new Error(data.error)
      result.value = data
      offsets.value = path
    }
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    cancel = undefined
    busy.value = false
  }
}

function show(what: 'rows' | 'columns') {
  mode.value = what
  error.value = ''
  if (what === 'columns' ? !info.value : !result.value) void load()
}

function next() {
  const n = result.value?.rows?.length ?? 0
  if (n) void load([...offsets.value, offsets.value.at(-1)! + n])
}

onMounted(() => void load())
onBeforeUnmount(() => cancel?.())
defineExpose({ reload: () => load() })
</script>

<template>
  <div class="browse-table">
    <div class="bar">
      <el-button-group>
        <el-button size="small" :type="mode === 'rows' ? 'primary' : 'default'" :disabled="busy" @click="show('rows')">数据</el-button>
        <el-button size="small" :type="mode === 'columns' ? 'primary' : 'default'" :disabled="busy" @click="show('columns')">列结构</el-button>
      </el-button-group>
      <el-button size="small" :loading="busy" @click="load()">刷新</el-button>
      <el-button size="small" :disabled="!result?.sql" @click="result?.sql && emit('query', result.sql)">在查询窗口打开</el-button>
      <el-button size="small" @click="exportTo(conn, db, { schema, table }, 'csv')">导出 CSV</el-button>
      <span v-if="mode === 'columns' && info?.engine">引擎 {{ info.engine }}</span>
    </div>
    <el-alert v-if="error" type="error" :closable="false" :title="error" />
    <div v-if="mode === 'rows'" v-loading="busy" class="content">
      <DbGrid :columns="result?.columns ?? []" :rows="result?.rows ?? []" />
    </div>
    <el-table v-else v-loading="busy" :data="info?.columns ?? []" height="100%">
      <el-table-column prop="name" label="列" min-width="140" />
      <el-table-column prop="type" label="类型" min-width="140" />
      <el-table-column label="可空" width="70"><template #default="{ row }">{{ row.nullable ? '是' : '否' }}</template></el-table-column>
      <el-table-column label="主键" width="70"><template #default="{ row }">{{ row.key ? '是' : '' }}</template></el-table-column>
      <el-table-column prop="default" label="默认值" min-width="160" />
    </el-table>
    <div v-if="mode === 'rows' && result" class="bar paging">
      <el-button size="small" :disabled="busy || offsets.length <= 1" @click="load(offsets.slice(0, -1))">上一页</el-button>
      <span>第 {{ offsets.length }} 页 · {{ result.rows?.length ?? 0 }} 行</span>
      <el-button size="small" :disabled="busy || !result.more" @click="next">下一页</el-button>
    </div>
  </div>
</template>

<style scoped>
.browse-table { height: 100%; display: flex; flex-direction: column; min-height: 0; }
.content { flex: 1; min-height: 0; }
.bar { display: flex; gap: 8px; align-items: center; padding: 8px 12px; flex-wrap: wrap; }
.bar :deep(.el-button + .el-button) { margin-left: 0; }
.paging { color: var(--el-text-color-secondary); font-size: 12px; border-top: 1px solid var(--el-border-color-lighter); }
</style>
