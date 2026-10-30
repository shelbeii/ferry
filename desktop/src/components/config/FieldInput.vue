<script setup lang="ts">
// 表单里一个配置的输入框：按类型用文字框、下拉、是 / 否、列表、键值对。
// 文字在离开输入框或回车时才写回（打字过程中不改配置）；清空 = 删除这项（用默认值）。
import { computed, ref, watch } from 'vue'
import { Delete, Plus } from '@element-plus/icons-vue'
import type { SchemaNode } from '../../editor/schema'

type Scalar = string | number | boolean | null
export type FieldValue = Scalar | string[] | [string, string][] | undefined

const props = defineProps<{
  node: SchemaNode
  value: FieldValue
  disabled?: boolean
  /** 列表的候选值（应用名、服务器名……） */
  options?: string[]
}>()
const emit = defineEmits<{ commit: [value: FieldValue] }>()

const placeholder = computed(() => (props.node.default ? `不写：${props.node.default}` : props.node.required ? '必填' : '不写'))

// ---------------------------------------------------------------- 文字、数字、时长

const draft = ref('')
watch(
  () => props.value,
  (v) => (draft.value = v == null || Array.isArray(v) ? '' : String(v)),
  { immediate: true },
)

function toNumber(s: string): number | string {
  return /^-?\d+(\.\d+)?$/.test(s) ? Number(s) : s
}

function commitText() {
  const s = draft.value.trim()
  const cur = props.value == null || Array.isArray(props.value) ? '' : String(props.value)
  if (s === cur) return
  if (!s) return emit('commit', undefined)
  if (props.node.kind === 'num') return emit('commit', toNumber(s))
  // 时长只写数字表示秒，按数字写（不加引号）
  if (props.node.kind === 'dur' && /^\d+$/.test(s)) return emit('commit', Number(s))
  emit('commit', s)
}

const numError = computed(() => props.node.kind === 'num' && draft.value.trim() !== '' && !/^-?\d+$/.test(draft.value.trim()) && !draft.value.includes('${'))

// ---------------------------------------------------------------- 下拉

const selectValue = computed(() => (props.value == null || Array.isArray(props.value) ? '' : String(props.value)))

function commitSelect(v: string | number | undefined) {
  if (v === '' || v == null) return emit('commit', undefined)
  emit('commit', props.node.kind === 'num' ? toNumber(String(v)) : v)
}

// ---------------------------------------------------------------- 是 / 否

const boolValue = computed(() => (props.value === true || props.value === 'true' ? 'yes' : props.value === false || props.value === 'false' ? 'no' : 'unset'))

function commitBool(v: string | number | boolean | undefined) {
  emit('commit', v === 'yes' ? true : v === 'no' ? false : undefined)
}

// ---------------------------------------------------------------- 列表

const rows = ref<string[]>([])
watch(
  () => props.value,
  (v) => {
    const list = Array.isArray(v) ? (v as unknown[]).map((x) => (Array.isArray(x) ? '' : String(x))) : v != null && v !== '' ? [String(v)] : []
    // 正在编辑的空行不丢
    if (rows.value.filter((r) => r.trim()).join('\n') !== list.join('\n')) rows.value = list
  },
  { immediate: true },
)

function commitRows() {
  const vals = rows.value.map((r) => r.trim()).filter(Boolean)
  emit('commit', vals.length ? (props.node.numbers ? vals.map(toNumber) : vals) as string[] : undefined)
}

function addRow() {
  rows.value.push('')
}

function delRow(i: number) {
  rows.value.splice(i, 1)
  commitRows()
}

// 有候选值的列表（只部署这些应用、只在这些服务器上）用多选
const multi = computed(() => (Array.isArray(props.value) ? (props.value as string[]) : props.value ? [String(props.value)] : []))

function commitMulti(v: string[]) {
  emit('commit', v.length ? v : undefined)
}

// ---------------------------------------------------------------- 键值对

const pairs = ref<{ k: string; v: string }[]>([])
watch(
  () => props.value,
  (v) => {
    const list = Array.isArray(v) ? (v as [string, string][]).filter((p) => Array.isArray(p)) : []
    const cur = pairs.value.filter((p) => p.k.trim()).map((p) => p.k + '\0' + p.v)
    if (cur.join('\n') !== list.map(([k, val]) => k + '\0' + val).join('\n')) pairs.value = list.map(([k, val]) => ({ k, v: val }))
  },
  { immediate: true },
)

function commitPairs() {
  const list = pairs.value.filter((p) => p.k.trim()).map((p) => [p.k.trim(), p.v] as [string, string])
  emit('commit', list.length ? list : undefined)
}

function delPair(i: number) {
  pairs.value.splice(i, 1)
  commitPairs()
}
</script>

<template>
  <!-- 是 / 否 -->
  <el-radio-group v-if="node.kind === 'bool'" :model-value="boolValue" size="small" :disabled="disabled" @update:model-value="commitBool">
    <el-radio-button value="unset">不写{{ node.default ? `（${node.default}）` : '' }}</el-radio-button>
    <el-radio-button value="yes">是</el-radio-button>
    <el-radio-button value="no">否</el-radio-button>
  </el-radio-group>

  <!-- 下拉 -->
  <el-select
    v-else-if="node.values && (node.kind === 'str' || node.kind === 'num')"
    :model-value="selectValue"
    :placeholder="placeholder"
    :disabled="disabled"
    :filterable="node.open"
    :allow-create="node.open"
    clearable
    size="small"
    class="ctl-select"
    @update:model-value="commitSelect"
  >
    <el-option v-for="v in node.values" :key="v" :label="v" :value="v" />
  </el-select>

  <!-- 文字、数字、时长 -->
  <div v-else-if="node.kind === 'str' || node.kind === 'num' || node.kind === 'dur'" class="ctl-text">
    <el-input v-model="draft" :placeholder="placeholder" :disabled="disabled" clearable size="small" @change="commitText" />
    <span v-if="numError" class="ev-error small">要写数字</span>
  </div>

  <!-- 列表：有候选值时多选 -->
  <el-select
    v-else-if="node.kind === 'list' && options"
    :model-value="multi"
    :placeholder="placeholder"
    :disabled="disabled"
    multiple
    filterable
    allow-create
    default-first-option
    size="small"
    class="ctl-multi"
    @update:model-value="commitMulti"
  >
    <el-option v-for="v in options" :key="v" :label="v" :value="v" />
  </el-select>

  <!-- 列表 -->
  <div v-else-if="node.kind === 'list'" class="ctl-rows">
    <div v-for="(_, i) in rows" :key="i" class="ctl-row">
      <el-input v-model="rows[i]" size="small" :disabled="disabled" @change="commitRows" />
      <el-button :icon="Delete" size="small" text :disabled="disabled" aria-label="删除这一项" @click="delRow(i)" />
    </div>
    <el-button :icon="Plus" size="small" text type="primary" :disabled="disabled" @click="addRow">添加一项</el-button>
    <span v-if="!rows.length && node.default" class="muted small">不写：{{ node.default }}</span>
  </div>

  <!-- 键值对 -->
  <div v-else-if="node.kind === 'map'" class="ctl-rows">
    <div v-for="(p, i) in pairs" :key="i" class="ctl-row">
      <el-input v-model="p.k" size="small" placeholder="名字" class="ctl-key mono" :disabled="disabled" @change="commitPairs" />
      <el-input v-model="p.v" size="small" placeholder="值" :disabled="disabled" @change="commitPairs" />
      <el-button :icon="Delete" size="small" text :disabled="disabled" aria-label="删除这一项" @click="delPair(i)" />
    </div>
    <el-button :icon="Plus" size="small" text type="primary" :disabled="disabled" @click="pairs.push({ k: '', v: '' })">添加一项</el-button>
  </div>
</template>

<style scoped>
.ctl-select {
  width: 260px;
}
.ctl-multi {
  width: 100%;
  max-width: 520px;
}
.ctl-text {
  display: flex;
  align-items: center;
  gap: 8px;
  max-width: 520px;
}
.ctl-rows {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 4px;
  max-width: 520px;
}
.ctl-row {
  display: flex;
  gap: 4px;
  width: 100%;
}
.ctl-key {
  width: 40%;
  flex: none;
}
.small {
  font-size: 12px;
  white-space: nowrap;
}
</style>
