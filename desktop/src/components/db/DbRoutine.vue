<script setup lang="ts">
// 函数、存储过程的定义：看、改、保存（Ctrl+S）。新建时 routine 为空，内容是模板。
// MySQL 不能直接替换：保存时先删掉原来的再建，建不成时 ferry 会把原来的建回去；PostgreSQL 用 CREATE OR REPLACE。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Check, VideoPlay, Refresh } from '@element-plus/icons-vue'
import { createSqlEditor, type SqlEditor } from '../../editor/sqlEditor'
import { dbCall, confirmSql, parseRoutineHead, type DbConn, type DbRoutine } from '../../db'

const props = defineProps<{ conn: DbConn; db: string; routine?: DbRoutine; template?: string }>()
const emit = defineEmits<{
  /** 保存好了：现在的函数（新建的、改了名的） */
  saved: [r: DbRoutine | null]
  run: [r: DbRoutine]
}>()

const host = ref<HTMLDivElement>()
let editor: SqlEditor | null = null
const current = ref<DbRoutine | undefined>(props.routine)
const loaded = ref(props.template ?? '')
const text = ref(props.template ?? '')
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const dirty = computed(() => !current.value || text.value !== loaded.value)
const ro = computed(() => !!props.conn.readOnly)
const isDark = () => document.documentElement.classList.contains('dark')
let observer: MutationObserver | null = null

const ref0 = (r: DbRoutine) => ({ schema: r.schema, name: r.name, type: r.type, args: r.args, oid: r.oid })

async function load() {
  if (!current.value) return
  loading.value = true
  error.value = ''
  try {
    const r = await dbCall<{ sql: string }>(props.conn, 'routine', props.db, { routine: ref0(current.value) })
    loaded.value = r.sql
    text.value = r.sql
    editor?.setDoc(r.sql)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

async function reload() {
  if (dirty.value && current.value) {
    try {
      await ElMessageBox.confirm('改了的内容还没保存，重新读取会丢掉。', '重新读取', { type: 'warning', confirmButtonText: '重新读取', cancelButtonText: '取消' })
    } catch {
      return
    }
  }
  await load()
}

async function save() {
  if (ro.value || saving.value) return
  const sql = text.value.trim()
  if (!sql) return
  const head = parseRoutineHead(sql)
  if (!head) {
    void ElMessageBox.alert('这里只能写建函数、存储过程的语句（CREATE FUNCTION / CREATE PROCEDURE …）。', '没有保存', { type: 'warning' })
    return
  }
  const old = current.value
  const mysql = props.conn.kind === 'mysql'
  const q = (s: string) => '`' + s.replace(/`/g, '``') + '`'
  const stmts = mysql && old ? [`DROP ${old.type === 'function' ? 'FUNCTION' : 'PROCEDURE'} IF EXISTS ${q(props.db)}.${q(old.name)}`, sql] : [sql]
  const note = mysql && old
    ? 'MySQL 不能直接修改函数、存储过程：先删掉原来的再建。新的建不成时，Ferry 会把原来的建回去。'
    : !mysql && old && (head.name !== old.name || (head.schema ?? old.schema) !== old.schema)
      ? '名称改了：会新建一个，原来的还在（不要的话再删掉）。'
      : undefined
  if (!(await confirmSql(props.conn, old ? `保存 ${old.name}` : `新建${head.type === 'function' ? '函数' : '存储过程'} ${head.name}`, stmts, { okText: '保存', note, whole: true }))) return
  saving.value = true
  try {
    await dbCall(props.conn, 'routine-save', props.db, { routine: old ? ref0(old) : null, sql })
    ElMessage.success('已保存')
    // 找到保存后的那个（新建、改名后要知道它是谁，下次保存才对）
    const r = await dbCall<{ routines: DbRoutine[] }>(props.conn, 'routines', props.db)
    const same = r.routines.filter((x) => x.name === head.name && x.type === head.type && (!head.schema || x.schema === head.schema) && (mysql || head.schema || !old || x.schema === old.schema || x.schema === 'public'))
    const found = same.sort((a, b) => (b.oid ?? 0) - (a.oid ?? 0))[0]
    if (found) current.value = found
    await load()
    emit('saved', current.value ?? null)
  } catch (e) {
    void ElMessageBox.alert((e as Error).message, '没有保存', { type: 'error' })
  } finally {
    saving.value = false
  }
}

function onKey(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && (e.key === 's' || e.key === 'S')) {
    e.preventDefault()
    void save()
  }
}

onMounted(() => {
  editor = createSqlEditor({
    parent: host.value!,
    doc: text.value,
    kind: props.conn.kind === 'postgres' ? 'postgres' : 'mysql',
    dark: isDark(),
    onChange: (t) => (text.value = t),
  })
  observer = new MutationObserver(() => editor?.setDark(isDark()))
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
  void load()
  editor.focus()
})
onBeforeUnmount(() => {
  observer?.disconnect()
  editor?.view.destroy()
})

defineExpose({ hasChanges: () => !!current.value && dirty.value })
</script>

<template>
  <div class="rt" @keydown.capture="onKey">
    <div class="bar">
      <el-button size="small" type="primary" :icon="Check" :loading="saving" :disabled="ro || !dirty" @click="save">保存</el-button>
      <el-button size="small" :icon="VideoPlay" :disabled="!current" @click="current && emit('run', current)">运行</el-button>
      <el-button size="small" :icon="Refresh" :disabled="!current" :loading="loading" @click="reload">重新读取</el-button>
      <span class="tip">{{ current ? (current.type === 'procedure' ? '存储过程' : '函数') + ' ' + current.name : '新建：改好名称、参数和内容后保存' }}<template v-if="dirty && current"> · 改了还没保存</template></span>
      <span class="grow" />
      <el-tag v-if="ro" size="small" type="info">只读连接，不能保存</el-tag>
      <span class="tip">Ctrl+S 保存；整段作为一条语句执行，不用写 DELIMITER</span>
    </div>
    <el-alert v-if="error" type="error" :closable="false" :title="error" class="err" />
    <div class="editor"><div ref="host" class="cm" /></div>
  </div>
</template>

<style scoped>
.rt {
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
.bar .el-button + .el-button {
  margin-left: 0;
}
.tip {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.grow {
  flex: 1;
}
.err {
  margin: 6px 10px 0;
  width: auto;
  white-space: pre-wrap;
}
.editor {
  flex: 1;
  min-height: 0;
  overflow: hidden;
}
.cm {
  height: 100%;
}
</style>
