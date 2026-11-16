<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import { ElMessage } from 'element-plus'
import { dbRun, type DbConn, type MongoResult } from '../../db'
import { pickSaveFile, reveal } from '../../platform'
import { startTask } from '../../state'

const props = defineProps<{ conn: DbConn; db: string; collection: string }>()
const draft = ref('')
const filter = ref('')
const offsets = ref([0])
const result = shallowRef<MongoResult>()
const busy = ref(false)
const error = ref('')
let cancel: (() => void) | undefined

async function load(nextFilter = filter.value, path = offsets.value) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  const r = dbRun<MongoResult>(props.conn, 'mongo-find', props.db, {
    table: props.collection, mongo: { filter: nextFilter, skip: path.at(-1), limit: 25 },
  })
  cancel = r.cancel
  try {
    result.value = await r.done
    filter.value = nextFilter
    offsets.value = path
  } catch (err) {
    error.value = (err as Error).message
  } finally {
    cancel = undefined
    busy.value = false
  }
}

function next() {
  const n = result.value?.documents.length ?? 0
  if (n) void load(filter.value, [...offsets.value, offsets.value.at(-1)! + n])
}

function stop() {
  cancel?.()
}

async function exportJSON() {
  const currentFilter = filter.value
  const name = props.collection.replace(/[<>:"/\\|?*]/g, '_')
  const out = await pickSaveFile('导出 MongoDB 文档', `${name}.json`, 'Extended JSON', ['json'])
  if (!out) return
  startTask(`导出 ${props.db} / ${props.collection}`, ['db', 'mongo-export', props.conn.id, props.db], (t) => {
    if (t.status === 'ok') {
      ElMessage.success({ message: `已导出到 ${out}`, duration: 4000 })
      void reveal(out)
    }
  }, { FERRY_DB_REQ: JSON.stringify({ table: props.collection, mongo: { filter: currentFilter }, out }) })
}

onMounted(() => void load())
onBeforeUnmount(() => cancel?.())
defineExpose({ reload: () => load() })
</script>

<template>
  <div class="documents">
    <div class="filter">
      <el-input v-model="draft" type="textarea" :rows="2" aria-label="MongoDB 文档过滤条件" placeholder='过滤文档，例如 {"status": "active"}' @keydown.ctrl.enter.prevent="load(draft, [0])" />
      <el-button size="small" type="primary" :loading="busy" @click="load(draft, [0])">查询</el-button>
      <el-button v-if="busy" size="small" @click="stop">停止</el-button>
      <el-button size="small" :disabled="busy || !result || filter !== draft" @click="exportJSON">导出匹配文档</el-button>
    </div>
    <el-alert v-if="error" type="error" :closable="false" :title="error" />
    <div v-loading="busy" class="content">
      <el-empty v-if="result && !result.documents.length" description="没有匹配的文档" :image-size="60" />
      <article v-for="(d, i) in result?.documents" :key="i" class="doc">
        <pre>{{ d.json }}</pre>
        <span v-if="d.cut" class="cut">预览已截断，导出包含完整文档。</span>
      </article>
    </div>
    <div v-if="result" class="paging">
      <el-button size="small" :disabled="busy || offsets.length <= 1" @click="load(filter, offsets.slice(0, -1))">上一页</el-button>
      <span>第 {{ offsets.length }} 页 · {{ result.documents.length }} 个文档</span>
      <el-button size="small" :disabled="busy || !result.more" @click="next">下一页</el-button>
    </div>
  </div>
</template>

<style scoped>
.documents { height: 100%; display: flex; flex-direction: column; min-height: 0; }
.filter, .paging { display: flex; align-items: center; gap: 8px; padding: 8px 12px; }
.filter :deep(.el-textarea__inner), pre { font-family: 'Cascadia Mono', Consolas, monospace; font-size: 12px; }
.filter :deep(.el-button + .el-button), .paging :deep(.el-button + .el-button) { margin-left: 0; }
.content { flex: 1; min-height: 0; overflow: auto; padding: 0 12px; }
.doc { border: 1px solid var(--el-border-color-lighter); border-radius: 6px; margin-bottom: 8px; background: var(--el-fill-color-lighter); }
pre { margin: 0; padding: 10px 12px; white-space: pre-wrap; overflow-wrap: anywhere; line-height: 1.5; }
.cut, .paging { color: var(--el-text-color-secondary); font-size: 12px; }
.cut { display: block; padding: 0 12px 8px; }
.paging { border-top: 1px solid var(--el-border-color-lighter); }
</style>
