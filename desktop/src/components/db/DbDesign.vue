<script setup lang="ts">
// 设计表：新建表，或改已有的表（列、主键、索引、说明）。保存时由 ferry 算出要执行的语句，给用户看过再执行。
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Delete, Top, Bottom, Check, Refresh, View } from '@element-plus/icons-vue'
import { dbCall, confirmSql, type DbConn, type DbDesign, type DbColumnDef, type DbIndexDef, type DbTableInfo, type DbResult } from '../../db'

const props = defineProps<{ conn: DbConn; db: string; schema?: string; table?: string }>()
const emit = defineEmits<{ saved: [name: string, schema?: string] }>()

interface Col extends DbColumnDef { key: number; pk: boolean; generated?: boolean; hasDefault: boolean; defaultText: string }
interface Idx extends DbIndexDef { key: number; kind: 'index' | 'unique' | 'fulltext' }

const mysql = computed(() => props.conn.kind === 'mysql')
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const tab = ref<'columns' | 'indexes' | 'options' | 'sql'>('columns')
const name = ref('')
const schemaName = ref(props.schema || (mysql.value ? '' : 'public'))
const comment = ref('')
const engine = ref('')
const charset = ref('')
const collation = ref('')
const cols = ref<Col[]>([])
const idxs = ref<Idx[]>([])
const preview = ref<string[]>([])
const orig = ref<DbTableInfo | null>(null)
let seq = 0
const oldNames: Record<number, string> = {}

const types = computed(() =>
  mysql.value
    ? ['int', 'bigint', 'tinyint', 'smallint', 'int unsigned', 'bigint unsigned', 'decimal(10,2)', 'double', 'float', 'varchar(255)', 'varchar(64)', 'char(32)', 'text', 'mediumtext', 'longtext', 'json', 'datetime', 'timestamp', 'date', 'time', 'year', 'tinyint(1)', 'blob', 'longblob', 'varbinary(255)', "enum('a','b')"]
    : ['integer', 'bigint', 'smallint', 'numeric(10,2)', 'double precision', 'real', 'varchar(255)', 'char(32)', 'text', 'boolean', 'timestamp', 'timestamptz', 'date', 'time', 'interval', 'uuid', 'json', 'jsonb', 'bytea', 'inet', 'integer[]', 'text[]'],
)
function suggestType(q: string, cb: (v: { value: string }[]) => void) {
  const s = q.toLowerCase()
  cb(types.value.filter((t) => !s || t.includes(s)).map((value) => ({ value })))
}

function newCol(): Col {
  return { key: ++seq, name: '', type: mysql.value ? 'varchar(255)' : 'varchar(255)', nullable: true, default: null, pk: false, hasDefault: false, defaultText: '' }
}

async function load() {
  error.value = ''
  if (!props.table) {
    name.value = ''
    if (mysql.value) {
      engine.value = 'InnoDB'
      charset.value = 'utf8mb4'
    }
    const id: Col = { ...newCol(), name: 'id', type: mysql.value ? 'bigint' : 'bigint', nullable: false, auto: true, pk: true }
    cols.value = [id, newCol()]
    idxs.value = []
    orig.value = null
    return
  }
  loading.value = true
  try {
    const ti = await dbCall<DbTableInfo>(props.conn, 'table', props.db, { schema: props.schema, table: props.table })
    orig.value = ti
    name.value = ti.name
    comment.value = ti.comment ?? ''
    engine.value = ti.engine ?? ''
    collation.value = ti.collation ?? ''
    charset.value = ti.charset ?? ''
    cols.value = ti.columns.map((c) => ({
      key: ++seq, name: c.name, orig: c.name, type: c.type, nullable: c.nullable, default: c.default, auto: c.auto, extra: c.extra, collation: c.collation, comment: c.comment,
      pk: ti.primaryKey.includes(c.name), generated: c.generated, hasDefault: c.default !== null, defaultText: c.default ?? '',
    }))
    idxs.value = ti.indexes.filter((i) => !i.primary).map((i) => ({
      key: ++seq, name: i.name, orig: i.name, columns: [...i.columns], unique: i.unique, method: i.method, def: i.def,
      kind: i.unique ? 'unique' : (i.method ?? '').toUpperCase() === 'FULLTEXT' ? 'fulltext' : 'index',
    }))
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

function addCol(after?: number) {
  const c = newCol()
  if (after === undefined) cols.value.push(c)
  else cols.value.splice(after + 1, 0, c)
}
function removeCol(i: number) {
  const c = cols.value[i]
  cols.value.splice(i, 1)
  for (const ix of idxs.value) ix.columns = ix.columns.filter((x) => x !== c.name && !x.startsWith(c.name + '('))
}
function move(i: number, d: number) {
  const j = i + d
  if (j < 0 || j >= cols.value.length) return
  const a = cols.value
  ;[a[i], a[j]] = [a[j], a[i]]
}
function addIndex() {
  const n = idxs.value.length + 1
  idxs.value.push({ key: ++seq, name: `idx_${name.value || 'table'}_${n}`, columns: [], kind: 'index' })
}
const colNames = computed(() => cols.value.map((c) => c.name).filter(Boolean))

/** 改列名时，索引里的列跟着改 */
function renameCol(c: Col, old: string) {
  if (!old || old === c.name) return
  for (const ix of idxs.value) ix.columns = ix.columns.map((x) => (x === old ? c.name : x.startsWith(old + '(') ? c.name + x.slice(old.length) : x))
}

function design(): DbDesign {
  return {
    schema: mysql.value ? undefined : schemaName.value || 'public',
    name: name.value.trim(),
    orig: props.table || undefined,
    comment: comment.value,
    engine: mysql.value ? engine.value : undefined,
    charset: mysql.value ? charset.value : undefined,
    collation: mysql.value ? collation.value : undefined,
    columns: cols.value.map((c) => ({
      name: c.name.trim(), orig: c.orig, type: c.type.trim(), nullable: c.pk ? false : c.nullable,
      default: c.hasDefault && c.defaultText.trim() !== '' ? c.defaultText.trim() : null,
      auto: c.auto, extra: c.extra, collation: c.collation, comment: c.comment,
    })),
    primaryKey: cols.value.filter((c) => c.pk).map((c) => c.name.trim()),
    indexes: idxs.value.map((i) => ({ name: i.name.trim(), orig: i.orig, columns: i.columns, unique: i.kind === 'unique', method: i.kind === 'fulltext' ? 'FULLTEXT' : i.kind === 'unique' || i.kind === 'index' ? (i.method && i.method.toUpperCase() !== 'FULLTEXT' ? i.method : '') : i.method, def: i.def })),
  }
}

async function buildSQL(): Promise<string[] | null> {
  error.value = ''
  try {
    const r = await dbCall<{ sql: string[] }>(props.conn, 'design', props.db, { design: design() })
    preview.value = r.sql ?? []
    return preview.value
  } catch (e) {
    error.value = (e as Error).message
    return null
  }
}

async function showSQL() {
  tab.value = 'sql'
  await buildSQL()
}

async function save() {
  if (!name.value.trim()) {
    error.value = '请填表名'
    return
  }
  saving.value = true
  try {
    const stmts = await buildSQL()
    if (!stmts) return
    if (!stmts.length) {
      ElMessage.info('没有改动')
      return
    }
    const note = mysql.value && stmts.length > 0 && props.table ? 'MySQL 改表结构不能撤销；表很大时可能要等一会儿，期间表会被锁住。' : undefined
    if (!(await confirmSql(props.conn, props.table ? `修改表 ${props.table}` : '新建表', stmts, { okText: props.table ? '修改' : '新建', note }))) return
    const r = await dbCall<{ results: DbResult[] }>(props.conn, 'exec', props.db, { stmts })
    const bad = r.results.find((x) => x.error)
    if (bad) {
      const done = r.results.length - 1
      await ElMessageBox.alert(`${bad.error}\n\n出错的语句：\n${bad.sql}${done ? `\n\n前面 ${done} 条已经执行了。` : ''}`, '没有全部成功', { type: 'error' })
      return
    }
    ElMessage.success(props.table ? '已修改' : '已新建')
    emit('saved', name.value.trim(), mysql.value ? undefined : schemaName.value)
  } finally {
    saving.value = false
  }
}

onMounted(load)
defineExpose({ reload: load })
</script>

<template>
  <div v-loading="loading" class="ds">
    <div class="bar">
      <el-input v-if="!mysql" v-model="schemaName" size="small" class="schema" :disabled="!!table" placeholder="模式">
        <template #prepend>模式</template>
      </el-input>
      <el-input v-model="name" size="small" class="tname" placeholder="表名">
        <template #prepend>表名</template>
      </el-input>
      <el-radio-group v-model="tab" size="small">
        <el-radio-button value="columns">列（{{ cols.length }}）</el-radio-button>
        <el-radio-button value="indexes">索引（{{ idxs.length }}）</el-radio-button>
        <el-radio-button value="options">表选项</el-radio-button>
        <el-radio-button value="sql" @click="showSQL">SQL 预览</el-radio-button>
      </el-radio-group>
      <span class="grow" />
      <el-button v-if="table" size="small" :icon="Refresh" @click="load">重新读取</el-button>
      <el-button size="small" :icon="View" @click="showSQL">看 SQL</el-button>
      <el-button size="small" type="primary" :icon="Check" :loading="saving" @click="save">{{ table ? '保存修改' : '新建表' }}</el-button>
    </div>
    <el-alert v-if="error" type="error" :closable="false" :title="error" class="err" />

    <div v-if="tab === 'columns'" class="pane">
      <table class="cols">
        <thead>
          <tr>
            <th class="w-move" />
            <th>列名</th>
            <th>类型</th>
            <th class="w-chk" title="主键">主键</th>
            <th class="w-chk">可为空</th>
            <th class="w-chk">自增</th>
            <th>默认值</th>
            <th>说明</th>
            <th class="w-op" />
          </tr>
        </thead>
        <tbody>
          <tr v-for="(c, i) in cols" :key="c.key" :class="{ gen: c.generated }">
            <td class="w-move">
              <el-button link size="small" :icon="Top" :disabled="i === 0 || (!mysql && !!table)" @click="move(i, -1)" />
              <el-button link size="small" :icon="Bottom" :disabled="i === cols.length - 1 || (!mysql && !!table)" @click="move(i, 1)" />
            </td>
            <td><el-input v-model="c.name" size="small" :disabled="c.generated" @focus="oldNames[c.key] = c.name" @blur="renameCol(c, oldNames[c.key] || '')" /></td>
            <td>
              <el-autocomplete v-model="c.type" size="small" :fetch-suggestions="suggestType" :disabled="c.generated" class="type" />
            </td>
            <td class="w-chk"><el-checkbox v-model="c.pk" :disabled="c.generated" /></td>
            <td class="w-chk"><el-checkbox v-model="c.nullable" :disabled="c.pk || c.generated" /></td>
            <td class="w-chk"><el-checkbox v-model="c.auto" :disabled="c.generated" /></td>
            <td>
              <div class="dflt">
                <el-checkbox v-model="c.hasDefault" :disabled="c.auto || c.generated" title="有默认值" />
                <el-input v-model="c.defaultText" size="small" :disabled="!c.hasDefault || c.auto || c.generated" :placeholder="c.hasDefault ? `'文字' 或 0 或 CURRENT_TIMESTAMP` : '没有默认值'" class="mono" />
              </div>
            </td>
            <td><el-input v-model="c.comment" size="small" :disabled="c.generated" /></td>
            <td class="w-op">
              <el-button link size="small" :icon="Plus" title="在下面插一列" @click="addCol(i)" />
              <el-button link size="small" type="danger" :icon="Delete" title="删掉这一列" :disabled="c.generated" @click="removeCol(i)" />
            </td>
          </tr>
        </tbody>
      </table>
      <el-button size="small" :icon="Plus" class="add" @click="addCol()">加一列</el-button>
      <div class="help">
        默认值按 SQL 写：文字加单引号，如 <code>'abc'</code>；数字直接写；函数如 <code>CURRENT_TIMESTAMP</code>；<code>NULL</code> 表示默认为空。
        <template v-if="!mysql && table">PostgreSQL 不能调整已有列的顺序。</template>
        <template v-if="cols.some((c) => c.generated)">灰色的是计算列，这里不改它。</template>
      </div>
    </div>

    <div v-else-if="tab === 'indexes'" class="pane">
      <table class="cols">
        <thead>
          <tr>
            <th>索引名</th>
            <th>列（按顺序）</th>
            <th class="w-kind">类型</th>
            <th class="w-op" />
          </tr>
        </thead>
        <tbody>
          <tr v-for="(ix, i) in idxs" :key="ix.key">
            <td><el-input v-model="ix.name" size="small" /></td>
            <td>
              <span v-if="ix.def" class="mono" :title="ix.def">表达式索引：{{ ix.def }}</span>
              <el-select v-else v-model="ix.columns" size="small" multiple filterable allow-create class="icols" placeholder="选列（MySQL 前缀索引写 列名(10)）">
                <el-option v-for="n in colNames" :key="n" :value="n" :label="n" />
              </el-select>
            </td>
            <td class="w-kind">
              <el-select v-model="ix.kind" size="small" :disabled="!!ix.def">
                <el-option value="index" label="普通" />
                <el-option value="unique" label="唯一" />
                <el-option v-if="mysql" value="fulltext" label="全文（FULLTEXT）" />
              </el-select>
            </td>
            <td class="w-op"><el-button link size="small" type="danger" :icon="Delete" @click="idxs.splice(i, 1)" /></td>
          </tr>
        </tbody>
      </table>
      <el-button size="small" :icon="Plus" class="add" @click="addIndex">加一个索引</el-button>
      <div class="help">主键在「列」里勾选。</div>
    </div>

    <div v-else-if="tab === 'options'" class="pane opts">
      <el-form label-width="90px" size="small">
        <el-form-item label="表的说明"><el-input v-model="comment" /></el-form-item>
        <template v-if="mysql">
          <el-form-item label="存储引擎">
            <el-select v-model="engine" allow-create filterable>
              <el-option v-for="e in ['InnoDB', 'MyISAM', 'MEMORY', 'ARCHIVE']" :key="e" :value="e" />
            </el-select>
          </el-form-item>
          <el-form-item label="字符集">
            <el-select v-model="charset" allow-create filterable clearable>
              <el-option v-for="e in ['utf8mb4', 'utf8mb3', 'utf8', 'latin1', 'gbk', 'binary']" :key="e" :value="e" />
            </el-select>
          </el-form-item>
          <el-form-item label="排序规则">
            <el-input v-model="collation" placeholder="留空用字符集的默认规则，如 utf8mb4_general_ci" />
          </el-form-item>
        </template>
      </el-form>
    </div>

    <div v-else class="pane">
      <div v-if="!preview.length && !error" class="help">没有改动</div>
      <pre v-else class="sql">{{ preview.map((s) => s + ';').join('\n\n') }}</pre>
    </div>
  </div>
</template>

<style scoped>
.ds {
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
.tname {
  width: 260px;
}
.schema {
  width: 170px;
}
.grow {
  flex: 1;
}
.err {
  margin: 6px 10px 0;
  width: auto;
}
.pane {
  flex: 1;
  overflow: auto;
  padding: 8px 10px 20px;
}
.cols {
  width: 100%;
  border-collapse: collapse;
}
.cols th {
  text-align: left;
  font-weight: 600;
  font-size: 12.5px;
  padding: 4px 4px;
  color: var(--el-text-color-secondary);
  border-bottom: 1px solid var(--el-border-color-lighter);
}
.cols td {
  padding: 3px 4px;
  border-bottom: 1px solid var(--el-border-color-extra-light);
}
.cols tr.gen td {
  opacity: 0.55;
}
.w-move {
  width: 52px;
  white-space: nowrap;
}
.w-chk {
  width: 52px;
  text-align: center;
}
.w-op {
  width: 60px;
  white-space: nowrap;
}
.w-kind {
  width: 160px;
}
.type {
  width: 100%;
}
.icols {
  width: 100%;
}
.dflt {
  display: flex;
  align-items: center;
  gap: 6px;
}
.mono :deep(input),
.mono {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12px;
}
.add {
  margin-top: 8px;
}
.help {
  margin-top: 10px;
  font-size: 12.5px;
  color: var(--el-text-color-secondary);
  line-height: 1.7;
}
.help code {
  background: var(--el-fill-color-light);
  padding: 0 4px;
  border-radius: 3px;
}
.opts {
  max-width: 560px;
}
.sql {
  margin: 0;
  padding: 10px 12px;
  background: var(--el-fill-color-lighter);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12.5px;
  line-height: 1.6;
  white-space: pre-wrap;
}
</style>
