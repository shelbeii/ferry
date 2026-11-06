<script setup lang="ts">
// 表单里一个配置的输入框：按类型用文字框、数字框、时长（数字 + 单位）、下拉、是 / 否、列表、键值对。
// 文字在离开输入框或回车时才写回（打字过程中不改配置）；清空 = 删除这项（用默认值）。
import { computed, ref, watch } from 'vue'
import { Delete, Plus } from '@element-plus/icons-vue'
import type { SchemaNode } from '../../editor/schema'
import { DUR_UNITS, durMs, durText, splitDur } from '../../editor/duration'

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
/** 数字框、时长框很窄，默认值写在框后面 */
const shortPlaceholder = computed(() => (props.node.required ? '必填' : '不写'))
const defaultText = computed(() => {
  const d = props.node.default
  if (!d) return ''
  if (props.node.kind === 'dur') return durMs(d) != null ? durText(d) : d
  return props.node.unit && /^\d+$/.test(d) ? `${d} ${props.node.unit}` : d
})

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

// 数字框只能输入数字（min 小于 0 或没写 min 时可以有负号）
const allowNeg = computed(() => props.node.min == null || props.node.min < 0)
function onNumInput(v: string) {
  let s = v.replace(/[^\d-]/g, '')
  s = allowNeg.value && s.startsWith('-') ? '-' + s.slice(1).replace(/-/g, '') : s.replace(/-/g, '')
  draft.value = s
}

function rangeText(lo?: number, hi?: number, fmt: (n: number) => string = String): string {
  if (lo != null && hi != null) return `要在 ${fmt(lo)}～${fmt(hi)} 之间`
  if (lo != null) return `至少 ${fmt(lo)}`
  if (hi != null) return `最多 ${fmt(hi)}`
  return ''
}

const numError = computed(() => {
  if (props.node.kind !== 'num') return ''
  const s = draft.value.trim()
  if (!s) return ''
  if (!/^-?\d+$/.test(s)) return '要写整数'
  const n = Number(s)
  const { min, max } = props.node
  return (min != null && n < min) || (max != null && n > max) ? rangeText(min, max) : ''
})

// ---------------------------------------------------------------- 时长：数字 + 单位

const durNum = ref('')
const durUnit = ref('s')
/** 配置里的值写法不认识（如 1.5x）：用文字框原样改 */
const durRaw = computed(() => props.node.kind === 'dur' && props.value != null && !Array.isArray(props.value) && props.value !== '' && splitDur(props.value) == null)
watch(
  () => props.value,
  (v) => {
    const p = v == null || Array.isArray(v) || v === '' ? null : splitDur(v)
    durNum.value = p ? String(p.n) : ''
    if (p) durUnit.value = p.u
    else if (v == null && props.node.default) durUnit.value = splitDur(props.node.default)?.u ?? 's'
  },
  { immediate: true },
)

function onDurInput(v: string) {
  durNum.value = v.replace(/\D/g, '')
}

function commitDur() {
  const s = durNum.value.trim()
  if (!s) return props.value == null ? undefined : emit('commit', undefined)
  const next = `${Number(s)}${durUnit.value}`
  const cur = splitDur(props.value)
  if (cur && cur.n === Number(s) && cur.u === durUnit.value) return
  emit('commit', next)
}

function setDurUnit(u: string) {
  durUnit.value = u
  if (durNum.value.trim()) commitDur()
}

const durError = computed(() => {
  if (props.node.kind !== 'dur' || !durNum.value.trim()) return ''
  const sec = (Number(durNum.value) * (DUR_UNITS.find((u) => u.v === durUnit.value)?.ms ?? 1000)) / 1000
  const { min, max } = props.node
  return (min != null && sec < min) || (max != null && sec > max) ? rangeText(min, max, (n) => durText(n)) : ''
})

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

  <!-- 数字：只能输入数字 -->
  <div v-else-if="node.kind === 'num'" class="ctl-text">
    <el-input
      :model-value="draft"
      :placeholder="shortPlaceholder"
      :disabled="disabled"
      inputmode="numeric"
      clearable
      size="small"
      class="ctl-num"
      @update:model-value="onNumInput"
      @change="commitText"
    >
      <template v-if="node.unit" #append>{{ node.unit }}</template>
    </el-input>
    <span v-if="numError" class="ev-error small">{{ numError }}</span>
    <span v-else-if="defaultText && !draft.trim()" class="muted small">不写时：{{ defaultText }}</span>
  </div>

  <!-- 时长：数字 + 单位 -->
  <div v-else-if="node.kind === 'dur' && !durRaw" class="ctl-text">
    <el-input
      :model-value="durNum"
      :placeholder="shortPlaceholder"
      :disabled="disabled"
      inputmode="numeric"
      clearable
      size="small"
      class="ctl-num"
      @update:model-value="onDurInput"
      @change="commitDur"
    />
    <el-select :model-value="durUnit" size="small" class="ctl-unit" :disabled="disabled" aria-label="单位" @update:model-value="setDurUnit">
      <el-option v-for="u in DUR_UNITS" :key="u.v" :label="u.label" :value="u.v" />
    </el-select>
    <span v-if="durError" class="ev-error small">{{ durError }}</span>
    <span v-else-if="defaultText && !durNum.trim()" class="muted small">不写时：{{ defaultText }}</span>
  </div>

  <!-- 文字（还有写法不认识的时长） -->
  <div v-else-if="node.kind === 'str' || node.kind === 'dur'" class="ctl-text">
    <el-input v-model="draft" :placeholder="placeholder" :disabled="disabled" clearable size="small" @change="commitText" />
    <span v-if="durRaw" class="ev-error small">时长写法不对，如 30s、5m、1h，清空后重新填</span>
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
      <el-input
        :model-value="rows[i]"
        size="small"
        :disabled="disabled"
        :inputmode="node.numbers ? 'numeric' : undefined"
        :class="{ 'ctl-num': node.numbers }"
        @update:model-value="(v: string) => (rows[i] = node.numbers ? v.replace(/\D/g, '') : v)"
        @change="commitRows"
      />
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
.ctl-num {
  width: 160px;
  flex: none;
}
.ctl-unit {
  width: 88px;
  flex: none;
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
