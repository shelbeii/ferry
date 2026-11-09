<script setup lang="ts">
// 「对象」标签：点左边的库（模式）或下面的「表 / 视图 / 函数 / 查询 / 备份」时，右边列出里面的东西，像 Navicat 的对象列表。
// 单击选中、双击打开；上面的按钮和右键菜单对选中的那个操作（具体做法交给 DbDrawer）。
import { computed, nextTick, onMounted, ref, watch, type Component } from 'vue'
import {
  Refresh, Search, Grid, View, Plus, Delete, EditPen, Upload, Download, FolderOpened, ArrowDown, ArrowUp, VideoPlay, Document, Box, RefreshLeft, Folder,
} from '@element-plus/icons-vue'
import { dbCall, humanSize, exportLabels, type DbConn, type DbTable, type DbRoutine, type DbFileItem, type DbObjItem, type DbObjWhat } from '../../db'

const props = defineProps<{ conn: DbConn; db: string; schema?: string; what: DbObjWhat }>()
const emit = defineEmits<{
  open: [it: DbObjItem]
  /** 做什么（右键菜单里的 k）；it 为空时是对整个库（模式、分组） */
  action: [k: string, it: DbObjItem | null]
  context: [e: MouseEvent, it: DbObjItem]
}>()

const all = ref<DbObjItem[]>([])
const loading = ref(false)
const error = ref('')
const filter = ref('')
const selected = ref('')
const sort = ref<{ col: string; desc: boolean }>({ col: 'name', desc: false })
const body = ref<HTMLElement>()

const isPg = computed(() => props.conn.kind === 'postgres')
const ro = computed(() => !!props.conn.readOnly)
const isTables = computed(() => props.what === 'all' || props.what === 'table' || props.what === 'view')
const inSchema = (s?: string) => !props.schema || (s || 'public') === props.schema

let seq = 0
async function load() {
  const my = ++seq
  loading.value = true
  error.value = ''
  try {
    let items: DbObjItem[]
    if (isTables.value) {
      const r = await dbCall<{ tables: DbTable[] }>(props.conn, 'tables', props.db, { detail: true })
      items = r.tables
        .filter((t) => inSchema(t.schema) && (props.what === 'all' || t.type === props.what))
        .map((t) => ({ kind: t.type, key: `${t.schema ?? ''}.${t.name}`, name: t.name, t }))
    } else if (props.what === 'func') {
      const r = await dbCall<{ routines: DbRoutine[] }>(props.conn, 'routines', props.db)
      items = r.routines.filter((x) => inSchema(x.schema)).map((x) => ({ kind: 'func', key: `${x.schema ?? ''}.${x.name}.${x.type}.${x.oid ?? ''}`, name: x.name, r: x }))
    } else {
      const r = await dbCall<{ items: DbFileItem[] }>(props.conn, props.what === 'query' ? 'queries' : 'backups', props.db)
      items = r.items.map((f) => ({ kind: props.what as 'query' | 'backup', key: f.name, name: f.name, f }))
    }
    if (my !== seq) return
    all.value = items
    if (!items.some((x) => x.key === selected.value)) selected.value = ''
  } catch (e) {
    if (my !== seq) return
    error.value = (e as Error).message
    all.value = []
  } finally {
    if (my === seq) loading.value = false
  }
}
watch(() => [props.conn.id, props.db, props.schema, props.what], () => {
  selected.value = ''
  filter.value = ''
  sort.value = { col: props.what === 'backup' ? 'modified' : 'name', desc: props.what === 'backup' }
  void load()
})
onMounted(() => {
  if (props.what === 'backup') sort.value = { col: 'modified', desc: true }
  void load()
})

/** PostgreSQL 点的是库、里面有好几个模式时多一列「模式」 */
const showSchema = computed(() => isPg.value && !props.schema && new Set(all.value.map((x) => x.t?.schema || x.r?.schema || 'public')).size > 1)

interface Col { k: string; t: string; w: number; num?: boolean; tip?: string }
const cols = computed<Col[]>(() => {
  const schemaCol = showSchema.value ? [{ k: 'schema', t: '模式', w: 110 }] : []
  switch (props.what) {
    case 'func':
      return [
        { k: 'name', t: '名', w: 200 },
        ...schemaCol,
        { k: 'type', t: '类型', w: 80 },
        { k: 'args', t: '参数', w: 220 },
        { k: 'returns', t: '返回', w: 120 },
        ...(isPg.value ? [{ k: 'language', t: '语言', w: 80 }] : [{ k: 'updated', t: '修改日期', w: 150 }]),
        { k: 'comment', t: '注释', w: 160 },
      ]
    case 'query':
      return [
        { k: 'name', t: '名', w: 200 },
        { k: 'modified', t: '修改日期', w: 150 },
        { k: 'size', t: '大小', w: 80, num: true },
        { k: 'preview', t: '内容', w: 360 },
      ]
    case 'backup':
      return [
        { k: 'name', t: '名', w: 220 },
        { k: 'modified', t: '备份时间', w: 160 },
        { k: 'size', t: '大小', w: 100, num: true },
      ]
  }
  if (isPg.value) {
    return [
      { k: 'name', t: '名', w: 220 },
      ...schemaCol,
      { k: 'rows', t: '行', w: 90, num: true, tip: '统计信息里的估计值，准确的行数打开表后点「数一下共多少行」' },
      { k: 'dataLen', t: '数据长度', w: 96, num: true },
      { k: 'indexLen', t: '索引长度', w: 96, num: true },
      { k: 'owner', t: '所有者', w: 110 },
      { k: 'comment', t: '注释', w: 200 },
    ]
  }
  return [
    { k: 'name', t: '名', w: 220 },
    { k: 'autoInc', t: '自动递增值', w: 96, num: true },
    { k: 'updated', t: '修改日期', w: 150, tip: '最后一次改数据的时间（MySQL 重启后可能是空的）' },
    { k: 'dataLen', t: '数据长度', w: 90, num: true },
    { k: 'engine', t: '引擎', w: 80 },
    { k: 'rows', t: '行', w: 80, num: true, tip: 'InnoDB 的行数是估计值，准确的行数打开表后点「数一下共多少行」' },
    { k: 'comment', t: '注释', w: 200 },
  ]
})

/** 一行里某一列的原始值（排序用） */
function raw(it: DbObjItem, k: string): string | number | undefined {
  const src = (it.t ?? it.r ?? it.f) as unknown as Record<string, string | number | undefined>
  if (k === 'name') return it.name
  if (k === 'schema') return it.t?.schema || it.r?.schema || 'public'
  return src?.[k]
}

const list = computed(() => {
  const f = filter.value.trim().toLowerCase()
  const rows = f
    ? all.value.filter((x) => x.name.toLowerCase().includes(f) || String(x.t?.comment ?? x.r?.comment ?? x.f?.preview ?? '').toLowerCase().includes(f))
    : [...all.value]
  const { col, desc } = sort.value
  const empty = (v: unknown) => v === undefined || v === null || v === ''
  rows.sort((a, b) => {
    const x = raw(a, col)
    const y = raw(b, col)
    // 空的（视图没有行数、自增值……）不管升序降序都排在后面
    if (empty(x) || empty(y)) return empty(x) && empty(y) ? a.name.localeCompare(b.name) : empty(x) ? 1 : -1
    let c = typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))
    if (desc) c = -c
    return c || a.name.localeCompare(b.name)
  })
  return rows
})
function sortBy(k: string) {
  sort.value = sort.value.col === k ? { col: k, desc: !sort.value.desc } : { col: k, desc: false }
}

const current = computed(() => all.value.find((x) => x.key === selected.value) ?? null)

const countText = computed(() => {
  const n = all.value.length
  switch (props.what) {
    case 'func': {
      const p = all.value.filter((x) => x.r?.type === 'procedure').length
      return `${n - p} 个函数${p ? `，${p} 个存储过程` : ''}`
    }
    case 'query': return `${n} 个保存的查询`
    case 'backup': return `${n} 个备份，共 ${humanSize(all.value.reduce((s, x) => s + (x.f?.size ?? 0), 0))}`
    default: {
      const v = all.value.filter((x) => x.kind === 'view').length
      return props.what === 'view' ? `${v} 个视图` : `${n - v} 张表${v ? `，${v} 个视图` : ''}`
    }
  }
})
const emptyText = computed(() => {
  if (filter.value) return '没有对得上的'
  return {
    all: '这个库里还没有表', table: '还没有表', view: '还没有视图', func: '还没有函数和存储过程',
    query: '还没有保存的查询：在查询窗口里点「保存」（Ctrl+S）就会出现在这里',
    backup: '还没有备份：点上面的「新建备份」，把整个库（结构和数据）存到这台电脑上',
  }[props.what]
})

function cell(it: DbObjItem, k: string): string {
  const v = raw(it, k)
  switch (k) {
    case 'rows':
    case 'autoInc':
      return v === undefined || v === null ? '' : Number(v).toLocaleString()
    case 'dataLen':
    case 'indexLen':
    case 'size':
      return humanSize(v as number | undefined)
    case 'type':
      return it.r ? (it.r.type === 'procedure' ? '存储过程' : '函数') : ''
    default:
      return String(v ?? '')
  }
}
function cellTip(it: DbObjItem, k: string): string {
  const t = it.t
  if (t && (k === 'dataLen' || k === 'indexLen')) {
    return [t.dataLen !== undefined ? `数据 ${humanSize(t.dataLen)}` : '', t.indexLen !== undefined ? `索引 ${humanSize(t.indexLen)}` : ''].filter(Boolean).join('，')
  }
  if (t && k === 'name') return [t.type === 'view' ? '视图' : '表', t.created ? `建于 ${t.created}` : '', t.collation ? `排序规则 ${t.collation}` : ''].filter(Boolean).join(' · ')
  if (it.r && k === 'name') return [it.r.created ? `建于 ${it.r.created}` : '', it.r.updated ? `改于 ${it.r.updated}` : ''].filter(Boolean).join(' · ')
  if (it.f && k === 'name') return it.f.path
  if (k === 'comment' || k === 'args' || k === 'preview' || k === 'returns') return cell(it, k)
  return ''
}
const icons: Record<DbObjItem['kind'], Component> = { table: Grid, view: View, func: Grid, query: Document, backup: Box }

function pick(it: DbObjItem) {
  selected.value = it.key
}
function onContext(e: MouseEvent, it: DbObjItem) {
  pick(it)
  emit('context', e, it)
}
function act(k: string) {
  if (current.value) emit('action', k, current.value)
}

// ↑↓ 换选中的行，回车打开
async function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' && current.value) {
    e.preventDefault()
    emit('open', current.value)
    return
  }
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return
  e.preventDefault()
  const rows = list.value
  if (!rows.length) return
  const i = rows.findIndex((x) => x.key === selected.value)
  const j = i < 0 ? 0 : Math.max(0, Math.min(rows.length - 1, i + (e.key === 'ArrowDown' ? 1 : -1)))
  selected.value = rows[j].key
  await nextTick()
  body.value?.querySelector('tr.on')?.scrollIntoView({ block: 'nearest' })
}

defineExpose({ reload: load })
</script>

<template>
  <div class="objs">
    <div class="bar">
      <template v-if="isTables">
        <el-button size="small" :icon="FolderOpened" :disabled="!current" @click="current && emit('open', current)">{{ what === 'view' ? '打开视图' : '打开表' }}</el-button>
        <template v-if="what !== 'view'">
          <el-button size="small" :icon="EditPen" :disabled="!current || current.kind !== 'table' || ro" @click="act('design')">设计表</el-button>
          <el-button size="small" :icon="Plus" :disabled="ro" @click="emit('action', 'new-table', null)">新建表</el-button>
        </template>
        <el-button v-else size="small" :icon="Plus" :disabled="ro" @click="emit('action', 'new-view', null)">新建视图</el-button>
        <el-button size="small" :icon="Delete" :disabled="!current || ro" @click="act(current?.kind === 'view' ? 'drop-view' : 'drop-table')">{{ current?.kind === 'view' ? '删除视图' : '删除表' }}</el-button>
        <el-dropdown trigger="click" :disabled="ro" @command="(k: string) => (k === 'import-csv' ? act(k) : emit('action', k, null))">
          <el-button size="small" :icon="Upload" :disabled="ro">导入</el-button>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item command="import-csv" :disabled="!current || current.kind !== 'table'">导入 CSV 到选中的表…</el-dropdown-item>
              <el-dropdown-item command="import-sql">执行 SQL 文件…</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
        <el-dropdown trigger="click" @command="(k: string) => (k.startsWith('db-') ? emit('action', k.slice(3), null) : act(k))">
          <el-button size="small" :icon="Download">导出</el-button>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item command="export-struct" :disabled="!current">选中的：{{ current?.kind === 'view' ? '视图定义' : exportLabels.struct }}</el-dropdown-item>
              <el-dropdown-item command="export-dump" :disabled="!current || current.kind !== 'table'">选中的：{{ exportLabels.dump }}</el-dropdown-item>
              <el-dropdown-item command="export-csv" :disabled="!current">选中的：{{ exportLabels.csv }}</el-dropdown-item>
              <el-dropdown-item command="export-insert" :disabled="!current">选中的：{{ exportLabels.insert }}</el-dropdown-item>
              <el-dropdown-item command="db-export-struct" divided>{{ schema ? '整个模式' : '整个库' }}：全部表的结构</el-dropdown-item>
              <el-dropdown-item command="db-export-dump">{{ schema ? '整个模式' : '整个库' }}：全部表的结构和数据</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </template>
      <template v-else-if="what === 'func'">
        <el-button size="small" :icon="FolderOpened" :disabled="!current" @click="current && emit('open', current)">打开</el-button>
        <el-button size="small" :icon="VideoPlay" :disabled="!current" @click="act('run')">运行</el-button>
        <el-button size="small" :icon="Plus" :disabled="ro" @click="emit('action', 'new-func', null)">新建函数</el-button>
        <el-button size="small" :icon="Plus" :disabled="ro" @click="emit('action', 'new-proc', null)">新建存储过程</el-button>
        <el-button size="small" :icon="Delete" :disabled="!current || ro" @click="act('drop-routine')">删除</el-button>
      </template>
      <template v-else-if="what === 'query'">
        <el-button size="small" :icon="FolderOpened" :disabled="!current" @click="current && emit('open', current)">打开</el-button>
        <el-button size="small" :icon="Plus" @click="emit('action', 'query', null)">新建查询</el-button>
        <el-button size="small" :icon="EditPen" :disabled="!current" @click="act('rename-query')">重命名</el-button>
        <el-button size="small" :icon="Delete" :disabled="!current" @click="act('delete-query')">删除</el-button>
        <el-button size="small" :icon="Folder" @click="emit('action', 'reveal', current)">打开所在文件夹</el-button>
      </template>
      <template v-else>
        <el-button size="small" type="primary" :icon="Plus" @click="emit('action', 'backup', null)">新建备份</el-button>
        <el-button size="small" :icon="RefreshLeft" :disabled="!current || ro" @click="act('restore')">还原</el-button>
        <el-button size="small" :icon="Delete" :disabled="!current" @click="act('delete-backup')">删除</el-button>
        <el-button size="small" :icon="Folder" @click="emit('action', 'reveal', current)">打开所在文件夹</el-button>
      </template>
      <el-button size="small" :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
      <span class="grow" />
      <el-input v-model="filter" size="small" :prefix-icon="Search" :placeholder="what === 'query' ? '按名称、内容筛选' : what === 'backup' ? '按名称筛选' : '按名称、注释筛选'" clearable class="filter" />
    </div>
    <el-alert v-if="error" type="error" :closable="false" :title="error" class="err" />
    <div ref="body" v-loading="loading && !all.length" class="box" tabindex="0" @keydown="onKey">
      <table>
        <colgroup>
          <col v-for="c in cols" :key="c.k" :style="{ width: c.w + 'px' }" />
          <col />
        </colgroup>
        <thead>
          <tr>
            <th v-for="c in cols" :key="c.k" :class="{ num: c.num }" :title="c.tip" @click="sortBy(c.k)">
              {{ c.t }}<el-icon v-if="sort.col === c.k" class="sort"><ArrowDown v-if="sort.desc" /><ArrowUp v-else /></el-icon>
            </th>
            <th />
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="it in list"
            :key="it.key"
            :class="[it.kind, { on: it.key === selected }]"
            @mousedown="pick(it)"
            @dblclick="emit('open', it)"
            @contextmenu.prevent="onContext($event, it)"
          >
            <td v-for="c in cols" :key="c.k" :class="{ num: c.num, muted: c.k === 'updated' || c.k === 'owner' || c.k === 'modified' || c.k === 'preview', mono: c.k === 'preview' || c.k === 'args' }" :title="cellTip(it, c.k)">
              <template v-if="c.k === 'name'">
                <span v-if="it.kind === 'func'" class="ti fx">ƒ</span>
                <el-icon v-else class="ti"><component :is="icons[it.kind]" /></el-icon>{{ it.name }}
              </template>
              <template v-else>{{ cell(it, c.k) }}</template>
            </td>
            <td />
          </tr>
        </tbody>
      </table>
      <div v-if="!loading && !error && !list.length" class="empty">{{ emptyText }}</div>
    </div>
    <div class="foot">
      <span>{{ conn.name }} / {{ db }}{{ schema ? ' / ' + schema : '' }}</span>
      <span class="muted">{{ countText }}{{ filter ? `，筛出 ${list.length} 个` : '' }}</span>
      <span class="grow" />
      <span v-if="current" class="muted">选中 {{ current.name }}{{ current.t?.comment || current.r?.comment ? '（' + (current.t?.comment || current.r?.comment) + '）' : '' }}{{ what === 'backup' ? '' : ' · 双击打开' }}</span>
    </div>
  </div>
</template>

<style scoped>
.objs {
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.bar {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 10px;
  border-bottom: 1px solid var(--el-border-color-lighter);
  flex-wrap: wrap;
}
.bar .el-button + .el-dropdown,
.bar .el-dropdown + .el-dropdown,
.bar .el-dropdown + .el-button,
.bar .el-button + .el-button {
  margin-left: 0;
}
.grow {
  flex: 1;
}
.filter {
  width: 200px;
}
.err {
  margin: 6px 10px 0;
  width: auto;
}
.box {
  flex: 1;
  min-height: 0;
  overflow: auto;
  outline: none;
  position: relative;
}
table {
  border-collapse: separate;
  border-spacing: 0;
  table-layout: fixed;
  width: 100%;
  font-size: 13px;
}
th {
  position: sticky;
  top: 0;
  z-index: 1;
  background: var(--el-fill-color-light);
  font-weight: normal;
  color: var(--el-text-color-secondary);
  text-align: left;
  padding: 5px 8px;
  border-bottom: 1px solid var(--el-border-color-lighter);
  white-space: nowrap;
  cursor: pointer;
  user-select: none;
}
th:hover {
  color: var(--el-text-color-primary);
}
th[title]:not([title='']) {
  text-decoration: underline dotted var(--el-text-color-placeholder);
  text-underline-offset: 3px;
}
.sort {
  vertical-align: -2px;
  margin-left: 2px;
  font-size: 12px;
}
td {
  padding: 0 8px;
  height: 26px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  border-bottom: 1px solid var(--el-border-color-extra-light);
  cursor: default;
}
tbody tr:hover td {
  background: var(--el-fill-color-lighter);
}
tbody tr.on td {
  background: var(--el-color-primary-light-9);
}
.num {
  text-align: right;
  font-variant-numeric: tabular-nums;
}
.muted {
  color: var(--el-text-color-secondary);
}
td.muted {
  font-variant-numeric: tabular-nums;
}
td.mono {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12px;
}
.ti {
  vertical-align: -2px;
  margin-right: 5px;
  color: var(--el-color-primary);
}
.ti.fx {
  display: inline-block;
  width: 1em;
  text-align: center;
  font-style: italic;
  font-weight: 600;
  font-family: Georgia, 'Times New Roman', serif;
  vertical-align: 0;
}
tr.view .ti {
  color: var(--el-color-warning);
}
tr.backup .ti {
  color: var(--el-color-success);
}
.empty {
  padding: 40px;
  text-align: center;
  color: var(--el-text-color-placeholder);
}
.foot {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 5px 10px;
  border-top: 1px solid var(--el-border-color-lighter);
  font-size: 12.5px;
  min-width: 0;
}
.foot > span {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.foot .muted {
  font-size: 12px;
}
</style>
