<script setup lang="ts">
import { computed, nextTick, ref, shallowRef, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { call, FerryError, isSecretKind, type DbNativeProblem, type Inspect } from '../../ferry'
import {
  dbCall,
  dbConns,
  dbNativePlan,
  type DbConn,
  type DbDatabase,
  type DbMigrationOptions,
  type DbMigrationPlan,
  type DbMigrationTable,
  type DbNativeMigrationOptions,
  type DbNativeMigrationPlan,
  type DbTable,
  type DbVia,
} from '../../db'
import { connections, connectionsPath, loadConnections, projects, startTask } from '../../state'

type MigrationSource = { conn: DbConn; db: string; table?: DbMigrationTable }
type ToolPkg = 'mydumper' | 'mysql-shell' | 'postgresql-client'

const props = defineProps<{ source: MigrationSource | null }>()
const emit = defineEmits<{ close: []; done: [conn: string, db: string] }>()

const runner = ref<'direct' | 'server'>('direct')
const mode = ref<'whole' | 'tables'>('whole')
const selected = ref<string[]>([])
const dataOnly = ref(false)
const createTargetDb = ref(false)
const concurrency = ref(1)
const ifExists = ref<'fail' | 'skip' | 'replace'>('fail')
const targetID = ref('')
const targetDB = ref('')
const tables = ref<DbTable[]>([])
const databases = ref<DbDatabase[]>([])
const tablesLoading = ref(false)
const databasesLoading = ref(false)

// 服务器端迁移选项
const workerKey = ref('')
const relay = ref(false)
const allowLocalInfile = ref(false)
const allowDumpFallback = ref(false)
const threads = ref(4)
const tempDir = ref('')
const projectWorkers = ref<DbVia[]>([])

// 预检与问题处理状态
const checking = ref(false)
const problem = ref('')
const directPlan = ref<DbMigrationPlan | null>(null)
const serverPlan = ref<DbNativeMigrationPlan | null>(null)
const remedy = shallowRef<{ message: string; detail?: DbNativeProblem } | null>(null)
const remedyOpen = ref(false)
const installing = ref(false)
const installError = ref('')
const runningSource = shallowRef<MigrationSource | null>(null)

const toolLabels: Record<ToolPkg, string> = {
  'mysql-shell': 'MySQL Shell（官方通用包）',
  mydumper: 'MyDumper / MyLoader',
  'postgresql-client': 'PostgreSQL 客户端',
}
const advice: Record<string, string> = {
  'target-not-empty': '返回后选择空目标库，或改为迁移目标库中尚不存在的指定表。',
  'target-table-exists': '返回后选择其他目标库，或取消选择已存在的表。',
  'target-db-missing': '可点击下方「自动创建目标库并预检」，或在目标实例中先手动建库。',
  'target-read-only': '返回后选择可写的目标实例。',
  'target-busy': '等待目标实例上的迁移结束，再重新预检。',
  'disk-space': '可在「临时目录」中改填空间充足的目录（如 /data/tmp），或点击下方改用流式 / 本机直连迁移。',
  'source-unreachable': '检查源库的地址、端口和执行服务器的网络，或启用本机中转 / 改用本机直连迁移。',
  'target-unreachable': '检查目标库的地址、端口和执行服务器的网络，或启用本机中转 / 改用本机直连迁移。',
  'local-infile-denied': '请用有实例参数管理权限的目标账号，或安装 MyDumper / 改用 mysqldump 流式或本机直连迁移。',
  'local-infile-restore': '请先检查迁移任务和目标数据，再确认目标实例的 local_infile 已恢复为 OFF。',
  'migration-failed': '请先在任务面板检查已完成的步骤和目标库中的数据，再决定是否重新预检。',
}
const helpPopover = {
  trigger: 'click',
  placement: 'bottom-start',
  width: 420,
  popperStyle: { maxWidth: 'calc(100vw - 32px)' },
} as const

const key = (v: DbVia) => JSON.stringify([v.config, v.target])
const tableKey = (t: DbMigrationTable) => JSON.stringify([t.schema ?? '', t.name])
const tableLabel = (t: DbMigrationTable) => (t.schema ? `${t.schema}.${t.name}` : t.name)

const targets = computed(() => dbConns.value.filter(c => c.kind === props.source?.conn.kind && !c.readOnly))
const target = computed(() => targets.value.find(c => c.id === targetID.value))
const targetDatabases = computed(() => databases.value.filter(d => !d.system && (targetID.value !== props.source?.conn.id || d.name !== props.source?.db)))
const targetDbIsNew = computed(() => !!targetDB.value && !databases.value.some(d => d.name === targetDB.value))

const workers = computed(() => {
  const all = [
    props.source?.conn.via,
    target.value?.via,
    ...connections.value.map(c => ({ config: connectionsPath.value, target: c.name, title: c.title || c.name })),
    ...projectWorkers.value,
  ]
  const unique = new Map<string, DbVia>()
  for (const worker of all) if (worker?.config && !unique.has(key(worker))) unique.set(key(worker), worker)
  return unique
})

const orderedInstallPkgs = computed<ToolPkg[]>(() => {
  const list = remedy.value?.detail?.install ?? []
  const priority: Record<ToolPkg, number> = { 'mysql-shell': 0, 'postgresql-client': 1, mydumper: 2 }
  return [...list].sort((a, b) => (priority[a] ?? 9) - (priority[b] ?? 9))
})

const suggestRelay = computed(
  () =>
    runner.value === 'server' &&
    !relay.value &&
    ['source-unreachable', 'target-unreachable', 'relay-required'].includes(remedy.value?.detail?.code || ''),
)

const selectedTables = computed<DbMigrationTable[]>(() =>
  selected.value.map(k => {
    const [schema, name] = JSON.parse(k) as [string, string]
    return { schema, name }
  }),
)

const directOptions = computed<DbMigrationOptions | null>(() => {
  if (!target.value || !targetDB.value.trim() || !props.source) return null
  const db = targetDB.value.trim()
  if (mode.value === 'whole') {
    return {
      targetConn: target.value.id,
      targetDb: db,
      whole: true,
      dataOnly: false,
      createTargetDb: createTargetDb.value,
      concurrency: concurrency.value,
      ifExists: ifExists.value,
    }
  }
  if (!selected.value.length) return null
  return {
    targetConn: target.value.id,
    targetDb: db,
    whole: false,
    dataOnly: dataOnly.value,
    createTargetDb: createTargetDb.value,
    concurrency: concurrency.value,
    ifExists: ifExists.value,
    tables: selectedTables.value,
  }
})

const serverOptions = computed<DbNativeMigrationOptions | null>(() => {
  const source = props.source
  const worker = workers.value.get(workerKey.value)
  if (!source || !worker || !target.value || !targetDB.value.trim()) return null
  if (mode.value === 'tables' && !selected.value.length) return null
  const td = tempDir.value.trim()
  if (td && !td.startsWith('/')) return null
  return {
    worker,
    relay: relay.value,
    allowLocalInfile: allowLocalInfile.value,
    allowDumpFallback: allowDumpFallback.value,
    createTargetDb: createTargetDb.value,
    scope: mode.value === 'whole' ? 'database' : 'tables',
    targetConn: target.value.id,
    targetDb: targetDB.value.trim(),
    threads: threads.value,
    tempDir: td || undefined,
    tables: mode.value === 'tables' ? selectedTables.value : undefined,
  }
})

const canCheck = computed(() => (runner.value === 'direct' ? !!directOptions.value : !!serverOptions.value))
const hasPlan = computed(() => (runner.value === 'direct' ? !!directPlan.value : !!serverPlan.value))

function resetPlan() {
  directPlan.value = null
  serverPlan.value = null
  problem.value = ''
  remedy.value = null
  remedyOpen.value = false
  installError.value = ''
}

watch(
  () => props.source,
  async source => {
    runner.value = 'direct'
    mode.value = source?.table ? 'tables' : 'whole'
    selected.value = source?.table ? [tableKey(source.table)] : []
    dataOnly.value = false
    createTargetDb.value = false
    concurrency.value = 1
    ifExists.value = 'fail'
    targetID.value = ''
    targetDB.value = ''
    tables.value = []
    databases.value = []
    workerKey.value = source?.conn.via ? key(source.conn.via) : ''
    relay.value = false
    allowLocalInfile.value = false
    allowDumpFallback.value = false
    threads.value = 4
    tempDir.value = ''
    projectWorkers.value = []
    resetPlan()
    if (!source) return
    void loadConnections().catch(() => {})
    void Promise.allSettled(
      projects.value.map(async p => {
        const shape = await call<Inspect>(['inspect', '-c', p.path])
        return shape.targets.map(t => ({ config: p.path, target: t.name, title: `${p.name} / ${t.name}` }))
      }),
    ).then(results => {
      if (props.source === source) projectWorkers.value = results.flatMap(item => (item.status === 'fulfilled' ? item.value : []))
    })
    tablesLoading.value = true
    try {
      const result = await dbCall<{ tables: DbTable[] }>(source.conn, 'tables', source.db)
      if (props.source === source) tables.value = result.tables.filter(t => t.type === 'table')
    } catch (error) {
      if (props.source === source) problem.value = (error as Error).message
    } finally {
      if (props.source === source) tablesLoading.value = false
    }
  },
  { immediate: true },
)

watch(target, async conn => {
  allowLocalInfile.value = false
  allowDumpFallback.value = false
  targetDB.value = ''
  createTargetDb.value = false
  databases.value = []
  if (!conn) return
  const source = props.source
  if (!workerKey.value && conn.via) workerKey.value = key(conn.via)
  databasesLoading.value = true
  try {
    const result = await dbCall<{ databases: DbDatabase[] }>(conn, 'databases', undefined)
    if (props.source === source && target.value === conn) databases.value = result.databases
  } catch (error) {
    if (props.source === source && target.value === conn) problem.value = (error as Error).message
  } finally {
    if (props.source === source && target.value === conn) databasesLoading.value = false
  }
})

// 手输不存在的新目标库名时自动勾选「自动创建目标库」
watch(targetDB, name => {
  const trimmed = name.trim()
  if (trimmed && databases.value.length > 0 && !databases.value.some(d => d.name === trimmed)) {
    createTargetDb.value = true
  }
})

watch([runner, mode, selected, dataOnly, createTargetDb, concurrency, ifExists, targetID, targetDB, workerKey, relay, threads, tempDir], () => {
  allowLocalInfile.value = false
  allowDumpFallback.value = false
  resetPlan()
})

function showRemedy(message: string, detail?: DbNativeProblem) {
  problem.value = message
  remedy.value = { message, detail }
  remedyOpen.value = true
  installError.value = ''
}

async function check() {
  const source = props.source
  if (!source || checking.value || installing.value) return
  if (runner.value === 'direct') {
    const request = directOptions.value
    if (!request) return
    checking.value = true
    problem.value = ''
    directPlan.value = null
    try {
      const result = await dbCall<DbMigrationPlan>(source.conn, 'migrate-plan', source.db, { migration: request })
      if (props.source === source && runner.value === 'direct' && directOptions.value === request) {
        directPlan.value = result
      }
    } catch (error) {
      if (props.source === source && runner.value === 'direct') {
        problem.value = (error as Error).message
      }
    } finally {
      checking.value = false
    }
    return
  }

  const options = serverOptions.value
  if (!options) return
  remedyOpen.value = false
  checking.value = true
  problem.value = ''
  serverPlan.value = null
  try {
    const result = await dbNativePlan(source.conn, source.db, options)
    if (props.source === source && runner.value === 'server' && serverOptions.value === options) {
      serverPlan.value = result
      remedy.value = null
    }
  } catch (error) {
    if (props.source === source && runner.value === 'server') {
      showRemedy((error as Error).message, error instanceof FerryError ? error.detail?.nativeProblem : undefined)
    }
  } finally {
    checking.value = false
  }
}

async function approveLocalInfile() {
  allowLocalInfile.value = true
  await nextTick()
  await check()
}

async function approveDumpStream() {
  allowDumpFallback.value = true
  await nextTick()
  await check()
}

async function approveCreateTargetDb() {
  createTargetDb.value = true
  await nextTick()
  await check()
}

async function retryWithRelay() {
  relay.value = true
  await nextTick()
  await check()
}

async function switchToDirectAndCheck() {
  remedyOpen.value = false
  runner.value = 'direct'
  await nextTick()
  if (directOptions.value) await check()
}

function installTools(pkg: ToolPkg) {
  const source = props.source
  const options = serverOptions.value
  if (!source || !options || installing.value || !remedy.value?.detail?.install?.includes(pkg)) return
  installing.value = true
  installError.value = ''
  startTask(`安装迁移工具 · ${toolLabels[pkg]}`, ['db', 'native-install-tools', options.worker.target, pkg, '-c', options.worker.config], task => {
    installing.value = false
    if (props.source !== source || serverOptions.value !== options) return
    if (task.status === 'ok') void check()
    else installError.value = task.error || '安装失败，请查看任务详情'
  })
}

function run() {
  const source = props.source
  const conn = target.value
  if (!source || !conn) return

  if (runner.value === 'direct') {
    const request = directOptions.value
    if (!request || !directPlan.value) return
    emit('close')
    const title = `迁移数据库 · ${source.conn.name} / ${source.db} → ${conn.name} / ${request.targetDb}`
    startTask(
      title,
      ['db', 'migrate-run', source.conn.id, source.db],
      task => {
        if (task.status === 'ok') ElMessage.success('数据库迁移完成')
        emit('done', conn.id, request.targetDb)
      },
      { FERRY_DB_REQ: JSON.stringify({ migration: request }) },
    )
    return
  }

  const options = serverOptions.value
  if (!options || !serverPlan.value || runningSource.value === source) return
  runningSource.value = source
  startTask(
    `服务器端迁移数据库 · ${source.conn.name} / ${source.db} → ${conn.name} / ${options.targetDb}`,
    ['db', 'native-run', source.conn.id, source.db, '-c', options.worker.config],
    async task => {
      if (task.status === 'ok') ElMessage.success('服务器端迁移已完成')
      if (options.targetConn && options.targetDb) emit('done', options.targetConn, options.targetDb)
      if (runningSource.value === source) runningSource.value = null
      if (props.source !== source) return
      const detail = task.detail?.nativeProblem
      const kind = task.detail?.kind
      if (task.status === 'ok' || (!detail && (task.error?.startsWith('已取消') || (kind && (isSecretKind(kind) || kind === 'unknown-host'))))) {
        emit('close')
        return
      }
      serverPlan.value = null
      allowLocalInfile.value = false
      await nextTick()
      if (props.source === source) showRemedy(task.error || '迁移未完成', detail ?? { code: 'migration-failed' })
    },
    { FERRY_DB_REQ: JSON.stringify({ nativeMigration: options }) },
  )
}
</script>

<template>
  <el-dialog
    :model-value="!!source && runningSource !== source"
    title="数据库迁移"
    width="680px"
    append-to-body
    :close-on-click-modal="false"
    :before-close="() => emit('close')"
  >
    <el-form v-if="source" label-position="top" :disabled="checking || installing" @submit.prevent>
      <p class="migration-source">源库 <strong>{{ source.conn.name }} / {{ source.db }}</strong></p>

      <el-form-item label="执行方式">
        <el-radio-group v-model="runner">
          <el-radio-button value="direct">本机直连迁移</el-radio-button>
          <el-radio-button value="server">服务器端迁移（大库高速）</el-radio-button>
        </el-radio-group>
        <p v-if="runner === 'direct'" class="migration-help">
          无需在服务器安装工具，由 Ferry 直接流式复制（支持改库名、跨网络）；适合中小库或无执行服务器场景。
        </p>
        <p v-else class="migration-help">
          在 Linux 执行服务器运行原生工具（MySQL Shell / mydumper / mysqldump 流式 / pg_dump）；适合大库或同机房内网高速迁移。
        </p>
      </el-form-item>

      <el-form-item v-if="runner === 'server'" label="执行服务器">
        <template #label="{ label }">
          <span class="native-label">
            {{ label }}
            <el-popover v-bind="helpPopover">
              <template #reference>
                <el-button link type="primary" size="small" native-type="button" aria-label="查看执行服务器说明" @click.stop>查看说明</el-button>
              </template>
              <div class="native-help">
                <p>选择 Ferry 中已保存的 Linux SSH 服务器。逻辑迁移支持普通 SSH 用户免 sudo 运行；仅通过系统源安装软件或实例物理克隆时需要 root / sudo。</p>
                <p>执行服务器需要能连通源和目标数据库的地址与端口；两端隔离时可勾选「通过这台电脑中转」。</p>
                <p>直连模式下，如果数据库连接通过 SSH 使用本机地址（如 <code>127.0.0.1</code>）或 Docker 容器，执行服务器应与该连接配置的 SSH 主机一致。</p>
              </div>
            </el-popover>
          </span>
        </template>
        <el-select v-model="workerKey" filterable placeholder="选择已保存的 Linux SSH 服务器">
          <el-option v-for="[id, w] in workers" :key="id" :value="id" :label="w.title || w.target" />
        </el-select>
        <el-checkbox v-model="relay">通过这台电脑中转</el-checkbox>
        <p v-if="relay" class="migration-help">原生工具仍在执行服务器运行。请保持 Ferry 在线，并确保这台电脑能打开所选数据库连接。</p>
      </el-form-item>

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
        <el-checkbox v-if="runner === 'direct'" v-model="dataOnly">仅迁数据，目标表必须已存在</el-checkbox>
      </el-form-item>

      <div class="migration-target">
        <el-form-item label="目标连接">
          <el-select v-model="targetID" filterable placeholder="选择同引擎可写连接">
            <el-option v-for="conn in targets" :key="conn.id" :label="conn.name" :value="conn.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="目标库">
          <el-select
            v-model="targetDB"
            filterable
            allow-create
            default-first-option
            :disabled="!target"
            :loading="databasesLoading"
            placeholder="选择已有库，或输入新库名"
          >
            <el-option v-for="db in targetDatabases" :key="db.name" :label="db.name" :value="db.name" />
          </el-select>
        </el-form-item>
      </div>

      <el-checkbox v-if="target && !dataOnly" v-model="createTargetDb" class="create-db-check">
        目标库不存在时自动创建{{ targetDbIsNew ? `（将新建 ${targetDB}）` : '' }}
      </el-checkbox>

      <div v-if="runner === 'direct'" class="migration-target server-extra">
        <el-form-item label="并发任务数">
          <el-input-number v-model="concurrency" :min="1" :max="8" />
          <p class="migration-help">1 = 单事务严格快照；2–8 = 多表并发加速。</p>
        </el-form-item>
        <el-form-item label="目标已有数据表时">
          <el-select v-model="ifExists">
            <el-option value="fail" label="停止并报错（默认安全）" />
            <el-option value="skip" label="跳过已有数据的表（断点续传）" />
            <el-option value="replace" label="清空已有表并覆盖写入" />
          </el-select>
        </el-form-item>
      </div>

      <div v-if="runner === 'server'" class="migration-target server-extra">
        <el-form-item label="并行任务数">
          <el-input-number v-model="threads" :min="2" :max="16" />
        </el-form-item>
        <el-form-item label="临时目录（可选）">
          <el-input v-model="tempDir" clearable placeholder="默认 /tmp；大库可填 /data/tmp" />
          <p class="migration-help">仅 mydumper / pg_dump 落盘时使用，预检会自动校验可用空间。</p>
        </el-form-item>
      </div>

      <p v-if="mode === 'whole'" class="migration-help">整库迁移需要空目标库（或新建目标库）。迁移期间请保持 Ferry 运行。</p>
      <p v-else class="migration-help">
        {{ runner === 'direct' ? '已有目标表必须为空，迁移期间请保持 Ferry 运行。' : '指定表要求目标库没有同名表，迁移期间请保持 Ferry 运行。' }}
      </p>
    </el-form>

    <el-alert v-if="problem" type="error" :title="problem" :closable="false" show-icon />
    <el-button v-if="runner === 'server' && remedy && !remedyOpen" link type="primary" @click="remedyOpen = true">处理问题</el-button>

    <section v-if="runner === 'direct' && directPlan" class="migration-plan" aria-label="迁移预检结果">
      <p><strong>预检通过</strong> · {{ directPlan.tables.length }} 张表 · {{ directPlan.method }}</p>
      <ul v-if="directPlan.tables.length" class="migration-tables">
        <li v-for="table in directPlan.tables" :key="tableKey(table)">
          {{ tableLabel(table) }}
          <span>{{ table.skip ? '跳过（已有数据）' : table.replace ? '清空并覆盖' : table.create ? '新建并复制' : '复制到空表' }}</span>
        </li>
      </ul>
      <p v-for="object in directPlan.objects" :key="object">{{ object }}</p>
      <p v-for="warning in directPlan.warnings" :key="warning" class="migration-help">{{ warning }}</p>
    </section>

    <section v-if="runner === 'server' && serverPlan" class="migration-plan" aria-label="迁移预检结果">
      <p><strong>{{ serverPlan.method }}</strong> · {{ serverPlan.tables }} 张表</p>
      <p class="migration-help">
        源版本 {{ serverPlan.sourceVersion }}<template v-if="serverPlan.targetVersion"> → 目标版本 {{ serverPlan.targetVersion }}</template>
      </p>
      <p v-for="warning in serverPlan.warnings" :key="warning" class="migration-help">{{ warning }}</p>
    </section>

    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button v-if="!hasPlan" type="primary" :disabled="!canCheck || installing" :loading="checking" @click="check">预检</el-button>
      <el-button v-else type="primary" @click="run">开始迁移</el-button>
    </template>
  </el-dialog>

  <el-dialog v-model="remedyOpen" title="处理迁移问题" append-to-body width="560px" :close-on-click-modal="false">
    <div v-if="remedy">
      <el-alert type="warning" :title="remedy.message" :closable="false" show-icon />
      <p v-if="orderedInstallPkgs.includes('mysql-shell')" class="remedy-note">
        <strong>推荐：</strong>安装 <strong>MySQL Shell（官方通用包）</strong> 会由本机下载（可复用「设置 → 运行环境」里的 MySQL 镜像源）并上传解压到执行服务器，<strong>不依赖 openEuler / CentOS 等系统软件源，断网服务器也可用</strong>。
      </p>
      <p v-if="orderedInstallPkgs.some(p => p !== 'mysql-shell')" class="remedy-note">
        安装位置为 <strong>{{ serverOptions?.worker.title || serverOptions?.worker.target }}</strong>。MyDumper / PostgreSQL 客户端从服务器软件源安装；源缺包时可改用 MySQL Shell、mysqldump 流式迁移或直接切换为本机直连迁移。
      </p>
      <p v-if="remedy.detail?.canUseDumpStream" class="remedy-note">
        执行服务器已具备 mysqldump / mariadb-dump，可直接以<strong>单流式管道</strong>迁移（不占临时磁盘、无需开启 local_infile）。
      </p>
      <p v-if="remedy.detail?.canEnableLocalInfile" class="remedy-note">
        临时开启会影响整个 <strong>{{ target?.name }}</strong> 实例。确认后先重新预检，开始迁移时才开启，任务结束时 Ferry 会尝试恢复原值。
      </p>
      <p v-if="remedy.detail && advice[remedy.detail.code]" class="remedy-note">{{ advice[remedy.detail.code] }}</p>
      <p v-else-if="!orderedInstallPkgs.length && !remedy.detail?.canEnableLocalInfile && !remedy.detail?.canUseDumpStream" class="remedy-note">
        可返回修改迁移选项，或直接改用本机直连迁移。
      </p>
      <el-alert v-if="installError" type="error" :title="installError" :closable="false" show-icon />
      <p v-if="installing" class="remedy-note" aria-live="polite">正在安装迁移工具，可在任务面板查看进度。</p>
    </div>
    <template #footer>
      <div class="remedy-actions">
        <el-button @click="remedyOpen = false">返回修改</el-button>
        <template v-if="remedy?.detail?.code !== 'local-infile-restore'">
          <el-button :disabled="installing" @click="check">重新预检</el-button>
          <el-button v-if="directOptions" :disabled="installing" @click="switchToDirectAndCheck">改用本机直连迁移</el-button>
          <el-button v-if="suggestRelay" type="primary" plain :disabled="installing" @click="retryWithRelay">改用本机中转并预检</el-button>
          <el-button v-if="remedy?.detail?.canCreateTargetDb" type="primary" plain :disabled="installing" @click="approveCreateTargetDb">
            自动创建目标库并预检
          </el-button>
          <el-button v-if="remedy?.detail?.canUseDumpStream" type="primary" plain :disabled="installing" @click="approveDumpStream">
            使用 mysqldump 流式迁移
          </el-button>
          <el-button
            v-for="pkg in orderedInstallPkgs"
            :key="pkg"
            type="primary"
            plain
            :disabled="installing"
            @click="installTools(pkg)"
          >
            安装 {{ toolLabels[pkg] }}
          </el-button>
          <el-button v-if="remedy?.detail?.canEnableLocalInfile" type="warning" :disabled="installing" @click="approveLocalInfile">
            允许临时开启并预检
          </el-button>
        </template>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.migration-source { margin: 0 0 14px; }
.migration-target { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.server-extra { margin-top: 8px; }
.create-db-check { margin: -4px 0 8px; display: block; }
.migration-help { margin: 6px 0 0; width: 100%; color: var(--el-text-color-secondary); font-size: 13px; line-height: 1.45; }
.migration-plan { border-top: 1px solid var(--el-border-color); margin-top: 12px; padding-top: 10px; }
.migration-tables { max-height: 140px; overflow: auto; padding-left: 20px; }
.migration-tables span { margin-left: 8px; color: var(--el-text-color-secondary); }
.native-label { display: inline-flex; align-items: center; gap: 8px; }
.native-label .el-button { height: auto; min-height: 24px; margin: 0; }
.native-help { line-height: 1.5; overflow-wrap: anywhere; }
.native-help p { margin: 0 0 8px; }
.native-help p:last-child { margin-bottom: 0; }
.remedy-note { margin: 10px 0 0; font-size: 13px; line-height: 1.5; }
.remedy-actions { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 8px; }
.remedy-actions .el-button { margin: 0; }
</style>
