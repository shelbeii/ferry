<script setup lang="ts">
// 服务器上的文件（SFTP，以登录用户的身份）：浏览目录、查看和编辑内容、上传、下载、新建文件夹、改名、删除。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox, ElNotification } from 'element-plus'
import { ArrowRight, Back, Delete, Document, Download, EditPen, Folder, FolderAdd, HomeFilled, Link, Refresh, Upload, View } from '@element-plus/icons-vue'
import { bytes, call, ferry, FerryError, isSecretKind, type DirListing, type ErrorDetail, type FileEntry, type Running } from '../ferry'
import { validRemoteName } from '../fileNames'
import FilePermissions from './FilePermissions.vue'
import FilePreviewDialog from './FilePreviewDialog.vue'
import { onFileDrop, pickDir, pickMany, reveal } from '../platform'
import { secretRequest, trustRequest, trustedTick } from '../state'
import { glass, setVar } from '../glass/runtime'

const props = defineProps<{ config: string; target: string }>()

const path = ref('')
const home = ref('')
const pathInput = ref('')
const editingPath = ref(false)
const breadcrumbs = computed(() => path.value.split('/').filter(Boolean).map((label, i, all) => ({ label, path: '/' + all.slice(0, i + 1).join('/') })))
const entries = ref<FileEntry[]>([])
const loading = ref(false)
const error = ref('')
const selected = ref<FileEntry[]>([])
const table = ref<{ clearSelection: () => void }>()
const focusedName = ref<string>()
const preview = ref<{ entry: FileEntry; dir: string }>()
const query = ref('')
const shownEntries = computed(() => entries.value.filter((e) => e.name.toLowerCase().includes(query.value.trim().toLowerCase())).slice().sort((a, b) => Number(b.dir) - Number(a.dir) || a.name.localeCompare(b.name, 'zh-CN', { numeric: true })))
const selectedNames = computed(() => new Set(selected.value.map((row) => row.name)))
const working = ref('')
/** 正在删除的（完整路径）：删除在后台进行，别的文件夹照常能进、能操作 */
const deleting = ref(new Set<string>())
const isDeleting = (row: FileEntry) => deleting.value.has(join(path.value, row.name))
const busy = computed(() => loading.value || !!working.value || !!xfer.value)
const xfer = ref<{ kind: 'upload' | 'download'; what: string; file: string; done: number; total: number; kill: () => Promise<void> }>()
const dropOver = ref(false)
const root = ref<HTMLDivElement>()
let waitTrust = false
let listing: Running<DirListing> | undefined
let loadSeq = 0
let transferSeq = 0
let context = 0
let requestedPath = ''
let disposed = false

const join = (dir: string, name: string) => (dir.endsWith('/') ? dir + name : dir + '/' + name)
const parent = computed(() => (path.value === '/' ? '' : path.value.replace(/\/[^/]+\/?$/, '') || '/'))
const percent = computed(() => (xfer.value && xfer.value.total > 0 ? Math.min(100, Math.round((xfer.value.done / xfer.value.total) * 100)) : 0))

watch(() => `${props.config}|${props.target}`, () => {
  context++
  transferSeq++
  xfer.value?.kill()
  xfer.value = undefined
  working.value = ''
  deleting.value = new Set()
  preview.value = undefined
  path.value = ''; home.value = ''; entries.value = []; clearSelection(); query.value = ''; editingPath.value = false
  load('')
}, { immediate: true })
watch(trustedTick, () => {
  if (waitTrust) load(requestedPath)
})

let unlisten: (() => void) | undefined
// 液态玻璃：量出表头高度，文件列表顶部留出这么多，第一行不被浮着的表头挡住
// 表格的表头比这里的 onMounted 晚出来：看整个面板的尺寸变化，表头出来后也看它
let headResize: ResizeObserver | undefined
let observedHead: HTMLElement | undefined
function measureHead() {
  const head = root.value?.querySelector<HTMLElement>('.el-table__header-wrapper')
  if (!head) return
  if (head !== observedHead) { observedHead = head; headResize?.observe(head) }
  if (root.value) setVar(root.value, '--th-h', `${head.offsetHeight}px`)
}
onMounted(() => {
  headResize = new ResizeObserver(measureHead)
  if (root.value) headResize.observe(root.value)
  requestAnimationFrame(measureHead)
})
onBeforeUnmount(() => headResize?.disconnect())
onMounted(async () => {
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
  listing?.kill()
  unlisten?.()
  xfer.value?.kill()
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

async function load(dir: string) {
  const seq = ++loadSeq
  listing?.kill()
  requestedPath = dir
  loading.value = true
  waitTrust = false
  const proc = ferry<DirListing>(['files', 'ls', props.target, ...(dir ? [dir] : []), '-c', props.config])
  listing = proc
  try {
    const result = await proc.done
    if (seq !== loadSeq) return
    if (!result.ok || !result.data) throw new FerryError(result.error ?? '目录读取失败', result.detail)
    const r = result.data
    if (path.value !== r.path) query.value = ''
    path.value = r.path
    pathInput.value = r.path
    home.value = r.home
    entries.value = r.entries
    clearSelection()
    error.value = ''
  } catch (e) {
    if (seq !== loadSeq) return
    error.value = handle(e, () => { if (seq === loadSeq) load(dir) })
    pathInput.value = path.value
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

watch(query, clearSelection)

function clearSelection() {
  selected.value = []
  focusedName.value = undefined
  table.value?.clearSelection()
}

function focusList() {
  root.value?.querySelector<HTMLElement>('.el-table')?.focus({ preventScroll: true })
}

// 行点击只定位，勾选由复选框处理；打开由 dblclick 单独处理。
function rowClick(row: FileEntry, column: { type?: string } | null, event: MouseEvent) {
  if (busy.value || column?.type === 'selection' || isDeleting(row) || event.detail > 1) return
  focusedName.value = row.name
  focusList()
}

function checkboxSelect(_selection: FileEntry[], row: FileEntry) {
  focusedName.value = row.name
}

function selectAll(selection: FileEntry[]) {
  focusedName.value = selection[0]?.name
}

function clearFocusOnBackground(event: MouseEvent) {
  if (busy.value || !(event.target instanceof Element) || event.target.closest('tr, input, button, .el-checkbox, .el-scrollbar__bar')) return
  focusedName.value = undefined
  focusList()
}

function viewFile(row: FileEntry) {
  if (!busy.value && !row.dir && !isDeleting(row)) preview.value = { entry: row, dir: path.value }
}

function openEntry(row: FileEntry) {
  if (busy.value || isDeleting(row)) return
  if (row.dir) load(join(path.value, row.name))
  else viewFile(row)
}

function rowDblclick(row: FileEntry, column: { type?: string } | null, event: MouseEvent) {
  if (column?.type === 'selection' || event.ctrlKey || event.metaKey || event.shiftKey) return
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
  } else if (event.key === 'F2' && selected.value.length === 1) {
    event.preventDefault(); rename()
  } else if (event.key === 'Delete' && selected.value.length) {
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
    nextTick(() => {
      if (focusedName.value !== row.name) return
      const position = shownEntries.value.findIndex((item) => item.name === row.name)
      root.value?.querySelectorAll<HTMLElement>('.el-table__body-wrapper tr.el-table__row')[position]?.scrollIntoView({ block: 'nearest' })
    })
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
  if (path.value === dir) load(dir)
}

async function mkdir() {
  if (busy.value) return
  const at = context
  let name: string
  try {
    ;({ value: name } = await ElMessageBox.prompt('文件夹名', '新建文件夹', { inputValidator: (v) => validRemoteName(v) || '请输入有效的文件夹名，不能用 .、.. 或 /' }))
  } catch {
    return
  }
  if (at !== context) return
  run(['files', 'mkdir', props.target, join(path.value, name.trim())], '文件夹已创建')
}

async function rename() {
  if (busy.value) return
  const at = context
  const row = selected.value[0]
  if (!row) return
  let name: string
  try {
    ;({ value: name } = await ElMessageBox.prompt('新名字', `改名：${row.name}`, { inputValue: row.name, inputValidator: (v) => validRemoteName(v) || '请输入有效的名字，不能用 .、.. 或 /' }))
  } catch {
    return
  }
  if (at !== context || name.trim() === row.name) return
  run(['files', 'mv', props.target, join(path.value, row.name), join(path.value, name.trim())], '已改名')
}

async function remove() {
  if (busy.value) return
  const at = context
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
  if (at !== context) return
  const dir = path.value
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
    done()
    ElMessage.success(`已删除 ${what}`)
  } catch (e) {
    if (at !== context) return
    done()
    ElMessage.error({ message: handle(e, () => {}), duration: 6000 })
  }
  // 还在这个文件夹时刷新
  if (path.value === dir && !loading.value) load(dir)
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
    if (path.value === dir) load(dir)
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
  <div ref="root" class="files" :class="{ over: dropOver, 'glass-head': glass.enabled }">
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
      <el-dropdown trigger="click" @command="(c: string) => pickUpload(c === 'dir')">
        <el-button size="small" type="primary" :icon="Upload" :disabled="busy || !path">上传</el-button>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item command="files">选择文件…</el-dropdown-item>
            <el-dropdown-item command="dir">选择文件夹…</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
      <el-button size="small" :icon="View" :disabled="busy || selected.length !== 1 || selected[0]?.dir || (selected[0] && isDeleting(selected[0]))" @click="viewFile(selected[0])">查看内容</el-button>
      <el-button size="small" :icon="Download" :disabled="busy || !selected.length" @click="download(selected)">下载</el-button>
      <el-button size="small" :icon="FolderAdd" :disabled="busy || !path" @click="mkdir">新建文件夹</el-button>
      <el-button size="small" :icon="EditPen" :disabled="busy || selected.length !== 1" @click="rename">改名</el-button>
      <el-button size="small" :icon="Delete" :disabled="busy || !selected.length" type="danger" plain @click="remove">删除</el-button>
    </div>

    <div class="file-filter"><el-input v-model="query" size="small" placeholder="搜索当前文件夹" clearable aria-label="搜索当前文件夹" /><span class="muted small">{{ shownEntries.length }} / {{ entries.length }} 项<span v-if="selected.length"> · 已勾选 {{ selected.length }} 项</span></span><el-button v-if="selected.length" size="small" text @click="clearSelection">取消勾选</el-button></div>
    <el-alert v-if="error" type="error" :closable="false" show-icon class="err" :title="error"><el-button link type="primary" @click="load(requestedPath)">重试</el-button></el-alert>
    <el-table
      ref="table"
      v-loading="loading"
      :data="shownEntries"
      size="small"
      height="100%"
      class="list"
      tabindex="0"
      aria-label="服务器文件列表"
      row-key="name"
      :empty-text="error ? '目录读取失败，请重试' : query.trim() ? '没有匹配的文件' : '空文件夹'"
      @selection-change="(v: FileEntry[]) => (selected = v)"
      @select="checkboxSelect"
      @select-all="selectAll"
      @row-click="rowClick"
      @row-dblclick="rowDblclick"
      @click="clearFocusOnBackground"
      @keydown="listKeydown"
      :row-class-name="({ row }: { row: FileEntry }) => [isDeleting(row) ? 'is-deleting' : '', selectedNames.has(row.name) ? 'file-selected' : '', focusedName === row.name ? 'file-focused' : ''].join(' ')"
    >
      <el-table-column type="selection" width="34" :selectable="(row: FileEntry) => !busy && !isDeleting(row)" />
      <el-table-column label="名称" min-width="160" show-overflow-tooltip>
        <template #default="{ row }">
          <span class="name" :class="{ dir: row.dir }">
            <el-icon><Folder v-if="row.dir" /><Document v-else /></el-icon>
            <span>{{ row.name }}</span>
            <el-icon v-if="row.link" class="link"><Link /></el-icon>
            <span v-if="isDeleting(row)" class="deleting">删除中…</span>
          </span>
        </template>
      </el-table-column>
      <el-table-column label="大小" width="88" align="right">
        <template #default="{ row }">{{ row.dir ? '' : bytes(row.size) }}</template>
      </el-table-column>
      <el-table-column label="修改时间" width="156">
        <template #default="{ row }">{{ time(row.mtime) }}</template>
      </el-table-column>
      <el-table-column label="权限" width="270">
        <template #default="{ row }"><FilePermissions :mode="row.mode" :dir="row.dir" :link="row.link" /></template>
      </el-table-column>
    </el-table>

    <div v-if="xfer" class="xfer">
      <div class="xfer-head">
        <span class="xfer-what">{{ xfer.what }}</span>
        <el-button link size="small" @click="xfer.kill()">取消</el-button>
      </div>
      <el-progress :percentage="percent" :stroke-width="6" />
      <div class="muted small mono">{{ xfer.file }} {{ xfer.total ? `${bytes(xfer.done)} / ${bytes(xfer.total)}` : '' }}</div>
    </div>
    <div v-else class="muted small tip" role="status">{{ working || (deleting.size ? `正在删除 ${deleting.size} 项，可以继续浏览别的文件夹` : '双击打开文件或文件夹；勾选复选框后可下载、改名或删除；可拖入文件上传') }}</div>
    <div v-if="dropOver" class="drop">松开就上传到 {{ path }}</div>
    <FilePreviewDialog v-if="preview" :config="config" :target="target" :path="join(preview.dir, preview.entry.name)" @close="preview = undefined" @download="download([preview!.entry], preview!.dir)" @saved="load(path)" />
  </div>
</template>

<style scoped>
/* 液态玻璃：表头浮在文件列表上，文件行从它下面滚过，表头是磨砂玻璃（高度 --th-h 由 headResize 量出） */
.glass-head :deep(.el-table__inner-wrapper) { position: relative; }
.glass-head :deep(.el-table__header-wrapper) {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  z-index: 3;
  /* 深浅跟着设置里的「玻璃透明度」（--cg-k、--cg-blur-k，见 glass/runtime.ts） */
  background: color-mix(in srgb, var(--el-bg-color) min(100%, calc(55% * var(--cg-k, 1))), transparent);
  -webkit-backdrop-filter: blur(calc(14px * var(--cg-blur-k, 1))) saturate(1.3);
  backdrop-filter: blur(calc(14px * var(--cg-blur-k, 1))) saturate(1.3);
}
.glass-head :deep(.el-table__header-wrapper tr),
.glass-head :deep(.el-table__header-wrapper th.el-table__cell) { background: transparent; }
.glass-head :deep(.el-table__body-wrapper .el-scrollbar__view) { padding-top: var(--th-h, 0px); }
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
.list {
  flex: 1;
  min-height: 0;
  font-size: 15px;
}
.list :deep(th.el-table__cell) { font-size: 14px; }
.list :deep(.cell) { line-height: 24px; }
.list :deep(.permission-summary) { font-size: 15px; }
.list :deep(.el-table__body) { user-select: none; -webkit-user-select: none; }
.list :deep(.file-selected > td.el-table__cell) { background: var(--el-color-primary-light-9); }
.list:focus { outline: none; }
.list:focus :deep(.file-focused) { outline: 1px solid var(--el-color-primary-light-5); outline-offset: -1px; }
.name {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.deleting { margin-left: 8px; font-size: 12px; color: var(--el-color-danger); }
.list :deep(.is-deleting) { opacity: .55; cursor: default; }
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
