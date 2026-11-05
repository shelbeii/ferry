<script setup lang="ts">
// 把 CSV 导进一张表：选文件、看前几行、对列（文件的哪一列进表的哪一列），在一个事务里导入，有一行失败全部撤回。
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Document } from '@element-plus/icons-vue'
import { call } from '../../ferry'
import { dbCall, type DbConn, type DbTableInfo } from '../../db'
import { pickFile } from '../../platform'
import { startTask } from '../../state'

const props = defineProps<{ target: { conn: DbConn; db: string; schema?: string; table: string } | null }>()
const emit = defineEmits<{ close: []; done: [] }>()

const file = ref('')
const delimiter = ref('auto')
const encoding = ref('auto')
const val = (v: string) => (v === 'auto' ? '' : v) // 自动 = 不传
const delimName: Record<string, string> = { ',': '逗号', '\t': '制表符', ';': '分号', '|': '竖线' }
const header = ref(true)
const emptyNull = ref(true)
const preview = ref<{ delimiter: string; encoding: string; rows: string[][]; size: number } | null>(null)
const columns = ref<string[]>([]) // 表里能导入的列（去掉计算列）
const mapping = ref<string[]>([]) // 文件第 i 列 → 表的列；'' = 不导
const error = ref('')
const loading = ref(false)

watch(() => props.target, async (t) => {
  file.value = ''
  preview.value = null
  error.value = ''
  mapping.value = []
  columns.value = []
  if (!t) return
  try {
    const info = await dbCall<DbTableInfo>(t.conn, 'table', t.db, { schema: t.schema, table: t.table })
    columns.value = info.columns.filter((c) => !c.generated).map((c) => c.name)
  } catch (e) {
    error.value = (e as Error).message
  }
}, { immediate: true })

const width = computed(() => Math.max(0, ...(preview.value?.rows ?? []).map((r) => r.length)))
const body = computed(() => (preview.value?.rows ?? []).slice(header.value ? 1 : 0, 11))
const picked = computed(() => mapping.value.filter(Boolean))
const dup = computed(() => picked.value.find((c, i) => picked.value.indexOf(c) !== i) ?? '')

async function choose() {
  const f = await pickFile('选择 CSV 文件', 'CSV 文件', ['csv', 'tsv', 'txt'])
  if (!f) return
  file.value = f
  delimiter.value = 'auto'
  encoding.value = 'auto'
  await load(true)
}

// 读前几行；remap 为 true 时按表头（或者顺序）重新对列
async function load(remap: boolean) {
  if (!file.value) return
  loading.value = true
  error.value = ''
  try {
    preview.value = await call(['db', 'csv-preview'], { FERRY_DB_REQ: JSON.stringify({ csv: { file: file.value, delimiter: val(delimiter.value), encoding: val(encoding.value) } }) })
    if (remap) autoMap()
  } catch (e) {
    preview.value = null
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

function autoMap() {
  const first = preview.value?.rows[0] ?? []
  const lower = new Map(columns.value.map((c) => [c.toLowerCase(), c]))
  const byName = first.map((h) => lower.get(h.trim().toLowerCase()) ?? '')
  // 表头能对上一半以上的列：当作有表头、按名字对；否则按顺序对
  if (byName.filter(Boolean).length >= Math.max(1, Math.ceil(first.length / 2))) {
    header.value = true
    mapping.value = byName
  } else {
    header.value = false
    mapping.value = Array.from({ length: width.value }, (_, i) => columns.value[i] ?? '')
  }
}

function run() {
  const t = props.target
  if (!t || !file.value) return
  if (!picked.value.length) return void (error.value = '至少选一列要导入的')
  if (dup.value) return void (error.value = `列 ${dup.value} 选了两次`)
  const csv = {
    file: file.value, delimiter: val(delimiter.value) || preview.value?.delimiter || '', encoding: val(encoding.value) || preview.value?.encoding || '',
    header: header.value, schema: t.schema, table: t.table, columns: mapping.value, emptyNull: emptyNull.value,
  }
  startTask(`导入 CSV 到 ${t.conn.name} / ${t.db} / ${t.table}`, ['db', 'import-csv', t.conn.id, t.db], (task) => {
    if (task.status === 'ok') {
      ElMessage.success({ message: `已导入到 ${t.table}`, duration: 3000 })
      emit('done')
    }
  }, { FERRY_DB_REQ: JSON.stringify({ csv }) })
  emit('close')
}
</script>

<template>
  <el-dialog :model-value="!!target" :title="target ? `导入 CSV 到 ${target.table}` : ''" width="780px" append-to-body :close-on-click-modal="false" @close="emit('close')">
    <template v-if="target">
      <div class="row">
        <el-button :icon="Document" @click="choose">选择文件</el-button>
        <span class="file mono" :title="file">{{ file || '还没选文件' }}</span>
      </div>
      <div v-if="preview" class="row opts">
        <span>分隔符</span>
        <el-select v-model="delimiter" size="small" class="sel" @change="load(false)">
          <el-option value="auto" :label="`自动（${delimName[preview.delimiter] ?? preview.delimiter}）`" />
          <el-option value="," label="逗号 ," />
          <el-option value="\t" label="制表符" />
          <el-option value=";" label="分号 ;" />
          <el-option value="|" label="竖线 |" />
        </el-select>
        <span>编码</span>
        <el-select v-model="encoding" size="small" class="sel" @change="load(false)">
          <el-option value="auto" :label="`自动（${preview.encoding === 'gbk' ? 'GBK' : 'UTF-8'}）`" />
          <el-option value="utf-8" label="UTF-8" />
          <el-option value="gbk" label="GBK（Excel 中文版另存的）" />
        </el-select>
        <el-checkbox v-model="header">第一行是列名</el-checkbox>
        <el-checkbox v-model="emptyNull">空的格子写 NULL</el-checkbox>
      </div>
      <div v-if="preview" class="grid">
        <table>
          <thead>
            <tr>
              <th v-for="i in width" :key="i">
                <el-select v-model="mapping[i - 1]" size="small" placeholder="不导入" clearable class="map" :class="{ off: !mapping[i - 1] }">
                  <el-option v-for="c in columns" :key="c" :value="c" :label="c" />
                </el-select>
                <div v-if="header" class="muted small head">{{ preview.rows[0]?.[i - 1] }}</div>
              </th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(r, k) in body" :key="k">
              <td v-for="i in width" :key="i" :class="{ off: !mapping[i - 1] }">{{ r[i - 1] ?? '' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-if="preview" class="muted small">
        上面是文件的前几行；每一列上面选导进表的哪一列（不选 = 不导入）。在一个事务里导入，有一行失败全部撤回；表里原来的数据不动。
      </p>
      <el-alert v-if="error" type="error" :closable="false" :title="error" class="msg" />
    </template>
    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button type="primary" :disabled="!preview || !picked.length" :loading="loading" @click="run">导入</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.opts {
  margin-top: 12px;
  font-size: 13px;
}
.sel {
  width: 190px;
}
.file {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--el-text-color-secondary);
}
.grid {
  margin: 12px 0 6px;
  max-height: 340px;
  overflow: auto;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
}
table {
  border-collapse: collapse;
  font-size: 12.5px;
}
th,
td {
  border-bottom: 1px solid var(--el-border-color-lighter);
  border-right: 1px solid var(--el-border-color-lighter);
  padding: 4px 8px;
  text-align: left;
  white-space: nowrap;
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
}
th {
  position: sticky;
  top: 0;
  background: var(--el-bg-color);
  min-width: 140px;
  vertical-align: top;
}
.map {
  width: 100%;
}
.head {
  margin-top: 4px;
}
td.off {
  color: var(--el-text-color-placeholder);
}
.msg {
  margin-top: 8px;
}
</style>
