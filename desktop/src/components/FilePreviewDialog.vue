<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { CopyDocument, Download, EditPen, Finished, Refresh, Search } from '@element-plus/icons-vue'
import { Compartment, EditorState } from '@codemirror/state'
import { EditorView, drawSelection, highlightSpecialChars, keymap, lineNumbers } from '@codemirror/view'
import { defaultKeymap, history, historyKeymap } from '@codemirror/commands'
import { highlightSelectionMatches, openSearchPanel, search, searchKeymap } from '@codemirror/search'
import { bytes, ferry, FerryError, isSecretKind, type FilePreview, type Running } from '../ferry'
import { secretRequest, trustRequest, trustedTick } from '../state'

const props = defineProps<{ config: string; target: string; path: string; readArgs?: string[]; saveArgs?: string[]; downloadable?: boolean }>()
const emit = defineEmits<{ close: []; download: []; saved: [] }>()
const visible = ref(true)
const name = computed(() => props.path.split('/').pop() || props.path)
const loading = ref(false)
const error = ref('')
const data = ref<FilePreview>()
const host = ref<HTMLElement>()
const wrap = ref(false)
const editing = ref(false)
const draft = ref('')
const saving = ref(false)
const saveError = ref('')
const saved = ref(false)
const canRead = computed(() => !!data.value && !data.value.unsupported && !loading.value)
const canEdit = computed(() => canRead.value && !data.value!.truncated && !!data.value!.revision && !data.value!.readOnly)
const comparing = ref(false)
const normalize = (text: string) => text.replace(/\r\n?/g, '\n')
const dirty = computed(() => editing.value && draft.value !== normalize(data.value?.content ?? ''))
const wrapping = new Compartment()
const editMode = new Compartment()
let editor: EditorView | undefined
let reading: Running<FilePreview> | undefined
let writing: Running<FilePreview> | undefined
let seq = 0
let disposed = false
let waitTrust = false

function cancel() {
  seq++
  waitTrust = false
  reading?.kill()
  reading = undefined
  writing?.kill()
  writing = undefined
  editor?.destroy()
  editor = undefined
  data.value = undefined
  editing.value = false
  draft.value = ''
  saved.value = false
  saveError.value = ''
}

async function load() {
  if (disposed || !visible.value) return
  cancel()
  const at = seq
  loading.value = true
  error.value = ''
  const proc = ferry<FilePreview>([...(props.readArgs ?? ['files', 'preview', props.target, props.path]), '-c', props.config])
  reading = proc
  try {
    const result = await proc.done
    if (at !== seq) return
    if (!result.ok || !result.data) throw new FerryError(result.error || '文件读取失败', result.detail)
    data.value = result.data
    draft.value = normalize(result.data.content)
  } catch (e) {
    if (at !== seq) return
    error.value = (e as Error).message
    const detail = e instanceof FerryError ? e.detail : undefined
    if (detail?.kind === 'unknown-host' || (e instanceof FerryError && e.unknownHost)) {
      waitTrust = true
      trustRequest.value = { target: props.target, config: props.config }
    } else if (detail && isSecretKind(detail.kind)) {
      secretRequest.value = { detail, config: props.config, retry: () => { if (at === seq) load() } }
    }
  } finally {
    if (at === seq) {
      reading = undefined
      loading.value = false
    }
  }
  await nextTick()
  if (at !== seq || !canRead.value || !host.value) return
  editor = new EditorView({
    parent: host.value,
    state: EditorState.create({
      doc: draft.value,
      extensions: [
        editMode.of(editExtensions()),
        history(),
        EditorView.updateListener.of((update) => {
          if (update.docChanged) { draft.value = update.state.doc.toString(); saved.value = false }
        }),
        lineNumbers(), drawSelection(), highlightSpecialChars(), highlightSelectionMatches(),
        keymap.of([{ key: 'Mod-s', run: () => { save(); return true } }, ...searchKeymap, ...defaultKeymap, ...historyKeymap]),
        search({ top: true }),
        EditorState.phrases.of({ Find: '查找', Replace: '替换为', replace: '替换', 'replace all': '全部替换', next: '下一个', previous: '上一个', all: '全选匹配', 'match case': '区分大小写', regexp: '正则', 'by word': '全词', close: '关闭查找', 'current match': '当前匹配', 'on line': '所在行' }),
        wrapping.of(wrap.value ? EditorView.lineWrapping : []),
        EditorView.theme({
          '&': { height: '100%', color: 'var(--el-text-color-primary)', backgroundColor: 'var(--el-bg-color)' },
          '&.cm-focused': { outline: 'none' },
          '.cm-scroller': { overflow: 'auto', fontFamily: "'Cascadia Mono', Consolas, monospace", fontSize: '13px', lineHeight: '1.65' },
          '.cm-content': { padding: '10px 0' },
          '.cm-gutters': { backgroundColor: 'var(--el-fill-color-light)', color: 'var(--el-text-color-secondary)', borderRight: '1px solid var(--el-border-color-lighter)' },
          '.cm-panels': { backgroundColor: 'var(--el-fill-color-light)', color: 'var(--el-text-color-primary)' },
          '.cm-textfield, .cm-button': { background: 'var(--el-bg-color)', color: 'var(--el-text-color-primary)', border: '1px solid var(--el-border-color)', borderRadius: '4px' },
          '.cm-searchMatch': { backgroundColor: '#ffcc0040' },
          '.cm-searchMatch-selected': { backgroundColor: '#ff990080' },
          '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection': { backgroundColor: 'var(--el-color-primary-light-7)' },
        }),
      ],
    }),
  })
}

function editExtensions() {
  const writable = editing.value && !saving.value
  return [
    EditorState.readOnly.of(!writable),
    EditorView.editable.of(writable),
    EditorView.contentAttributes.of({ 'aria-label': writable ? '文件内容（可编辑）' : '文件内容（只读）', 'aria-readonly': String(!writable), tabindex: '0' }),
  ]
}

async function confirmDiscard(action: string): Promise<boolean> {
  if (!dirty.value) return true
  try {
    await ElMessageBox.confirm(`有未保存的修改，${action}将放弃这些修改。`, '未保存的修改', { type: 'warning', confirmButtonText: '放弃修改', cancelButtonText: '继续编辑' })
    return !disposed && visible.value
  } catch {
    return false
  }
}

async function toggleEdit() {
  if (!canEdit.value || saving.value) return
  if (editing.value) {
    if (!(await confirmDiscard('退出编辑'))) return
    editor?.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: normalize(data.value!.content) } })
    saveError.value = ''
  }
  editing.value = !editing.value
  await nextTick()
  editor?.focus()
}

async function refresh() {
  if (!saving.value && await confirmDiscard('重新读取')) load()
}

async function save() {
  if (!canEdit.value || !dirty.value || saving.value) return
  const at = seq
  const value = JSON.stringify({ revision: data.value!.revision, content: draft.value })
  saving.value = true
  saveError.value = ''
  const proc = ferry<FilePreview>([...(props.saveArgs ?? ['files', 'save', props.target, props.path]), '-c', props.config], undefined, { FERRY_FILE_VALUE: value })
  writing = proc
  try {
    const result = await proc.done
    if (at !== seq) return
    if (!result.ok || !result.data) throw new FerryError(result.error || '保存失败', result.detail)
    data.value = result.data
    // 保存保留原文件的编码和换行；编辑器内部统一用 LF。
    draft.value = normalize(result.data.content)
    saved.value = true
    ElMessage.success('文件已保存到服务器')
    emit('saved')
  } catch (e) {
    if (at !== seq) return
    saveError.value = (e as Error).message
    // 保存失败时保留草稿，重新登录后仍要由用户主动重试保存。
    const detail = e instanceof FerryError ? e.detail : undefined
    if (detail && isSecretKind(detail.kind)) secretRequest.value = { detail, config: props.config, retry: () => {} }
    else if (detail?.kind === 'unknown-host' || (e instanceof FerryError && e.unknownHost)) trustRequest.value = { target: props.target, config: props.config }
  } finally {
    if (at === seq) { saving.value = false; writing = undefined }
  }
}

async function beforeClose(done: () => void) {
  if (saving.value) { ElMessage.info('正在保存，请稍候'); return }
  if (await confirmDiscard('关闭窗口')) done()
}

function close() { beforeClose(() => { visible.value = false }) }

function find() {
  if (editor) { editor.focus(); openSearchPanel(editor) }
}

async function copy() {
  if (!canRead.value) return
  const preview = data.value!
  const text = editing.value ? draft.value : preview.content
  const message = editing.value ? '已复制当前编辑内容' : preview.truncated ? '已复制当前预览内容' : '已复制文件内容'
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success(message)
  } catch {
    ElMessage.error('复制失败，请手动选择内容复制')
  }
}

watch(wrap, (value) => editor?.dispatch({ effects: wrapping.reconfigure(value ? EditorView.lineWrapping : []) }))
watch([editing, saving], () => editor?.dispatch({ effects: editMode.reconfigure(editExtensions()) }), { flush: 'sync' })
watch(() => [props.config, props.target, props.path], load, { immediate: true })
watch(trustedTick, () => { if (waitTrust) load() })
onBeforeUnmount(() => { disposed = true; cancel() })
</script>

<template>
  <el-dialog v-model="visible" :title="`${editing ? '编辑内容' : '查看内容'} · ${name}${dirty ? ' *' : ''}`" width="min(1040px, 94vw)" top="5vh" append-to-body class="file-preview-dialog" :before-close="beforeClose" :close-on-click-modal="false" :close-on-press-escape="false" @opened="editor?.requestMeasure()" @close="cancel" @closed="emit('close')">
    <div class="preview-path mono" :title="path">{{ path }}</div>
    <div class="preview-toolbar">
      <el-button size="small" :icon="EditPen" :disabled="!canEdit || saving" @click="toggleEdit">{{ editing ? '取消编辑' : '编辑' }}</el-button>
      <el-button v-if="editing" size="small" type="primary" :icon="Finished" :loading="saving" :disabled="!dirty || saving" title="Ctrl+S / ⌘S" @click="save">保存</el-button>
      <el-button v-if="editing" size="small" :disabled="!dirty || saving" @click="comparing = true">对比改动</el-button>
      <el-button size="small" :icon="Search" :disabled="!canRead" @click="find">查找</el-button>
      <el-button size="small" :icon="CopyDocument" :disabled="!canRead" @click="copy">复制内容</el-button>
      <el-checkbox v-model="wrap" :disabled="!canRead">自动换行</el-checkbox>
      <span class="preview-spacer" />
      <el-button size="small" :icon="Refresh" :loading="loading" :disabled="saving" @click="refresh">刷新</el-button>
      <el-button v-if="downloadable !== false" size="small" :icon="Download" :disabled="saving" title="下载服务器上已保存的文件" @click="emit('download')">下载</el-button>
    </div>
    <el-alert v-if="data?.truncated && !data.unsupported" type="warning" :closable="false" show-icon :title="`文件较大，仅预览前 ${bytes(data.limit)}，无法在此编辑；请下载后编辑并上传。`" class="preview-notice" />
    <el-alert v-if="saveError" type="error" :closable="false" show-icon :title="saveError" class="preview-notice"><el-button link type="primary" :disabled="saving" @click="refresh">重新读取</el-button></el-alert>
    <el-alert v-if="data?.readOnly" :title="data.readOnly" type="info" :closable="false" class="preview-notice" />
    <div v-loading="loading" class="preview-body" element-loading-text="正在读取文件…">
      <div v-if="error" class="preview-message" role="alert"><el-alert :title="error" type="error" :closable="false" show-icon /><el-button @click="load">重试</el-button></div>
      <div v-else-if="data?.unsupported" class="preview-message"><el-empty description="暂不支持预览此文件"><p class="muted">{{ data.unsupported }}</p><el-button :icon="Download" @click="emit('download')">下载文件</el-button></el-empty></div>
      <div v-else-if="canRead" ref="host" class="preview-editor" />
    </div>
    <template #footer>
      <div class="preview-footer"><span class="muted"><span :class="{ 'preview-dirty': dirty }">{{ saving ? '正在保存…' : dirty ? '未保存' : saved ? '已保存' : editing ? '编辑中' : '只读' }}</span><span v-if="data"> · {{ bytes(data.size) }}<span v-if="data.encoding"> · {{ data.encoding }}</span><span v-if="canRead && !draft"> · 空文件</span></span></span><el-button :disabled="saving" @click="close">关闭</el-button></div>
    </template>
  </el-dialog>
  <el-dialog v-model="comparing" title="对比改动" width="min(1180px, 94vw)" append-to-body top="5vh"><div class="compare"><section><strong>服务器原内容</strong><pre>{{ normalize(data?.content ?? '') }}</pre></section><section><strong>当前编辑内容</strong><pre>{{ draft }}</pre></section></div></el-dialog>
</template>

<style scoped>
.preview-path { overflow-wrap: anywhere; max-height: 56px; overflow-y: auto; color: var(--el-text-color-secondary); }
.preview-toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin: 12px 0; }
.preview-toolbar .el-button + .el-button { margin-left: 0; }
.preview-toolbar .el-checkbox { height: 24px; margin: 0 8px; }
.preview-spacer { flex: 1; }
.preview-notice { margin-bottom: 10px; }
.preview-body { height: min(62vh, 680px); min-height: 220px; border: 1px solid var(--el-border-color); border-radius: 6px; overflow: hidden; }
.preview-editor { height: 100%; }
.preview-message { height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 16px; box-sizing: border-box; padding: 24px; overflow-y: auto; }
.preview-message p { max-width: 540px; line-height: 1.7; }
.preview-message :deep(.el-alert__title) { overflow-wrap: anywhere; }
.preview-footer { display: flex; align-items: center; justify-content: space-between; gap: 12px; font-size: 12px; }
.preview-dirty { color: var(--el-color-warning); }
.compare { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.compare section { min-width: 0; }
.compare pre { max-height: 65vh; overflow: auto; padding: 12px; background: var(--el-fill-color-light); font: 12px/1.6 Consolas, monospace; }
</style>
