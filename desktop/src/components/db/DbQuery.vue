<script setup lang="ts">
// 查询窗口：上面写 SQL，下面看结果。Ctrl+Enter 执行光标所在的一条（或选中的），F9 执行全部。
// 同一个窗口里的语句用同一条连接（USE、SET、BEGIN … COMMIT 都有效）。
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { CaretRight, VideoPlay, CircleClose, Clock, Download, Delete } from '@element-plus/icons-vue'
import DbGrid from './DbGrid.vue'
import { createSqlEditor, type SqlEditor } from '../../editor/sqlEditor'
import { dbCall, dbRun, confirmSql, dangerOf, closeDbSession, type DbConn, type DbResult, type DbDatabase, type DbTable } from '../../db'
import { startTask } from '../../state'
import { pickSaveFile, reveal } from '../../platform'

const props = defineProps<{ conn: DbConn; db: string; sql: string; sid: string }>()
const emit = defineEmits<{ 'update:sql': [sql: string]; 'update:db': [db: string] }>()

const host = ref<HTMLDivElement>()
let editor: SqlEditor | null = null
const dbName = ref(props.db)
const databases = ref<DbDatabase[]>([])
const running = ref(false)
const results = shallowRef<DbResult[]>([])
const active = ref('0')
const error = ref('')
const totalMs = ref(0)
let cancel: (() => void) | null = null
const split = ref(Number(localStorage.getItem('ferry.db.split')) || 45)

const isDark = () => document.documentElement.classList.contains('dark')
let observer: MutationObserver | null = null

interface Hist { sql: string; db: string; at: number; ok: boolean; ms: number }
const histKey = computed(() => `ferry.db.history.${props.conn.id}`)
const history = ref<Hist[]>([])
function loadHistory() {
  try { history.value = JSON.parse(localStorage.getItem(histKey.value) || '[]') } catch { history.value = [] }
}
function pushHistory(h: Hist) {
  const list = [h, ...history.value.filter((x) => x.sql !== h.sql)].slice(0, 200)
  history.value = list
  try { localStorage.setItem(histKey.value, JSON.stringify(list)) } catch { /* 存不下就算了 */ }
}
function clearHistory() {
  history.value = []
  localStorage.removeItem(histKey.value)
}

async function loadCompletion() {
  if (!dbName.value && props.conn.kind === 'mysql') return editor?.setSchema(undefined)
  try {
    const r = await dbCall<{ tables: DbTable[] }>(props.conn, 'tables', dbName.value)
    const ns: Record<string, string[]> = {}
    for (const t of r.tables) {
      ns[t.name] = []
      if (t.schema && t.schema !== 'public') ns[`${t.schema}.${t.name}`] = []
    }
    editor?.setSchema(ns)
  } catch { /* 补全不是必须的 */ }
}

async function loadDatabases() {
  try {
    databases.value = (await dbCall<{ databases: DbDatabase[] }>(props.conn, 'databases', undefined)).databases
  } catch { /* 选择框里先只有当前库 */ }
}

watch(dbName, (v) => {
  emit('update:db', v)
  closeDbSession(props.conn.id, props.sid)
  void loadCompletion()
})

async function run(text: string) {
  const sql = text.trim()
  if (!sql || running.value) return
  const dangers = sql.split(/;\s*\n/).map(dangerOf).filter(Boolean)
  if (dangers.length && !(await confirmSql(props.conn, '确认执行', [sql], { note: '查询窗口里的语句直接执行，没有撤销。' }))) return
  running.value = true
  error.value = ''
  const started = Date.now()
  const r = dbRun<{ results: DbResult[] }>(props.conn, 'query', dbName.value, { sql, session: props.sid, max: 1000 })
  cancel = r.cancel
  try {
    const out = await r.done
    results.value = out.results
    totalMs.value = Date.now() - started
    const firstRows = out.results.findIndex((x) => x.columns?.length)
    const firstErr = out.results.findIndex((x) => x.error)
    active.value = firstErr >= 0 ? 'msg' : firstRows >= 0 ? String(firstRows) : 'msg'
    pushHistory({ sql, db: dbName.value, at: Date.now(), ok: firstErr < 0, ms: totalMs.value })
    // 改了结构：刷新补全
    if (out.results.some((x) => /^\s*(CREATE|DROP|ALTER|RENAME)\b/i.test(x.sql))) void loadCompletion()
  } catch (e) {
    error.value = (e as Error).message
    results.value = []
  } finally {
    running.value = false
    cancel = null
  }
}

function runCurrent() {
  if (!editor) return
  void run(editor.selection() || editor.current())
}
function runAll() {
  if (!editor) return
  void run(editor.selection() || editor.view.state.doc.toString())
}
function stop() {
  cancel?.()
}

function useHistory(h: Hist) {
  editor?.setDoc(h.sql)
  if (h.db !== dbName.value) dbName.value = h.db
  editor?.focus()
}

const rowResults = computed(() => results.value.map((r, i) => ({ r, i })).filter((x) => x.r.columns?.length))

async function exportResult(r: DbResult, format: 'csv' | 'sql') {
  const out = await pickSaveFile('导出查询结果', `查询结果.${format}`, format === 'csv' ? 'CSV 表格' : 'SQL 文件', [format])
  if (!out) return
  startTask(`导出 ${props.conn.name} 的查询结果`, ['db', 'export', props.conn.id, dbName.value], (t) => {
    if (t.status === 'ok') {
      ElMessage.success({ message: `已导出到 ${out}`, duration: 4000 })
      void reveal(out)
    }
  }, { FERRY_DB_REQ: JSON.stringify({ sql: r.sql, format, out, table: '导出' }) })
}

function startSplit(e: MouseEvent) {
  e.preventDefault()
  const box = (e.currentTarget as HTMLElement).parentElement!
  const rect = box.getBoundingClientRect()
  const move = (ev: MouseEvent) => {
    split.value = Math.min(85, Math.max(15, ((ev.clientY - rect.top) / rect.height) * 100))
  }
  const up = () => {
    window.removeEventListener('mousemove', move)
    window.removeEventListener('mouseup', up)
    localStorage.setItem('ferry.db.split', String(Math.round(split.value)))
  }
  window.addEventListener('mousemove', move)
  window.addEventListener('mouseup', up)
}

function resultLabel(r: DbResult, i: number) {
  return `结果 ${i + 1}` + (r.more ? '（前 1000 行）' : `（${r.rows?.length ?? 0} 行）`)
}
function shortSql(s: string) {
  const one = s.replace(/\s+/g, ' ').trim()
  return one.length > 120 ? one.slice(0, 120) + '…' : one
}
function timeText(at: number) {
  const d = new Date(at)
  return `${d.getMonth() + 1}-${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}

onMounted(() => {
  loadHistory()
  editor = createSqlEditor({
    parent: host.value!,
    doc: props.sql,
    kind: props.conn.kind === 'postgres' ? 'postgres' : 'mysql',
    dark: isDark(),
    onChange: (t) => emit('update:sql', t),
    onRun: (t) => void run(t),
  })
  observer = new MutationObserver(() => editor?.setDark(isDark()))
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
  void loadDatabases()
  void loadCompletion()
  editor.focus()
})
onBeforeUnmount(() => {
  observer?.disconnect()
  editor?.view.destroy()
  cancel?.()
  closeDbSession(props.conn.id, props.sid)
})

defineExpose({ focus: () => editor?.focus(), insert: (s: string) => { if (editor) { const v = editor.view; v.dispatch(v.state.replaceSelection(s)); v.focus() } } })
</script>

<template>
  <div class="q">
    <div class="bar">
      <el-select v-model="dbName" size="small" filterable class="dbsel" :placeholder="conn.kind === 'mysql' ? '选库' : '库'">
        <el-option v-if="dbName && !databases.some((d) => d.name === dbName)" :value="dbName" :label="dbName" />
        <el-option v-for="d in databases" :key="d.name" :value="d.name" :label="d.name" />
      </el-select>
      <el-button size="small" type="primary" :icon="CaretRight" :loading="running" @click="runCurrent">执行当前句</el-button>
      <el-button size="small" :icon="VideoPlay" :disabled="running" @click="runAll">全部执行</el-button>
      <el-button v-if="running" size="small" type="danger" :icon="CircleClose" @click="stop">停止</el-button>
      <span class="keys">Ctrl+Enter 执行光标所在的一条（或选中的），F9 执行全部</span>
      <span class="grow" />
      <el-tag v-if="conn.readOnly" size="small" type="info">只读</el-tag>
      <el-popover placement="bottom-end" :width="560" trigger="click">
        <template #reference>
          <el-button size="small" :icon="Clock">历史</el-button>
        </template>
        <div class="hist">
          <div class="hist-head">
            <span>最近执行的（这台电脑上，最多 200 条）</span>
            <el-button link size="small" :icon="Delete" :disabled="!history.length" @click="clearHistory">清空</el-button>
          </div>
          <el-empty v-if="!history.length" description="还没有执行过" :image-size="50" />
          <ul v-else>
            <li v-for="h in history" :key="h.at" @click="useHistory(h)">
              <code :class="{ bad: !h.ok }">{{ shortSql(h.sql) }}</code>
              <span class="meta">{{ timeText(h.at) }} · {{ h.db || '（没选库）' }} · {{ h.ms }} 毫秒</span>
            </li>
          </ul>
        </div>
      </el-popover>
    </div>
    <div class="body">
      <div class="editor" :style="{ height: split + '%' }"><div ref="host" class="cm" /></div>
      <div class="splitter" @mousedown="startSplit" />
      <div class="results">
        <el-alert v-if="error" type="error" :closable="false" :title="error" class="err" />
        <div v-else-if="!results.length" class="hint">{{ running ? '正在执行…' : '执行结果显示在这里' }}</div>
        <el-tabs v-else v-model="active" type="card" class="rtabs">
          <el-tab-pane v-for="x in rowResults" :key="x.i" :name="String(x.i)" :label="resultLabel(x.r, x.i)">
            <div class="rpane">
              <div class="rbar">
                <code class="rsql" :title="x.r.sql">{{ shortSql(x.r.sql) }}</code>
                <span class="meta">{{ x.r.ms }} 毫秒</span>
                <el-dropdown trigger="click" @command="(f: 'csv' | 'sql') => exportResult(x.r, f)">
                  <el-button link size="small" :icon="Download">导出全部</el-button>
                  <template #dropdown>
                    <el-dropdown-menu>
                      <el-dropdown-item command="csv">导出为 CSV（重新执行，不限行数）</el-dropdown-item>
                      <el-dropdown-item command="sql">导出为 SQL（INSERT 语句）</el-dropdown-item>
                    </el-dropdown-menu>
                  </template>
                </el-dropdown>
              </div>
              <div class="rgrid"><DbGrid :columns="x.r.columns ?? []" :rows="x.r.rows ?? []" /></div>
            </div>
          </el-tab-pane>
          <el-tab-pane name="msg" :label="`消息（${results.length} 条，${totalMs} 毫秒）`">
            <div class="msgs">
              <div v-for="(r, i) in results" :key="i" class="msg" :class="{ bad: r.error }">
                <code>{{ shortSql(r.sql) }}</code>
                <div v-if="r.error" class="m-err">出错{{ r.code ? `（${r.code}）` : '' }}：{{ r.error }}</div>
                <div v-else class="m-ok">
                  <template v-if="r.columns?.length">返回 {{ r.rows?.length ?? 0 }} 行{{ r.more ? '（只显示前 1000 行）' : '' }}</template>
                  <template v-else>成功{{ r.affected !== undefined ? `，影响 ${r.affected} 行` : '' }}{{ r.lastId ? `，新行的自增 ID 是 ${r.lastId}` : '' }}</template>
                  · {{ r.ms }} 毫秒
                </div>
              </div>
            </div>
          </el-tab-pane>
        </el-tabs>
      </div>
    </div>
  </div>
</template>

<style scoped>
.q {
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
}
.dbsel {
  width: 180px;
}
.keys {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.grow {
  flex: 1;
}
.body {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.editor {
  min-height: 60px;
  overflow: hidden;
}
.cm {
  height: 100%;
}
.splitter {
  height: 5px;
  cursor: row-resize;
  background: var(--el-border-color-lighter);
  flex-shrink: 0;
}
.splitter:hover {
  background: var(--el-color-primary-light-5);
}
.results {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.hint {
  padding: 30px;
  text-align: center;
  color: var(--el-text-color-secondary);
}
.err {
  margin: 10px;
  width: auto;
  white-space: pre-wrap;
}
.rtabs {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.rtabs :deep(.el-tabs__header) {
  margin: 0;
}
.rtabs :deep(.el-tabs__content) {
  flex: 1;
  min-height: 0;
}
.rtabs :deep(.el-tab-pane) {
  height: 100%;
}
.rpane {
  height: 100%;
  display: flex;
  flex-direction: column;
}
.rbar {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 3px 10px;
  font-size: 12px;
}
.rsql {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--el-text-color-secondary);
}
.meta {
  color: var(--el-text-color-secondary);
  font-size: 12px;
  flex-shrink: 0;
}
.rgrid {
  flex: 1;
  min-height: 0;
}
.msgs {
  height: 100%;
  overflow: auto;
  padding: 8px 12px;
}
.msg {
  padding: 6px 0;
  border-bottom: 1px dashed var(--el-border-color-lighter);
  font-size: 12.5px;
}
.msg code {
  color: var(--el-text-color-regular);
}
.m-err {
  color: var(--el-color-danger);
  white-space: pre-wrap;
  margin-top: 2px;
}
.m-ok {
  color: var(--el-color-success);
  margin-top: 2px;
}
.hist-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-size: 12px;
  color: var(--el-text-color-secondary);
  margin-bottom: 4px;
}
.hist ul {
  list-style: none;
  margin: 0;
  padding: 0;
  max-height: 360px;
  overflow: auto;
}
.hist li {
  padding: 6px 4px;
  cursor: pointer;
  border-radius: 4px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.hist li:hover {
  background: var(--el-fill-color-light);
}
.hist code {
  font-size: 12px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.hist code.bad {
  color: var(--el-color-danger);
}
</style>
