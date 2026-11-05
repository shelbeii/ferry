<script setup lang="ts">
// 「数据库」抽屉：左边是连接 → 库 →（模式）→ 表 的树，右边是标签页（表数据、查询、设计表）。
import { computed, h, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElCheckbox, ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Refresh, Close, Search, Coin, Document, Grid, View, Folder, Connection, Lock, Key, Monitor } from '@element-plus/icons-vue'
import type Node from 'element-plus/es/components/tree/src/model/node'
import DbTableView from './DbTableView.vue'
import DbQuery from './DbQuery.vue'
import DbDesign from './DbDesign.vue'
import DbRedis from './DbRedis.vue'
import DbRedisConsole from './DbRedisConsole.vue'
import DbConnDialog from './DbConnDialog.vue'
import DbSqlConfirm from './DbSqlConfirm.vue'
import DbCsvImport from './DbCsvImport.vue'
import {
  dbOpen, dbConns, closeOnOutside, dbOpened, dbRequest, dbPasswordRequest, loadDbConns, dbCall, saveDbConn, removeDbConn, closeDbConn, confirmSql, passwordEnv,
  humanSize, kindLabel, exportTo, type ExportKind, type DbConn, type DbDatabase, type DbTable, type DbResult,
} from '../../db'
import { sessionEnv } from '../../ferry'
import { pickFile } from '../../platform'
import { startTask } from '../../state'

const props = defineProps<{ side: number }>()

// ---------------------------------------------------------------- 树

interface TreeData {
  id: string
  label: string
  type: 'conn' | 'db' | 'schema' | 'table' | 'view' | 'rdb' | 'none'
  conn: DbConn
  db?: string
  schema?: string
  name?: string
  system?: boolean
  hint?: string
  leaf?: boolean
}
const tree = ref()
const filter = ref('')
const treeKey = ref(0)
const roots = computed<TreeData[]>(() => dbConns.value.map((c) => ({ id: 'c:' + c.id, label: c.name, type: 'conn', conn: c })))

async function loadNode(node: Node, resolve: (d: TreeData[]) => void, reject: () => void) {
  if (node.level === 0) return resolve(roots.value)
  const d = node.data as TreeData
  try {
    if (d.type === 'conn' && d.conn.kind === 'redis') {
      // Redis：db0、db1……（没有键的显示成灰色）
      const r = await dbCall<{ databases: DbDatabase[] }>(d.conn, 'databases', undefined)
      return resolve(r.databases.map((x) => ({
        id: `r:${d.conn.id}:${x.name}`, label: `db${x.name}`, type: 'rdb', conn: d.conn, db: x.name, leaf: true, system: !x.keys,
        hint: x.keys ? `${x.keys.toLocaleString()} 个键` : '空的',
      })))
    } else if (d.type === 'conn') {
      const r = await dbCall<{ databases: DbDatabase[] }>(d.conn, 'databases', undefined)
      const dbs = [...r.databases].sort((a, b) => Number(!!a.system) - Number(!!b.system) || a.name.localeCompare(b.name))
      resolve(dbs.length ? dbs.map((x) => ({ id: `d:${d.conn.id}:${x.name}`, label: x.name, type: 'db', conn: d.conn, db: x.name, system: x.system })) : [{ id: `n:${d.id}`, label: '（没有库）', type: 'none', conn: d.conn, leaf: true }])
    } else if (d.type === 'db' || d.type === 'schema') {
      const r = await dbCall<{ tables: DbTable[] }>(d.conn, 'tables', d.db)
      let ts = r.tables
      if (d.conn.kind === 'postgres' && d.type === 'db') {
        const schemas = [...new Set(ts.map((t) => t.schema ?? 'public'))]
        if (schemas.length > 1 || (schemas.length === 1 && schemas[0] !== 'public')) {
          return resolve(schemas.map((s) => ({ id: `s:${d.conn.id}:${d.db}:${s}`, label: s, type: 'schema', conn: d.conn, db: d.db, schema: s })))
        }
      }
      if (d.type === 'schema') ts = ts.filter((t) => (t.schema ?? 'public') === d.schema)
      resolve(ts.length ? ts.map((t) => ({
        id: `t:${d.conn.id}:${d.db}:${t.schema ?? ''}:${t.name}`, label: t.name, type: t.type, conn: d.conn, db: d.db, schema: t.schema || undefined, name: t.name, leaf: true,
        hint: [t.rows !== undefined && t.rows !== null ? `约 ${t.rows.toLocaleString()} 行` : '', humanSize(t.size), t.comment ?? ''].filter(Boolean).join(' · '),
      })) : [{ id: `n:${d.id}`, label: '（没有表）', type: 'none', conn: d.conn, leaf: true }])
    } else resolve([])
  } catch (e) {
    ElMessage.error({ message: (e as Error).message, duration: 6000 })
    reject()
  }
}

function refreshNode(id: string) {
  const n = tree.value?.getNode(id) as Node | undefined
  if (!n) return
  n.loaded = false
  n.childNodes = []
  n.expand()
}
async function refreshAll() {
  await loadDbConns()
  treeKey.value++
}
watch(filter, (v) => tree.value?.filter(v))
function filterNode(v: string, d: TreeData) {
  if (!v) return true
  return d.label.toLowerCase().includes(v.toLowerCase())
}

function onNodeClick(d: TreeData, node: Node) {
  if (d.type === 'table' || d.type === 'view') openTable(d)
  else if (d.type === 'rdb') openRedis(d.conn, d.db!)
  else if (d.type !== 'none' && !node.expanded && !node.isLeaf) node.expand()
}

// ---------------------------------------------------------------- 标签页

interface Tab {
  key: string
  kind: 'table' | 'query' | 'design' | 'redis' | 'rcmd'
  title: string
  conn: DbConn
  db: string
  schema?: string
  table?: string
  view?: boolean
  sql?: string
  sid?: string
}
const tabs = ref<Tab[]>([])
const active = ref('')
const refs = reactive<Record<string, { hasChanges?: () => boolean; reload?: () => void; newKey?: () => void } | null>>({})
let qn = 0

function openTable(d: { conn: DbConn; db?: string; schema?: string; name?: string; type: string }) {
  const key = `t:${d.conn.id}:${d.db}:${d.schema ?? ''}:${d.name}`
  if (!tabs.value.some((t) => t.key === key)) {
    tabs.value.push({ key, kind: 'table', title: d.name!, conn: d.conn, db: d.db ?? '', schema: d.schema, table: d.name, view: d.type === 'view' })
  }
  active.value = key
}
function openQuery(conn: DbConn, db = '', sql = '') {
  const sid = `q${Date.now().toString(36)}${++qn}`
  const key = 'q:' + sid
  tabs.value.push({ key, kind: 'query', title: `查询 ${qn}`, conn, db: db || conn.database || '', sql, sid })
  active.value = key
  saveTabs()
}
function openDesign(conn: DbConn, db: string, schema?: string, table?: string) {
  const key = `g:${conn.id}:${db}:${schema ?? ''}:${table ?? '新表' + Date.now()}`
  if (!tabs.value.some((t) => t.key === key)) tabs.value.push({ key, kind: 'design', title: table ? `设计 ${table}` : '新建表', conn, db, schema, table })
  active.value = key
}
function openRedis(conn: DbConn, db: string) {
  const key = `r:${conn.id}:${db}`
  if (!tabs.value.some((t) => t.key === key)) tabs.value.push({ key, kind: 'redis', title: `db${db}`, conn, db })
  active.value = key
  return key
}
function openRedisConsole(conn: DbConn, db = '') {
  const sid = `rc${Date.now().toString(36)}${++qn}`
  const key = 'rc:' + sid
  tabs.value.push({ key, kind: 'rcmd', title: '命令行', conn, db: db || conn.database?.replace(/^db/i, '') || '0', sid })
  active.value = key
}
/** Redis 的库里键变了：树上的键数跟着刷新 */
function redisChanged(t: Tab) {
  refreshNode(`c:${t.conn.id}`)
}
async function closeTab(key: string) {
  await closeTabs([key])
}
/** 关闭几个标签；其中有没保存的改动时先问一次 */
async function closeTabs(keys: string[]) {
  const list = tabs.value.filter((t) => keys.includes(t.key))
  if (!list.length) return
  const dirty = list.filter((t) => refs[t.key]?.hasChanges?.())
  if (dirty.length) {
    try {
      await ElMessageBox.confirm(`${dirty.map((t) => `「${t.title}」`).join('、')}里有没保存的改动，关掉就没了。`, '关闭标签', { type: 'warning', confirmButtonText: '关闭', cancelButtonText: '不关' })
    } catch {
      return
    }
  }
  const before = tabs.value.findIndex((t) => t.key === active.value)
  tabs.value = tabs.value.filter((t) => !keys.includes(t.key))
  for (const k of keys) delete refs[k]
  if (!tabs.value.some((t) => t.key === active.value)) active.value = tabs.value[Math.min(Math.max(before, 0), tabs.value.length - 1)]?.key ?? ''
  saveTabs()
}

// 标签上的右键菜单
const tabMenu = ref<{ x: number; y: number; key: string } | null>(null)
const tabMenuEl = ref<HTMLElement>()
let offTabMenu: (() => void) | null = null
function onTabsContext(e: MouseEvent) {
  const item = (e.target as HTMLElement).closest('.el-tabs__item')
  const key = item?.id?.replace(/^tab-/, '')
  if (!key || !tabs.value.some((t) => t.key === key)) return
  e.preventDefault()
  offTabMenu?.()
  tabMenu.value = { x: e.clientX, y: e.clientY, key }
  offTabMenu = closeOnOutside((t) => !!tabMenuEl.value?.contains(t), () => { tabMenu.value = null })
}
const tabMenuItems = computed(() => {
  const m = tabMenu.value
  if (!m) return []
  const i = tabs.value.findIndex((t) => t.key === m.key)
  const n = tabs.value.length
  return [
    { k: 'close', t: '关闭' },
    { k: 'others', t: '关闭其他标签', off: n <= 1 },
    { k: 'left', t: '关闭左侧标签', off: i <= 0 },
    { k: 'right', t: '关闭右侧标签', off: i >= n - 1 },
    { k: 'all', t: '关闭全部标签', sep: true },
  ]
})
function tabMenuDo(k: string) {
  const m = tabMenu.value
  tabMenu.value = null
  offTabMenu?.()
  if (!m) return
  const i = tabs.value.findIndex((t) => t.key === m.key)
  const keys = tabs.value.map((t) => t.key)
  const pick = { close: [m.key], others: keys.filter((k) => k !== m.key), left: keys.slice(0, i), right: keys.slice(i + 1), all: keys }[k] ?? []
  void closeTabs(pick)
}
function tabLabel(t: Tab) {
  if (t.kind === 'redis' || t.kind === 'rcmd') return `${t.title} · ${t.conn.name}`
  return t.kind === 'query' ? `${t.title}${t.db ? ' · ' + t.db : ''}` : t.title
}
function tabTip(t: Tab) {
  return `${t.conn.name}${t.db ? ' / ' + t.db : ''}${t.schema ? ' / ' + t.schema : ''}${t.table ? ' / ' + t.table : ''}`
}

function onDesignSaved(t: Tab, name: string, schema?: string) {
  // 刷新树里这个库，表的标签重新读
  refreshNode(t.schema ? `s:${t.conn.id}:${t.db}:${t.schema}` : `d:${t.conn.id}:${t.db}`)
  for (const x of tabs.value) {
    if (x.kind === 'table' && x.conn.id === t.conn.id && x.db === t.db && (x.table === t.table || x.table === name)) refs[x.key]?.reload?.()
  }
  if (!t.table || t.table !== name) {
    void closeTab(t.key).then(() => openTable({ conn: t.conn, db: t.db, schema: schema ?? t.schema, name, type: 'table' }))
  }
}

// 查询标签的内容存在这台电脑上，下次打开还在
const TABS_KEY = 'ferry.db.queries'
let saveTimer = 0
function saveTabs() {
  clearTimeout(saveTimer)
  saveTimer = window.setTimeout(() => {
    const qs = tabs.value.filter((t) => t.kind === 'query').map((t) => ({ conn: t.conn.id, db: t.db, sql: t.sql ?? '', title: t.title }))
    try { localStorage.setItem(TABS_KEY, JSON.stringify(qs)) } catch { /* 存不下就算了 */ }
  }, 500)
}
function restoreTabs() {
  try {
    const qs = JSON.parse(localStorage.getItem(TABS_KEY) || '[]') as { conn: string; db: string; sql: string; title: string }[]
    for (const q of qs) {
      const conn = dbConns.value.find((c) => c.id === q.conn)
      if (!conn) continue
      const sid = `q${Date.now().toString(36)}${++qn}`
      tabs.value.push({ key: 'q:' + sid, kind: 'query', title: q.title || `查询 ${qn}`, conn, db: q.db, sql: q.sql, sid })
    }
    if (tabs.value.length && !active.value) active.value = tabs.value[0].key
  } catch { /* 没有存过 */ }
}

// ---------------------------------------------------------------- 右键菜单

// 右键菜单：点菜单外面（含右键别处）、按 Esc、滚动时关闭；靠近窗口边时挪进来
const menuEl = ref<HTMLElement>()
let offMenu: (() => void) | null = null
async function fitMenu() {
  await nextTick()
  const m = menu.value
  const el = menuEl.value
  if (!m || !el) return
  const r = el.getBoundingClientRect()
  m.x = Math.max(4, Math.min(m.x, window.innerWidth - r.width - 4))
  m.y = Math.max(4, Math.min(m.y, window.innerHeight - r.height - 4))
}
onBeforeUnmount(() => { offMenu?.(); offTabMenu?.() })

const menu = ref<{ x: number; y: number; d: TreeData } | null>(null)
function onContext(e: MouseEvent, d: TreeData) {
  if (d.type === 'none') return
  e.preventDefault()
  menu.value = { x: e.clientX, y: e.clientY, d }
  offMenu?.()
  offMenu = closeOnOutside((t) => !!menuEl.value?.contains(t), () => { menu.value = null })
  void fitMenu()
}
const menuItems = computed(() => {
  const d = menu.value?.d
  if (!d) return []
  const ro = d.conn.readOnly
  if (d.conn.kind === 'redis') {
    if (d.type === 'conn') {
      return [
        { k: 'rcmd', t: '命令行' }, { k: 'refresh', t: '刷新' },
        { k: 'edit', t: '修改连接…', sep: true }, { k: 'copy', t: '复制一份连接…' }, { k: 'close', t: '断开', off: !dbOpened[d.conn.id] }, { k: 'remove', t: '删除连接', danger: true, sep: true },
      ]
    }
    return [
      { k: 'open', t: '打开' }, { k: 'rcmd', t: '命令行' }, { k: 'new-key', t: '新建键…', off: ro },
      { k: 'flushdb', t: '清空这个库…', danger: true, off: ro, sep: true },
    ]
  }
  switch (d.type) {
    case 'conn':
      return [
        { k: 'query', t: '新建查询' }, { k: 'refresh', t: '刷新' }, { k: 'create-db', t: d.conn.kind === 'mysql' ? '新建库…' : '新建库…', off: ro },
        { k: 'edit', t: '修改连接…', sep: true }, { k: 'copy', t: '复制一份连接…' }, { k: 'close', t: '断开', off: !dbOpened[d.conn.id] }, { k: 'remove', t: '删除连接', danger: true, sep: true },
      ]
    case 'db':
      return [
        { k: 'query', t: '新建查询' }, { k: 'new-table', t: '新建表…', off: ro }, ...(d.conn.kind === 'postgres' ? [{ k: 'create-schema', t: '新建模式…', off: ro }] : []), { k: 'refresh', t: '刷新' },
        { k: 'export-struct', t: '导出全部表的结构', sep: true }, { k: 'export-dump', t: '导出全部表的结构和数据' }, { k: 'import-sql', t: '执行 SQL 文件…', off: ro },
        { k: 'copy-name', t: '复制库名', sep: true }, { k: 'drop-db', t: '删除库…', danger: true, off: ro, sep: true },
      ]
    case 'schema':
      return [
        { k: 'query', t: '新建查询' }, { k: 'new-table', t: '新建表…', off: ro }, { k: 'refresh', t: '刷新' },
        { k: 'export-struct', t: '导出全部表的结构', sep: true }, { k: 'export-dump', t: '导出全部表的结构和数据' }, { k: 'import-sql', t: '执行 SQL 文件…', off: ro },
        { k: 'copy-name', t: '复制模式名', sep: true },
      ]
    default:
      return [
        { k: 'open', t: '打开' }, ...(d.type === 'table' ? [{ k: 'design', t: '设计表', off: ro }] : []), { k: 'select', t: '新建查询（SELECT）' },
        { k: 'rename', t: '重命名…', off: ro, sep: true }, { k: 'copy-name', t: '复制名称' },
        ...(d.type === 'table' ? [{ k: 'export-struct', t: '导出结构', sep: true }, { k: 'export-dump', t: '导出结构和数据' }] : [{ k: 'export-struct', t: '导出视图定义', sep: true }]),
        { k: 'export-csv', t: '导出数据为 CSV' }, { k: 'export-insert', t: '导出数据为 SQL（INSERT）' },
        ...(d.type === 'table' ? [{ k: 'import-csv', t: '导入 CSV…', off: ro }] : []),
        ...(d.type === 'table' ? [{ k: 'truncate', t: '清空表…', danger: true, off: ro, sep: true }, { k: 'drop-table', t: '删除表…', danger: true, off: ro }] : [{ k: 'drop-view', t: '删除视图…', danger: true, off: ro, sep: true }]),
      ]
  }
})

function qualified(d: TreeData) {
  const q = (s: string) => (d.conn.kind === 'mysql' ? '`' + s.replace(/`/g, '``') + '`' : '"' + s.replace(/"/g, '""') + '"')
  if (d.conn.kind === 'mysql') return q(d.name!)
  return d.schema && d.schema !== 'public' ? `${q(d.schema)}.${q(d.name!)}` : q(d.name!)
}

async function runOp(d: TreeData, op: string, args: string[], title: string, refresh: string, db = d.db): Promise<boolean> {
  try {
    const plan = await dbCall<{ sql: string[] }>(d.conn, 'op', db, { op, args, schema: d.schema })
    if (!(await confirmSql(d.conn, title, plan.sql))) return false
    const r = await dbCall<{ results: DbResult[] }>(d.conn, 'exec', db, { stmts: plan.sql })
    const bad = r.results.find((x) => x.error)
    if (bad) {
      void ElMessageBox.alert(bad.error, '没有成功', { type: 'error' })
      return false
    }
    ElMessage.success('完成')
    refreshNode(refresh)
    return true
  } catch (e) {
    void ElMessageBox.alert((e as Error).message, '没有成功', { type: 'error' })
    return false
  }
}

async function askName(title: string, label: string, value = ''): Promise<string | null> {
  try {
    const r = await ElMessageBox.prompt(label, title, { inputValue: value, confirmButtonText: '确定', cancelButtonText: '取消', inputValidator: (v) => (!!v?.trim() || '请填名称') })
    return (r as { value: string }).value.trim()
  } catch {
    return null
  }
}

// 执行 SQL 文件：边读边执行（文件可以很大），任务面板里看进度、可以停止
async function importSql(d: TreeData) {
  const file = await pickFile('选择要执行的 SQL 文件', 'SQL 文件', ['sql', 'txt'])
  if (!file) return
  const cont = ref(false)
  try {
    await ElMessageBox.confirm(() => h('div', { class: 'import-sql' }, [
      h('p', `在 ${d.conn.name} 的库 ${d.db} 里执行：`),
      h('p', { class: 'mono', style: 'word-break: break-all' }, file),
      h('p', { style: 'margin-top: 8px' }, '文件里的语句一条条直接执行，执行了的不能撤销（Ferry 导出的文件会先删掉同名的表再重建）。'),
      h(ElCheckbox, { modelValue: cont.value, 'onUpdate:modelValue': (v: unknown) => (cont.value = !!v) }, () => '有语句出错时接着执行后面的（默认停下）'),
    ]), '执行 SQL 文件', { confirmButtonText: '执行', cancelButtonText: '取消', type: 'warning' })
  } catch {
    return
  }
  startTask(`执行 SQL 文件：${d.conn.name} / ${d.db}`, ['db', 'import-sql', d.conn.id, d.db ?? ''], (t) => {
    if (t.status === 'ok') ElMessage.success({ message: '执行完了', duration: 3000 })
    refreshNode(d.id)
  }, { FERRY_DB_REQ: JSON.stringify({ file, continue: cont.value }) })
}

const csvTarget = ref<{ conn: DbConn; db: string; schema?: string; table: string } | null>(null)
let csvNode = '' // 导入完刷新这个节点（表的行数）：对话框先关了，所以另外记着

function exportNode(d: TreeData, kind: ExportKind) {
  void exportTo(d.conn, d.db ?? '', { schema: d.schema, table: d.name }, kind)
}

/** 重命名表或视图：打开着的标签跟着改 */
async function renameNode(d: TreeData) {
  const name = await askName(d.type === 'view' ? '重命名视图' : '重命名表', '新名称', d.name)
  if (!name || name === d.name) return
  const ok = await runOp(d, 'rename', [d.name!, name, d.type], `把 ${d.name} 改名为 ${name}`, parentId(d))
  if (!ok) return
  for (const t of tabs.value) {
    if (t.conn.id === d.conn.id && t.db === d.db && (t.schema ?? '') === (d.schema ?? '') && t.table === d.name && t.kind !== 'query') {
      t.table = name
      t.title = t.kind === 'design' ? `设计 ${name}` : name
    }
  }
}

async function menuDo(k: string) {
  const d = menu.value?.d
  menu.value = null
  offMenu?.()
  if (!d) return
  switch (k) {
    case 'query': return openQuery(d.conn, d.db)
    case 'rcmd': return openRedisConsole(d.conn, d.db)
    case 'new-key': {
      const key = openRedis(d.conn, d.db!)
      await nextTick()
      await nextTick()
      return refs[key]?.newKey?.()
    }
    case 'flushdb': {
      const name = await askName('清空库', `会删掉 db${d.db} 里的全部键，删了就找不回来。确认要清空，请输入「db${d.db}」：`)
      if (name === null) return
      if (name !== `db${d.db}`) return void ElMessage.warning('输入的不对，没有清空')
      try {
        await dbCall(d.conn, 'redis-edit', d.db, { edit: { op: 'flushdb' } })
        ElMessage.success('已清空')
        refreshNode(`c:${d.conn.id}`)
        refs[`r:${d.conn.id}:${d.db}`]?.reload?.()
      } catch (e) {
        void ElMessageBox.alert((e as Error).message, '没有成功', { type: 'error' })
      }
      return
    }
    case 'refresh': return d.type === 'conn' ? refreshNode(d.id) : refreshNode(d.id)
    case 'edit': return void (connDialog.value = { ...d.conn })
    case 'copy': return void (connDialog.value = { ...d.conn, id: '', name: d.conn.name + ' 副本' })
    case 'close':
      await closeDbConn(d.conn.id)
      tree.value?.getNode(d.id)?.collapse()
      return
    case 'remove':
      try {
        await ElMessageBox.confirm(`删除连接「${d.conn.name}」？只删 Ferry 里保存的连接和密码，不动数据库。`, '删除连接', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' })
      } catch { return }
      for (const t of tabs.value.filter((x) => x.conn.id === d.conn.id)) await closeTab(t.key)
      await removeDbConn(d.conn.id)
      treeKey.value++
      return
    case 'create-db': {
      const name = await askName('新建库', '库名')
      if (name) await runOp(d, 'create-db', [name], '新建库', d.id, '')
      return
    }
    case 'create-schema': {
      const name = await askName('新建模式', '模式名')
      if (name) await runOp(d, 'create-schema', [name], '新建模式', d.id)
      return
    }
    case 'drop-db': {
      const name = await askName('删除库', `删除库会删掉里面全部的表和数据，做了就改不回来。确认要删，请输入库名「${d.db}」：`)
      if (name === null) return
      if (name !== d.db) return void ElMessage.warning('库名不对，没有删除')
      return runOp(d, 'drop-db', [d.db!], '删除库', `c:${d.conn.id}`, d.conn.kind === 'postgres' ? 'postgres' : '')
    }
    case 'new-table': return openDesign(d.conn, d.db!, d.schema)
    case 'open': return d.type === 'rdb' ? openRedis(d.conn, d.db!) : openTable(d)
    case 'design': return openDesign(d.conn, d.db!, d.schema, d.name)
    case 'select': return openQuery(d.conn, d.db, `SELECT * FROM ${qualified(d)} LIMIT 100;\n`)
    case 'export-csv': return exportNode(d, 'csv')
    case 'export-insert': return exportNode(d, 'insert')
    case 'export-struct': return exportNode(d, 'struct')
    case 'export-dump': return exportNode(d, 'dump')
    case 'import-sql': return importSql(d)
    case 'import-csv':
      csvNode = parentId(d)
      csvTarget.value = { conn: d.conn, db: d.db ?? '', schema: d.schema, table: d.name! }
      return
    case 'rename': return renameNode(d)
    case 'copy-name':
      await navigator.clipboard.writeText(d.name ?? d.schema ?? d.db ?? '')
      return void ElMessage.success({ message: '已复制', duration: 1200 })
    case 'truncate': return runOp(d, 'truncate', [d.name!], `清空表 ${d.name}`, parentId(d))
    case 'drop-table': return runOp(d, 'drop-table', [d.name!], `删除表 ${d.name}`, parentId(d))
    case 'drop-view': return runOp(d, 'drop-view', [d.name!], `删除视图 ${d.name}`, parentId(d))
  }
}
function parentId(d: TreeData) {
  const n = tree.value?.getNode(d.id) as Node | undefined
  return (n?.parent?.data as TreeData | undefined)?.id ?? `d:${d.conn.id}:${d.db}`
}

// ---------------------------------------------------------------- 连接对话框、密码

const connDialog = ref<Partial<DbConn> | null>(null)
async function onSaved(c: DbConn) {
  connDialog.value = null
  for (const t of tabs.value) if (t.conn.id === c.id) t.conn = c
  treeKey.value++
}

const pw = reactive({ value: '', remember: false })
watch(dbPasswordRequest, (r) => {
  pw.value = ''
  pw.remember = !!r?.conn.savePassword
})
async function submitPassword(ok: boolean) {
  const r = dbPasswordRequest.value
  dbPasswordRequest.value = null
  if (!r) return
  if (!ok) return r.resolve(false)
  if (pw.remember) {
    try {
      await saveDbConn({ ...r.conn, savePassword: true }, pw.value)
    } catch (e) {
      ElMessage.error((e as Error).message)
    }
  }
  sessionEnv[passwordEnv(r.conn.id)] = pw.value
  r.resolve(true)
}

// 「软件」页点了「管理数据」
watch(dbRequest, async (r) => {
  if (!r) return
  dbRequest.value = null
  dbOpen.value = true
  if (!dbConns.value.length) await loadDbConns().catch(() => {})
  const same = dbConns.value.find((c) => c.via && r.via && c.via.config === r.via.config && c.via.target === r.via.target && c.port === r.port && (c.container ?? '') === (r.container ?? '') && c.kind === r.kind)
  if (same) {
    void nextTick(() => {
      tree.value?.getNode('c:' + same.id)?.expand()
      if (same.kind === 'redis') openRedis(same, same.database?.replace(/^db/i, '') || '0')
      else openQuery(same)
    })
  } else {
    connDialog.value = r
  }
}, { immediate: true })

onMounted(async () => {
  try {
    await loadDbConns()
    treeKey.value++
    restoreTabs()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
})

function nodeIcon(d: TreeData) {
  return { conn: Coin, db: Folder, schema: Folder, table: Grid, view: View, rdb: Folder, none: Document }[d.type]
}
</script>

<template>
  <el-drawer v-model="dbOpen" :size="`calc(100vw - ${props.side}px)`" direction="rtl" :with-header="false" :close-on-press-escape="false" append-to-body class="db-drawer" modal-class="db-overlay">
    <div class="dbx">
      <aside class="side">
        <div class="side-head">
          <span class="title">数据库</span>
          <span class="grow" />
          <el-tooltip content="新建连接" placement="bottom" :show-after="500">
            <el-button link :icon="Plus" @click="connDialog = { kind: 'mysql', savePassword: true }" />
          </el-tooltip>
          <el-tooltip content="刷新" placement="bottom" :show-after="500">
            <el-button link :icon="Refresh" @click="refreshAll" />
          </el-tooltip>
        </div>
        <el-input v-model="filter" size="small" :prefix-icon="Search" placeholder="筛选连接、库、表（已展开的）" clearable class="filter" />
        <div class="tree-box">
          <el-empty v-if="!dbConns.length" description="还没有数据库连接" :image-size="60">
            <el-button type="primary" size="small" :icon="Plus" @click="connDialog = { kind: 'mysql', savePassword: true }">新建连接</el-button>
            <div class="empty-tip">也可以在服务器的「软件」页，从 MySQL、PostgreSQL、Redis 的「更多」里点「管理数据」</div>
          </el-empty>
          <el-tree
            v-else
            :key="treeKey"
            ref="tree"
            lazy
            :load="loadNode"
            node-key="id"
            :props="{ label: 'label', isLeaf: 'leaf' }"
            :filter-node-method="filterNode"
            :expand-on-click-node="false"
            highlight-current
            @node-click="onNodeClick"
            @node-contextmenu="(e: MouseEvent, d: TreeData) => onContext(e, d)"
          >
            <template #default="{ data }">
              <span class="node" :class="[data.type, { system: data.system }]" :title="data.hint || (data.type === 'conn' ? `${kindLabel[data.conn.kind as 'mysql']} · ${data.conn.user ? data.conn.user + '@' : ''}${data.conn.container ? '容器 ' + data.conn.container : data.conn.host + ':' + data.conn.port}${data.conn.via ? ' · 经 ' + (data.conn.via.title || data.conn.via.target) : ''}` : '')">
                <el-icon class="ni" :class="{ on: data.type === 'conn' && dbOpened[data.conn.id] }"><component :is="nodeIcon(data)" /></el-icon>
                <span class="nl">{{ data.label }}</span>
                <span v-if="data.type === 'rdb' && !data.system" class="keys">{{ data.hint.replace(' 个键', '') }}</span>
                <span v-if="data.type === 'conn' && data.conn.kind === 'redis'" class="kind-mark">Redis</span>
                <el-icon v-if="data.type === 'conn' && data.conn.readOnly" class="ro" title="只读"><Lock /></el-icon>
                <span v-if="data.type === 'conn' && data.conn.ai" class="ai-mark" :class="{ write: data.conn.aiWrite && !data.conn.readOnly }" :title="data.conn.aiWrite && !data.conn.readOnly ? '允许 AI 查询和修改（修改每次都要你确认）' : '允许 AI 查询（只读）'">{{ data.conn.aiWrite && !data.conn.readOnly ? 'AI 改' : 'AI' }}</span>
                <el-icon v-if="data.type === 'conn' && data.conn.via" class="via" :title="'经服务器 ' + (data.conn.via.title || data.conn.via.target)"><Connection /></el-icon>
              </span>
            </template>
          </el-tree>
        </div>
      </aside>
      <main class="main">
        <div v-if="!tabs.length" class="welcome">
          <h3>数据库</h3>
          <p>左边点一张表看数据；右键连接、库、表有更多操作（新建查询、新建表、导出……）。Redis 点一个库看里面的键，右键有命令行。</p>
          <p>经服务器连的数据库走 SSH 转发，不用在防火墙上开放数据库端口。</p>
          <div class="welcome-btns">
            <el-button type="primary" :icon="Plus" @click="connDialog = { kind: 'mysql', savePassword: true }">新建连接</el-button>
            <el-button v-if="dbConns.some((c) => c.kind !== 'redis')" @click="openQuery(dbConns.find((c) => c.kind !== 'redis')!)">打开查询窗口</el-button>
          </div>
        </div>
        <el-tabs v-else v-model="active" type="card" closable class="tabs" @tab-remove="closeTab" @contextmenu="onTabsContext">
          <el-tab-pane v-for="t in tabs" :key="t.key" :name="t.key" lazy>
            <template #label>
              <span :title="tabTip(t)" class="tl">
                <el-icon><Grid v-if="t.kind === 'table'" /><Document v-else-if="t.kind === 'query'" /><Key v-else-if="t.kind === 'redis'" /><Monitor v-else-if="t.kind === 'rcmd'" /><Folder v-else /></el-icon>
                {{ tabLabel(t) }}
              </span>
            </template>
            <DbTableView
              v-if="t.kind === 'table'"
              :ref="(el) => (refs[t.key] = el as never)"
              :conn="t.conn"
              :db="t.db"
              :schema="t.schema"
              :table="t.table!"
              :view="t.view"
              @design="openDesign(t.conn, t.db, t.schema, t.table)"
              @query="(sql: string) => openQuery(t.conn, t.db, sql + ';\n')"
            />
            <DbQuery
              v-else-if="t.kind === 'query'"
              :conn="t.conn"
              :db="t.db"
              :sql="t.sql ?? ''"
              :sid="t.sid!"
              @update:sql="(s: string) => { t.sql = s; saveTabs() }"
              @update:db="(d: string) => { t.db = d; saveTabs() }"
            />
            <DbRedis
              v-else-if="t.kind === 'redis'"
              :ref="(el) => (refs[t.key] = el as never)"
              :conn="t.conn"
              :db="t.db"
              @console="openRedisConsole(t.conn, t.db)"
              @changed="redisChanged(t)"
            />
            <DbRedisConsole v-else-if="t.kind === 'rcmd'" :conn="t.conn" :db="t.db" :sid="t.sid!" @update:db="(d: string) => (t.db = d)" />
            <DbDesign v-else :conn="t.conn" :db="t.db" :schema="t.schema" :table="t.table" @saved="(n: string, s?: string) => onDesignSaved(t, n, s)" />
          </el-tab-pane>
        </el-tabs>
        <el-button class="close" link :icon="Close" title="关闭（数据库连接留着，10 分钟没用自动断开）" @click="dbOpen = false" />
      </main>
    </div>

    <teleport to="body">
      <ul v-if="menu" ref="menuEl" class="db-grid-menu" :style="{ left: menu.x + 'px', top: menu.y + 'px' }" @click.stop>
        <li v-for="m in menuItems" :key="m.k" :class="{ sep: m.sep, danger: m.danger, off: m.off }" @click="!m.off && menuDo(m.k)">{{ m.t }}</li>
      </ul>
    </teleport>

    <teleport to="body">
      <ul v-if="tabMenu" ref="tabMenuEl" class="db-grid-menu" :style="{ left: tabMenu.x + 'px', top: tabMenu.y + 'px' }" @click.stop>
        <li v-for="m in tabMenuItems" :key="m.k" :class="{ sep: m.sep, off: m.off }" @click="!m.off && tabMenuDo(m.k)">{{ m.t }}</li>
      </ul>
    </teleport>

    <DbConnDialog :conn="connDialog" @close="connDialog = null" @saved="onSaved" />
    <DbSqlConfirm />
    <DbCsvImport :target="csvTarget" @close="csvTarget = null" @done="refreshNode(csvNode)" />
    <el-dialog :model-value="!!dbPasswordRequest" title="数据库密码" width="420px" append-to-body :close-on-click-modal="false" @close="submitPassword(false)">
      <template v-if="dbPasswordRequest">
        <p class="pw-tip">
          {{ dbPasswordRequest.rejected ? `${dbPasswordRequest.conn.name} 的密码不对，请重新填：` : `请填 ${dbPasswordRequest.conn.name}${dbPasswordRequest.conn.user ? `（${dbPasswordRequest.conn.user}）` : ''}的密码：` }}
        </p>
        <el-input v-model="pw.value" type="password" show-password autofocus @keyup.enter="submitPassword(true)" />
        <el-checkbox v-model="pw.remember" class="pw-remember">记住密码（存在系统凭据库里）</el-checkbox>
      </template>
      <template #footer>
        <el-button @click="submitPassword(false)">取消</el-button>
        <el-button type="primary" @click="submitPassword(true)">连接</el-button>
      </template>
    </el-dialog>
  </el-drawer>
</template>

<style>
/* 抽屉里是不透明的：不要毛玻璃（整屏模糊每次滚动、背后动画时都要重算，表格滚动会卡） */
html .db-drawer.el-drawer {
  -webkit-backdrop-filter: none !important;
  backdrop-filter: none !important;
  background: var(--el-bg-color) !important;
}
.db-drawer .el-drawer__body {
  padding: 0;
  overflow: hidden;
}
.db-grid-menu li.off {
  color: var(--el-text-color-disabled);
  cursor: not-allowed;
}
</style>

<style scoped>
.dbx {
  height: 100%;
  display: flex;
  background: var(--el-bg-color);
}
.side {
  width: 270px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  border-right: 1px solid var(--el-border-color-lighter);
  background: var(--el-fill-color-extra-light);
}
.side-head {
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 10px 10px 6px 14px;
}
.title {
  font-weight: 600;
}
.grow {
  flex: 1;
}
.filter {
  margin: 0 10px 6px;
  width: auto;
}
.tree-box {
  flex: 1;
  overflow: auto;
  padding-bottom: 20px;
}
.tree-box :deep(.el-tree) {
  background: transparent;
  font-size: 13px;
}
.empty-tip {
  margin-top: 10px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
  padding: 0 16px;
  line-height: 1.6;
}
.node {
  display: flex;
  align-items: center;
  gap: 5px;
  min-width: 0;
  flex: 1;
  padding-right: 8px;
}
.nl {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ni {
  color: var(--el-text-color-secondary);
  flex-shrink: 0;
}
.ni.on {
  color: var(--el-color-success);
}
.node.view .ni {
  color: var(--el-color-warning);
}
.node.system .nl {
  color: var(--el-text-color-secondary);
}
.node.none .nl {
  color: var(--el-text-color-placeholder);
}
.ai-mark.write {
  color: var(--el-color-warning);
  border-color: var(--el-color-warning-light-5);
}
.ai-mark {
  flex-shrink: 0;
  font-size: 10px;
  line-height: 14px;
  padding: 0 3px;
  border-radius: 3px;
  color: var(--el-color-primary);
  border: 1px solid var(--el-color-primary-light-5);
}
.keys {
  flex-shrink: 0;
  font-size: 11px;
  color: var(--el-text-color-placeholder);
}
.kind-mark {
  flex-shrink: 0;
  font-size: 10px;
  line-height: 14px;
  padding: 0 3px;
  border-radius: 3px;
  color: #d6453d;
  border: 1px solid #d6453d55;
}
.ro,
.via {
  color: var(--el-text-color-placeholder);
  font-size: 12px;
  flex-shrink: 0;
}
.main {
  flex: 1;
  min-width: 0;
  position: relative;
  display: flex;
  flex-direction: column;
}
.close {
  position: absolute;
  right: 10px;
  top: 8px;
  font-size: 16px;
  z-index: 5;
}
.welcome {
  margin: auto;
  max-width: 520px;
  padding: 30px;
  color: var(--el-text-color-regular);
  line-height: 1.8;
}
.welcome h3 {
  margin: 0 0 8px;
}
.welcome-btns {
  margin-top: 14px;
}
.tabs {
  height: 100%;
  display: flex;
  flex-direction: column;
}
.tabs :deep(.el-tabs__header) {
  margin: 0;
  padding: 4px 44px 0 6px;
}
.tabs :deep(.el-tabs__content) {
  flex: 1;
  min-height: 0;
}
.tabs :deep(.el-tab-pane) {
  height: 100%;
}
.tl {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.pw-tip {
  margin: 0 0 10px;
}
.pw-remember {
  margin-top: 8px;
}
</style>
