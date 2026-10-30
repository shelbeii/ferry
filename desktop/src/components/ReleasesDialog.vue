<script setup lang="ts">
import LoadingBlock from './LoadingBlock.vue'
import { computed, onMounted, ref } from 'vue'
import { bytes, call, shortCommit, time, type AppReleases, type ReleaseInfo } from '../ferry'
import { startTask } from '../state'
import { previousRelease } from '../deploymentReview'

const props = defineProps<{ config: string; target: string; app: string; confirm: boolean; production?: boolean }>()
const emit = defineEmits<{ close: []; finished: []; logs: [release: string] }>()

const visible = ref(true)
const loading = ref(true)
const error = ref('')
const data = ref<AppReleases>()

async function load() {
  loading.value = true
  error.value = ''
  try {
    const all = await call<AppReleases[]>(['releases', props.target, props.app, '-c', props.config])
    data.value = all[0]
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(load)

const choice = ref<{ to: string; label: string; release?: ReleaseInfo }>()
const prodInput = ref('')
const starting = ref(false)
const current = computed(() => data.value?.releases?.find((r) => r.current))
const previous = computed(() => previousRelease(data.value?.releases ?? []))
const query = ref('')
const visibleReleases = computed(() => (data.value?.releases ?? []).filter((r) => `${r.id} ${r.commit ?? ''} ${r.branch ?? ''} ${r.deployedBy ?? ''}`.toLowerCase().includes(query.value.trim().toLowerCase())))
function review(to: string, label: string, release?: ReleaseInfo) {
  choice.value = { to, label, release }
  prodInput.value = ''
}
function rollback() {
  if (!choice.value || starting.value || (props.production && prodInput.value.trim() !== props.target)) return
  starting.value = true
  const { to, label } = choice.value
  const args = ['rollback', props.target, props.app, '-c', props.config, '--yes']
  if (to) args.push('--to', to)
  startTask(`回滚 ${props.app}@${props.target} → ${label}`, args, () => emit('finished'))
  visible.value = false
}

// 没有任何版本记录提交 / 分支时（配置目录不是 git 仓库）不显示这两列
const hasCommit = computed(() => (data.value?.releases ?? []).some((r) => r.commit))
const hasBranch = computed(() => (data.value?.releases ?? []).some((r) => r.branch))
const hasFiles = computed(() => (data.value?.releases ?? []).some((r) => r.files?.length))

function rowClass({ row }: { row: ReleaseInfo }) {
  return row.current ? 'current-row' : ''
}
</script>

<template>
  <el-dialog v-model="visible" :title="`${app} 的版本（${target}）`" width="min(1120px, 94vw)" top="6vh" @closed="emit('close')">
    <!-- 加载前后同样的最小高度：对话框不会先撑大再缩小 -->
    <div class="content">
      <LoadingBlock v-if="loading" :text="`正在连接 ${target}，读取 ${app} 的版本…`" :rows="6" min-height="300px" />
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon><el-button link type="primary" @click="load">重新读取</el-button></el-alert>
      <div v-if="choice" class="rollback-review">
        <el-tag v-if="production" type="danger" effect="plain">生产服务器</el-tag>
        <h3>确认回滚 {{ app }} · {{ target }}</h3>
        <div class="rollback-flow"><div><span>当前版本</span><b class="mono">{{ current?.id || '未读取到当前版本' }}</b></div><span>→</span><div><span>回滚目标</span><b class="mono">{{ choice.label }}</b></div></div>
        <p v-if="choice.release" class="muted small">{{ time(choice.release.deployedAt) }} · {{ choice.release.deployedBy || '未记录部署人' }} · {{ shortCommit(choice.release.commit, choice.release.dirty) }}</p>
        <el-alert :type="choice.release?.failed ? 'error' : 'warning'" :closable="false" show-icon :title="choice.release?.failed ? '这个版本有失败记录，回滚后可能仍无法正常运行。请先查看它的运行日志。' : '回滚会切换并重新启动该应用；应用数据和数据库不会随版本自动恢复。'" />
        <div v-if="production" class="production-confirm"><p>输入服务器名 <b class="mono">{{ target }}</b> 确认本次回滚：</p><el-input v-model="prodInput" :placeholder="target" aria-label="确认回滚的生产服务器名" /></div>
      </div>
      <template v-else-if="data && !loading">
        <div class="toolbar">
          <el-input v-model="query" placeholder="搜索版本、提交或部署人" clearable aria-label="搜索版本" class="release-search" />
          <el-button type="warning" plain :disabled="!previous && !data.legacy" @click="previous ? review(previous.id, previous.id, previous) : review('legacy', '接管前的服务')">回滚到上一个可用版本</el-button>
          <el-button v-if="data.legacy && previous" plain @click="review('legacy', '接管前的服务')">回到接管前的服务</el-button>
        </div>
        <!-- 单元格不换行：版本号与标记在同一行，放不下的文字省略并在悬停时显示全文 -->
        <el-table :data="visibleReleases" size="small" border :row-class-name="rowClass" :empty-text="query.trim() ? '没有匹配的版本' : '还没有版本'" class="releases">
          <el-table-column label="版本" min-width="250">
            <template #default="{ row }">
              <span class="ver">
                <span class="mono">{{ row.id }}</span>
                <el-tag v-if="row.current" size="small" type="success" disable-transitions>当前</el-tag>
                <el-tag v-if="row.failed" size="small" type="danger" disable-transitions>失败</el-tag>
              </span>
            </template>
          </el-table-column>
          <el-table-column v-if="hasCommit" label="提交" width="100" :show-overflow-tooltip="{ showAfter: HOVER_DELAY }">
            <template #default="{ row }"><span class="mono">{{ shortCommit(row.commit, row.dirty) }}</span></template>
          </el-table-column>
          <el-table-column v-if="hasBranch" prop="branch" label="分支" width="110" :show-overflow-tooltip="{ showAfter: HOVER_DELAY }" />
          <el-table-column label="部署时间" width="170">
            <template #default="{ row }">{{ time(row.deployedAt) }}</template>
          </el-table-column>
          <el-table-column prop="deployedBy" label="部署人" min-width="130" :show-overflow-tooltip="{ showAfter: HOVER_DELAY }" />
          <el-table-column label="大小" width="90" align="right">
            <template #default="{ row }">{{ bytes(row.size) }}</template>
          </el-table-column>
          <el-table-column v-if="hasFiles" label="附带文件" min-width="140" :show-overflow-tooltip="{ showAfter: HOVER_DELAY }">
            <template #default="{ row }"><span class="mono">{{ row.files?.join('、') || '-' }}</span></template>
          </el-table-column>
          <el-table-column label="运行日志" width="110">
            <template #default="{ row }">
              <el-button v-if="row.current || row.logRuns" link type="primary" @click="emit('logs', row.id)">
                {{ row.current ? '查看' : `${row.logRuns} 次运行` }}
              </el-button>
              <span v-else class="muted">-</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="100" fixed="right">
            <template #default="{ row }">
              <el-button v-if="!row.current" link :type="row.failed ? 'warning' : 'primary'" @click="review(row.id, row.id, row)">审阅回滚</el-button>
            </template>
          </el-table-column>
        </el-table>
      </template>
    </div>
    <template #footer>
      <template v-if="choice"><el-button :disabled="starting" @click="choice = undefined">返回版本列表</el-button><el-button :type="production || choice.release?.failed ? 'danger' : 'warning'" :loading="starting" :disabled="production && prodInput.trim() !== target" @click="rollback">确认回滚</el-button></template>
      <el-button v-else @click="visible = false">关闭</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.content {
  min-height: 300px;
}
.toolbar {
  margin-bottom: 10px;
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.toolbar .el-button { margin-left: 0; }
.release-search { flex: 1; min-width: 220px; }
.rollback-review { display: flex; flex-direction: column; align-items: flex-start; gap: 14px; }
.rollback-review h3, .rollback-review p { margin: 0; }
.rollback-flow { display: flex; align-items: center; gap: 24px; width: 100%; padding: 20px; box-sizing: border-box; border: 1px solid var(--ferry-stroke); border-radius: 10px; background: var(--el-fill-color-light); }
.rollback-flow > div { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 10px; overflow-wrap: anywhere; }
.rollback-flow span, .small, .production-confirm { font-size: 12px; color: var(--el-text-color-secondary); }
.production-confirm { width: min(420px, 100%); }
.production-confirm p { margin-bottom: 8px; color: var(--el-color-danger); }
.ver {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  white-space: nowrap;
}
.releases :deep(.cell) {
  white-space: nowrap;
}
:deep(.current-row) {
  background: var(--el-color-success-light-9);
}
</style>
