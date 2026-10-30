<script setup lang="ts">
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import { call, type BusyItem } from '../ferry'

// 部署前发现本机有程序正占着要构建的 jar（Windows 会锁住正在运行的文件，mvn clean 删不掉）：
// 列出这些进程，可以在这里结束它们，然后重新部署
const props = defineProps<{ config: string; target: string; items: BusyItem[] }>()
const emit = defineEmits<{ close: []; retry: [] }>()

const visible = ref(true)
const items = ref<BusyItem[]>(props.items)
const apps = computed(() => [...new Set(props.items.map((i) => i.app))])
const busy = ref(0) // 正在结束的进程号
const loading = ref(false)

interface Row {
  app: string
  path: string
  pid: number
  name: string
  cmd?: string
}
const rows = computed<Row[]>(() =>
  items.value.flatMap((it) =>
    it.holders.length ? it.holders.map((h) => ({ app: it.app, path: it.path, ...h })) : [{ app: it.app, path: it.path, pid: 0, name: '查不到是哪个程序' }],
  ),
)
const pids = computed(() => [...new Set(rows.value.filter((r) => r.pid).map((r) => r.pid))])

function args(extra: string[] = []) {
  return ['busy', props.target, ...apps.value, '-c', props.config, ...extra]
}

async function refresh() {
  loading.value = true
  try {
    items.value = await call<BusyItem[]>(args())
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

async function kill(pid: number, ask = true) {
  const r = rows.value.find((x) => x.pid === pid)
  if (ask) {
    try {
      await ElMessageBox.confirm(
        `结束进程 ${pid}（${r?.name ?? ''}）？它在本机运行的服务会停止，没保存的数据会丢失。`,
        '结束进程',
        { type: 'warning', confirmButtonText: '结束', cancelButtonText: '取消' },
      )
    } catch {
      return false
    }
  }
  busy.value = pid
  try {
    items.value = await call<BusyItem[]>(args(['--kill', String(pid)]))
    return true
  } catch (e) {
    ElMessage.error((e as Error).message)
    await refresh()
    return false
  } finally {
    busy.value = 0
  }
}

async function killAll() {
  const list = pids.value
  try {
    await ElMessageBox.confirm(
      `结束这 ${list.length} 个进程（${list.join('、')}）？它们在本机运行的服务会停止，没保存的数据会丢失。`,
      '全部结束',
      { type: 'warning', confirmButtonText: '全部结束', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  for (const pid of list) {
    if (!(await kill(pid, false))) break
  }
}

function retry() {
  visible.value = false
  emit('retry')
}
</script>

<template>
  <el-dialog v-model="visible" title="本机有程序占着要构建的 jar" width="860px" append-to-body @closed="emit('close')">
    <p class="intro">
      部署 <b>{{ target }}</b> 前要重新构建这些 jar，但它们正被本机的程序运行着（Windows 会锁住正在运行的文件，构建时删不掉）。
      停掉这些程序后再部署：可以在这里结束进程，也可以自己去关掉（如 IDEA 里运行的服务、命令行 java -jar 启动的）。
    </p>
    <el-table v-if="rows.length" :data="rows" border size="small" v-loading="loading">
      <el-table-column label="应用" prop="app" width="110" />
      <el-table-column label="进程" width="200">
        <template #default="{ row }">
          <div>{{ row.name }}</div>
          <div v-if="row.pid" class="muted mono">进程号 {{ row.pid }}</div>
        </template>
      </el-table-column>
      <el-table-column label="正在使用 / 命令行" min-width="300">
        <template #default="{ row }">
          <div class="mono small">{{ row.path }}</div>
          <el-tooltip v-if="row.cmd" :show-after="HOVER_DELAY" placement="top" popper-class="busy-cmd">
            <template #content><span class="mono">{{ row.cmd }}</span></template>
            <div class="mono small muted cmd">{{ row.cmd }}</div>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="110" align="center">
        <template #default="{ row }">
          <el-button v-if="row.pid" type="danger" size="small" plain :loading="busy === row.pid" :disabled="!!busy" @click="kill(row.pid)">
            结束进程
          </el-button>
        </template>
      </el-table-column>
    </el-table>
    <el-result v-else icon="success" title="已经没有程序占着了" sub-title="可以重新部署" />
    <template #footer>
      <el-button :icon="Refresh" :loading="loading" @click="refresh">重新检查</el-button>
      <el-button v-if="pids.length > 1" type="danger" plain :disabled="!!busy" @click="killAll">全部结束</el-button>
      <el-button @click="visible = false">关闭</el-button>
      <el-button type="primary" :disabled="rows.length > 0 || !!busy" @click="retry">重新部署</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.intro {
  margin: 0 0 12px;
  line-height: 1.7;
}
.small {
  font-size: 12px;
}
.cmd {
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>

<style>
.busy-cmd {
  max-width: 720px;
  word-break: break-all;
}
</style>
