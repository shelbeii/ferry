<script setup lang="ts">
// 一张表的标签页：数据（翻页、排序、筛选、改格子、加行、删行，保存前看 SQL）、结构、建表语句。
import { computed, onMounted, reactive, ref, shallowRef } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Refresh, Plus, Delete, Check, RefreshLeft, Download, EditPen, CopyDocument, ArrowLeft, ArrowRight } from '@element-plus/icons-vue'
import DbGrid from './DbGrid.vue'
import DbFilterBar from './DbFilterBar.vue'
import { dbCall, confirmSql, exportTo, exportLabels, type DbConn, type DbResult, type DbTableInfo, type DbChange, type DbCell, type DbFilter, type DbSort, type ExportKind } from '../../db'
import { HOVER_DELAY } from '../../ui'

const props = defineProps<{ conn: DbConn; db: string; schema?: string; table: string; view?: boolean }>()
const emit = defineEmits<{ design: []; query: [sql: string] }>()

const tab = ref<'data' | 'structure' | 'ddl'>('data')
const info = shallowRef<DbTableInfo | null>(null)
const result = shallowRef<DbResult | null>(null)
const loading = ref(false)
const error = ref('')
// 生效的筛选、排序（筛选面板里点了「应用」的）
const filters = ref<DbFilter[]>([])
const sorts = ref<DbSort[]>([])
const where = ref('')
const panel = ref<'' | 'filter' | 'sort'>('')
const browse = () => ({ where: where.value, filters: filters.value, sorts: sorts.value })
const pageSize = ref(200)
const offset = ref(0)
const total = ref<number | null>(null)
const counting = ref(false)
const saving = ref(false)

// 改动
const edits = reactive<Record<number, Record<number, string | null>>>({})
const deleted = ref(new Set<number>())
const added = ref<(string | null | undefined)[][]>([])
const changeCount = computed(() => Object.values(edits).reduce((n, r) => n + Object.keys(r).length, 0) + deleted.value.size + added.value.length)
const grid = ref<InstanceType<typeof DbGrid>>()

const editable = computed(() => !props.view && !props.conn.readOnly && (info.value?.key.length ?? 0) > 0)
const readOnlyCols = computed(() => {
  const cols = result.value?.columns ?? []
  return cols.map((c, i) => (info.value?.columns.find((x) => x.name === c.name)?.generated ? i : -1)).filter((i) => i >= 0)
})
const whyReadOnly = computed(() => {
  if (props.view) return '视图不能直接改数据'
  if (props.conn.readOnly) return '这个连接设成了只读'
  if (info.value && !info.value.key.length) return '这张表没有主键（或不可为空的唯一索引），不能直接改数据；可以在查询里写 UPDATE'
  return ''
})

function resetChanges() {
  for (const k of Object.keys(edits)) delete edits[Number(k)]
  deleted.value = new Set()
  added.value = []
}

async function confirmDiscard(): Promise<boolean> {
  if (!changeCount.value) return true
  try {
    await ElMessageBox.confirm(`有 ${changeCount.value} 处改动还没保存，要放弃吗？`, '放弃改动', { type: 'warning', confirmButtonText: '放弃', cancelButtonText: '不放弃' })
    return true
  } catch {
    return false
  }
}

async function loadInfo() {
  info.value = await dbCall<DbTableInfo>(props.conn, 'table', props.db, { schema: props.schema, table: props.table })
}

async function load(keepCount = true) {
  loading.value = true
  error.value = ''
  try {
    if (!info.value) await loadInfo()
    const r = await dbCall<DbResult>(props.conn, 'rows', props.db, { schema: props.schema, table: props.table, ...browse(), offset: offset.value, limit: pageSize.value })
    result.value = r
    resetChanges()
    grid.value?.clearSelection()
    if (!keepCount) total.value = null
    if (total.value === null && (r.more || offset.value > 0)) void count()
    else if (!r.more && offset.value === 0) total.value = r.rows?.length ?? 0
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function count() {
  counting.value = true
  try {
    total.value = (await dbCall<{ count: number }>(props.conn, 'count', props.db, { schema: props.schema, table: props.table, ...browse() })).count
  } catch {
    total.value = null
  } finally {
    counting.value = false
  }
}

async function reload() {
  if (!(await confirmDiscard())) return
  info.value = null
  await load(false)
}
async function applyFilter(f: DbFilter[], s: DbSort[], w: string) {
  if (!(await confirmDiscard())) return
  const filterChanged = JSON.stringify([f, w]) !== JSON.stringify([filters.value, where.value])
  filters.value = f
  sorts.value = s
  where.value = w
  offset.value = 0
  await load(!filterChanged)
}
// 点表头：只按这一列，升序 → 降序 → 不排序
async function sortBy(col: string) {
  if (!(await confirmDiscard())) return
  const cur = sorts.value.length === 1 && sorts.value[0].col === col ? sorts.value[0] : null
  sorts.value = !cur ? [{ col, desc: false }] : !cur.desc ? [{ col, desc: true }] : []
  offset.value = 0
  await load()
}
async function page(dir: number) {
  if (!(await confirmDiscard())) return
  offset.value = Math.max(0, offset.value + dir * pageSize.value)
  await load()
}
async function changePageSize() {
  offset.value = 0
  await load()
}

const pageText = computed(() => {
  const n = result.value?.rows?.length ?? 0
  if (!n) return offset.value ? `第 ${offset.value + 1} 行之后没有了` : ''
  return `第 ${offset.value + 1}–${offset.value + n} 行`
})

// 改格子、加行、删行
function onEdit(row: number, col: number, v: string | null) {
  const orig = result.value?.rows?.[row]?.[col]
  if (!edits[row]) edits[row] = {}
  if (orig === v) {
    delete edits[row][col]
    if (!Object.keys(edits[row]).length) delete edits[row]
  } else {
    edits[row][col] = v
  }
}
function onEditAdded(row: number, col: number, v: string | null | undefined) {
  const r = [...added.value[row]]
  r[col] = v
  added.value[row] = r
}
function addRow() {
  added.value = [...added.value, (result.value?.columns ?? []).map(() => undefined)]
}
function onRemove(rows: number[]) {
  const s = new Set(deleted.value)
  const allDeleted = rows.every((r) => s.has(r))
  for (const r of rows) {
    if (allDeleted) s.delete(r)
    else s.add(r)
  }
  deleted.value = s
}
function removeSelected() {
  const rows = grid.value?.selectedRows() ?? []
  if (!rows.length) return ElMessage.info('先点选要删的行（按住 Ctrl 或 Shift 可以选多行）')
  onRemove(rows)
}
function onRemoveAdded(row: number) {
  added.value = added.value.filter((_, i) => i !== row)
}

function buildChanges(): DbChange[] {
  const cols = result.value?.columns ?? []
  const keyIdx = (info.value?.key ?? []).map((k) => cols.findIndex((c) => c.name === k))
  if (keyIdx.some((i) => i < 0)) throw new Error('结果里没有主键列')
  const keyOf = (row: DbCell[]) => Object.fromEntries((info.value?.key ?? []).map((k, j) => [k, row[keyIdx[j]] as string | null]))
  const out: DbChange[] = []
  for (const [r, cs] of Object.entries(edits)) {
    const row = result.value!.rows![Number(r)]
    if (deleted.value.has(Number(r))) continue
    out.push({ op: 'update', key: keyOf(row), values: Object.fromEntries(Object.entries(cs).map(([c, v]) => [cols[Number(c)].name, v])) })
  }
  for (const r of deleted.value) out.push({ op: 'delete', key: keyOf(result.value!.rows![r]) })
  for (const a of added.value) {
    const values: Record<string, string | null> = {}
    a.forEach((v, i) => { if (v !== undefined) values[cols[i].name] = v })
    out.push({ op: 'insert', values })
  }
  return out
}

async function save() {
  if (!changeCount.value) return
  saving.value = true
  try {
    const changes = buildChanges()
    const req = { schema: props.schema, table: props.table, changes }
    const pre = await dbCall<{ sql: string[] }>(props.conn, 'apply', props.db, { ...req, preview: true })
    if (!(await confirmSql(props.conn, '保存改动', pre.sql, { okText: '保存', note: '在一个事务里执行：有一条没改到正好一行（比如这一行已经被别人改了），全部撤销。' }))) return
    const r = await dbCall<{ affected: number; ms: number }>(props.conn, 'apply', props.db, req)
    ElMessage.success(`已保存（影响 ${r.affected} 行，${r.ms} 毫秒）`)
    total.value = null
    await load()
  } catch (e) {
    ElMessageBox.alert((e as Error).message, '保存失败', { type: 'error' })
  } finally {
    saving.value = false
  }
}

// 导出：数据按当前的筛选、排序
function exportTable(kind: ExportKind) {
  void exportTo(props.conn, props.db, { schema: props.schema, table: props.table }, kind, browse())
}

async function copyDDL() {
  await navigator.clipboard.writeText(info.value?.ddl ?? '')
  ElMessage.success({ message: '已复制', duration: 1200 })
}

async function openStructure(t: 'structure' | 'ddl') {
  tab.value = t
  if (!info.value) {
    try { await loadInfo() } catch (e) { error.value = (e as Error).message }
  }
}

onMounted(() => load(false))

defineExpose({ hasChanges: () => changeCount.value > 0, reload: () => { info.value = null; return load(false) } })
</script>

<template>
  <div class="tv">
    <div class="bar">
      <el-radio-group v-model="tab" size="small">
        <el-radio-button value="data">数据</el-radio-button>
        <el-radio-button value="structure" @click="openStructure('structure')">结构</el-radio-button>
        <el-radio-button value="ddl" @click="openStructure('ddl')">{{ view ? '视图定义' : '建表语句' }}</el-radio-button>
      </el-radio-group>
      <template v-if="tab === 'data'">
        <DbFilterBar v-model:panel="panel" :columns="info?.columns ?? []" :filters="filters" :sorts="sorts" :where="where" @apply="applyFilter">
          <el-button size="small" :icon="Refresh" :loading="loading" @click="reload">刷新</el-button>
        </DbFilterBar>
      </template>
      <span class="grow" />
      <el-button v-if="!view" size="small" :icon="EditPen" :disabled="conn.readOnly" @click="emit('design')">设计表</el-button>
      <el-dropdown trigger="click" @command="exportTable">
        <el-button size="small" :icon="Download">导出</el-button>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item command="struct">{{ exportLabels.struct }}</el-dropdown-item>
            <el-dropdown-item command="dump">{{ exportLabels.dump }}</el-dropdown-item>
            <el-dropdown-item command="csv" divided>{{ exportLabels.csv }}{{ filters.length || where ? '，按当前筛选' : '' }}</el-dropdown-item>
            <el-dropdown-item command="insert">{{ exportLabels.insert }}{{ filters.length || where ? '，按当前筛选' : '' }}</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
    </div>

    <template v-if="tab === 'data'">
      <div v-if="editable || changeCount" class="bar edit-bar">
        <el-button size="small" :icon="Plus" @click="addRow">加一行</el-button>
        <el-button size="small" :icon="Delete" @click="removeSelected">删除选中行</el-button>
        <span v-if="changeCount" class="changes">{{ changeCount }} 处改动没保存</span>
        <el-button size="small" type="primary" :icon="Check" :disabled="!changeCount" :loading="saving" @click="save">保存</el-button>
        <el-button size="small" :icon="RefreshLeft" :disabled="!changeCount" @click="resetChanges">撤销改动</el-button>
        <span class="tip">双击格子修改，回车确认；按住拖动选多行；右键可以设为 NULL</span>
      </div>
      <el-alert v-else-if="whyReadOnly && result" type="info" :closable="false" :title="whyReadOnly" class="ro" />
      <el-alert v-if="error" type="error" :closable="false" :title="error" class="err" />
      <div v-loading="loading" class="grid-box">
        <DbGrid
          v-if="result"
          ref="grid"
          :columns="result.columns ?? []"
          :rows="result.rows ?? []"
          :editable="editable"
          :read-only-cols="readOnlyCols"
          :edits="edits"
          :deleted="deleted"
          :added="added"
          :sorts="sorts"
          :offset="offset"
          @sort="sortBy"
          @edit="onEdit"
          @edit-added="onEditAdded"
          @remove="onRemove"
          @remove-added="onRemoveAdded"
        />
      </div>
      <div class="foot">
        <el-button size="small" :icon="ArrowLeft" :disabled="offset === 0 || loading" @click="page(-1)" />
        <span>{{ pageText }}</span>
        <el-button size="small" :icon="ArrowRight" :disabled="!result?.more || loading" @click="page(1)" />
        <el-select v-model="pageSize" size="small" class="psize" @change="changePageSize">
          <el-option v-for="n in [100, 200, 500, 1000]" :key="n" :value="n" :label="`每页 ${n} 行`" />
        </el-select>
        <span class="muted">
          <template v-if="total !== null">共 {{ total.toLocaleString() }} 行</template>
          <el-button v-else link size="small" :loading="counting" @click="count">数一下共多少行</el-button>
        </span>
        <span class="grow" />
        <span v-if="result" class="muted">{{ result.ms }} 毫秒</span>
        <el-tooltip :show-after="HOVER_DELAY" content="在查询窗口里打开这条 SELECT" placement="top">
          <el-button v-if="result" link size="small" @click="emit('query', result.sql)">在查询里打开</el-button>
        </el-tooltip>
      </div>
    </template>

    <div v-else-if="tab === 'structure'" class="structure">
      <el-alert v-if="error && !info" type="error" :closable="false" :title="error" />
      <template v-if="info">
        <div v-if="info.comment" class="comment">说明：{{ info.comment }}</div>
        <h4>列（{{ info.columns.length }}）</h4>
        <el-table :data="info.columns" size="small" border>
          <el-table-column label="" width="40" align="center">
            <template #default="{ row }"><span v-if="row.key" title="主键" class="pk">🔑</span></template>
          </el-table-column>
          <el-table-column prop="name" label="列名" min-width="140" />
          <el-table-column prop="type" label="类型" min-width="140" />
          <el-table-column label="可为空" width="70" align="center">
            <template #default="{ row }">{{ row.nullable ? '是' : '' }}</template>
          </el-table-column>
          <el-table-column label="默认值" min-width="120">
            <template #default="{ row }"><span class="mono">{{ row.default ?? '' }}</span></template>
          </el-table-column>
          <el-table-column label="其他" min-width="120">
            <template #default="{ row }">{{ [row.auto ? '自增' : '', row.generated ? '计算列' : '', row.extra, row.collation].filter(Boolean).join('，') }}</template>
          </el-table-column>
          <el-table-column prop="comment" label="说明" min-width="160" />
        </el-table>
        <h4>索引（{{ info.indexes.length }}）</h4>
        <el-table :data="info.indexes" size="small" border>
          <el-table-column prop="name" label="名称" min-width="160" />
          <el-table-column label="列" min-width="200">
            <template #default="{ row }">{{ row.def || row.columns.join(', ') }}</template>
          </el-table-column>
          <el-table-column label="类型" width="160">
            <template #default="{ row }">{{ row.primary ? '主键' : row.unique ? '唯一' : '普通' }}{{ row.method ? ' · ' + row.method : '' }}</template>
          </el-table-column>
        </el-table>
        <template v-if="info.foreign.length">
          <h4>外键（{{ info.foreign.length }}）</h4>
          <el-table :data="info.foreign" size="small" border>
            <el-table-column prop="name" label="名称" min-width="160" />
            <el-table-column prop="def" label="定义" min-width="300" />
          </el-table>
        </template>
      </template>
    </div>

    <div v-else class="ddl">
      <div class="ddl-bar"><el-button size="small" :icon="CopyDocument" @click="copyDDL">复制</el-button></div>
      <pre>{{ info?.ddl }}</pre>
    </div>
  </div>
</template>

<style scoped>
.tv {
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.bar {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  border-bottom: 1px solid var(--el-border-color-lighter);
  flex-wrap: wrap;
}
.edit-bar {
  padding-top: 4px;
  padding-bottom: 4px;
}
.where {
  width: 420px;
  max-width: 50%;
}
.where :deep(.el-input-group__prepend) {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12px;
  padding: 0 8px;
}
.grow {
  flex: 1;
}
.changes {
  color: var(--el-color-warning);
  font-size: 12.5px;
}
.tip,
.muted {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.ro,
.err {
  margin: 6px 10px 0;
  width: auto;
}
.grid-box {
  flex: 1;
  min-height: 0;
  position: relative;
}
.foot {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 10px;
  border-top: 1px solid var(--el-border-color-lighter);
  font-size: 12.5px;
}
.psize {
  width: 120px;
}
.structure {
  flex: 1;
  overflow: auto;
  padding: 8px 14px 20px;
}
.structure h4 {
  margin: 14px 0 6px;
  font-size: 13px;
}
.comment {
  margin-top: 6px;
  color: var(--el-text-color-regular);
}
.mono {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12px;
}
.pk {
  font-size: 11px;
}
.ddl {
  flex: 1;
  overflow: auto;
  position: relative;
}
.ddl-bar {
  position: sticky;
  top: 0;
  display: flex;
  justify-content: flex-end;
  padding: 6px 10px 0;
}
.ddl pre {
  margin: 0;
  padding: 4px 16px 20px;
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12.5px;
  line-height: 1.6;
  white-space: pre-wrap;
}
</style>
