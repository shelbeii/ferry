<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { dbCall, dbConns, type DbConn, type DbDatabase, type DbMigrationOptions, type DbMigrationPlan, type DbMigrationTable, type DbTable } from '../../db'
import { startTask } from '../../state'

const props = defineProps<{ source: { conn: DbConn; db: string; table?: DbMigrationTable } | null }>()
const emit = defineEmits<{ close: []; done: [conn: string, db: string] }>()

const mode = ref<'whole' | 'tables'>('whole')
const selected = ref<string[]>([])
const dataOnly = ref(false)
const targetID = ref('')
const targetDB = ref('')
const tables = ref<DbTable[]>([])
const databases = ref<DbDatabase[]>([])
const tablesLoading = ref(false)
const databasesLoading = ref(false)
const checking = ref(false)
const problem = ref('')
const plan = ref<DbMigrationPlan | null>(null)

const targets = computed(() => dbConns.value.filter(c => c.kind === props.source?.conn.kind && !c.readOnly))
const target = computed(() => targets.value.find(c => c.id === targetID.value))
const targetDatabases = computed(() => databases.value.filter(d => !d.system && (targetID.value !== props.source?.conn.id || d.name !== props.source?.db)))
const options = computed<DbMigrationOptions | null>(() => {
  if (!target.value || !targetDB.value || !props.source) return null
  if (mode.value === 'whole') return { targetConn: target.value.id, targetDb: targetDB.value, whole: true, dataOnly: false }
  if (!selected.value.length) return null
  return { targetConn: target.value.id, targetDb: targetDB.value, whole: false, dataOnly: dataOnly.value, tables: selected.value.map(key => {
    const [schema, name] = JSON.parse(key) as [string, string]
    return { schema, name }
  }) }
})

function tableKey(t: DbMigrationTable) { return JSON.stringify([t.schema ?? '', t.name]) }
function tableLabel(t: DbMigrationTable) { return t.schema ? `${t.schema}.${t.name}` : t.name }

watch(() => props.source, async (source) => {
  mode.value = source?.table ? 'tables' : 'whole'
  selected.value = source?.table ? [tableKey(source.table)] : []
  dataOnly.value = false
  targetID.value = ''
  targetDB.value = ''
  tables.value = []
  problem.value = ''
  plan.value = null
  if (!source) return
  tablesLoading.value = true
  try {
    const result = await dbCall<{ tables: DbTable[] }>(source.conn, 'tables', source.db)
    if (props.source === source) tables.value = result.tables.filter(t => t.type === 'table')
  } catch (error) {
    if (props.source === source) problem.value = (error as Error).message
  } finally {
    if (props.source === source) tablesLoading.value = false
  }
}, { immediate: true })

watch(target, async (conn) => {
  targetDB.value = ''
  databases.value = []
  if (!conn) return
  databasesLoading.value = true
  try {
    const result = await dbCall<{ databases: DbDatabase[] }>(conn, 'databases', undefined)
    if (target.value === conn) databases.value = result.databases
  } catch (error) {
    if (target.value === conn) problem.value = (error as Error).message
  } finally {
    if (target.value === conn) databasesLoading.value = false
  }
})

watch([mode, selected, dataOnly, targetID, targetDB], () => {
  plan.value = null
  problem.value = ''
})

async function check() {
  const source = props.source
  const request = options.value
  if (!source || !request) return
  checking.value = true
  problem.value = ''
  try {
    const result = await dbCall<DbMigrationPlan>(source.conn, 'migrate-plan', source.db, { migration: request })
    if (props.source === source && options.value === request) plan.value = result
  } catch (error) {
    if (props.source === source && options.value === request) problem.value = (error as Error).message
  } finally {
    checking.value = false
  }
}

function run() {
  const source = props.source
  const request = options.value
  const conn = target.value
  if (!source || !request || !conn || !plan.value) return
  emit('close')
  const title = `迁移数据库 · ${source.conn.name} / ${source.db} → ${conn.name} / ${request.targetDb}`
  startTask(title, ['db', 'migrate-run', source.conn.id, source.db], (task) => {
    if (task.status === 'ok') ElMessage.success('数据库迁移完成')
    emit('done', conn.id, request.targetDb)
  }, { FERRY_DB_REQ: JSON.stringify({ migration: request }) })
}
</script>

<template>
  <el-dialog :model-value="!!source" title="迁移到另一个数据库" width="640px" append-to-body
    :close-on-click-modal="false" @close="emit('close')">
    <el-form v-if="source" label-position="top" :disabled="checking" @submit.prevent>
      <p class="migration-source">源库 <strong>{{ source.conn.name }} / {{ source.db }}</strong></p>
      <el-form-item label="迁移范围">
        <el-radio-group v-model="mode">
          <el-radio-button value="whole">整库</el-radio-button>
          <el-radio-button value="tables">指定表</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item v-if="mode === 'tables'" label="要迁移的表">
        <el-select v-model="selected" multiple filterable :loading="tablesLoading" placeholder="选择一张或多张表">
          <el-option v-for="table in tables" :key="tableKey(table)" :label="tableLabel(table)" :value="tableKey(table)" />
        </el-select>
        <el-checkbox v-model="dataOnly">仅迁数据，目标表必须已存在</el-checkbox>
      </el-form-item>
      <div class="migration-target">
        <el-form-item label="目标连接">
          <el-select v-model="targetID" filterable placeholder="选择同引擎连接">
            <el-option v-for="conn in targets" :key="conn.id" :label="conn.name" :value="conn.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="目标库">
          <el-select v-model="targetDB" filterable :disabled="!target" :loading="databasesLoading" placeholder="选择已有的库">
            <el-option v-for="db in targetDatabases" :key="db.name" :label="db.name" :value="db.name" />
          </el-select>
        </el-form-item>
      </div>
      <p v-if="mode === 'whole'" class="migration-help">请选择空库。迁移期间保持 Ferry 运行。</p>
      <p v-else class="migration-help">已有目标表必须为空，迁移期间保持 Ferry 运行。</p>
    </el-form>
    <el-alert v-if="problem" type="error" :title="problem" :closable="false" show-icon />
    <section v-if="plan" class="migration-plan" aria-label="迁移预检结果">
      <p><strong>预检通过</strong> · {{ plan.tables.length }} 张表 · {{ plan.method }}</p>
      <ul v-if="plan.tables.length" class="migration-tables">
        <li v-for="table in plan.tables" :key="tableKey(table)">
          {{ tableLabel(table) }} <span>{{ table.create ? '新建并复制' : '复制到空表' }}</span>
        </li>
      </ul>
      <p v-for="object in plan.objects" :key="object">{{ object }}</p>
      <p v-for="warning in plan.warnings" :key="warning" class="migration-help">{{ warning }}</p>
    </section>
    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button v-if="!plan" type="primary" :disabled="!options" :loading="checking" @click="check">预检</el-button>
      <el-button v-else type="primary" @click="run">开始迁移</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.migration-source { margin: 0 0 14px; }
.migration-target { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.migration-help { color: var(--el-text-color-secondary); font-size: 13px; }
.migration-plan { border-top: 1px solid var(--el-border-color); margin-top: 12px; }
.migration-tables { max-height: 140px; overflow: auto; padding-left: 20px; }
.migration-tables span { margin-left: 8px; color: var(--el-text-color-secondary); }
</style>
