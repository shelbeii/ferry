<script setup lang="ts">
// 服务器上的文件（SFTP，以登录用户的身份）：浏览目录、查看和编辑内容、上传、下载、新建文件夹、改名、删除。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox, ElNotification } from 'element-plus'
import { ArrowRight, Back, Delete, Document, Download, EditPen, Folder, FolderAdd, HomeFilled, Link, Refresh, Upload, View } from '@element-plus/icons-vue'
import { bytes, call, ferry, FerryError, isSecretKind, type DirListing, type ErrorDetail, type FileEntry } from '../ferry'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
import { validRemoteName } from '../fileNames'
import { filePermissions, type FilePermissionInfo } from '../filePermissions'
import { useFileSizes } from '../fileSizes'
import { useHoverTip } from '../hoverTip'
import { useVirtualRows } from '../virtualRows'
import FilePermissionDetails from './FilePermissionDetails.vue'
import FilePreviewDialog from './FilePreviewDialog.vue'
import FileSizeCell from './FileSizeCell.vue'
import { onFileDrop, pickDir, pickMany, reveal } from '../platform'
import { secretRequest, trustRequest, trustedTick } from '../state'
import { glass } from '../glass/runtime'

const props = defineProps<{ config: string; target: string; initialPath?: string; initialFocus?: string; directoryOnly?: boolean }>()
const emit = defineEmits<{ directoryChange: [path: string] }>()

// 名称排序：每次比较都 localeCompare(…, 'zh-CN') 要现建排序规则，上千个文件要几十毫秒；共用一个
const nameOrder = new Intl.Collator('zh-CN', { numeric: true })

const path = ref('')
const home = ref('')
const pathInput = ref('')
const editingPath = ref(false)
const breadcrumbs = computed(() => path.value.split('/').filter(Boolean).map((label, i, all) => ({ label, path: '/' + all.slice(0, i + 1).join('/') })))
const entries = ref<FileEntry[]>([])
const sizes = useFileSizes()
const loading = ref(false)
const error = ref('')
/** 勾选的（按名字）；只算当前列表里有的 */
const selectedNames = ref(new Set<string>())
let selectAnchor: string | undefined
const focusedName = ref<string>()
const preview = ref<{ entry: FileEntry; dir: string }>()
const query = ref('')
const directoryEntries = computed(() => entries.value.filter((e) => !props.directoryOnly || e.dir))
const shownEntries = computed(() => {
  const q = query.value.trim().toLowerCase()
  return (q ? directoryEntries.value.filter((e) => e.name.toLowerCase().includes(q)) : directoryEntries.value.slice()).sort((a, b) => Number(b.dir) - Number(a.dir) || nameOrder.compare(a.name, b.name))
})
const selected = computed(() => shownEntries.value.filter((row) => selectedNames.value.has(row.name)))
const working = ref('')
/** 正在删除的（完整路径）：删除在后台进行，别的文件夹照常能进、能操作 */
const deleting = ref(new Set<string>())
const isDeleting = (row: FileEntry) => deleting.value.has(join(path.value, row.name))
const busy = computed(() => loading.value || !!working.value || !!xfer.value)
const xfer = ref<{ kind: 'upload' | 'download'; what: string; file: string; done: number; total: number; kill: () => Promise<void> }>()
const dropOver = ref(false)
const root = ref<HTMLDivElement>()
let waitTrust = false
const listingReads = useReadRequests()
let loadSeq = 0
let transferSeq = 0
let context = 0
let requestedPath = ''
let requestedFocus: string | undefined
let disposed = false

const join = (dir: string, name: string) => (dir.endsWith('/') ? dir + name : dir + '/' + name)
const parent = computed(() => (path.value === '/' ? '' : path.value.replace(/\/[^/]+\/?$/, '') || '/'))
const percent = computed(() => (xfer.value && xfer.value.total > 0 ? Math.min(100, Math.round((xfer.value.done / xfer.value.total) * 100)) : 0))
const chosenDirectory = computed(() => {
  if (!props.directoryOnly || busy.value || error.value || !path.value) return ''
  const folder = shownEntries.value.find((entry) => entry.name === focusedName.value && entry.dir)
  return folder ? join(path.value, folder.name) : path.value
})
watch(chosenDirectory, (dir) => emit('directoryChange', dir), { immediate: true })

watch(() => [props.config, props.target, props.initialPath, props.initialFocus, props.directoryOnly], () => {
  context++
  transferSeq++
  xfer.value?.kill()
  xfer.value = undefined
  working.value = ''
  deleting.value = new Set()
  preview.value = undefined
  path.value = ''; home.value = ''; entries.value = []; clearSelection(); query.value = ''; editingPath.value = false
  sizes.clear()
  load(props.initialPath ?? '', props.initialFocus)
}, { immediate: true })
watch(trustedTick, () => {
  if (waitTrust) load(requestedPath, requestedFocus)
})

// ---- 文件列表：只画看得见的行（目录里几千个文件时，全画出来打开、勾选、删除都要卡好几秒，整个 Ferry 跟着不动）
// 行高由权限摘要的行数决定（CSS 里 .file-row 的高度直接用这里的值）
const ROW_PAD = 13 // 上下留白 12 + 底边线 1
const PERM_LINE = 22
const PERM_SPECIAL = 16
function rowHeight(p: FilePermissionInfo): number {
  return ROW_PAD + Math.max(24, p.summary.length * PERM_LINE + (p.special.length ? PERM_SPECIAL : 0))
}
const rowPermissions = computed(() => shownEntries.value.map((e) => filePermissions(e)))
const rowHeights = computed(() => rowPermissions.value.map(rowHeight))
const scroller = ref<HTMLDivElement>()
const head = ref<HTMLDivElement>()
const virtual = useVirtualRows(scroller, head, rowHeights)
const visibleRows = computed(() => {
  const [start, end] = virtual.range.value
  const list = shownEntries.value
  const out = []
  for (let i = start; i < end && i < list.length; i++) {
    out.push({ row: list[i], index: i, top: virtual.offsets.value[i], height: rowHeights.value[i], permissions: rowPermissions.value[i] })
  }
  return out
})
const emptyText = computed(() => error.value ? '目录读取失败，请重试' : query.value.trim() ? (props.directoryOnly ? '没有匹配的子目录' : '没有匹配的文件') : props.directoryOnly ? '没有子目录，可以选择当前目录' : '空文件夹')
function scrollToName(name: string) {
  virtual.scrollToIndex(shownEntries.value.findIndex((entry) => entry.name === name))
}

// 列表共用一个悬停提示：权限详情、大小说明、名字太长时的全名
const hoverTip = useHoverTip<{ entry: FileEntry; permissions: FilePermissionInfo }>((el) => {
  const kind = el.dataset.tip
  if (kind === 'size') return el.dataset.tipText ? { placement: 'left', effect: 'dark', text: el.dataset.tipText } : undefined
  if (kind === 'name') return el.scrollWidth > el.clientWidth ? { placement: 'top', effect: 'dark', text: el.textContent ?? '' } : undefined
  if (kind === 'perm') {
    const name = el.closest<HTMLElement>('[data-name]')?.dataset.name
    const entry = shownEntries.value.find((e) => e.name === name)
    return entry ? { placement: 'left', effect: 'light', data: { entry, permissions: filePermissions(entry) } } : undefined
  }
})
function onListScroll() {
  virtual.onScroll()
  hoverTip.hide()
}

let unlisten: (() => void) | undefined
onMounted(async () => {
  if (props.directoryOnly) return
  unlisten = await onFileDrop(
    (x, y) => {
      const r = root.value?.getBoundingClientRect()
      return !!r && r.width > 0 && x >= r.left && x <= r.right && y >= r.top && y <= r.bottom
    },
    (paths) => upload(paths),
    (over) => (dropOver.value = over && !xfer.value),
  )
  if (disposed) unlisten?.()
})
onBeforeUnmount(() => {
  disposed = true
  context++; loadSeq++; transferSeq++
  listingReads.cancelAll()
  unlisten?.()
  xfer.value?.kill()
  sizes.clear()
  hoverTip.hide()
})

/** 连接问题：没核对主机公钥、要密码时弹出对应的框，处理好后自动重试 */
function handle(e: unknown, retry: () => void): string {
  const msg = (e as Error).message
  const d: ErrorDetail | undefined = e instanceof FerryError ? e.detail : undefined
  if (d?.kind === 'unknown-host' || (e instanceof FerryError && e.unknownHost)) {
    waitTrust = true
    trustRequest.value = { target: props.target, config: props.config }
  } else if (d && isSecretKind(d.kind)) {
    secretRequest.value = { detail: d, config: props.config, retry }
  }
  return msg
}

/**
 * 列出目录。soft：改名、删除、上传等操作后刷新同一个目录——留着勾选、焦点和已经统计的大小（条目变了的大小自动作废）；
 * 否则（换目录、点刷新）全部重来。
 */
async function load(dir: string, initialFocus?: string, soft = false) {
  const seq = ++loadSeq
  if (!soft) sizes.clear()
  listingReads.cancelAll()
  requestedPath = dir
  requestedFocus = initialFocus
  loading.value = true
  waitTrust = false
  try {
    const r = await listingReads.run('listing', (s) => readCall<DirListing>(['files', 'ls', props.target, ...(dir ? [dir] : []), '-c', props.config], s))
    if (seq !== loadSeq) return
    if (!r) throw new Error('目录读取失败')
    const samePath = path.value === r.path
    if (!samePath) query.value = ''
    path.value = r.path
    pathInput.value = r.path
    home.value = r.home
    entries.value = r.entries
    if (soft && samePath) {
      // 还在的留着勾选和焦点
      const names = new Set(r.entries.map((e) => e.name))
      const kept = [...selectedNames.value].filter((name) => names.has(name))
      if (kept.length !== selectedNames.value.size) selectedNames.value = new Set(kept)
      if (focusedName.value && !names.has(focusedName.value)) focusedName.value = undefined
    } else {
      if (soft) sizes.clear() // 操作后刷新时已经换了目录
      clearSelection()
      if (!samePath) virtual.scrollToTop()
    }
    const focus = r.focus || initialFocus
    if (focus && shownEntries.value.some((entry) => entry.name === focus)) {
      focusedName.value = focus
      nextTick(() => {
        if (seq !== loadSeq) return
        focusList()
        scrollToName(focus)
      })
    }
    error.value = ''
  } catch (e) {
    if (seq !== loadSeq || e instanceof ReadCanceled) return
    error.value = handle(e, () => { if (seq === loadSeq) load(dir, initialFocus) })
    pathInput.value = dir
    if (!path.value) editingPath.value = true
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

watch(query, clearSelection)

function clearSelection() {
  if (selectedNames.value.size) selectedNames.value = new Set()
  selectAnchor = undefined
  focusedName.value = undefined
}

function focusList() {
  scroller.value?.focus({ preventScroll: true })
}

const canSelect = (row: FileEntry) => !busy.value && !isDeleting(row)
const selectableCount = computed(() => (busy.value ? 0 : shownEntries.value.reduce((n, row) => n + (isDeleting(row) ? 0 : 1), 0)))
const allChecked = computed(() => selectableCount.value > 0 && selected.value.length >= selectableCount.value)

/** 点复选框：勾上 / 取消；按住 Shift 时把上次点的那行到这行都设成一样 */
function toggleRow(row: FileEntry, event: MouseEvent) {
  if (!canSelect(row)) return
  const next = new Set(selectedNames.value)
  const on = !next.has(row.name)
  const list = shownEntries.value
  const from = event.shiftKey && selectAnchor ? list.findIndex((e) => e.name === selectAnchor) : -1
  const to = list.indexOf(row)
  if (from >= 0 && to >= 0) {
    for (let i = Math.min(from, to); i <= Math.max(from, to); i++) {
      if (!canSelect(list[i])) continue
      if (on) next.add(list[i].name)
      else next.delete(list[i].name)
    }
  } else if (on) next.add(row.name)
  else next.delete(row.name)
  selectedNames.value = next
  selectAnchor = row.name
  focusedName.value = row.name
}

function toggleAll() {
  if (!selectableCount.value) return
  if (allChecked.value) {
    clearSelection()
    return
  }
  const all = shownEntries.value.filter(canSelect)
  selectedNames.value = new Set(all.map((row) => row.name))
  selectAnchor = undefined
  focusedName.value = all[0]?.name
}

// 行点击只定位，勾选由复选框处理；打开由 dblclick 单独处理。
function rowClick(row: FileEntry, event: MouseEvent) {
  if (busy.value || isDeleting(row) || event.detail > 1) return
  focusedName.value = row.name
  focusList()
}

function clearFocusOnBackground(event: MouseEvent) {
  if (busy.value || !(event.target instanceof Element) || event.target.closest('.file-row, .file-head, input, button, .el-checkbox')) return
  focusedName.value = undefined
  focusList()
}

function viewFile(row: FileEntry) {
  if (!props.directoryOnly && !busy.value && !row.dir && !isDeleting(row)) preview.value = { entry: row, dir: path.value }
}

function openEntry(row: FileEntry) {
  if (busy.value || isDeleting(row)) return
  if (row.dir) load(join(path.value, row.name))
  else viewFile(row)
}

function rowDblclick(row: FileEntry, event: MouseEvent) {
  if (event.ctrlKey || event.metaKey || event.shiftKey) return
  openEntry(row)
}

function listKeydown(event: KeyboardEvent) {
  const target = event.target
  if (target instanceof Element && target.closest('input, textarea, button, a, [contenteditable="true"]')) return
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'a') {
    event.preventDefault()
    return
  }
  if (busy.value) return
  const focusedRow = shownEntries.value.find((row) => row.name === focusedName.value)
  if (event.key === 'Enter' && focusedRow) {
    event.preventDefault(); openEntry(focusedRow)
  } else if (!props.directoryOnly && event.key === 'F2' && selected.value.length === 1) {
    event.preventDefault(); rename()
  } else if (!props.directoryOnly && event.key === 'Delete' && selected.value.length) {
    event.preventDefault(); remove()
  } else if (event.key === 'Escape') {
    event.preventDefault(); clearSelection()
  } else if ((event.altKey && event.key === 'ArrowUp') || event.key === 'Backspace') {
    event.preventDefault(); if (parent.value) load(parent.value)
  } else if (event.key === 'F5') {
    event.preventDefault(); load(path.value)
  } else if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
    event.preventDefault()
    const rows = shownEntries.value.filter((row) => !isDeleting(row))
    if (!rows.length) return
    const current = rows.findIndex((row) => row.name === focusedName.value)
    let index: number
    if (event.key === 'Home') index = 0
    else if (event.key === 'End') index = rows.length - 1
    else if (current < 0) index = event.key === 'ArrowUp' ? rows.length - 1 : 0
    else index = Math.max(0, Math.min(rows.length - 1, current + (event.key === 'ArrowDown' ? 1 : -1)))
    const row = rows[index]
    focusedName.value = row.name
    scrollToName(row.name)
  }
}

async function run(args: string[], ok: string) {
  if (busy.value) return
  const at = context
  const dir = path.value
  working.value = '正在处理文件…'
  try {
    await call([...args, '-c', props.config])
    if (at !== context) return
    if (ok) ElMessage.success(ok)
  } catch (e) {
    if (at !== context) return
    ElMessage.error({ message: handle(e, () => { if (at === context) run(args, ok) }), duration: 6000 })
    return
  } finally {
    if (at === context) working.value = ''
  }
  if (path.value === dir) load(dir, undefined, true)
}

async function mkdir() {
  if (busy.value) return
  const at = context
  const dir = path.value
  let name: string
  try {
    ;({ value: name } = await ElMessageBox.prompt('文件夹名', '新建文件夹', { inputValidator: (v) => validRemoteName(v) || '请输入有效的文件夹名，不能用 .、.. 或 /' }))
  } catch {
    return
  }
  if (at !== context || dir !== path.value) return
  run(['files', 'mkdir', props.target, join(dir, name.trim())], '文件夹已创建')
}

async function rename() {
  if (busy.value) return
  const at = context
  const dir = path.value
  const row = selected.value[0]
  if (!row) return
  let name: string
  try {
    ;({ value: name } = await ElMessageBox.prompt('新名字', `改名：${row.name}`, { inputValue: row.name, inputValidator: (v) => validRemoteName(v) || '请输入有效的名字，不能用 .、.. 或 /' }))
  } catch {
    return
  }
  if (at !== context || dir !== path.value || name.trim() === row.name) return
  run(['files', 'mv', props.target, join(dir, row.name), join(dir, name.trim())], '已改名')
}

async function remove() {
  if (busy.value) return
  const at = context
  const dir = path.value
  const rows = selected.value
  if (!rows.length) return
  const names = rows.map((r) => r.name + (r.dir ? '/' : ''))
  try {
    await ElMessageBox.confirm(
      `删除 ${names.slice(0, 5).join('、')}${names.length > 5 ? ` 等 ${names.length} 项` : ''}？文件夹连同里面的东西一起删除，不能恢复。`,
      '删除',
      { type: 'warning', confirmButtonText: '删除' },
    )
  } catch {
    return
  }
  if (at !== context || dir !== path.value) return
  const paths = rows.map((r) => join(dir, r.name))
  const what = names.length === 1 ? names[0] : `${names.length} 项`
  deleting.value = new Set([...deleting.value, ...paths])
  clearSelection()
  const done = () => {
    const left = new Set(deleting.value)
    for (const p of paths) left.delete(p)
    deleting.value = left
  }
  try {
    await call(['files', 'rm', props.target, ...paths, '-c', props.config])
    if (at !== context) return
    // 删掉了：直接从列表里去掉，不用等刷新
    if (path.value === dir) {
      const gone = new Set(rows.map((r) => r.name))
      entries.value = entries.value.filter((e) => !gone.has(e.name))
    }
    done()
    ElMessage.success(`已删除 ${what}`)
  } catch (e) {
    if (at !== context) return
    done()
    ElMessage.error({ message: handle(e, () => {}), duration: 6000 })
  }
  // 还在这个文件夹时在后台刷新一次（可能没删全、或者别人也改了）
  if (path.value === dir && !loading.value) load(dir, undefined, true)
}

async function pickUpload(directory: boolean) {
  const at = context
  const paths = await pickMany(directory ? '选择要上传的文件夹' : '选择要上传的文件', directory)
  if (at === context && paths.length) upload(paths)
}

function upload(locals: string[]) {
  if (busy.value || !path.value) {
    ElMessage.warning('请等待当前操作完成后再上传')
    return
  }
  const dir = path.value
  const what = locals.length === 1 ? (locals[0].split(/[\\/]/).pop() ?? '') : `${locals.length} 项`
  transfer('upload', ['files', 'upload', props.target, '--dir', dir, ...locals], `上传 ${what} 到 ${dir}`, () => {
    ElMessage.success(`已上传 ${what}`)
    if (path.value === dir) load(dir, undefined, true)
  })
}

const downloadDirKey = 'ferry:downloadDir'

async function download(rows: FileEntry[], remoteDir = path.value) {
  if (!rows.length) return
  if (busy.value) {
    ElMessage.warning('请等待当前操作完成后再下载')
    return
  }
  const at = context
  const dir = await pickDir('下载到哪个文件夹')
  if (!dir || at !== context || busy.value) return
  try {
    localStorage.setItem(downloadDirKey, dir)
  } catch {
    // 只是记一下
  }
  const what = rows.length === 1 ? rows[0].name : `${rows.length} 项`
  const first = rows[0].name
  transfer('download', ['files', 'download', props.target, '--to', dir, ...rows.map((r) => join(remoteDir, r.name))], `下载 ${what}`, () => {
    ElNotification({
      type: 'success',
      title: '下载完成',
      message: `${what} 保存在 ${dir}（点这里打开文件夹）`,
      onClick: () => reveal(dir.replace(/[\\/]$/, '') + (dir.includes('\\') ? '\\' : '/') + first),
    })
  })
}

/** 上传 / 下载：显示进度，可以取消；那边已有同名的先问要不要覆盖 */
function transfer(kind: 'upload' | 'download', args: string[], what: string, done: () => void, overwrite = false) {
  if (xfer.value) return
  const seq = ++transferSeq
  const at = context
  const r = ferry([...args, '-c', props.config, ...(overwrite ? ['--overwrite'] : [])], (l) => {
    if (seq === transferSeq && l.kind === 'progress' && xfer.value) {
      xfer.value.file = l.label ?? ''
      xfer.value.done = l.done ?? 0
      xfer.value.total = l.total ?? 0
    }
  })
  xfer.value = { kind, what, file: '', done: 0, total: 0, kill: r.kill }
  r.done.then(async (res) => {
    if (seq !== transferSeq) return
    xfer.value = undefined
    if (res.ok) return done()
    if (res.error === '已取消') return ElMessage.info('已取消')
    if (res.detail?.kind === 'files-exist') {
      try {
        const names = res.detail.names ?? []
        await ElMessageBox.confirm(
          `${kind === 'upload' ? '服务器上' : '本机'}已经有 ${names.slice(0, 5).join('、')}${names.length > 5 ? ` 等 ${names.length} 项` : ''}，覆盖吗？`,
          '已经存在',
          { type: 'warning', confirmButtonText: '覆盖' },
        )
      } catch {
        return
      }
      if (seq === transferSeq) return transfer(kind, args, what, done, true)
    }
    const err = new FerryError(res.error ?? '未知错误', res.detail)
    ElMessage.error({ message: handle(err, () => { if (at === context && seq === transferSeq) transfer(kind, args, what, done, overwrite) }), duration: 6000 })
  })
}

function time(t: string): string {
  const d = new Date(t)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}
</script>

<template>
  <div ref="root" class="files" :class="{ over: dropOver, 'glass-head': glass.enabled, 'directory-picker': directoryOnly }">
    <div class="bar">
      <el-tooltip :show-after="HOVER_DELAY" content="上一级" placement="bottom">
        <el-button size="small" :icon="Back" aria-label="上一级目录" :disabled="!parent" @click="load(parent)" />
      </el-tooltip>
      <el-tooltip :show-after="HOVER_DELAY" content="主目录" placement="bottom">
        <el-button size="small" :icon="HomeFilled" aria-label="主目录" @click="load(home)" />
      </el-tooltip>
      <el-input v-if="editingPath" v-model="pathInput" size="small" class="path" aria-label="服务器路径" @keyup.enter="load(pathInput.trim()); editingPath = false" @keyup.esc="editingPath = false" />
      <nav v-else class="file-breadcrumbs" aria-label="服务器文件路径">
        <button type="button" aria-label="根目录" title="/" @click="load('/')">/</button>
        <template v-for="(crumb, i) in breadcrumbs" :key="crumb.path">
          <el-icon class="file-crumb-separator" aria-hidden="true"><ArrowRight /></el-icon>
          <button type="button" :class="{ current: i === breadcrumbs.length - 1 }" :aria-current="i === breadcrumbs.length - 1 ? 'page' : undefined" :title="crumb.path" @click="load(crumb.path)">{{ crumb.label }}</button>
        </template>
      </nav>
      <el-button size="small" text @click="editingPath = !editingPath">{{ editingPath ? '收起' : '路径' }}</el-button>
      <el-tooltip :show-after="HOVER_DELAY" content="刷新" placement="bottom">
        <el-button size="small" :icon="Refresh" :loading="loading" aria-label="刷新目录" @click="load(path)" />
      </el-tooltip>
    </div>
    <div class="bar">
      <el-dropdown v-if="!directoryOnly" trigger="click" @command="(c: string) => pickUpload(c === 'dir')">
        <el-button size="small" type="primary" :icon="Upload" :disabled="busy || !path">上传</el-button>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item command="files">选择文件…</el-dropdown-item>
            <el-dropdown-item command="dir">选择文件夹…</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
      <el-button v-if="!directoryOnly" size="small" :icon="View" :disabled="busy || selected.length !== 1 || selected[0]?.dir || (selected[0] && isDeleting(selected[0]))" @click="viewFile(selected[0])">查看内容</el-button>
      <el-button v-if="!directoryOnly" size="small" :icon="Download" :disabled="busy || !selected.length" @click="download(selected)">下载</el-button>
      <el-button size="small" :icon="FolderAdd" :disabled="busy || !path" @click="mkdir">新建文件夹</el-button>
      <el-button v-if="!directoryOnly" size="small" :icon="EditPen" :disabled="busy || selected.length !== 1" @click="rename">改名</el-button>
      <el-button v-if="!directoryOnly" size="small" :icon="Delete" :disabled="busy || !selected.length" type="danger" plain @click="remove">删除</el-button>
    </div>

    <div class="file-filter"><el-input v-model="query" size="small" :placeholder="directoryOnly ? '搜索子目录' : '搜索当前文件夹'" clearable :aria-label="directoryOnly ? '搜索子目录' : '搜索当前文件夹'" /><span class="muted small">{{ shownEntries.length }} / {{ directoryEntries.length }} 项<span v-if="selected.length"> · 已勾选 {{ selected.length }} 项</span></span><el-button v-if="selected.length" size="small" text @click="clearSelection">取消勾选</el-button></div>
    <el-alert v-if="error" type="error" :closable="false" show-icon class="err" :title="error"><el-button link type="primary" @click="load(requestedPath, requestedFocus)">重试</el-button></el-alert>
    <div v-loading="loading" class="list-wrap">
      <div
        ref="scroller"
        class="list"
        tabindex="0"
        role="table"
        :aria-label="directoryOnly ? '服务器目录列表' : '服务器文件列表'"
        :aria-rowcount="shownEntries.length + 1"
        @scroll="onListScroll"
        @click="clearFocusOnBackground"
        @keydown="listKeydown"
        @mouseover="hoverTip.over"
        @focusin="hoverTip.over"
        @mouseleave="hoverTip.hide"
        @focusout="hoverTip.hide"
      >
        <div ref="head" class="file-head file-grid" role="row" aria-rowindex="1">
          <span v-if="!directoryOnly" class="file-check" role="columnheader" @click.stop="toggleAll">
            <el-checkbox :model-value="allChecked" :indeterminate="!allChecked && selected.length > 0" :disabled="!selectableCount" aria-label="全选" @click.prevent />
          </span>
          <span role="columnheader">名称</span>
          <span v-if="!directoryOnly" role="columnheader" class="file-size">大小</span>
          <span role="columnheader">修改时间</span>
          <span role="columnheader">权限</span>
        </div>
        <div class="file-body" :style="{ height: `${virtual.total.value}px` }">
          <div
            v-for="item in visibleRows"
            :key="item.row.name"
            class="file-row file-grid"
            :class="{ 'is-deleting': isDeleting(item.row), 'file-selected': selectedNames.has(item.row.name), 'file-focused': focusedName === item.row.name }"
            :style="{ transform: `translateY(${item.top}px)`, height: `${item.height}px` }"
            role="row"
            :aria-rowindex="item.index + 2"
            :aria-selected="directoryOnly ? focusedName === item.row.name : selectedNames.has(item.row.name)"
            :data-name="item.row.name"
            @click="rowClick(item.row, $event)"
            @dblclick="rowDblclick(item.row, $event)"
          >
            <span v-if="!directoryOnly" class="file-check" role="cell" @click.stop="toggleRow(item.row, $event)" @dblclick.stop>
              <el-checkbox :model-value="selectedNames.has(item.row.name)" :disabled="!canSelect(item.row)" :aria-label="`勾选 ${item.row.name}`" @click.prevent />
            </span>
            <span class="name" :class="{ dir: item.row.dir }" role="cell">
              <el-icon><Folder v-if="item.row.dir" /><Document v-else /></el-icon>
              <span class="name-text" data-tip="name">{{ item.row.name }}</span>
              <el-icon v-if="item.row.link" class="link"><Link /></el-icon>
              <span v-if="isDeleting(item.row)" class="deleting">删除中…</span>
            </span>
            <span v-if="!directoryOnly" class="file-size" role="cell">
              <FileSizeCell
                :entry="item.row"
                :state="sizes.get(join(path, item.row.name), item.row)"
                :disabled="busy || isDeleting(item.row)"
                @measure="sizes.measure(config, target, join(path, item.row.name), item.row)"
                @cancel="sizes.cancel(join(path, item.row.name))"
              />
            </span>
            <span role="cell">{{ time(item.row.mtime) }}</span>
            <span role="cell">
              <span class="permission-summary" data-tip="perm" tabindex="0" @click.stop @dblclick.stop>
                <span v-for="line in item.permissions.summary" :key="line.role" class="permission-line"><span v-if="line.role" class="permission-role">{{ line.role }}</span><span>{{ line.text }}</span></span>
                <span v-if="item.permissions.special.length" class="permission-special">含特殊权限</span>
              </span>
            </span>
          </div>
        </div>
        <div v-if="!shownEntries.length && !loading" class="file-empty">{{ emptyText }}</div>
      </div>
    </div>
    <el-tooltip :visible="!!hoverTip.tip.value" virtual-triggering :virtual-ref="hoverTip.tip.value?.el" :placement="hoverTip.tip.value?.placement ?? 'top'" :effect="hoverTip.tip.value?.effect ?? 'dark'">
      <template #content>
        <FilePermissionDetails v-if="hoverTip.tip.value?.data" :permissions="hoverTip.tip.value.data.permissions" :mode="hoverTip.tip.value.data.entry.mode" :dir="hoverTip.tip.value.data.entry.dir" />
        <span v-else>{{ hoverTip.tip.value?.text }}</span>
      </template>
    </el-tooltip>

    <div v-if="xfer" class="xfer">
      <div class="xfer-head">
        <span class="xfer-what">{{ xfer.what }}</span>
        <el-button link size="small" @click="xfer.kill()">取消</el-button>
      </div>
      <el-progress :percentage="percent" :stroke-width="6" />
      <div class="muted small mono">{{ xfer.file }} {{ xfer.total ? `${bytes(xfer.done)} / ${bytes(xfer.total)}` : '' }}</div>
    </div>
    <div v-else class="muted small tip" role="status">{{ working || (directoryOnly ? '单击选中目录，双击进入；未选中子目录时使用当前目录' : deleting.size ? `正在删除 ${deleting.size} 项，可以继续浏览别的文件夹` : '双击打开文件或文件夹；勾选复选框后可下载、改名或删除；可拖入文件上传') }}</div>
    <div v-if="dropOver" class="drop">松开就上传到 {{ path }}</div>
    <FilePreviewDialog v-if="preview" :config="config" :target="target" :path="join(preview.dir, preview.entry.name)" @close="preview = undefined" @download="download([preview!.entry], preview!.dir)" @saved="load(path, undefined, true)" />
  </div>
</template>

<style scoped>
/* 液态玻璃：表头吸顶，文件行从它下面滚过，表头是磨砂玻璃 */
.glass-head .file-head {
  /* 深浅跟着设置里的「玻璃透明度」（--cg-k、--cg-blur-k，见 glass/runtime.ts） */
  background: color-mix(in srgb, var(--el-bg-color) min(100%, calc(55% * var(--cg-k, 1))), transparent);
  -webkit-backdrop-filter: blur(calc(14px * var(--cg-blur-k, 1))) saturate(1.3);
  backdrop-filter: blur(calc(14px * var(--cg-blur-k, 1))) saturate(1.3);
}
.file-breadcrumbs { display: flex; align-items: center; gap: 2px; flex: 1; min-width: 0; overflow-x: auto; white-space: nowrap; font-size: 16px; line-height: 24px; }
.file-breadcrumbs .file-crumb-separator { flex-shrink: 0; color: var(--el-text-color-placeholder); font-size: 12px; }
.file-breadcrumbs button { flex-shrink: 0; border: 0; background: none; color: var(--el-text-color-regular); cursor: pointer; padding: 4px 7px; border-radius: 5px; font: inherit; }
.file-breadcrumbs button.current { color: var(--el-text-color-primary); font-weight: 600; }
.file-breadcrumbs button:hover { color: var(--el-color-primary); background: var(--el-fill-color-light); }
.file-breadcrumbs button:focus-visible { outline: 2px solid var(--el-color-primary-light-5); outline-offset: -2px; }
.file-filter { display: flex; align-items: center; gap: 12px; }
.file-filter .el-input { flex: 1; min-width: 100px; }
.file-filter > span { white-space: nowrap; }
.bar :deep(.el-button--small), .file-filter :deep(.el-button--small) { font-size: 14px; height: 30px; }
.path, .file-filter .el-input { --el-input-height: 30px; --el-input-text-height: 28px; --el-input-text-color: var(--el-text-color-regular); font-size: 14px; }
.files {
  position: relative;
  box-sizing: border-box;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
  height: 100%;
  min-height: 0;
  padding: 6px 8px;
  background: var(--el-bg-color);
  color: var(--el-text-color-primary);
}
.bar {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-shrink: 0;
  overflow-x: auto;
}
.bar > *:not(.path, .file-breadcrumbs) { flex-shrink: 0; }
.bar .el-button + .el-button {
  margin-left: 0;
}
/* 加载遮罩默认向外扩 1px，会触发工具栏的自动滚动并撑高这一行。 */
.bar :deep(.el-button.is-loading::before) {
  inset: 0;
}
.path {
  flex: 1;
}
.err {
  padding: 4px 8px;
}
.list-wrap {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
}
.list {
  flex: 1;
  min-width: 0;
  overflow: auto;
  font-size: 15px;
  color: var(--el-text-color-regular);
  border-bottom: 1px solid var(--el-border-color-lighter);
}
.list:focus { outline: none; }
/* 列宽和原来的表格一样；窄了左右滚动（表头在同一个滚动区里，跟着一起动） */
.file-grid {
  display: grid;
  grid-template-columns: 34px minmax(160px, 1fr) 124px 156px 270px;
  min-width: 744px;
  align-items: center;
}
.directory-picker .file-grid { grid-template-columns: minmax(160px, 1fr) 156px 270px; min-width: 586px; }
.file-grid > span { min-width: 0; padding: 0 10px; }
.file-grid > .file-check { padding: 0 0 0 10px; display: flex; align-items: center; }
.file-check .el-checkbox { height: auto; }
.file-grid > .file-size { text-align: right; }
.file-head {
  position: sticky;
  top: 0;
  z-index: 3;
  height: 40px;
  box-sizing: border-box;
  border-bottom: 1px solid var(--el-border-color-lighter);
  background: var(--el-bg-color);
  color: var(--el-text-color-secondary);
  font-size: 14px;
  font-weight: 500;
}
.file-body { position: relative; }
/* 高度由脚本按权限摘要的行数给（rowHeight），这里不能让内容把行撑高 */
.file-row {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  box-sizing: border-box;
  overflow: hidden;
  border-bottom: 1px solid var(--el-border-color-lighter);
  user-select: none;
  -webkit-user-select: none;
  line-height: 24px;
}
.file-row:hover { background: var(--el-fill-color-light); }
.file-row.file-selected { background: var(--el-color-primary-light-9); }
.directory-picker .file-row.file-focused { background: var(--el-color-primary-light-9); }
.list:focus .file-row.file-focused { outline: 1px solid var(--el-color-primary-light-5); outline-offset: -1px; }
.file-empty { padding: 40px 0; text-align: center; color: var(--el-text-color-secondary); font-size: 14px; }
.name {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--el-text-color-primary);
}
.name > .el-icon { flex-shrink: 0; }
.name-text { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.deleting { flex-shrink: 0; margin-left: 2px; font-size: 12px; color: var(--el-color-danger); }
.file-row.is-deleting { opacity: .55; cursor: default; }
.permission-summary { display: inline-flex; flex-direction: column; max-width: 100%; border-radius: 4px; cursor: help; }
.permission-summary:focus-visible { outline-offset: 1px; }
.permission-line { display: flex; flex-wrap: nowrap; column-gap: 8px; line-height: 22px; white-space: nowrap; }
.permission-role { color: var(--el-text-color-secondary); }
.permission-special { color: var(--el-text-color-secondary); font-size: 11px; line-height: 16px; }
.name.dir {
  color: var(--el-color-primary);
  cursor: pointer;
}
.link {
  color: var(--el-text-color-secondary);
}
.xfer {
  border-top: 1px solid var(--el-border-color-lighter);
  padding-top: 4px;
}
.xfer-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.xfer-what {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 14px;
}
.small {
  font-size: 13px;
}
.tip {
  padding-top: 2px;
}
.drop {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: color-mix(in srgb, var(--el-color-primary) 15%, transparent);
  border: 2px dashed var(--el-color-primary);
  color: var(--el-color-primary);
  font-size: 15px;
  pointer-events: none;
}
</style>
