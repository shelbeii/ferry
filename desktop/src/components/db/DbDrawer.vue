<script setup lang="ts">
// 「数据库」抽屉：左边是连接 → 库 →（模式）→ 表 / 视图 / 函数 / 查询 / 备份 的树，右边是标签页（对象列表、表数据、查询、设计表、函数）。
import { computed, h, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElCheckbox, ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Refresh, Close, Search, Coin, Document, Grid, View, Folder, Connection, Lock, Key, Monitor, List, Box, EditPen } from '@element-plus/icons-vue'
import type Node from 'element-plus/es/components/tree/src/model/node'
import DbTableView from './DbTableView.vue'
import DbObjects from './DbObjects.vue'
import DbQuery from './DbQuery.vue'
import DbRoutineEditor from './DbRoutine.vue'
import DbDesign from './DbDesign.vue'
import DbRedis from './DbRedis.vue'
import DbRedisConsole from './DbRedisConsole.vue'
import DbConnDialog from './DbConnDialog.vue'
import DbSqlConfirm from './DbSqlConfirm.vue'
import DbCsvImport from './DbCsvImport.vue'
import {
  dbOpen, dbConns, closeOnOutside, dbOpened, dbRequest, dbPasswordRequest, loadDbConns, dbCall, saveDbConn, removeDbConn, closeDbConn, confirmSql, passwordEnv,
  humanSize, kindLabel, exportTo, routineCallSql, type ExportKind, type DbConn, type DbDatabase, type DbTable, type DbResult, type DbFrom,
  type DbRoutine, type DbFileItem, type DbObjItem, type DbObjWhat,
} from '../../db'
import { sessionEnv } from '../../ferry'
import { pickFile, reveal } from '../../platform'
import { startTask } from '../../state'

const props = defineProps<{ side: number }>()

// ---------------------------------------------------------------- 树

/** 库（模式）下面的分组，像 Navicat：表、视图、函数、查询（这台电脑上保存的）、备份（这台电脑上的） */
type GroupKind = 'tables' | 'views' | 'funcs' | 'queries' | 'backups'
const groupLabel: Record<GroupKind, string> = { tables: '表', views: '视图', funcs: '函数', queries: '查询', backups: '备份' }
const groupWhat: Record<GroupKind, DbObjWhat> = { tables: 'table', views: 'view', funcs: 'func', queries: 'query', backups: 'backup' }
const whatGroup: Partial<Record<DbObjWhat, GroupKind>> = { table: 'tables', view: 'views', func: 'funcs', query: 'queries', backup: 'backups' }
/** 查询、备份是按库存的（PostgreSQL 也不分模式） */
const perDb = (g: GroupKind) => g === 'queries' || g === 'backups'

interface TreeData {
  id: string
  label: string
  type: 'conn' | 'db' | 'schema' | 'grp' | 'table' | 'view' | 'func' | 'savedq' | 'backup' | 'rdb' | 'none'
  conn: DbConn
  db?: string
  schema?: string
  name?: string
  group?: GroupKind
  routine?: DbRoutine
  file?: DbFileItem
  system?: boolean
  hint?: string
  leaf?: boolean
}
const tree = ref()
const filter = ref('')
const treeKey = ref(0)
const roots = computed<TreeData[]>(() => dbConns.value.map((c) => ({ id: 'c:' + c.id, label: c.name, type: 'conn', conn: c })))

function groupId(conn: DbConn, db: string, g: GroupKind, schema?: string) {
  return `g:${g}:${conn.id}:${db}:${perDb(g) ? '' : schema ?? ''}`
}
function groupNode(conn: DbConn, db: string, g: GroupKind, schema?: string): TreeData {
  return { id: groupId(conn, db, g, schema), label: groupLabel[g], type: 'grp', group: g, conn, db, schema: perDb(g) ? undefined : schema }
}
function none(d: TreeData, label: string): TreeData {
  return { id: `n:${d.id}`, label, type: 'none', conn: d.conn, leaf: true }
}
function tableLeaf(d: TreeData, t: DbTable): TreeData {
  return {
    id: `t:${d.conn.id}:${d.db}:${t.schema ?? ''}:${t.name}`, label: t.name, type: t.type, conn: d.conn, db: d.db, schema: t.schema || undefined, name: t.name, leaf: true,
    hint: [t.rows !== undefined && t.rows !== null ? `约 ${t.rows.toLocaleString()} 行` : '', humanSize(t.size), t.comment ?? ''].filter(Boolean).join(' · '),
  }
}
function funcLeaf(conn: DbConn, db: string, r: DbRoutine, label = r.name): TreeData {
  return {
    id: `f:${conn.id}:${db}:${r.schema ?? ''}:${r.name}:${r.type}:${r.oid ?? ''}`, label, type: 'func', conn, db, schema: r.schema || undefined, name: r.name, routine: r, leaf: true,
    hint: [r.type === 'procedure' ? '存储过程' : '函数', r.args ? `参数：${r.args}` : '', r.returns ? `返回 ${r.returns}` : '', r.comment ?? ''].filter(Boolean).join(' · '),
  }
}
function fileLeaf(conn: DbConn, db: string, kind: 'savedq' | 'backup', f: DbFileItem): TreeData {
  return {
    id: `${kind === 'savedq' ? 'sq' : 'b'}:${conn.id}:${db}:${f.name}`, label: f.name, type: kind, conn, db, name: f.name, file: f, leaf: true,
    hint: kind === 'savedq' ? f.preview || '（空的）' : `${humanSize(f.size)} · ${f.modified}`,
  }
}

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
      resolve(dbs.length ? dbs.map((x) => ({ id: `d:${d.conn.id}:${x.name}`, label: x.name, type: 'db', conn: d.conn, db: x.name, system: x.system })) : [none(d, '（没有库）')])
    } else if (d.type === 'db') {
      // PostgreSQL 有别的模式时先列模式（表、视图、函数在模式下面），查询和备份按库
      if (d.conn.kind === 'postgres') {
        const r = await dbCall<{ tables: DbTable[] }>(d.conn, 'tables', d.db)
        const schemas = [...new Set(r.tables.map((t) => t.schema ?? 'public'))]
        if (schemas.length > 1 || (schemas.length === 1 && schemas[0] !== 'public')) {
          return resolve([
            ...schemas.map((s): TreeData => ({ id: `s:${d.conn.id}:${d.db}:${s}`, label: s, type: 'schema', conn: d.conn, db: d.db, schema: s })),
            groupNode(d.conn, d.db!, 'queries'), groupNode(d.conn, d.db!, 'backups'),
          ])
        }
      }
      resolve((['tables', 'views', 'funcs', 'queries', 'backups'] as GroupKind[]).map((g) => groupNode(d.conn, d.db!, g)))
    } else if (d.type === 'schema') {
      resolve((['tables', 'views', 'funcs'] as GroupKind[]).map((g) => groupNode(d.conn, d.db!, g, d.schema)))
    } else if (d.type === 'grp') {
      const inSchema = (s?: string) => !d.schema || (s || 'public') === d.schema
      if (d.group === 'tables' || d.group === 'views') {
        const r = await dbCall<{ tables: DbTable[] }>(d.conn, 'tables', d.db)
        const want = d.group === 'tables' ? 'table' : 'view'
        const ts = r.tables.filter((t) => t.type === want && inSchema(t.schema))
        resolve(ts.length ? ts.map((t) => tableLeaf(d, t)) : [none(d, want === 'table' ? '（没有表）' : '（没有视图）')])
      } else if (d.group === 'funcs') {
        const r = await dbCall<{ routines: DbRoutine[] }>(d.conn, 'routines', d.db)
        const rs = r.routines.filter((x) => inSchema(x.schema))
        // PostgreSQL 的函数可以重名（参数不同）：重名的带上参数
        const n: Record<string, number> = {}
        for (const x of rs) n[x.name] = (n[x.name] ?? 0) + 1
        resolve(rs.length ? rs.map((x) => funcLeaf(d.conn, d.db!, x, n[x.name] > 1 ? `${x.name}(${x.args ?? ''})` : x.name)) : [none(d, '（没有函数）')])
      } else {
        const q = d.group === 'queries'
        const r = await dbCall<{ items: DbFileItem[] }>(d.conn, q ? 'queries' : 'backups', d.db)
        resolve(r.items.length ? r.items.map((f) => fileLeaf(d.conn, d.db!, q ? 'savedq' : 'backup', f)) : [none(d, q ? '（没有保存的查询）' : '（还没有备份）')])
      }
    } else resolve([])
  } catch (e) {
    ElMessage.error({ message: (e as Error).message, duration: 6000 })
    reject()
  }
}

/** 「对象」列表跟着刷新（表删了、改名了、导入了、存了查询……） */
function reloadObjects() {
  const o = tabs.value.find((t) => t.kind === 'objects')
  if (o) refs[o.key]?.reload?.()
}
function refreshNode(id: string) {
  reloadObjects()
  const n = tree.value?.getNode(id) as Node | undefined
  if (!n) return
  n.loaded = false
  n.childNodes = []
  n.expand()
}
/** 树上这个分组的节点（PostgreSQL 只有 public 时分组不带模式）；没展开过时是库 */
function groupNodeId(conn: DbConn, db: string, g: GroupKind, schema?: string) {
  for (const id of [groupId(conn, db, g, schema), groupId(conn, db, g)]) {
    if (tree.value?.getNode(id)) return id
  }
  return `d:${conn.id}:${db}`
}
function refreshGroup(conn: DbConn, db: string, g: GroupKind, schema?: string) {
  const id = groupNodeId(conn, db, g, schema)
  if (id.startsWith('g:')) refreshNode(id)
  else reloadObjects()
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
  else if (d.type === 'func') openRoutine(d.conn, d.db!, d.routine!)
  else if (d.type === 'savedq') void openSavedQuery(d.conn, d.db!, d.name!)
  else if (d.type === 'backup') openObjects({ ...d, schema: undefined }, 'backup')
  else if (d.type !== 'none' && !node.expanded && !node.isLeaf) node.expand()
  if (d.type === 'db' || d.type === 'schema') openObjects(d, 'all')
  else if (d.type === 'grp') openObjects(d, groupWhat[d.group!])
}

// ---------------------------------------------------------------- 标签页

interface Tab {
  key: string
  kind: 'table' | 'query' | 'design' | 'redis' | 'rcmd' | 'objects' | 'routine'
  title: string
  conn: DbConn
  db: string
  schema?: string
  table?: string
  view?: boolean
  sql?: string
  sid?: string
  /** 「对象」：列出什么 */
  what?: DbObjWhat
  /** 查询：保存过的名称、存在哪个库下面、保存时的内容（看改没改） */
  saved?: string
  savedDb?: string
  savedSql?: string
  /** 函数、存储过程（新建时为空，template 是模板） */
  routine?: DbRoutine
  template?: string
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
/** 「对象」标签只有一个、排在最前：点别的库、分组时换成那个 */
function openObjects(d: TreeData, what: DbObjWhat) {
  const key = 'objects'
  const schema = what === 'query' || what === 'backup' ? undefined : d.schema
  const o = tabs.value.find((t) => t.key === key)
  if (o) {
    o.conn = d.conn
    o.db = d.db ?? ''
    o.schema = schema
    o.what = what
  } else {
    tabs.value.unshift({ key, kind: 'objects', title: '对象', conn: d.conn, db: d.db ?? '', schema, what })
  }
  active.value = key
}
/** 「对象」列表里的一行 → 树上的节点（右键菜单、操作都按树节点做）；it 为空是整个库（模式、分组） */
function objNode(o: Tab, it: DbObjItem | null): TreeData {
  if (!it) {
    const g = o.what ? whatGroup[o.what] : undefined
    if (g) return groupNode(o.conn, o.db, g, o.schema)
    return o.schema
      ? { id: `s:${o.conn.id}:${o.db}:${o.schema}`, label: o.schema, type: 'schema', conn: o.conn, db: o.db, schema: o.schema }
      : { id: `d:${o.conn.id}:${o.db}`, label: o.db, type: 'db', conn: o.conn, db: o.db }
  }
  if (it.r) return funcLeaf(o.conn, o.db, it.r)
  if (it.f) return fileLeaf(o.conn, o.db, it.kind === 'query' ? 'savedq' : 'backup', it.f)
  return tableLeaf({ id: '', label: '', type: 'db', conn: o.conn, db: o.db }, it.t!)
}
function openObjItem(o: Tab, it: DbObjItem) {
  const d = objNode(o, it)
  if (d.type === 'table' || d.type === 'view') openTable(d)
  else if (d.type === 'func') openRoutine(d.conn, d.db!, d.routine!)
  else if (d.type === 'savedq') void openSavedQuery(d.conn, d.db!, d.name!)
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

// ---- 函数、存储过程

function routineKey(conn: DbConn, db: string, r: DbRoutine) {
  return `rt:${conn.id}:${db}:${r.schema ?? ''}:${r.name}:${r.type}:${r.oid ?? ''}`
}
function openRoutine(conn: DbConn, db: string, r: DbRoutine) {
  const key = routineKey(conn, db, r)
  if (!tabs.value.some((t) => t.key === key)) tabs.value.push({ key, kind: 'routine', title: r.name, conn, db, schema: r.schema, routine: r })
  active.value = key
}
async function newRoutine(d: TreeData, type: 'function' | 'procedure') {
  const word = type === 'function' ? '函数' : '存储过程'
  const name = await askName(`新建${word}`, `${word}名`)
  if (!name) return
  const mysql = d.conn.kind === 'mysql'
  const id = mysql ? '`' + name.replace(/`/g, '``') + '`' : `"${(d.schema ?? 'public').replace(/"/g, '""')}"."${name.replace(/"/g, '""')}"`
  let sql: string
  if (mysql) {
    sql = type === 'function'
      ? `CREATE FUNCTION ${id}(x INT)\nRETURNS INT\nDETERMINISTIC\nBEGIN\n  RETURN x + 1;\nEND`
      : `CREATE PROCEDURE ${id}(IN x INT)\nBEGIN\n  SELECT x;\nEND`
  } else {
    sql = type === 'function'
      ? `CREATE OR REPLACE FUNCTION ${id}(x integer)\nRETURNS integer\nLANGUAGE plpgsql\nAS $$\nBEGIN\n  RETURN x + 1;\nEND;\n$$`
      : `CREATE OR REPLACE PROCEDURE ${id}(x integer)\nLANGUAGE plpgsql\nAS $$\nBEGIN\n  RAISE NOTICE '%', x;\nEND;\n$$`
  }
  const key = `rt:new:${Date.now().toString(36)}`
  tabs.value.push({ key, kind: 'routine', title: `新建${word} ${name}`, conn: d.conn, db: d.db!, schema: d.schema, template: sql })
  active.value = key
}
function onRoutineSaved(t: Tab, r: DbRoutine | null) {
  if (r) {
    t.routine = r
    t.title = r.name
    t.template = undefined
  }
  refreshGroup(t.conn, t.db, 'funcs', r?.schema ?? t.schema)
}

// ---- 保存的查询（这台电脑上，按连接和库）

async function openSavedQuery(conn: DbConn, db: string, name: string) {
  const open = tabs.value.find((t) => t.kind === 'query' && t.conn.id === conn.id && t.saved === name && t.savedDb === db)
  if (open) return void (active.value = open.key)
  try {
    const r = await dbCall<{ sql: string }>(conn, 'query-load', db, { name })
    const sid = `q${Date.now().toString(36)}${++qn}`
    tabs.value.push({ key: 'q:' + sid, kind: 'query', title: name, conn, db, sql: r.sql, sid, saved: name, savedDb: db, savedSql: r.sql })
    active.value = 'q:' + sid
    saveTabs()
  } catch (e) {
    void ElMessageBox.alert((e as Error).message, '打不开', { type: 'error' })
  }
}
const queryDirty = (t: Tab) => t.kind === 'query' && !!t.saved && (t.sql ?? '') !== (t.savedSql ?? '')
/** 保存查询（Ctrl+S）；as 为 true 是另存为 */
async function saveQueryTab(t: Tab, as: boolean) {
  const update = !!t.saved && !as
  const db = update ? t.savedDb! : t.db
  if (!db) return void ElMessage.warning('先在上面选一个库：查询存在库下面的「查询」里')
  const name = update ? t.saved! : await askName(as ? '另存为' : '保存查询', `名称（存在这台电脑上，${t.conn.name} / ${db} 下面的「查询」里能找到）`, as ? `${t.saved} 副本` : /^查询 \d+$/.test(t.title) ? '' : t.title)
  if (!name) return
  const sql = t.sql ?? ''
  const save = (overwrite: boolean) => dbCall<{ name: string }>(t.conn, 'query-save', db, { name, old: update ? t.saved : undefined, sql, overwrite })
  try {
    let r: { name: string }
    try {
      r = await save(false)
    } catch (e) {
      if (!(e as Error).message.includes('已经有叫')) throw e
      try {
        await ElMessageBox.confirm(`已经有叫「${name}」的查询了，要换成现在的内容吗？`, '保存查询', { type: 'warning', confirmButtonText: '替换', cancelButtonText: '取消' })
      } catch {
        return
      }
      r = await save(true)
    }
    t.saved = r.name
    t.savedDb = db
    t.savedSql = sql
    t.title = r.name
    saveTabs()
    ElMessage.success({ message: '已保存', duration: 1500 })
    refreshGroup(t.conn, db, 'queries')
  } catch (e) {
    void ElMessageBox.alert((e as Error).message, '没有保存', { type: 'error' })
  }
}
async function renameSavedQuery(d: TreeData) {
  const name = await askName('重命名查询', '新名称', d.name)
  if (!name || name === d.name) return
  try {
    const r = await dbCall<{ name: string }>(d.conn, 'query-rename', d.db, { old: d.name, name })
    for (const t of tabs.value) {
      if (t.kind === 'query' && t.conn.id === d.conn.id && t.saved === d.name && t.savedDb === d.db) {
        t.saved = r.name
        t.title = r.name
      }
    }
    saveTabs()
    refreshGroup(d.conn, d.db!, 'queries')
  } catch (e) {
    void ElMessageBox.alert((e as Error).message, '没有改名', { type: 'error' })
  }
}
async function deleteSavedQuery(d: TreeData) {
  try {
    await ElMessageBox.confirm(`删除保存的查询「${d.name}」？这台电脑上的这个查询文件会删掉（不动数据库）。`, '删除查询', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' })
  } catch {
    return
  }
  try {
    await dbCall(d.conn, 'query-delete', d.db, { name: d.name })
    // 打开着的标签留着内容，变成没保存的
    for (const t of tabs.value) {
      if (t.kind === 'query' && t.conn.id === d.conn.id && t.saved === d.name && t.savedDb === d.db) t.saved = t.savedDb = t.savedSql = undefined
    }
    saveTabs()
    refreshGroup(d.conn, d.db!, 'queries')
  } catch (e) {
    void ElMessageBox.alert((e as Error).message, '没有删除', { type: 'error' })
  }
}

// ---- 备份（这台电脑上，就是「导出结构和数据」的 SQL 文件；还原就是执行这个文件）

function backupDb(conn: DbConn, db: string, then?: () => void) {
  startTask(`备份：${conn.name} / ${db}`, ['db', 'backup', conn.id, db], (t) => {
    refreshGroup(conn, db, 'backups')
    if (t.status !== 'ok') return
    ElMessage.success({ message: '备份好了', duration: 3000 })
    then?.()
  })
}
async function restoreBackup(d: TreeData) {
  const first = ref(true)
  try {
    await ElMessageBox.confirm(() => h('div', [
      h('p', `把备份「${d.name}」（${humanSize(d.file?.size)}）还原到 ${d.conn.name} 的库 ${d.db}。`),
      h('p', { style: 'margin-top: 8px' }, '备份里有的表会先删掉、再按备份重建，数据回到备份时的样子；函数、视图也按备份重建。备份之后新建的表不受影响。还原了不能撤销。'),
      h(ElCheckbox, { modelValue: first.value, 'onUpdate:modelValue': (v: unknown) => (first.value = !!v) }, () => '还原前先把现在的数据备份一次（推荐）'),
    ]), '还原备份', { confirmButtonText: '还原', cancelButtonText: '取消', type: 'warning' })
  } catch {
    return
  }
  const restore = () => startTask(`还原备份：${d.conn.name} / ${d.db} ← ${d.name}`, ['db', 'import-sql', d.conn.id, d.db ?? ''], (t) => {
    if (t.status === 'ok') ElMessage.success({ message: '还原好了', duration: 3000 })
    refreshNode(`d:${d.conn.id}:${d.db}`)
    for (const x of tabs.value) if (x.kind === 'table' && x.conn.id === d.conn.id && x.db === d.db) refs[x.key]?.reload?.()
  }, { FERRY_DB_REQ: JSON.stringify({ file: d.file!.path }) })
  if (first.value) backupDb(d.conn, d.db!, restore)
  else restore()
}
async function deleteBackup(d: TreeData) {
  try {
    await ElMessageBox.confirm(`删除备份「${d.name}」（${humanSize(d.file?.size)}）？删了就找不回来。`, '删除备份', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' })
  } catch {
    return
  }
  try {
    await dbCall(d.conn, 'backup-delete', d.db, { name: d.name })
    refreshGroup(d.conn, d.db!, 'backups')
  } catch (e) {
    void ElMessageBox.alert((e as Error).message, '没有删除', { type: 'error' })
  }
}
/** 打开查询、备份所在的文件夹 */
async function revealFiles(d: TreeData) {
  let path = d.file?.path
  if (!path) {
    try {
      path = (await dbCall<{ dir: string }>(d.conn, d.group === 'backups' || d.type === 'backup' ? 'backups' : 'queries', d.db)).dir
    } catch (e) {
      return void ElMessage.error((e as Error).message)
    }
  }
  if (!(await reveal(path).catch(() => false))) void ElMessageBox.alert(path, '文件在这里', { type: 'info' })
}

async function closeTab(key: string) {
  await closeTabs([key])
}
/** 关闭几个标签；其中有没保存的改动时先问一次 */
async function closeTabs(keys: string[]) {
  const list = tabs.value.filter((t) => keys.includes(t.key))
  if (!list.length) return
  const dirty = list.filter((t) => refs[t.key]?.hasChanges?.() || queryDirty(t))
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
const whatLabel: Record<DbObjWhat, string> = { all: '对象', table: '表', view: '视图', func: '函数', query: '查询', backup: '备份' }
function tabLabel(t: Tab) {
  if (t.kind === 'objects') return `${whatLabel[t.what ?? 'all']} · ${t.db}${t.schema ? '.' + t.schema : ''}`
  if (t.kind === 'redis' || t.kind === 'rcmd') return `${t.title} · ${t.conn.name}`
  if (t.kind === 'query') return `${t.title}${queryDirty(t) ? ' *' : ''}${t.db ? ' · ' + t.db : ''}`
  return t.title
}
function tabTip(t: Tab) {
  const where = `${t.conn.name}${t.db ? ' / ' + t.db : ''}${t.schema ? ' / ' + t.schema : ''}${t.table ? ' / ' + t.table : ''}`
  if (t.kind === 'query' && t.saved) return `${where}\n保存的查询「${t.saved}」（${t.savedDb}）${queryDirty(t) ? '，改了还没保存（Ctrl+S）' : ''}`
  return where
}

function onDesignSaved(t: Tab, name: string, schema?: string) {
  // 刷新树里这个库的表，表的标签重新读
  refreshGroup(t.conn, t.db, 'tables', schema ?? t.schema)
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
    const qs = tabs.value.filter((t) => t.kind === 'query').map((t) => ({ conn: t.conn.id, db: t.db, sql: t.sql ?? '', title: t.title, saved: t.saved, savedDb: t.savedDb, savedSql: t.savedSql }))
    try { localStorage.setItem(TABS_KEY, JSON.stringify(qs)) } catch { /* 存不下就算了 */ }
  }, 500)
}
function restoreTabs() {
  try {
    const qs = JSON.parse(localStorage.getItem(TABS_KEY) || '[]') as { conn: string; db: string; sql: string; title: string; saved?: string; savedDb?: string; savedSql?: string }[]
    for (const q of qs) {
      const conn = dbConns.value.find((c) => c.id === q.conn)
      if (!conn) continue
      const sid = `q${Date.now().toString(36)}${++qn}`
      tabs.value.push({ key: 'q:' + sid, kind: 'query', title: q.title || `查询 ${qn}`, conn, db: q.db, sql: q.sql, sid, saved: q.saved, savedDb: q.savedDb, savedSql: q.savedSql })
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
        { k: 'query', t: '新建查询' }, { k: 'refresh', t: '刷新' }, { k: 'create-db', t: '新建库…', off: ro },
        { k: 'edit', t: '修改连接…', sep: true }, { k: 'copy', t: '复制一份连接…' }, { k: 'close', t: '断开', off: !dbOpened[d.conn.id] }, { k: 'remove', t: '删除连接', danger: true, sep: true },
      ]
    case 'db':
      return [
        { k: 'query', t: '新建查询' }, { k: 'new-table', t: '新建表…', off: ro }, ...(d.conn.kind === 'postgres' ? [{ k: 'create-schema', t: '新建模式…', off: ro }] : []), { k: 'refresh', t: '刷新' },
        { k: 'export-struct', t: '导出全部表的结构', sep: true }, { k: 'export-dump', t: '导出全部表的结构和数据' }, { k: 'import-sql', t: '执行 SQL 文件…', off: ro },
        { k: 'backup', t: '新建备份（存在这台电脑上）', sep: true },
        { k: 'copy-name', t: '复制库名', sep: true }, { k: 'drop-db', t: '删除库…', danger: true, off: ro, sep: true },
      ]
    case 'schema':
      return [
        { k: 'query', t: '新建查询' }, { k: 'new-table', t: '新建表…', off: ro }, { k: 'refresh', t: '刷新' },
        { k: 'export-struct', t: '导出全部表的结构', sep: true }, { k: 'export-dump', t: '导出全部表的结构和数据' }, { k: 'import-sql', t: '执行 SQL 文件…', off: ro },
        { k: 'copy-name', t: '复制模式名', sep: true },
      ]
    case 'grp':
      switch (d.group) {
        case 'tables': return [{ k: 'new-table', t: '新建表…', off: ro }, { k: 'refresh', t: '刷新' }]
        case 'views': return [{ k: 'new-view', t: '新建视图…', off: ro }, { k: 'refresh', t: '刷新' }]
        case 'funcs': return [{ k: 'new-func', t: '新建函数…', off: ro }, { k: 'new-proc', t: '新建存储过程…', off: ro }, { k: 'refresh', t: '刷新' }]
        case 'queries': return [{ k: 'query', t: '新建查询' }, { k: 'refresh', t: '刷新' }, { k: 'reveal', t: '打开所在文件夹', sep: true }]
        default: return [{ k: 'backup', t: '新建备份' }, { k: 'refresh', t: '刷新' }, { k: 'reveal', t: '打开所在文件夹', sep: true }]
      }
    case 'func':
      return [
        { k: 'open', t: '打开' }, { k: 'run', t: '运行（在查询窗口里）' }, { k: 'copy-name', t: '复制名称', sep: true },
        { k: 'drop-routine', t: d.routine?.type === 'procedure' ? '删除存储过程…' : '删除函数…', danger: true, off: ro, sep: true },
      ]
    case 'savedq':
      return [
        { k: 'open', t: '打开' }, { k: 'rename-query', t: '重命名…' }, { k: 'copy-name', t: '复制名称' }, { k: 'reveal', t: '打开所在文件夹', sep: true },
        { k: 'delete-query', t: '删除…', danger: true, sep: true },
      ]
    case 'backup':
      return [
        { k: 'restore', t: '还原…', off: ro }, { k: 'reveal', t: '打开所在文件夹', sep: true },
        { k: 'delete-backup', t: '删除…', danger: true, sep: true },
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

function quoteId(conn: DbConn, s: string) {
  return conn.kind === 'mysql' ? '`' + s.replace(/`/g, '``') + '`' : '"' + s.replace(/"/g, '""') + '"'
}
function qualified(d: TreeData) {
  if (d.conn.kind === 'mysql') return quoteId(d.conn, d.name!)
  return d.schema && d.schema !== 'public' ? `${quoteId(d.conn, d.schema)}.${quoteId(d.conn, d.name!)}` : quoteId(d.conn, d.name!)
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
  if (d) await doAction(d, k)
}

async function doAction(d: TreeData, k: string) {
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
    case 'refresh': return refreshNode(d.id)
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
    case 'new-view': {
      const name = d.conn.kind === 'postgres' && d.schema && d.schema !== 'public' ? `${quoteId(d.conn, d.schema)}.v_name` : 'v_name'
      return openQuery(d.conn, d.db, `-- 新建视图：改好视图名和 SELECT 后执行（Ctrl+Enter）\nCREATE VIEW ${name} AS\nSELECT *\nFROM t\nWHERE 1 = 1;\n`)
    }
    case 'new-func': return newRoutine(d, 'function')
    case 'new-proc': return newRoutine(d, 'procedure')
    case 'open':
      if (d.type === 'rdb') return openRedis(d.conn, d.db!)
      if (d.type === 'func') return openRoutine(d.conn, d.db!, d.routine!)
      if (d.type === 'savedq') return openSavedQuery(d.conn, d.db!, d.name!)
      return openTable(d)
    case 'run': return openQuery(d.conn, d.db, routineCallSql(d.conn.kind, d.routine!))
    case 'drop-routine': {
      const r = d.routine!
      const ok = await runOp(d, r.type === 'procedure' ? 'drop-procedure' : 'drop-function', [r.name, r.args ?? ''], `删除${r.type === 'procedure' ? '存储过程' : '函数'} ${r.name}`, parentId(d))
      if (ok) {
        const key = routineKey(d.conn, d.db!, r)
        if (tabs.value.some((t) => t.key === key)) await closeTab(key)
      }
      return
    }
    case 'rename-query': return renameSavedQuery(d)
    case 'delete-query': return deleteSavedQuery(d)
    case 'reveal': return revealFiles(d)
    case 'backup': return backupDb(d.conn, d.db!)
    case 'restore': return restoreBackup(d)
    case 'delete-backup': return deleteBackup(d)
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
/** 刷新什么：树上的上一级；没展开过时按类型找分组，再没有就是库 */
function parentId(d: TreeData) {
  const n = tree.value?.getNode(d.id) as Node | undefined
  const p = (n?.parent?.data as TreeData | undefined)?.id
  if (p) return p
  const byType: Record<string, GroupKind> = { table: 'tables', view: 'views', func: 'funcs', savedq: 'queries', backup: 'backups' }
  const g = byType[d.type]
  return g ? groupNodeId(d.conn, d.db!, g, d.schema) : `d:${d.conn.id}:${d.db}`
}

// ---------------------------------------------------------------- 连接对话框、密码

const connDialog = ref<(Partial<DbConn> & { from?: DbFrom }) | null>(null)
async function onSaved(c: DbConn) {
  const fromSoftware = !!connDialog.value?.from && !connDialog.value.id
  connDialog.value = null
  for (const t of tabs.value) if (t.conn.id === c.id) t.conn = c
  treeKey.value++
  // 从「软件」页新建的：存好就打开
  if (fromSoftware) {
    await loadDbConns().catch(() => {})
    void nextTick(() => openConn(dbConns.value.find((x) => x.id === c.id) ?? c))
  }
}

/** 展开连接、打开查询窗口（Redis 打开键列表） */
function openConn(c: DbConn) {
  tree.value?.getNode('c:' + c.id)?.expand()
  if (c.kind === 'redis') openRedis(c, c.database?.replace(/^db/i, '') || '0')
  else openQuery(c)
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

// 「软件」页点了「连接数据库」
watch(dbRequest, async (r) => {
  if (!r) return
  dbRequest.value = null
  dbOpen.value = true
  if (!dbConns.value.length) await loadDbConns().catch(() => {})
  // 已经保存过这个数据库的连接（直接连的或经这台服务器连的）：直接打开它
  const f = r.from
  const same = dbConns.value.find((c) => {
    if (c.kind !== r.kind) return false
    if (f) {
      if (!c.via) return !!f.endpoint.direct && c.host === f.host && c.port === f.endpoint.direct
      return c.via.config === f.via.config && c.via.target === f.via.target && c.port === f.endpoint.ssh.port && (c.container ?? '') === (f.endpoint.ssh.container ?? '')
    }
    return !!c.via && !!r.via && c.via.config === r.via.config && c.via.target === r.via.target && c.port === r.port && (c.container ?? '') === (r.container ?? '')
  })
  if (same) {
    void nextTick(() => openConn(same))
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
  if (d.type === 'grp') return { tables: Grid, views: View, funcs: Grid, queries: Document, backups: Box }[d.group!]
  return { conn: Coin, db: Folder, schema: Folder, grp: Folder, table: Grid, view: View, func: Grid, savedq: Document, backup: Box, rdb: Folder, none: Document }[d.type]
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
            <div class="empty-tip">也可以在服务器的「软件」页，从 MySQL、PostgreSQL、Redis 的「更多」里点「连接数据库」</div>
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
                <span v-if="data.type === 'func' || (data.type === 'grp' && data.group === 'funcs')" class="ni fx">ƒ</span>
                <el-icon v-else class="ni" :class="{ on: data.type === 'conn' && dbOpened[data.conn.id] }"><component :is="nodeIcon(data)" /></el-icon>
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
          <p>左边点一个库列出里面的表，库下面还有视图、函数、保存的查询和备份；点一张表看数据；右键连接、库、表有更多操作（新建查询、新建表、导出……）。Redis 点一个库看里面的键，右键有命令行。</p>
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
                <el-icon><List v-if="t.kind === 'objects'" /><EditPen v-else-if="t.kind === 'routine'" /><Grid v-else-if="t.kind === 'table'" /><Document v-else-if="t.kind === 'query'" /><Key v-else-if="t.kind === 'redis'" /><Monitor v-else-if="t.kind === 'rcmd'" /><Folder v-else /></el-icon>
                {{ tabLabel(t) }}
              </span>
            </template>
            <DbObjects
              v-if="t.kind === 'objects'"
              :ref="(el) => (refs[t.key] = el as never)"
              :conn="t.conn"
              :db="t.db"
              :schema="t.schema"
              :what="t.what ?? 'all'"
              @open="(x: DbObjItem) => openObjItem(t, x)"
              @action="(k: string, x: DbObjItem | null) => doAction(objNode(t, x), k)"
              @context="(e: MouseEvent, x: DbObjItem) => onContext(e, objNode(t, x))"
            />
            <DbTableView
              v-else-if="t.kind === 'table'"
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
              :saved="t.saved"
              :dirty="queryDirty(t)"
              @save="(as: boolean) => saveQueryTab(t, as)"
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
            <DbRoutineEditor
              v-else-if="t.kind === 'routine'"
              :ref="(el) => (refs[t.key] = el as never)"
              :conn="t.conn"
              :db="t.db"
              :routine="t.routine"
              :template="t.template"
              @saved="(r: DbRoutine | null) => onRoutineSaved(t, r)"
              @run="(r: DbRoutine) => openQuery(t.conn, t.db, routineCallSql(t.conn.kind, r))"
            />
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
.node.backup .ni {
  color: var(--el-color-success);
}
.ni.fx {
  width: 1em;
  text-align: center;
  font-style: italic;
  font-weight: 600;
  font-family: Georgia, 'Times New Roman', serif;
  color: var(--el-color-primary);
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
