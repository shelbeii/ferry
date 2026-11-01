<script setup lang="ts">
// 数据备份（ferry backup）：配置里的 backups（MySQL、PostgreSQL、目录、自己写的命令）。
// 打开后服务器上按时备份、部署前备份，随时可以关；现在备份、恢复、下载到本机关着也能用。
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { call, ferry, type FerryLine } from '../ferry'
import { pickDir } from '../platform'
import { confirmProduction } from '../ui'
import LoadingBlock from './LoadingBlock.vue'
import GlassSwitch from './glass/GlassSwitch.vue'

const props = defineProps<{ config: string; target: string; production?: boolean }>()
const emit = defineEmits<{ close: [] }>()
const visible = ref(true)

interface Run {
  time: string
  ok: boolean
  error?: string
  file?: string
  size?: number
  sec: number
  reason: string
}
interface Item {
  name: string
  kind: 'mysql' | 'postgres' | 'paths' | 'command'
  database?: string
  container?: string
  paths?: string[]
  onCalendar: string
  keep: number
  beforeDeploy?: boolean
  next?: string
  last?: Run
  lastOk?: Run
  count: number
  bytes: number
  changed?: boolean
  removed?: boolean
}
interface BFile {
  item: string
  name: string
  path: string
  size: number
  time: string
}
interface Status {
  on: boolean
  dir: string
  items: Item[]
  files?: BFile[]
}

const status = ref<Status>()
const files = ref<BFile[]>([])
const error = ref('')
const busy = ref('')
const log = ref<string[]>([])
const progress = ref<{ label: string; pct: number }>()
const filter = ref('')

const shown = computed(() => files.value.filter((f) => !filter.value || f.item === filter.value))

async function load() {
  error.value = ''
  try {
    status.value = await call<Status>(['backup', props.target, 'status', '-c', props.config])
    files.value = (await call<Status>(['backup', props.target, 'list', '-c', props.config])).files ?? []
  } catch (e) {
    error.value = (e as Error).message
  }
}
onMounted(load)

/** 要等一会儿的操作：服务器上的进展显示在下面。 */
async function long<T>(key: string, args: string[]): Promise<T | undefined> {
  busy.value = key
  error.value = ''
  log.value = []
  progress.value = undefined
  const onLine = (l: FerryLine) => {
    if (l.kind === 'progress' && l.total) progress.value = { label: l.label ?? '', pct: Math.round(((l.done ?? 0) * 100) / l.total) }
    else if (l.msg && ['info', 'warn', 'error', 'event', 'step'].includes(l.kind)) log.value.push(l.msg)
  }
  try {
    const r = await ferry<T>([...args, '-c', props.config], onLine).done
    if (!r.ok) {
      error.value = r.error ?? '失败了'
      return
    }
    return r.data ?? ({} as T)
  } finally {
    busy.value = ''
    progress.value = undefined
  }
}

async function toggle(on: boolean) {
  if (!on) {
    try {
      await ElMessageBox.confirm('关掉后服务器上不再定时备份，部署前也不备份。已有的备份文件留着，随时可以再打开。', '关闭数据备份', { type: 'warning', confirmButtonText: '关闭' })
    } catch {
      return
    }
  }
  if (await long(on ? 'on' : 'off', ['backup', props.target, on ? 'on' : 'off'])) {
    ElMessage.success(on ? '数据备份已打开' : '数据备份已关闭')
    await load()
  }
}

async function runNow(name = '') {
  if (await long('run:' + name, ['backup', props.target, 'run', ...(name ? [name] : [])])) {
    ElMessage.success('备份好了')
    await load()
  }
}

async function restore(f: BFile) {
  const what = `用 ${f.name} 恢复 ${f.item}`
  try {
    await ElMessageBox.confirm(
      `${what}？现在的数据会被换成这份备份里的。恢复前会先自动备份一份现在的，恢复错了还能用那份再恢复回来。恢复期间最好先停掉用这些数据的应用。`,
      '恢复数据',
      { type: 'warning', confirmButtonText: '恢复' },
    )
  } catch {
    return
  }
  if (props.production && !(await confirmProduction(what, props.target))) return
  if (await long('restore:' + f.name, ['backup', props.target, 'restore', f.item, f.name, '--yes'])) {
    ElMessage.success('已恢复')
    await load()
  }
}

async function forceRestore(f: BFile) {
  try {
    await ElMessageBox.confirm('恢复前备份现在的数据失败了。照样恢复的话，现在的数据就找不回来了。', '照样恢复？', { type: 'error', confirmButtonText: '照样恢复' })
  } catch {
    return
  }
  if (await long('restore:' + f.name, ['backup', props.target, 'restore', f.item, f.name, '--yes', '--force'])) {
    ElMessage.success('已恢复')
    await load()
  }
}

async function download(f: BFile) {
  const dir = await pickDir('下载到哪个文件夹')
  if (!dir) return
  const r = await long<{ file: string }>('download:' + f.name, ['backup', props.target, 'download', f.item, f.name, dir])
  if (r) ElMessage.success(`已下载到 ${r.file}`)
}

async function remove(f: BFile) {
  try {
    await ElMessageBox.confirm(`删掉服务器上的 ${f.name}？删了就找不回来了。`, '删除备份', { type: 'warning', confirmButtonText: '删除' })
  } catch {
    return
  }
  if (await long('delete:' + f.name, ['backup', props.target, 'delete', f.item, f.name, '--yes'])) await load()
}

const kindText = (it: Item) =>
  ({ mysql: `MySQL ${it.database}`, postgres: `PostgreSQL ${it.database}`, paths: (it.paths ?? []).join('、'), command: '自己写的命令' })[it.kind] +
  (it.container ? `（容器 ${it.container}）` : '')
const calendar = (c: string) => ({ '*-*-* 03:00:00': '每天 3 点', hourly: '每小时', 'Sun *-*-* 03:00:00': '每周日 3 点' })[c] ?? c
const when = (t?: string) => (t ? new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '')
function size(n = 0) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(2) + ' GB'
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB'
  if (n >= 1 << 10) return Math.round(n / 1024) + ' KB'
  return n + ' B'
}
const preRestoreFailed = computed(() => error.value.includes('恢复前备份现在的数据失败'))
const lastRestore = ref<BFile>()
async function doRestore(f: BFile) {
  lastRestore.value = f
  await restore(f)
}

const example = `backups:
  - name: db
    mysql: { database: shop }      # 数据库在容器里：加 container: 1Panel-mysql-xxxx
    envFile: /etc/shop/db.env      # 密码：MYSQL_PWD=…（PostgreSQL 用 PGPASSWORD=…）
    schedule: daily                # daily | hourly | weekly | systemd 的 OnCalendar
    keep: 7
    beforeDeploy: true             # 部署前先备份一次
  - name: uploads
    paths: [/srv/shop/uploads]`
</script>

<template>
  <el-dialog v-model="visible" :title="`数据备份（${target}）`" width="820px" append-to-body @closed="emit('close')">
    <LoadingBlock v-if="!status && !error" :text="`正在连接 ${target}…`" :rows="4" min-height="200px" />
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="block">
      <el-button v-if="preRestoreFailed && lastRestore" size="small" type="danger" plain @click="forceRestore(lastRestore)">不备份，照样恢复</el-button>
    </el-alert>
    <template v-if="status">
      <template v-if="!status.items.length">
        <p class="muted small">配置里还没写这台服务器要备份什么。在「编辑配置」的表单里点「数据备份 → 新增备份」，或在 ferry.yaml 里加上 backups（MySQL、PostgreSQL、目录、自己写的备份命令），例如：</p>
        <pre class="example">{{ example }}</pre>
      </template>
      <template v-else>
        <div class="state">
          <GlassSwitch :model-value="status.on" :loading="busy === 'on' || busy === 'off'" active-text="开" inactive-text="关" aria-label="数据备份" @change="(v: boolean) => toggle(v)" />
          <span class="small">{{ status.on ? '开着：服务器上按时备份' : '没开：不定时备份、部署前也不备份（下面的「现在备份」、恢复照样能用）' }}</span>
          <span class="muted small">· 备份文件在服务器的 {{ status.dir }}</span>
        </div>
        <el-table :data="status.items" size="small" class="items">
          <el-table-column label="名字" prop="name" width="96" />
          <el-table-column label="备份什么" min-width="170">
            <template #default="{ row }">{{ kindText(row) }}</template>
          </el-table-column>
          <el-table-column label="什么时候" width="150">
            <template #default="{ row }">
              {{ calendar(row.onCalendar) }}<span v-if="row.beforeDeploy">、部署前</span>
              <div class="muted">留 {{ row.keep }} 份</div>
            </template>
          </el-table-column>
          <el-table-column label="上次" min-width="170">
            <template #default="{ row }">
              <template v-if="row.last">
                <span :class="row.last.ok ? 'ok' : 'bad'">{{ row.last.ok ? '成功' : '失败' }}</span>
                <span class="muted">（{{ row.last.reason }}，{{ when(row.last.time) }}）</span>
                <div v-if="!row.last.ok" class="bad err">{{ row.last.error }}</div>
              </template>
              <span v-else class="muted">还没备份过</span>
              <div v-if="row.changed" class="warn">配置改过了，打开或部署一次后同步到服务器</div>
              <div v-if="row.removed" class="warn">配置里已经没有了</div>
            </template>
          </el-table-column>
          <el-table-column label="现有" width="90">
            <template #default="{ row }">{{ row.count }} 份<div class="muted">{{ size(row.bytes) }}</div></template>
          </el-table-column>
          <el-table-column width="96">
            <template #default="{ row }">
              <el-button size="small" :loading="busy === 'run:' + row.name" :disabled="!!busy" @click="runNow(row.name)">现在备份</el-button>
            </template>
          </el-table-column>
        </el-table>

        <div class="files-head">
          <h4>备份文件</h4>
          <el-select v-model="filter" size="small" clearable placeholder="全部" style="width: 140px">
            <el-option v-for="it in status.items" :key="it.name" :label="it.name" :value="it.name" />
          </el-select>
        </div>
        <el-table :data="shown" size="small" max-height="260" empty-text="还没有备份文件">
          <el-table-column label="文件" prop="name" min-width="250" />
          <el-table-column label="大小" width="90">
            <template #default="{ row }">{{ size(row.size) }}</template>
          </el-table-column>
          <el-table-column label="时间" width="110">
            <template #default="{ row }">{{ when(row.time) }}</template>
          </el-table-column>
          <el-table-column width="190">
            <template #default="{ row }">
              <el-button link type="primary" size="small" :loading="busy === 'download:' + row.name" :disabled="!!busy" @click="download(row)">下载</el-button>
              <el-button link type="warning" size="small" :loading="busy === 'restore:' + row.name" :disabled="!!busy" @click="doRestore(row)">恢复</el-button>
              <el-button link type="danger" size="small" :loading="busy === 'delete:' + row.name" :disabled="!!busy" @click="remove(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </template>
      <el-progress v-if="progress" :percentage="progress.pct" class="block" />
      <pre v-if="log.length" class="log">{{ log.join('\n') }}</pre>
    </template>
    <template #footer>
      <el-button :disabled="!!busy" @click="load">刷新</el-button>
      <el-button v-if="status?.items.length" type="primary" :loading="busy === 'run:'" :disabled="!!busy" @click="runNow()">全部现在备份</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.state {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 10px;
}
.files-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin: 14px 0 6px;
}
.files-head h4 {
  margin: 0;
}
.example,
.log {
  background: var(--el-fill-color-light);
  padding: 8px 10px;
  border-radius: 6px;
  font-size: 12px;
  white-space: pre-wrap;
  max-height: 200px;
  overflow: auto;
}
.ok {
  color: var(--el-color-success);
}
.bad {
  color: var(--el-color-danger);
}
.warn {
  color: var(--el-color-warning);
}
.err {
  font-size: 12px;
  word-break: break-all;
}
.muted {
  color: var(--el-text-color-secondary);
}
.small {
  font-size: 13px;
}
.block {
  margin: 8px 0;
}
</style>
