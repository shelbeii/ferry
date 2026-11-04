<script setup lang="ts">
// 数据表格：表头可排序、可拖动列宽；单击选中格子，双击改（editable 时）；右键菜单复制、设为 NULL、删除行。
// 改动不在这里保存：通过 edit / remove 事件交给上层记下来，上层把改了的格子、删掉的行、新加的行传回来显示。
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { CaretTop, CaretBottom } from '@element-plus/icons-vue'
import { cellText, closeOnOutside, type DbCell, type DbColumn } from '../../db'

const props = defineProps<{
  columns: DbColumn[]
  rows: DbCell[][]
  /** 能改哪些列（列序号）；为空不能改 */
  editable?: boolean
  readOnlyCols?: number[]
  /** 改过的格子：行 → 列 → 新值 */
  edits?: Record<number, Record<number, string | null>>
  /** 删掉的行 */
  deleted?: Set<number>
  /** 新加的行（排在后面）：undefined = 用默认值 */
  added?: (string | null | undefined)[][]
  /** 排序（表头上显示箭头；多列时带序号） */
  sorts?: { col: string; desc?: boolean }[]
  /** 第一行的行号（翻页时） */
  offset?: number
}>()
const emit = defineEmits<{
  sort: [col: string]
  edit: [row: number, col: number, value: string | null]
  editAdded: [row: number, col: number, value: string | null | undefined]
  remove: [rows: number[]]
  removeAdded: [row: number]
}>()

const widths = reactive<number[]>([])
watch(() => props.columns, (cols) => {
  widths.length = 0
  for (const c of cols) widths.push(Math.min(320, Math.max(90, c.name.length * 9 + 40)))
}, { immediate: true })

// 选中的格子：added 为 true 时是新加的行
const sel = ref<{ row: number; col: number; added: boolean } | null>(null)
const selRows = ref<Set<number>>(new Set())
const editing = ref<{ row: number; col: number; added: boolean; value: string } | null>(null)
let editInput: HTMLTextAreaElement | null = null
function setEditInput(el: unknown) {
  editInput = el as HTMLTextAreaElement | null
}
const viewer = ref<{ title: string; text: string; row: number; col: number; added: boolean; editable: boolean } | null>(null)

const baseRows = computed(() => props.rows.length)

// 只画看得见的行（行高固定），上下多画几行；几千行、几十列滚动也不卡
const ROW = 28
const HEAD = 30
const EXTRA = 8
const wrap = ref<HTMLDivElement>()
const scrollTop = ref(0)
const viewHeight = ref(600)
let frame = 0
function onScroll() {
  if (frame) return
  frame = requestAnimationFrame(() => {
    frame = 0
    scrollTop.value = wrap.value?.scrollTop ?? 0
    viewHeight.value = wrap.value?.clientHeight ?? 600
  })
}
let resizeObs: ResizeObserver | null = null
onMounted(() => {
  viewHeight.value = wrap.value?.clientHeight ?? 600
  resizeObs = new ResizeObserver(() => { viewHeight.value = wrap.value?.clientHeight ?? viewHeight.value })
  if (wrap.value) resizeObs.observe(wrap.value)
})
onBeforeUnmount(() => {
  resizeObs?.disconnect()
  if (frame) cancelAnimationFrame(frame)
})
const total = computed(() => props.rows.length + (props.added?.length ?? 0))
const first = computed(() => Math.max(0, Math.floor((scrollTop.value - HEAD) / ROW) - EXTRA))
const last = computed(() => Math.min(total.value, Math.ceil((scrollTop.value + viewHeight.value) / ROW) + EXTRA))
const visible = computed(() => {
  const out: { row: number; added: boolean }[] = []
  const n = props.rows.length
  for (let i = first.value; i < last.value; i++) out.push(i < n ? { row: i, added: false } : { row: i - n, added: true })
  return out
})
const padTop = computed(() => first.value * ROW)
const padBottom = computed(() => (total.value - last.value) * ROW)
// 换了一批数据（翻页、刷新）：回到最上面
watch(() => props.rows, () => {
  if (wrap.value) wrap.value.scrollTop = 0
  scrollTop.value = 0
})
/** 键盘移动选中的格子时，让它保持在看得见的地方 */
function reveal(row: number, added: boolean) {
  const el = wrap.value
  if (!el) return
  const y = HEAD + (added ? props.rows.length + row : row) * ROW
  if (y < el.scrollTop + HEAD) el.scrollTop = y - HEAD
  else if (y + ROW > el.scrollTop + el.clientHeight) el.scrollTop = y + ROW - el.clientHeight
}

function cellClass(row: number, col: number, added: boolean) {
  const v = valueOf(row, col, added)
  const s = sel.value
  return {
    sel: !!s && s.added === added && s.row === row && s.col === col,
    editing: !!editing.value && editing.value.added === added && editing.value.row === row && editing.value.col === col,
    edited: !added && isEdited(row, col),
    null: v === null,
    dflt: added && v === undefined,
    obj: !!v && typeof v === 'object',
  }
}

function valueOf(row: number, col: number, added: boolean): DbCell | undefined {
  if (added) return props.added?.[row]?.[col]
  const e = props.edits?.[row]
  if (e && col in e) return e[col]
  return props.rows[row]?.[col]
}
function isEdited(row: number, col: number) {
  const e = props.edits?.[row]
  return !!e && col in e
}
function canEdit(col: number, added: boolean, row = -1) {
  if (!props.editable || props.readOnlyCols?.includes(col)) return false
  if (!added && props.deleted?.has(row)) return false
  const v = added ? undefined : props.rows[row]?.[col]
  return !(v && typeof v === 'object') // 二进制、截断的长文本不能直接改
}
function display(v: DbCell | undefined): string {
  if (v === undefined) return '(默认)'
  if (v === null) return 'NULL'
  if (typeof v === 'string') return v.length > 300 ? v.slice(0, 300) + '…' : v
  if ('bin' in v) return `(二进制 ${v.bin} 字节)`
  return v.cut.slice(0, 300) + '…'
}

function pick(row: number, col: number, added: boolean, e: MouseEvent) {
  sel.value = { row, col, added }
  if (added) return
  if (e.ctrlKey || e.metaKey) {
    const s = new Set(selRows.value)
    if (s.has(row)) s.delete(row)
    else s.add(row)
    selRows.value = s
  } else if (e.shiftKey && selRows.value.size) {
    const last = [...selRows.value].pop()!
    const s = new Set<number>()
    for (let i = Math.min(last, row); i <= Math.max(last, row); i++) s.add(i)
    selRows.value = s
  } else {
    selRows.value = new Set([row])
  }
}

async function startEdit(row: number, col: number, added: boolean) {
  if (!canEdit(col, added, row)) {
    const v = valueOf(row, col, added)
    if (v !== undefined) openViewer(row, col, added)
    return
  }
  const v = valueOf(row, col, added)
  const text = typeof v === 'string' ? v : ''
  if (text.length > 200 || text.includes('\n')) return openViewer(row, col, added)
  editing.value = { row, col, added, value: text }
  await nextTick()
  editInput?.focus()
  editInput?.select()
}
function commitEdit(setNull = false) {
  const ed = editing.value
  if (!ed) return
  editing.value = null
  const v = setNull ? null : ed.value
  const old = valueOf(ed.row, ed.col, ed.added)
  if (old === v) return
  if (ed.added) emit('editAdded', ed.row, ed.col, v)
  else emit('edit', ed.row, ed.col, v)
}
function onEditKey(e: KeyboardEvent) {
  if (e.key === 'Escape') {
    e.preventDefault()
    editing.value = null
  } else if (e.key === 'Enter' && !e.shiftKey && !e.altKey) {
    e.preventDefault()
    commitEdit()
  }
}

function openViewer(row: number, col: number, added: boolean) {
  const v = valueOf(row, col, added)
  viewer.value = { title: props.columns[col].name, text: v === undefined || v === null ? '' : cellText(v), row, col, added, editable: canEdit(col, added, row) }
}
function saveViewer(setNull = false) {
  const vw = viewer.value
  if (!vw) return
  viewer.value = null
  const v = setNull ? null : vw.text
  if (vw.added) emit('editAdded', vw.row, vw.col, v)
  else emit('edit', vw.row, vw.col, v)
}
function prettyJson() {
  if (!viewer.value) return
  try {
    viewer.value.text = JSON.stringify(JSON.parse(viewer.value.text), null, 2)
  } catch {
    ElMessage.warning('不是 JSON')
  }
}

async function copy(text: string) {
  await navigator.clipboard.writeText(text)
  ElMessage.success({ message: '已复制', duration: 1200 })
}
function rowText(row: number, added = false) {
  return props.columns.map((_, c) => { const v = valueOf(row, c, added); return v === undefined || v === null ? '' : cellText(v) }).join('\t')
}

// 右键菜单

// 右键菜单：点菜单外面（含右键别处）、按 Esc、滚动时关闭；靠近窗口边时挪进来
const menuEl = ref<HTMLElement>()
let offMenu: (() => void) | null = null
async function fitMenu() {
  await nextTick()
  const m = menu.value
  const el = menuEl.value
  if (!m || !el) return
  const r = el.getBoundingClientRect()
  m.x = Math.max(4, Math.min(m.x, window.innerWidth - r.width - 4))
  m.y = Math.max(4, Math.min(m.y, window.innerHeight - r.height - 4))
}
onBeforeUnmount(() => offMenu?.())
const menu = ref<{ x: number; y: number; row: number; col: number; added: boolean } | null>(null)
function openMenu(e: MouseEvent, row: number, col: number, added: boolean) {
  e.preventDefault()
  if (!added && !selRows.value.has(row)) selRows.value = new Set([row])
  sel.value = { row, col, added }
  menu.value = { x: e.clientX, y: e.clientY, row, col, added }
  offMenu?.()
  offMenu = closeOnOutside((t) => !!menuEl.value?.contains(t), () => { menu.value = null })
  void fitMenu()
}
function menuDo(action: string) {
  const m = menu.value
  menu.value = null
  offMenu?.()
  if (!m) return
  const v = valueOf(m.row, m.col, m.added)
  switch (action) {
    case 'copy': return copy(v === undefined || v === null ? '' : cellText(v))
    case 'copy-row': return copy([...(m.added ? [m.row] : [...selRows.value].sort((a, b) => a - b))].map((r) => rowText(r, m.added)).join('\n'))
    case 'copy-name': return copy(props.columns[m.col].name)
    case 'null': return m.added ? emit('editAdded', m.row, m.col, null) : emit('edit', m.row, m.col, null)
    case 'default': return emit('editAdded', m.row, m.col, undefined)
    case 'view': return openViewer(m.row, m.col, m.added)
    case 'remove': return m.added ? emit('removeAdded', m.row) : emit('remove', [...selRows.value])
  }
}

function onKey(e: KeyboardEvent) {
  if (editing.value || viewer.value) return
  const s = sel.value
  if (!s) return
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'c') {
    e.preventDefault()
    const v = valueOf(s.row, s.col, s.added)
    void copy(v === undefined || v === null ? '' : cellText(v))
  } else if (e.key === 'Enter' || e.key === 'F2') {
    e.preventDefault()
    void startEdit(s.row, s.col, s.added)
  } else if (e.key === 'Delete' && props.editable && !s.added && selRows.value.size) {
    e.preventDefault()
    emit('remove', [...selRows.value])
  } else if (e.key.startsWith('Arrow')) {
    e.preventDefault()
    const total = s.added ? (props.added?.length ?? 0) : baseRows.value
    let { row, col } = s
    if (e.key === 'ArrowUp') row = Math.max(0, row - 1)
    if (e.key === 'ArrowDown') row = Math.min(total - 1, row + 1)
    if (e.key === 'ArrowLeft') col = Math.max(0, col - 1)
    if (e.key === 'ArrowRight') col = Math.min(props.columns.length - 1, col + 1)
    sel.value = { row, col, added: s.added }
    reveal(row, s.added)
    if (!s.added) selRows.value = new Set([row])
  }
}

// 拖动列宽
function startResize(e: MouseEvent, i: number) {
  e.preventDefault()
  e.stopPropagation()
  const x0 = e.clientX
  const w0 = widths[i]
  const move = (ev: MouseEvent) => { widths[i] = Math.max(50, w0 + ev.clientX - x0) }
  const up = () => { window.removeEventListener('mousemove', move); window.removeEventListener('mouseup', up) }
  window.addEventListener('mousemove', move)
  window.addEventListener('mouseup', up)
}

defineExpose({ selectedRows: () => [...selRows.value], clearSelection: () => { selRows.value = new Set(); sel.value = null } })
</script>

<template>
  <div ref="wrap" class="grid-wrap" tabindex="0" @keydown="onKey" @scroll="onScroll">
    <table class="grid" :style="{ width: widths.reduce((a, b) => a + b, 52) + 'px' }">
      <colgroup>
        <col style="width: 52px" />
        <col v-for="(w, i) in widths" :key="i" :style="{ width: w + 'px' }" />
      </colgroup>
      <thead>
        <tr>
          <th class="num">#</th>
          <th v-for="(c, i) in columns" :key="i" :title="`${c.name}（${c.type || '未知类型'}）`" @click="emit('sort', c.name)">
            <span class="th-name">{{ c.name }}</span>
            <template v-for="(st, si) in sorts ?? []" :key="si">
              <span v-if="st.col === c.name" class="sort"><el-icon><CaretBottom v-if="st.desc" /><CaretTop v-else /></el-icon><sub v-if="(sorts?.length ?? 0) > 1">{{ si + 1 }}</sub></span>
            </template>
            <span class="th-type">{{ c.type.toLowerCase() }}</span>
            <span class="resize" @mousedown="startResize($event, i)" @click.stop />
          </th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="padTop" class="pad" :style="{ height: padTop + 'px' }"><td :colspan="columns.length + 1" /></tr>
        <tr v-for="v in visible" :key="(v.added ? 'a' : 'r') + v.row" :class="v.added ? 'added' : { deleted: deleted?.has(v.row), picked: selRows.has(v.row) }">
          <td class="num">{{ v.added ? '＋' : (offset ?? 0) + v.row + 1 }}</td>
          <td
            v-for="(_, ci) in columns"
            :key="ci"
            :class="cellClass(v.row, ci, v.added)"
            @click="pick(v.row, ci, v.added, $event)"
            @dblclick="startEdit(v.row, ci, v.added)"
            @contextmenu="openMenu($event, v.row, ci, v.added)"
          >
            <textarea v-if="editing && editing.added === v.added && editing.row === v.row && editing.col === ci" :ref="setEditInput" v-model="editing.value" class="cell-input" rows="1" spellcheck="false" @keydown.stop="onEditKey" @blur="commitEdit()" />
            <template v-else>{{ display(valueOf(v.row, ci, v.added)) }}</template>
          </td>
        </tr>
        <tr v-if="padBottom" class="pad" :style="{ height: padBottom + 'px' }"><td :colspan="columns.length + 1" /></tr>
      </tbody>
    </table>
    <div v-if="!rows.length && !added?.length" class="empty">没有数据</div>

    <teleport to="body">
      <ul v-if="menu" ref="menuEl" class="db-grid-menu" :style="{ left: menu.x + 'px', top: menu.y + 'px' }" @click.stop>
        <li @click="menuDo('copy')">复制值</li>
        <li @click="menuDo('copy-row')">复制整行（制表符分隔）</li>
        <li @click="menuDo('copy-name')">复制列名</li>
        <li @click="menuDo('view')">查看全部内容</li>
        <template v-if="editable && canEdit(menu.col, menu.added, menu.row)">
          <li class="sep" @click="menuDo('null')">设为 NULL</li>
          <li v-if="menu.added" @click="menuDo('default')">用默认值</li>
        </template>
        <li v-if="editable && (menu.added || !deleted?.has(menu.row))" class="sep danger" @click="menuDo('remove')">删除{{ menu.added ? '这一行' : selRows.size > 1 ? ` ${selRows.size} 行` : '这一行' }}</li>
      </ul>
    </teleport>

    <el-dialog :model-value="!!viewer" :title="viewer?.title" width="640px" append-to-body @close="viewer = null">
      <el-input v-if="viewer" v-model="viewer.text" type="textarea" :rows="16" :readonly="!viewer.editable" class="mono" />
      <template #footer>
        <el-button @click="prettyJson">格式化 JSON</el-button>
        <el-button v-if="viewer" @click="copy(viewer.text)">复制</el-button>
        <template v-if="viewer?.editable">
          <el-button @click="saveViewer(true)">设为 NULL</el-button>
          <el-button type="primary" @click="saveViewer()">改成这个值</el-button>
        </template>
        <el-button v-else @click="viewer = null">关闭</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.grid-wrap {
  position: relative;
  overflow: auto;
  height: 100%;
  outline: none;
  font-family: 'Cascadia Mono', Consolas, 'Courier New', monospace;
  font-size: 12.5px;
}
.grid {
  border-collapse: separate;
  border-spacing: 0;
  table-layout: fixed;
}
th,
td {
  border-right: 1px solid var(--el-border-color-lighter);
  border-bottom: 1px solid var(--el-border-color-lighter);
  padding: 3px 8px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 0;
  box-sizing: border-box;
}
td {
  height: 28px;
  line-height: 27px;
  padding-top: 0;
  padding-bottom: 0;
}
th {
  height: 30px;
}
tr.pad td {
  padding: 0;
  border: none;
  height: auto;
}
th {
  position: sticky;
  top: 0;
  z-index: 2;
  background: var(--el-fill-color-light);
  text-align: left;
  font-weight: 600;
  cursor: pointer;
  user-select: none;
  font-family: var(--el-font-family);
}
.th-type {
  margin-left: 6px;
  color: var(--el-text-color-placeholder);
  font-weight: 400;
  font-size: 11px;
}
.sort {
  vertical-align: -2px;
  color: var(--el-color-primary);
}
.sort sub {
  font-size: 9px;
}
.resize {
  position: absolute;
  right: -3px;
  top: 0;
  bottom: 0;
  width: 7px;
  cursor: col-resize;
  z-index: 3;
}
.num {
  position: sticky;
  left: 0;
  z-index: 1;
  background: var(--el-fill-color-lighter);
  color: var(--el-text-color-secondary);
  text-align: right;
  font-size: 11px;
}
th.num {
  z-index: 3;
}
td {
  cursor: default;
}
td.null,
td.dflt {
  color: var(--el-text-color-placeholder);
  font-style: italic;
}
td.obj {
  color: var(--el-color-info);
}
td.sel {
  outline: 2px solid var(--el-color-primary);
  outline-offset: -2px;
}
tr.picked td:not(.num) {
  background: var(--el-color-primary-light-9);
}
td.edited {
  background: var(--el-color-warning-light-8) !important;
}
tr.deleted td {
  text-decoration: line-through;
  color: var(--el-color-danger);
  background: var(--el-color-danger-light-9);
}
tr.added td:not(.num) {
  background: var(--el-color-success-light-9);
}
/* 编辑时输入框盖满整个格子，和格子一样大 */
td.editing {
  position: relative;
  padding: 0;
  overflow: visible;
}
.cell-input {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  box-sizing: border-box;
  border: 2px solid var(--el-color-primary);
  outline: none;
  resize: none;
  font: inherit;
  line-height: 23px;
  padding: 0 6px;
  margin: 0;
  background: var(--el-bg-color);
  color: var(--el-text-color-primary);
  overflow: hidden;
  white-space: pre;
}
.empty {
  padding: 40px;
  text-align: center;
  color: var(--el-text-color-secondary);
  font-family: var(--el-font-family);
}
.mono :deep(textarea) {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12.5px;
}
</style>

<style>
.db-grid-menu {
  position: fixed;
  z-index: 5000;
  margin: 0;
  padding: 4px 0;
  list-style: none;
  background: var(--el-bg-color-overlay);
  border: 1px solid var(--el-border-color-light);
  border-radius: 6px;
  box-shadow: var(--el-box-shadow-light);
  min-width: 170px;
  font-size: 13px;
}
.db-grid-menu li {
  padding: 6px 14px;
  cursor: pointer;
}
.db-grid-menu li:hover {
  background: var(--el-fill-color-light);
}
.db-grid-menu li.sep {
  border-top: 1px solid var(--el-border-color-lighter);
}
.db-grid-menu li.danger {
  color: var(--el-color-danger);
}
</style>
