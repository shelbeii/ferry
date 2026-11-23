<script setup lang="ts">
import { computed, nextTick, ref, shallowRef, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { call, FerryError, isSecretKind, type DbNativeProblem, type Inspect } from '../../ferry'
import { dbCall, dbConns, dbNativePlan, type DbConn, type DbDatabase, type DbMigrationTable, type DbNativeMigrationOptions, type DbNativeMigrationPlan, type DbTable, type DbVia } from '../../db'
import { connections, connectionsPath, loadConnections, projects, startTask } from '../../state'

type NativeSource = { conn: DbConn; db: string; table?: DbMigrationTable; instance?: boolean }
const props = defineProps<{ source: NativeSource | null }>()
const emit = defineEmits<{ close: []; done: [conn: string, db: string] }>()

const scope = ref<'database' | 'tables'>('database')
const workerKey = ref('')
const relay = ref(false)
const allowLocalInfile = ref(false)
const targetID = ref('')
const targetDB = ref('')
const threads = ref(4)
const directory = ref('')
const sourceSSH = ref('')
const selected = ref<string[]>([])
const sourceTables = ref<DbTable[]>([])
const projectWorkers = ref<DbVia[]>([])
const targetDatabases = ref<DbDatabase[]>([])
const engine = ref<'mysql' | 'mariadb' | 'postgres'>('mysql')
const plan = ref<DbNativeMigrationPlan | null>(null)
const problem = ref('')
const checking = ref(false)
const remedy = shallowRef<{ message: string; detail?: DbNativeProblem } | null>(null)
const remedyOpen = ref(false)
const installing = ref(false)
const installError = ref('')
const runningSource = shallowRef<NativeSource | null>(null)
const tools = { mydumper: 'MyDumper / MyLoader', 'mysql-shell': 'MySQL Shell' }
const advice: Record<string, string> = {
  'target-not-empty': '返回后选择空目标库，或改为迁移目标库中尚不存在的指定表。',
  'target-table-exists': '返回后选择其他目标库，或取消选择已存在的表。',
  'target-read-only': '返回后选择可写的目标实例。',
  'target-busy': '等待目标实例上的迁移结束，再重新预检。',
  'source-unreachable': '检查源库的地址、端口和执行服务器的网络。',
  'target-unreachable': '检查目标库的地址、端口和执行服务器的网络。',
  'local-infile-denied': '请用有实例参数管理权限的目标账号，或在执行服务器上安装 MyDumper / MyLoader。',
  'local-infile-restore': '请先检查迁移任务和目标数据，再确认目标实例的 local_infile 已恢复为 OFF。',
  'migration-failed': '请先在任务面板检查已完成的步骤和目标库中的数据，再决定是否重新预检。',
}

const targets = computed(() => dbConns.value.filter(c => c.kind === props.source?.conn.kind && !c.readOnly && (!props.source?.instance || c.id !== props.source.conn.id)))
const target = computed(() => targets.value.find(c => c.id === targetID.value))
const mysqlClone = computed(() => !!props.source?.instance && engine.value === 'mysql')
const needsTarget = computed(() => !props.source?.instance || mysqlClone.value)
const canRelay = computed(() => !props.source?.instance || engine.value === 'postgres')
const suggestRelay = computed(() => canRelay.value && !relay.value &&
  ['source-unreachable', 'target-unreachable', 'relay-required'].includes(remedy.value?.detail?.code || ''))
const helpPopover = {
  trigger: 'click',
  placement: 'bottom-start',
  width: 420,
  popperStyle: { maxWidth: 'calc(100vw - 32px)' },
} as const
const key = (v: DbVia) => JSON.stringify([v.config, v.target])
const tableKey = (t: DbMigrationTable) => JSON.stringify([t.schema || '', t.name])
const tableLabel = (t: DbMigrationTable) => t.schema ? `${t.schema}.${t.name}` : t.name
const workers = computed(() => {
  const all = [props.source?.conn.via, target.value?.via, ...connections.value.map(c => ({ config: connectionsPath.value, target: c.name, title: c.title || c.name })), ...projectWorkers.value]
  const unique = new Map<string, DbVia>()
  for (const worker of all) if (worker?.config && !unique.has(key(worker))) unique.set(key(worker), worker)
  return unique
})

const request = computed<DbNativeMigrationOptions | null>(() => {
  const source = props.source
  const worker = workers.value.get(workerKey.value)
  if (!source || !worker || (needsTarget.value && !target.value)) return null
  if (source.instance) {
    if (!directory.value.trim().startsWith('/')) return null
    return { worker, relay: canRelay.value && relay.value, scope: 'instance', targetConn: needsTarget.value ? target.value?.id : undefined, directory: directory.value.trim(),
      sourceSsh: engine.value === 'mariadb' ? sourceSSH.value.trim() : undefined, threads: threads.value }
  }
  if (!targetDB.value || (scope.value === 'tables' && !selected.value.length)) return null
  return { worker, relay: relay.value, allowLocalInfile: allowLocalInfile.value, scope: scope.value, targetConn: target.value?.id, targetDb: targetDB.value, threads: threads.value,
    tables: scope.value === 'tables' ? selected.value.map(value => {
      const [schema, name] = JSON.parse(value) as [string, string]
      return { schema, name }
    }) : undefined }
})

watch(() => props.source, async source => {
  targetID.value = ''
  targetDB.value = ''
  sourceTables.value = []
  projectWorkers.value = []
  targetDatabases.value = []
  directory.value = ''
  sourceSSH.value = ''
  relay.value = false
  allowLocalInfile.value = false
  threads.value = 4
  plan.value = null
  problem.value = ''
  selected.value = source?.table ? [tableKey(source.table)] : []
  scope.value = source?.table ? 'tables' : 'database'
  workerKey.value = source?.conn.via ? key(source.conn.via) : ''
  engine.value = source?.conn.kind === 'postgres' ? 'postgres' : 'mysql'
  if (!source) return
  void loadConnections().catch(error => { if (props.source === source) problem.value = (error as Error).message })
  void Promise.allSettled(projects.value.map(async p => {
    const shape = await call<Inspect>(['inspect', '-c', p.path])
    return shape.targets.map(t => ({ config: p.path, target: t.name, title: `${p.name} / ${t.name}` }))
  })).then(results => {
    if (props.source === source) projectWorkers.value = results.flatMap(item => item.status === 'fulfilled' ? item.value : [])
  })
  try {
    if (source.instance && source.conn.kind === 'mysql') {
      const r = await dbCall<{ version: string }>(source.conn, 'info', undefined)
      if (props.source === source && /mariadb/i.test(r.version)) engine.value = 'mariadb'
    } else if (!source.instance) {
      const r = await dbCall<{ tables: DbTable[] }>(source.conn, 'tables', source.db)
      if (props.source === source) sourceTables.value = r.tables.filter(t => t.type === 'table')
    }
  } catch (error) {
    if (props.source === source) problem.value = (error as Error).message
  }
})

watch(target, async conn => {
  allowLocalInfile.value = false
  targetDB.value = ''
  targetDatabases.value = []
  if (!conn) return
  const source = props.source
  if (!workerKey.value && conn.via) workerKey.value = key(conn.via)
  try {
    const r = await dbCall<{ databases: DbDatabase[] }>(conn, 'databases', undefined)
    if (props.source === source && target.value === conn) {
      targetDatabases.value = r.databases.filter(d => !d.system && (conn.id !== source?.conn.id || d.name !== source?.db))
    }
  } catch (error) {
    if (props.source === source && target.value === conn) problem.value = (error as Error).message
  }
})

watch(request, () => {
  plan.value = null
  problem.value = ''
  remedy.value = null
  remedyOpen.value = false
  installError.value = ''
})

function showRemedy(message: string, detail?: DbNativeProblem) {
  problem.value = message
  remedy.value = { message, detail }
  remedyOpen.value = true
  installError.value = ''
}

async function check() {
  const source = props.source
  const options = request.value
  if (!source || !options || checking.value || installing.value) return
  remedyOpen.value = false
  checking.value = true
  problem.value = ''
  try {
    const r = await dbNativePlan(source.conn, source.db, options)
    if (props.source === source && request.value === options) {
      plan.value = r
      remedy.value = null
    }
  } catch (error) {
    if (props.source === source && request.value === options) {
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

async function retryWithRelay() {
  relay.value = true
  await nextTick()
  await check()
}

function installTools(pkg: keyof typeof tools) {
  const source = props.source
  const options = request.value
  if (!source || !options || installing.value || !remedy.value?.detail?.install?.includes(pkg)) return
  installing.value = true
  installError.value = ''
  startTask(`安装迁移工具 · ${tools[pkg]}`, ['db', 'native-install-tools', options.worker.target, pkg, '-c', options.worker.config], task => {
    installing.value = false
    if (props.source !== source || request.value !== options) return
    if (task.status === 'ok') void check()
    else installError.value = task.error || '安装失败，请查看任务详情'
  })
}

function run() {
  const source = props.source
  const options = request.value
  if (!source || !options || !plan.value || runningSource.value === source) return
  const name = source.instance ? '实例物理克隆' : '服务器端迁移数据库'
  runningSource.value = source
  startTask(`${name} · ${source.conn.name}${source.db ? ' / ' + source.db : ''}`,
    ['db', 'native-run', source.conn.id, source.db, '-c', options.worker.config], async task => {
      if (task.status === 'ok') ElMessage.success(source.instance ? '物理克隆已完成' : '服务器端迁移已完成')
      if (options.targetConn && options.targetDb) emit('done', options.targetConn, options.targetDb)
      if (runningSource.value === source) runningSource.value = null
      if (props.source !== source) return
      const detail = task.detail?.nativeProblem
      const kind = task.detail?.kind
      if (task.status === 'ok' || (!detail && (task.error?.startsWith('已取消') || (kind && (isSecretKind(kind) || kind === 'unknown-host'))))) {
        emit('close')
        return
      }
      plan.value = null
      allowLocalInfile.value = false
      await nextTick()
      if (props.source === source) showRemedy(task.error || '迁移未完成', detail ?? { code: 'migration-failed' })
    }, { FERRY_DB_REQ: JSON.stringify({ nativeMigration: options }) })
}
</script>

<template>
  <el-dialog :model-value="!!source && runningSource !== source" :title="source?.instance ? '实例物理克隆' : '服务器端数据库迁移'" width="660px"
    append-to-body :close-on-click-modal="false" :before-close="() => emit('close')">
    <el-form v-if="source" label-position="top" :disabled="checking || installing" @submit.prevent>
      <p>源 <strong>{{ source.conn.name }}{{ source.db ? ` / ${source.db}` : '' }}</strong></p>
      <el-form-item label="执行服务器">
        <template #label="{ label }">
          <span class="native-label">
            {{ label }}
            <el-popover v-bind="helpPopover">
              <template #reference>
                <el-button link type="primary" size="small" native-type="button" aria-label="查看执行服务器说明" @click.stop>查看说明</el-button>
              </template>
              <div class="native-help">
                <p>选择 Ferry 中已保存的 Linux SSH 服务器。Ferry 会登录这台机器执行任务，SSH 用户需要是 root 或具备 sudo 权限。</p>
                <template v-if="!source.instance">
                  <p>执行服务器需要连通源和目标数据库的地址与端口。</p>
                  <p><strong>示例：</strong>源库在 A、目标库在 B。若 B 能访问 A 的 MySQL 端口，也能访问 B 上的目标库，就可以选 B；也可以选一台能访问两边的中转服务器。</p>
                </template>
                <template v-else-if="mysqlClone">
                  <p>执行服务器需要能连接源和接收 MySQL 实例。克隆时，接收实例会直接连接源实例拉取数据。</p>
                  <p><strong>示例：</strong>源实例在 A、接收实例在 B。若选 B 执行任务，B 上的执行环境和接收 MySQL 都要能连接 A。</p>
                </template>
                <template v-else>
                  <p>执行服务器负责接收克隆数据，需要能访问源实例。新数据目录也在这台服务器上。</p>
                  <p v-if="engine === 'mariadb'"><strong>示例：</strong>源 MariaDB 在 A，计划将文件保存到 B，就选择 B，并填写 B 上已配置、可登录 A 的 SSH 别名。</p>
                  <p v-else><strong>示例：</strong>源 PostgreSQL 在 A，计划将文件保存到 B，就选择能连通 A 的 B，填写 B 上的新数据目录。</p>
                </template>
                <p v-if="!source.instance">两端隔离时可勾选「通过这台电脑中转」。例如选源库主机 A 执行，Ferry 用自己的连接访问目标库 B，A 上的迁移工具通过这台电脑把数据批量写到 B。</p>
                <p v-else-if="canRelay">执行服务器连不到源库时，可勾选「通过这台电脑中转」。例如选 B 接收备份，B 通过 Ferry 的连接读取 A 上的 PostgreSQL，并在 B 上写入新数据目录。</p>
                <p v-if="!relay || !canRelay">直连模式下，如果数据库连接通过 SSH 使用本机地址（如 <code>127.0.0.1</code>）或 Docker 容器，执行服务器应与该连接配置的 SSH 主机一致。</p>
              </div>
            </el-popover>
          </span>
        </template>
        <el-select v-model="workerKey" filterable placeholder="选择已保存的 Linux SSH 服务器">
          <el-option v-for="[id, w] in workers" :key="id" :value="id" :label="w.title || w.target" />
        </el-select>
        <el-checkbox v-if="canRelay" v-model="relay">通过这台电脑中转</el-checkbox>
        <p v-if="canRelay && relay" class="native-field-note">原生工具仍在执行服务器运行。请保持 Ferry 在线，并确保这台电脑能打开所选数据库连接。</p>
        <p v-else class="native-field-note">{{ !source.instance || mysqlClone ? 'Ferry 在此运行任务，这台服务器要能连接两端数据库。' : 'Ferry 在此运行任务并接收克隆数据，这台服务器要能访问源实例。' }}</p>
      </el-form-item>
      <el-form-item v-if="!source.instance" label="迁移范围">
        <el-radio-group v-model="scope">
          <el-radio-button value="database">整库</el-radio-button>
          <el-radio-button value="tables">指定表</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item v-else-if="source.conn.kind === 'mysql'" label="实例类型">
        <el-radio-group v-model="engine">
          <el-radio-button value="mysql">MySQL</el-radio-button>
          <el-radio-button value="mariadb">MariaDB</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item v-if="!source.instance && scope === 'tables'" label="要迁移的表">
        <el-select v-model="selected" multiple filterable>
          <el-option v-for="table in sourceTables" :key="tableKey(table)" :label="tableLabel(table)" :value="tableKey(table)" />
        </el-select>
      </el-form-item>
      <el-form-item v-if="mysqlClone" label="接收 MySQL 实例">
        <template #label="{ label }">
          <span class="native-label">
            {{ label }}
            <el-popover v-bind="helpPopover">
              <template #reference>
                <el-button link type="primary" size="small" native-type="button" aria-label="查看接收 MySQL 实例说明" @click.stop>查看说明</el-button>
              </template>
              <div class="native-help">
                <p>选择 Ferry 中已保存的可写连接，指向另一个正在运行的 MySQL 实例。它会从源实例拉取数据并写入新目录，现有数据目录照常使用。</p>
                <p><strong>示例：</strong>源 MySQL 8.4 在 A，要把副本放在 B，就选 B 上运行的 MySQL 8.4。两端要启用 Clone 插件，B 要能连通 A，且接收实例的 <code>clone_valid_donor_list</code> 要允许 A 的地址和端口。</p>
              </div>
            </el-popover>
          </span>
        </template>
        <el-select v-model="targetID" filterable placeholder="选择接收克隆数据的 MySQL 连接">
          <el-option v-for="conn in targets" :key="conn.id" :label="conn.name" :value="conn.id" />
        </el-select>
        <p class="native-field-note">选择一条可写的 MySQL 连接，接收实例会从源实例拉取数据。</p>
      </el-form-item>
      <div v-if="!source.instance" class="native-fields">
        <el-form-item label="目标数据库连接">
          <el-select v-model="targetID" filterable>
            <el-option v-for="conn in targets" :key="conn.id" :label="conn.name" :value="conn.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="目标库">
          <el-select v-model="targetDB" filterable allow-create default-first-option :disabled="!target" placeholder="选择或输入已有的库名">
            <el-option v-for="db in targetDatabases" :key="db.name" :label="db.name" :value="db.name" />
          </el-select>
        </el-form-item>
      </div>
      <el-form-item v-if="source.instance" :label="mysqlClone ? '接收端的新数据目录' : '执行服务器上的新数据目录'">
        <template #label="{ label }">
          <span class="native-label">
            {{ label }}
            <el-popover v-bind="helpPopover">
              <template #reference>
                <el-button link type="primary" size="small" native-type="button" aria-label="查看新数据目录说明" @click.stop>查看说明</el-button>
              </template>
              <div class="native-help">
                <p v-if="mysqlClone">填写接收 MySQL 进程能访问的 Linux 绝对路径，目录应在现有 MySQL 数据目录之外。父目录要提前建好，并允许 MySQL 进程写入；最后一级目录由克隆任务创建。</p>
                <p v-else>填写执行服务器上的 Linux 绝对路径。父目录要提前建好，克隆任务会创建最后一级目录。</p>
                <p><strong>示例：</strong>先在{{ mysqlClone ? '接收实例所在环境' : '执行服务器' }}准备 <code>/srv/database-clones</code>，然后填写 <code>/srv/database-clones/clone-01</code>，让克隆任务创建 <code>clone-01</code>。</p>
                <p v-if="mysqlClone">如果接收 MySQL 运行在 Docker 中，请填写容器内的路径。需要把数据保存在宿主机时，将父目录挂载到容器中。</p>
              </div>
            </el-popover>
          </span>
        </template>
        <el-input v-model="directory" placeholder="/srv/database-clones/clone-01" />
        <p class="native-field-note">填写{{ mysqlClone ? '接收 MySQL 进程可访问的' : '执行服务器上的' }}绝对路径，父目录须已存在，最后一级目录须尚未创建。</p>
      </el-form-item>
      <el-form-item v-if="source.instance && engine === 'mariadb'" label="源主机的 SSH 别名（执行服务器已配置）">
        <el-input v-model="sourceSSH" placeholder="源 MariaDB 在执行服务器本机时留空" />
      </el-form-item>
      <el-form-item v-if="!source.instance || engine === 'mariadb'" label="并行任务数">
        <el-input-number v-model="threads" :min="2" :max="16" />
      </el-form-item>
    </el-form>
    <p v-if="source?.instance" class="native-note">克隆完成后，用与源库兼容的版本从新目录启动新实例。</p>
    <p v-else class="native-note">整库迁移需要空目标库，指定表要求目标没有同名表。</p>
    <el-alert v-if="problem" type="error" :title="problem" :closable="false" show-icon />
    <el-button v-if="remedy && !remedyOpen" link type="primary" @click="remedyOpen = true">处理问题</el-button>
    <section v-if="plan" aria-label="迁移预检结果">
      <p><strong>{{ plan.method }}</strong><template v-if="!source?.instance"> · {{ plan.tables }} 张表</template></p>
      <p class="native-note">源版本 {{ plan.sourceVersion }}<template v-if="plan.targetVersion"> → 目标版本 {{ plan.targetVersion }}</template></p>
      <p v-for="warning in plan.warnings" :key="warning" class="native-note">{{ warning }}</p>
    </section>
    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button v-if="!plan" type="primary" :disabled="!request || installing" :loading="checking" @click="check">预检</el-button>
      <el-button v-else type="primary" @click="run">{{ source?.instance ? '开始克隆' : '开始迁移' }}</el-button>
    </template>
  </el-dialog>
  <el-dialog v-model="remedyOpen" title="处理迁移问题" append-to-body width="530px" :close-on-click-modal="false">
    <div v-if="remedy">
      <el-alert type="warning" :title="remedy.message" :closable="false" show-icon />
      <p v-if="remedy.detail?.install?.length">
        安装位置为 <strong>{{ request?.worker.title || request?.worker.target }}</strong>。Ferry 从该服务器已配置的软件源安装客户端工具。软件源缺包时，可以换用已有工具的执行服务器。
      </p>
      <p v-if="remedy.detail?.canEnableLocalInfile">
        临时开启会影响整个 <strong>{{ target?.name }}</strong> 实例。确认后先重新预检，开始迁移时才开启。任务结束时 Ferry 会尝试恢复原值。
      </p>
      <p v-if="remedy.detail && advice[remedy.detail.code]">{{ advice[remedy.detail.code] }}</p>
      <p v-else-if="!remedy.detail?.install?.length && !remedy.detail?.canEnableLocalInfile">可返回修改迁移选项，或在处理上面的错误后重新预检。</p>
      <el-alert v-if="installError" type="error" :title="installError" :closable="false" show-icon />
      <p v-if="installing" aria-live="polite">正在安装迁移工具，可在任务面板查看进度。</p>
    </div>
    <template #footer>
      <div class="remedy-actions">
        <el-button @click="remedyOpen = false">返回修改</el-button>
        <template v-if="remedy?.detail?.code !== 'local-infile-restore'">
          <el-button :disabled="installing" @click="check">重新预检</el-button>
          <el-button v-if="suggestRelay" type="primary" plain @click="retryWithRelay">改用本机中转并预检</el-button>
          <el-button v-for="pkg in remedy?.detail?.install" :key="pkg" type="primary" plain :disabled="installing" @click="installTools(pkg)">安装 {{ tools[pkg] }}</el-button>
          <el-button v-if="remedy?.detail?.canEnableLocalInfile" type="warning" :disabled="installing" @click="approveLocalInfile">允许临时开启并预检</el-button>
        </template>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.native-fields { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.native-label { display: inline-flex; align-items: center; gap: 8px; }
.native-label .el-button { height: auto; min-height: 24px; margin: 0; }
.el-select + .el-checkbox { margin: 4px 0 0; }
.el-checkbox + .native-field-note { margin-top: 0; }
.native-field-note { margin: 6px 0 0; width: 100%; color: var(--el-text-color-secondary); font-size: 13px; line-height: 1.4; }
.native-help { line-height: 1.5; overflow-wrap: anywhere; }
.native-help p { margin: 0 0 8px; }
.native-help p:last-child { margin-bottom: 0; }
.native-note { color: var(--el-text-color-secondary); font-size: 13px; }
.remedy-actions { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 8px; }
.remedy-actions .el-button { margin: 0; }
</style>
