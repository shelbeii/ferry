<script setup lang="ts">
import LoadingBlock from './LoadingBlock.vue'
import { computed, onMounted, ref } from 'vue'
import { call, shortCommit, time, type HistoryEntry } from '../ferry'

const props = defineProps<{ config: string; target: string }>()
const emit = defineEmits<{ close: [] }>()

const visible = ref(true)
const loading = ref(true)
const error = ref('')
const rows = ref<HistoryEntry[]>([])
// 部署记录在服务器上：团队里每个人的部署、回滚、启停都在这里，可以按人、应用、操作筛
const who = ref('')
const app = ref('')
const action = ref('')
const uniq = (f: (r: HistoryEntry) => string | undefined) => [...new Set(rows.value.map(f).filter((v): v is string => !!v))].sort()
const actors = computed(() => uniq((r) => r.actor))
const apps = computed(() => uniq((r) => r.app))
const actions = computed(() => uniq((r) => r.action))
const shown = computed(() => rows.value.filter((r) => (!who.value || r.actor === who.value) && (!app.value || r.app === app.value) && (!action.value || r.action === action.value)))

onMounted(async () => {
  try {
    rows.value = (await call<HistoryEntry[] | null>(['history', props.target, '-c', props.config, '--limit', '200'])) ?? []
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
})

const actionText: Record<string, string> = { deploy: '部署', rollback: '回滚', start: '启动', stop: '停止', restart: '重启' }

function resultType(r: string): 'success' | 'danger' | 'warning' | 'info' {
  if (r === 'OK') return 'success'
  if (r === 'FAILED' || r === 'ROLLBACK_FAILED') return 'danger'
  if (r === 'ROLLED_BACK') return 'warning'
  return 'info'
}
</script>

<template>
  <el-dialog v-model="visible" :title="`部署历史（${target}）`" width="1000px" append-to-body @closed="emit('close')">
    <div>
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon />
      <LoadingBlock v-if="loading" :text="`正在连接 ${target}，读取部署历史…`" :rows="10" min-height="60vh" />
      <div v-if="!loading && rows.length" class="filters">
        <el-select v-model="who" size="small" clearable placeholder="全部操作人" style="width: 220px">
          <el-option v-for="a in actors" :key="a" :label="a" :value="a" />
        </el-select>
        <el-select v-model="app" size="small" clearable placeholder="全部应用" style="width: 150px">
          <el-option v-for="a in apps" :key="a" :label="a" :value="a" />
        </el-select>
        <el-select v-model="action" size="small" clearable placeholder="全部操作" style="width: 120px">
          <el-option v-for="a in actions" :key="a" :label="actionText[a] ?? a" :value="a" />
        </el-select>
        <span class="muted small">记录在服务器上，团队里每个人的操作都在这里（{{ shown.length }} 条）</span>
      </div>
      <el-table v-if="!loading" :data="shown" size="small" border height="56vh" empty-text="还没有记录">
        <el-table-column type="expand" width="36"><template #default="{ row }"><div class="history-detail"><p><span>完整版本</span><code>{{ row.release || '—' }}</code></p><p><span>提交</span><code>{{ row.commit || '—' }}</code></p><p><span>操作人</span>{{ row.actor || '—' }}</p><p v-if="row.msg"><span>说明</span>{{ row.msg }}</p></div></template></el-table-column>
        <el-table-column label="时间" width="165">
          <template #default="{ row }">{{ time(row.time) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="70">
          <template #default="{ row }">{{ actionText[row.action] ?? row.action }}</template>
        </el-table-column>
        <el-table-column prop="app" label="应用" width="120" />
        <el-table-column label="结果" width="130">
          <template #default="{ row }"><el-tag :type="resultType(row.result)" size="small">{{ row.result }}</el-tag></template>
        </el-table-column>
        <el-table-column label="提交" width="90">
          <template #default="{ row }"><span class="mono">{{ shortCommit(row.commit) }}</span></template>
        </el-table-column>
        <el-table-column prop="actor" label="操作人" min-width="140" show-overflow-tooltip />
        <!-- 说明：只有失败原因、「从某版本回滚」这类才有，成功的部署、启停没有；整列都空时不显示 -->
      </el-table>
    </div>
  </el-dialog>
</template>

<style scoped>
.history-detail { padding: 4px 24px 12px; font-size: 12px; }
.history-detail p { display: flex; gap: 18px; overflow-wrap: anywhere; }
.history-detail p > span { min-width: 60px; color: var(--el-text-color-secondary); }
.filters {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 8px;
}
.muted {
  color: var(--el-text-color-secondary);
}
.small {
  font-size: 12px;
}
</style>
