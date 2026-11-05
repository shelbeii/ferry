<script setup lang="ts">
// 表数据上面的「筛选」「排序」面板（像 Navicat）：一行一个条件 / 一行一个排序列，改好点「应用」。
// 也可以切到「直接写条件」自己写 WHERE。
import { computed, ref, watch } from 'vue'
import { Plus, Delete, Top, Bottom, Check, Close, Filter, Sort, EditPen } from '@element-plus/icons-vue'
import { filterOps, type DbColumnInfo, type DbFilter, type DbSort } from '../../db'

const props = defineProps<{
  columns: DbColumnInfo[]
  filters: DbFilter[]
  sorts: DbSort[]
  where: string
  /** 现在打开的面板 */
  panel: '' | 'filter' | 'sort'
}>()
const emit = defineEmits<{
  apply: [filters: DbFilter[], sorts: DbSort[], where: string]
  'update:panel': [panel: '' | 'filter' | 'sort']
}>()

// 正在编辑的（点「应用」才生效）
const fs = ref<DbFilter[]>([])
const ss = ref<DbSort[]>([])
const text = ref('')
const textMode = ref(false)
function reset() {
  fs.value = props.filters.map((f) => ({ ...f, off: !!f.off, or: !!f.or }))
  ss.value = props.sorts.map((s) => ({ ...s }))
  text.value = props.where
  textMode.value = !!props.where && !props.filters.length
}
watch(() => [props.filters, props.sorts, props.where], reset, { immediate: true })
watch(() => props.panel, (p) => {
  if (!p) return
  reset()
  if (p === 'filter' && !fs.value.length && !textMode.value) addFilter()
  if (p === 'sort' && !ss.value.length) addSort()
})

const opOf = (v: string) => filterOps.find((o) => o.value === v)
function addFilter() {
  fs.value.push({ col: props.columns[0]?.name ?? '', op: '=', value: '', off: false, or: false })
}
function addSort() {
  const used = new Set(ss.value.map((s) => s.col))
  ss.value.push({ col: props.columns.find((c) => !used.has(c.name))?.name ?? props.columns[0]?.name ?? '', desc: false })
}
function moveSort(i: number, d: number) {
  const j = i + d
  if (j < 0 || j >= ss.value.length) return
  const a = ss.value
  ;[a[i], a[j]] = [a[j], a[i]]
}

function apply() {
  const filters = textMode.value ? [] : fs.value.filter((f) => f.col && (opOf(f.op)?.noValue || f.value !== undefined))
  const sorts = ss.value.filter((s) => s.col)
  emit('apply', filters, sorts, textMode.value ? text.value.trim() : '')
}
function clearFilters() {
  fs.value = []
  text.value = ''
  emit('apply', [], props.sorts, '')
}
function clearSorts() {
  ss.value = []
  emit('apply', props.filters, [], props.where)
}

const activeFilters = computed(() => props.filters.filter((f) => !f.off).length + (props.where ? 1 : 0))
function toggle(p: 'filter' | 'sort') {
  emit('update:panel', props.panel === p ? '' : p)
}
defineExpose({ activeFilters })

// 鼠标移到字段上显示字段说明（整个面板共用一个提示框，字段多也不卡）
const byName = computed(() => new Map(props.columns.map((c) => [c.name, c])))
const tipCol = ref<DbColumnInfo | null>(null)
const tipEl = ref<HTMLElement>()
let tipTimer = 0
function showTip(e: MouseEvent, name: string) {
  const c = byName.value.get(name)
  clearTimeout(tipTimer)
  if (!c) return hideTip()
  tipEl.value = e.currentTarget as HTMLElement
  tipCol.value = c
}
function hideTip() {
  clearTimeout(tipTimer)
  tipTimer = window.setTimeout(() => (tipCol.value = null), 60)
}
</script>

<template>
  <div class="fb">
    <div class="btns">
      <el-button size="small" :type="panel === 'filter' || activeFilters ? 'primary' : ''" :plain="panel !== 'filter'" :icon="Filter" @click="toggle('filter')">
        筛选<template v-if="activeFilters">（{{ activeFilters }}）</template>
      </el-button>
      <el-button size="small" :type="panel === 'sort' || sorts.length ? 'primary' : ''" :plain="panel !== 'sort'" :icon="Sort" @click="toggle('sort')">
        排序<template v-if="sorts.length">（{{ sorts.length }}）</template>
      </el-button>
      <el-button v-if="activeFilters" size="small" link :icon="Close" @click="clearFilters">清除筛选</el-button>
      <el-button v-if="sorts.length" size="small" link :icon="Close" @click="clearSorts">清除排序</el-button>
      <slot />
    </div>

    <div v-if="panel === 'filter'" class="panel">
      <template v-if="!textMode">
        <div v-for="(f, i) in fs" :key="i" class="row" :class="{ off: f.off }">
          <el-checkbox v-model="f.off" :true-value="false" :false-value="true" title="用这个条件" />
          <el-select v-if="i > 0" v-model="f.or" size="small" class="join">
            <el-option :value="false" label="并且" />
            <el-option :value="true" label="或者" />
          </el-select>
          <span v-else class="join where">WHERE</span>
          <el-select v-model="f.col" size="small" filterable class="col" placeholder="列" @mouseenter="showTip($event, f.col)" @mouseleave="hideTip" @visible-change="hideTip">
            <el-option v-for="c in columns" :key="c.name" :value="c.name" :label="c.name">
            <div class="opt" @mouseenter="showTip($event, c.name)" @mouseleave="hideTip">{{ c.name }}</div>
          </el-option>
          </el-select>
          <el-select v-model="f.op" size="small" class="op">
            <el-option v-for="o in filterOps" :key="o.value" :value="o.value" :label="o.label" />
          </el-select>
          <template v-if="!opOf(f.op)?.noValue">
            <el-input v-model="f.value" size="small" class="val" :placeholder="opOf(f.op)?.hint || '值'" clearable @keyup.enter="apply" />
            <template v-if="opOf(f.op)?.two">
              <span class="and">和</span>
              <el-input v-model="f.value2" size="small" class="val" placeholder="值" clearable @keyup.enter="apply" />
            </template>
          </template>
          <el-button link size="small" type="danger" :icon="Delete" title="删掉这个条件" @click="fs.splice(i, 1)" />
        </div>
        <div v-if="!fs.length" class="empty">没有条件：显示全部数据</div>
      </template>
      <div v-else class="row">
        <span class="join where">WHERE</span>
        <el-input v-model="text" size="small" class="text" placeholder="自己写条件，如 status = 1 AND name LIKE '%张%'" clearable @keyup.enter="apply" />
      </div>
      <div class="actions">
        <el-button v-if="!textMode" size="small" :icon="Plus" @click="addFilter">加条件</el-button>
        <el-button size="small" link :icon="EditPen" @click="textMode = !textMode">{{ textMode ? '用条件列表' : '直接写条件' }}</el-button>
        <span class="grow" />
        <el-button size="small" @click="emit('update:panel', '')">收起</el-button>
        <el-button size="small" type="primary" :icon="Check" @click="apply">应用</el-button>
      </div>
    </div>

    <div v-else-if="panel === 'sort'" class="panel">
      <div v-for="(s, i) in ss" :key="i" class="row">
        <span class="join where">{{ i === 0 ? '先按' : '再按' }}</span>
        <el-select v-model="s.col" size="small" filterable class="col" placeholder="列" @mouseenter="showTip($event, s.col)" @mouseleave="hideTip" @visible-change="hideTip">
          <el-option v-for="c in columns" :key="c.name" :value="c.name" :label="c.name">
            <div class="opt" @mouseenter="showTip($event, c.name)" @mouseleave="hideTip">{{ c.name }}</div>
          </el-option>
        </el-select>
        <el-radio-group v-model="s.desc" size="small">
          <el-radio-button :value="false">升序（小到大）</el-radio-button>
          <el-radio-button :value="true">降序（大到小）</el-radio-button>
        </el-radio-group>
        <el-button link size="small" :icon="Top" :disabled="i === 0" @click="moveSort(i, -1)" />
        <el-button link size="small" :icon="Bottom" :disabled="i === ss.length - 1" @click="moveSort(i, 1)" />
        <el-button link size="small" type="danger" :icon="Delete" @click="ss.splice(i, 1)" />
      </div>
      <div v-if="!ss.length" class="empty">没有排序：按数据库默认的顺序</div>
      <div class="actions">
        <el-button size="small" :icon="Plus" @click="addSort">加排序列</el-button>
        <span class="tip">也可以点表头：升序 → 降序 → 不排序</span>
        <span class="grow" />
        <el-button size="small" @click="emit('update:panel', '')">收起</el-button>
        <el-button size="small" type="primary" :icon="Check" @click="apply">应用</el-button>
      </div>
    </div>

    <el-tooltip :visible="!!tipCol" :virtual-ref="tipEl" virtual-triggering placement="right" :show-arrow="false" :offset="10" popper-class="db-col-tip">
      <template #content>
        <div v-if="tipCol" class="col-tip">
          <div class="name">{{ tipCol.name }}<span class="type">{{ tipCol.type }}</span></div>
          <div v-if="tipCol.comment" class="desc">{{ tipCol.comment }}</div>
          <div v-else class="desc none">没有写字段说明</div>
        </div>
      </template>
    </el-tooltip>
  </div>
</template>

<style scoped>
.fb {
  display: contents;
}
.btns {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.btns .el-button + .el-button {
  margin-left: 0;
}
.panel {
  flex-basis: 100%;
  order: 10;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 8px 10px;
  background: var(--el-fill-color-extra-light);
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.row.off > :not(.el-checkbox) {
  opacity: 0.45;
}
.join {
  width: 82px;
  flex-shrink: 0;
}
.where {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12px;
  color: var(--el-text-color-secondary);
  text-align: center;
}
.col {
  width: 180px;
}
.op {
  width: 190px;
}
.val {
  width: 200px;
}
.text {
  flex: 1;
}
.and {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.empty,
.tip {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.actions {
  display: flex;
  align-items: center;
  gap: 6px;
  padding-top: 2px;
}
.actions .el-button + .el-button {
  margin-left: 0;
}
.grow {
  flex: 1;
}
.opt {
  margin: 0 -32px 0 -20px;
  padding: 0 32px 0 20px;
}
.col-tip {
  max-width: 320px;
  line-height: 1.5;
}
.col-tip .name {
  font-weight: 600;
}
.col-tip .type {
  margin-left: 8px;
  font-weight: 400;
  font-size: 12px;
  opacity: 0.7;
}
.col-tip .desc {
  margin-top: 2px;
  white-space: pre-wrap;
  word-break: break-word;
}
.col-tip .none {
  opacity: 0.6;
}
</style>
