<script setup lang="ts">
import LoadingBlock from './LoadingBlock.vue'
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Download } from '@element-plus/icons-vue'
import { AUDIT_GROUPS, auditKindText, time, type AuditEntry } from '../ferry'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
import { pickSaveFile, reveal } from '../platform'
import { PAGE_SIZES, usePager } from '../pager'
import type { TableInstance } from 'element-plus'

const props = defineProps<{ config: string; target: string }>()
const emit = defineEmits<{ close: [] }>()

const visible = ref(true)
const loading = ref(true)
const error = ref('')
const rows = ref<AuditEntry[]>([])
const reads = useReadRequests()

// 服务器上的：团队里每个人在这台服务器上做的（服务器上的 Ferry 组件记的）；这台电脑上的：只有自己做的，连不上服务器时也能看
const where = ref<'server' | 'local'>('server')
const range = ref('7d')
const ranges = [
  { value: '24h', label: '最近 24 小时' },
  { value: '7d', label: '最近 7 天' },
  { value: '30d', label: '最近 30 天' },
  { value: '', label: '全部' },
]
const who = ref('')
const kind = ref('')
const query = ref('')

const LIMIT = 1000
function args(): string[] {
  const a = ['audit', props.target, '-c', props.config, '--limit', String(LIMIT)]
  if (range.value) a.push('--since', range.value)
  if (where.value === 'local') a.push('--local')
  return a
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    rows.value = (await reads.run('audit', (s) => readCall<AuditEntry[] | null>(args(), s))) ?? []
  } catch (e) {
    if (e instanceof ReadCanceled) return
    rows.value = []
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(load)
watch([where, range], load)

const actors = computed(() => [...new Set(rows.value.map((r) => r.actor).filter(Boolean))].sort())
// 类型：选一组（group:<组名>）或其中一种
const kindOk = (r: AuditEntry) => {
  if (!kind.value) return true
  if (kind.value.startsWith('group:')) {
    const g = AUDIT_GROUPS.find((x) => x.label === kind.value.slice(6))
    return !!g && r.kind in g.kinds
  }
  return r.kind === kind.value
}
const shown = computed(() => {
  const q = query.value.trim().toLowerCase()
  return rows.value.filter(
    (r) => (!who.value || r.actor === who.value) && kindOk(r) && (!q || `${r.object ?? ''} ${r.detail ?? ''} ${r.error ?? ''}`.toLowerCase().includes(q)),
  )
})

// 一页 50 条：最多读 1000 条，一次全生成成表格要好几百毫秒，打开、换筛选条件时界面会卡住
const table = ref<TableInstance>()
const { page, pageSize, total, paged } = usePager(shown, [rows, who, kind, query], table)

async function exportCsv() {
  const out = await pickSaveFile('导出审计记录', `审计记录-${props.target}.csv`, 'CSV 表格', ['csv'])
  if (!out) return
  try {
    const r = await readCall<{ path: string; count: number }>([...args(), '--out', out], new AbortController().signal)
    ElMessage.success({ message: `已导出 ${r.count} 条到 ${r.path}`, duration: 4000 })
    void reveal(r.path)
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}
</script>

<template>
  <el-dialog v-model="visible" :title="`审计记录（${target}）`" width="1080px" append-to-body @closed="emit('close')">
    <div class="audit">
      <div class="filters">
        <el-radio-group v-model="where" size="small">
          <el-radio-button value="server">服务器上（团队）</el-radio-button>
          <el-radio-button value="local">这台电脑上</el-radio-button>
        </el-radio-group>
        <el-select v-model="range" size="small" style="width: 130px" aria-label="时间范围">
          <el-option v-for="r in ranges" :key="r.value" :label="r.label" :value="r.value" />
        </el-select>
        <el-select v-model="who" size="small" clearable placeholder="全部操作人" style="width: 210px" aria-label="操作人">
          <el-option v-for="a in actors" :key="a" :label="a" :value="a" />
        </el-select>
        <el-select v-model="kind" size="small" clearable filterable placeholder="全部操作" style="width: 170px" aria-label="操作类型">
          <el-option-group v-for="g in AUDIT_GROUPS" :key="g.label" :label="g.label">
            <el-option :label="`${g.label}：全部`" :value="`group:${g.label}`" />
            <el-option v-for="(text, k) in g.kinds" :key="k" :label="text" :value="k" />
          </el-option-group>
        </el-select>
        <el-input v-model="query" size="small" clearable placeholder="搜索路径、SQL、软件……" style="width: 200px" aria-label="搜索" />
        <span class="spacer" />
        <el-button size="small" :icon="Download" :disabled="loading || !rows.length" @click="exportCsv">导出 CSV</el-button>
      </div>
      <p class="muted small note">
        <template v-if="where === 'server'">记在服务器上（{{ target }} 的 Ferry 组件目录下的 audit.jsonl，同时写进系统日志 <code>journalctl -t ferry-audit</code>），团队里每个人的改动都在这里。</template>
        <template v-else>记在这台电脑上，只有在这台电脑上做的。</template>
        终端只记开、关，不记里面敲的命令；文件只记路径和大小，不记内容；数据库只记执行的语句，不记查出来的数据。
        <span v-if="!loading">（{{ shown.length }} 条{{ rows.length >= LIMIT ? `，只读了最近 ${LIMIT} 条` : '' }}）</span>
      </p>
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon>
        <el-button link type="primary" @click="load">重新读取</el-button>
        <el-button v-if="where === 'server'" link type="primary" @click="where = 'local'">看这台电脑上记的</el-button>
      </el-alert>
      <LoadingBlock v-if="loading" :text="where === 'server' ? `正在连接 ${target}，读取审计记录…` : '正在读取…'" :rows="10" min-height="56vh" />
      <el-table v-else-if="!error" ref="table" :data="paged" size="small" border height="52vh" empty-text="这段时间没有记录">
        <el-table-column type="expand" width="36">
          <template #default="{ row }">
            <div class="detail">
              <div v-if="row.detail" class="line"><span>细节</span><pre>{{ row.detail }}</pre></div>
              <div v-if="row.error" class="line"><span>失败原因</span>{{ row.error }}</div>
              <div class="line"><span>项目 / 服务器</span>{{ row.project || '—' }} / {{ row.target || target }}<template v-if="row.host">（{{ row.host }}）</template></div>
              <div v-if="row.session" class="line"><span>会话</span><code>{{ row.session }}</code></div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="时间" width="160">
          <template #default="{ row }">{{ time(row.time) }}</template>
        </el-table-column>
        <el-table-column prop="actor" label="操作人" width="200" show-overflow-tooltip />
        <el-table-column label="做了什么" width="120">
          <template #default="{ row }">{{ auditKindText(row.kind) }}</template>
        </el-table-column>
        <el-table-column label="结果" width="70">
          <template #default="{ row }">
            <el-tag :type="row.result === 'failed' ? 'danger' : 'success'" size="small">{{ row.result === 'failed' ? '失败' : '成功' }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="对象" min-width="220" show-overflow-tooltip>
          <template #default="{ row }"><span class="mono">{{ row.object || '—' }}</span></template>
        </el-table-column>
        <el-table-column label="细节" min-width="200" show-overflow-tooltip>
          <template #default="{ row }"><span class="mono">{{ row.detail || '' }}</span></template>
        </el-table-column>
      </el-table>
      <el-pagination
        v-if="!loading && !error && total > PAGE_SIZES[0]"
        v-model:current-page="page"
        v-model:page-size="pageSize"
        :page-sizes="PAGE_SIZES"
        :total="total"
        size="small"
        background
        layout="total, sizes, prev, pager, next"
        class="pager"
      />
    </div>
  </el-dialog>
</template>

<style scoped>
.filters {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.pager {
  justify-content: flex-end;
  margin-top: 10px;
}
.spacer {
  flex: 1;
}
.note {
  margin: 8px 0;
  line-height: 1.6;
}
.detail {
  padding: 4px 24px 12px;
  font-size: 12px;
}
.detail .line {
  display: flex;
  gap: 18px;
  overflow-wrap: anywhere;
  margin: 4px 0;
}
.detail .line > span {
  min-width: 84px;
  flex: none;
  color: var(--el-text-color-secondary);
}
.detail pre {
  margin: 0;
  white-space: pre-wrap;
  font-family: var(--el-font-family-mono, monospace);
}
.muted {
  color: var(--el-text-color-secondary);
}
.small {
  font-size: 12px;
}
.mono {
  font-family: var(--el-font-family-mono, monospace);
}
</style>
