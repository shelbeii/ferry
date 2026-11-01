<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { Search, Finished } from '@element-plus/icons-vue'
import { openSearchPanel } from '@codemirror/search'
import { forceLinting } from '@codemirror/lint'
import { readConfig, writeConfig } from '../platform'
import { ferry } from '../ferry'
import { createEditor, locate, reveal, type FerryEditor } from '../editor/yamlEditor'
import { diffHunks } from '../editor/configDoc'
import ConfigForm from './config/ConfigForm.vue'

// focus：打开后定位到的字段路径，如 ['targets', 'prod-b', 'root']
const props = defineProps<{ path: string; focus?: string[] }>()
const emit = defineEmits<{ saved: [] }>()

const visible = defineModel<boolean>({ required: true })
const host = ref<HTMLElement>()
const editor = shallowRef<FerryEditor>()
const form = ref<InstanceType<typeof ConfigForm>>()
const original = ref('')
const text = ref('')
const loading = ref(true)
const loaded = ref(false)
const refreshing = ref(false)
const saving = ref(false)
const loadError = ref('')
const saveMsg = ref<{ ok: boolean; text: string } | null>(null)
const cursor = ref({ line: 1, col: 1 })
const problems = ref(0)
const checking = ref(false)
const focusHint = ref('')
// 文件原来用 CRLF 换行时，保存时保持（编辑器内部统一用 LF）
let crlf = false
let observer: MutationObserver | undefined
let disposed = false
let active = false
let opened = false
let readSeq = 0
let idle: number | undefined
let focusPending = false
let pendingRead: { raw: string; baseline: string } | { error: string } | undefined

// 「表单」「YAML」两页编辑同一份内容；记住上次用的哪页
const TAB_KEY = 'ferry.configEditor.tab'
function savedTab(): 'form' | 'yaml' {
  try {
    return localStorage.getItem(TAB_KEY) === 'yaml' ? 'yaml' : 'form'
  } catch {
    return 'form'
  }
}
const tab = ref<'form' | 'yaml'>(props.focus?.length ? 'form' : savedTab())
watch(tab, (t) => {
  try {
    localStorage.setItem(TAB_KEY, t)
  } catch {
    /* 存不了就不记 */
  }
  if (t === 'yaml') {
    // 隐藏时编辑器量不了尺寸，显示后重新量；还没建好（打开时停在表单页）就现在建
    nextTick(() => {
      if (!active || !opened) return
      buildEditor()
      editor.value?.view.requestMeasure()
      editor.value?.view.focus()
    })
  } else checkForm()
})

// 实时校验：内容经环境变量交给 ferry（不用先保存）；Windows 单个环境变量上限约 32K 字符
async function validate(content: string): Promise<string | null> {
  if (!active || content.length > 30000) return null
  checking.value = true
  try {
    const r = await ferry(['validate', '-c', props.path, '--from-env'], undefined, { FERRY_CONFIG_CONTENT: content }).done
    return r.ok ? null : (r.error ?? '校验失败')
  } finally {
    checking.value = false
  }
}

// 表单页的校验：内容变了半秒后校验一次（YAML 页由编辑器自己校验）
const formErrors = ref('')
let formTimer: ReturnType<typeof setTimeout> | undefined
let formSeq = 0
function checkForm() {
  clearTimeout(formTimer)
  if (!active || !opened || !loaded.value) return
  formTimer = setTimeout(async () => {
    const seq = ++formSeq
    const err = await validate(text.value)
    if (seq === formSeq && active && !disposed) formErrors.value = err ?? ''
  }, 500)
}
watch(text, () => {
  if (tab.value === 'form') checkForm()
})
const formProblems = computed(() => formErrors.value.split('\n').filter((l) => l.trim() && !/ferry\.ya?ml:?$/i.test(l.trim())).length)

function onFormText(t: string) {
  text.value = t
  saveMsg.value = null
  editor.value?.setDoc(t)
}

const isDark = () => document.documentElement.classList.contains('dark')

function buildEditor() {
  if (!active || !opened || disposed || editor.value || !host.value || !loaded.value) return
  editor.value = createEditor({
    parent: host.value,
    doc: text.value,
    dark: isDark(),
    validate,
    onChange: (t) => {
      text.value = t
      saveMsg.value = null
    },
    onCursor: (line, col) => (cursor.value = { line, col }),
    onDiagnostics: (n) => (problems.value = n),
    onSave: save,
  })
}

// 首次打开才等待文件读取、创建表单；之后保留实例和位置，不再重复显示加载遮罩。
// 每次打开仍在后台读一次文件，外部改过的内容也能同步；结果等弹出动画结束再应用。
async function refreshFile() {
  const seq = ++readSeq
  const baseline = original.value
  pendingRead = undefined
  refreshing.value = true
  loadError.value = ''
  try {
    const raw = await readConfig(props.path)
    if (seq !== readSeq || disposed || !active) return
    pendingRead = { raw, baseline }
  } catch (e) {
    if (seq !== readSeq || disposed || !active) return
    pendingRead = { error: String(e) }
  }
  await applyRead()
}

async function applyRead() {
  if (!opened || !active || !pendingRead) return
  const result = pendingRead
  pendingRead = undefined
  refreshing.value = false
  loading.value = false
  if ('error' in result) {
    loadError.value = result.error
    return
  }
  const content = result.raw.replace(/\r\n?/g, '\n')
  if (loaded.value && content !== result.baseline) {
    const seq = readSeq
    // 表单输入到失焦时才提交；先收下仍在输入框里的草稿，再判断能否同步。
    if (tab.value === 'form') (document.activeElement as HTMLElement | null)?.blur?.()
    await nextTick()
    if (seq !== readSeq || !active || !opened || disposed) return
  }
  // 后台读文件期间用户可能已经开始编辑，不能用旧快照覆盖刚输入的内容。
  if (loaded.value && content !== result.baseline && text.value !== result.baseline) {
    loadError.value = '配置文件已在外部修改，请关闭后重新打开以加载最新内容。当前修改不会自动覆盖文件。'
    return
  }
  crlf = result.raw.includes('\r\n')
  if (!loaded.value || content !== original.value) {
    focusPending = !!props.focus?.length
    original.value = content
    text.value = content
    // 文件内容换了，旧的撤销记录也不能带进来。
    destroyEditor()
    formErrors.value = ''
  }
  loaded.value = true
  await nextTick()
  prepareContent()
  checkForm()
}

function prepareContent() {
  if (!active || !opened || !loaded.value || disposed) return
  if (tab.value === 'yaml' || props.focus?.length) buildEditor()
  else if (!editor.value && idle === undefined) idle = requestIdleCallback(() => {
    idle = undefined
    buildEditor()
  }, { timeout: 1000 })
  editor.value?.view.requestMeasure()
  if (focusPending && props.focus?.length && editor.value) {
    focusPending = false
    form.value?.focusPath(props.focus)
    const loc = locate(text.value, props.focus)
    if (loc) reveal(editor.value.view, loc)
    if (loc && !loc.found) focusHint.value = `配置里没有设置 ${props.focus.join('.')}，当前用的是默认值`
  } else if (tab.value === 'yaml') editor.value?.view.focus()
}

watch(visible, (value) => {
  if (!value) return
  active = true
  opened = false
  focusPending = !!props.focus?.length
  focusHint.value = ''
  saveMsg.value = null
  if (!loaded.value) loading.value = true
  if (focusPending) tab.value = 'form'
  void refreshFile()
}, { immediate: true })

async function onOpened() {
  opened = true
  if (pendingRead) await applyRead()
  else prepareContent()
}

function onClosing() {
  active = opened = false
  ++readSeq
  ++formSeq
  pendingRead = undefined
  refreshing.value = false
  clearTimeout(formTimer)
  if (idle !== undefined) cancelIdleCallback(idle)
  idle = undefined
}

function destroyEditor() {
  editor.value?.view.destroy()
  editor.value = undefined
}

function onClosed() {
  // 只在弹窗完全隐藏之后丢弃已确认放弃的修改，退出动画期间保持画面不变。
  if (text.value !== original.value) {
    text.value = original.value
    destroyEditor()
  }
  reviewing.value = false
  visible.value = false
}

onMounted(() => {
  // 跟随界面主题
  observer = new MutationObserver(() => editor.value?.setDark(isDark()))
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
})

onBeforeUnmount(() => {
  disposed = true
  onClosing()
  observer?.disconnect()
  destroyEditor()
})

// ---------------------------------------------------------------- 保存：先看改了什么

const reviewing = ref(false)
const hunks = computed(() => (reviewing.value ? diffHunks(original.value, text.value) : []))
const changeCount = computed(() => hunks.value.reduce((n, h) => n + h.lines.filter((l) => l.t !== ' ').length, 0))

async function save() {
  if (!active || !loaded.value || refreshing.value || loadError.value || saving.value) return
  // 表单里正在输入的先写回（离开输入框时才写回）
  ;(document.activeElement as HTMLElement | null)?.blur?.()
  await nextTick()
  if (text.value === original.value) {
    saveMsg.value = { ok: true, text: '没有修改' }
    return
  }
  reviewing.value = true
}

async function doSave() {
  if (!active || refreshing.value || loadError.value || saving.value) return
  saving.value = true
  saveMsg.value = null
  try {
    await writeConfig(props.path, crlf ? text.value.replace(/\n/g, '\r\n') : text.value)
    original.value = text.value
    reviewing.value = false
    const err = await validate(text.value)
    formErrors.value = err ?? ''
    if (err) {
      saveMsg.value = { ok: false, text: tab.value === 'form' ? '已保存，但配置有错误：见表单里的红字' : '已保存，但配置有错误：见编辑器中的红色标记' }
      if (editor.value) forceLinting(editor.value.view)
    } else {
      saveMsg.value = { ok: true, text: '已保存，校验通过' }
      emit('saved')
    }
  } catch (e) {
    saveMsg.value = { ok: false, text: String(e) }
  } finally {
    saving.value = false
  }
}

function onKeydown(e: KeyboardEvent) {
  // YAML 页的 Ctrl+S 由编辑器处理
  if (tab.value === 'form' && (e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
    e.preventDefault()
    save()
  }
}

async function restore() {
  try {
    await ElMessageBox.confirm('撤销还没保存的全部修改，回到文件里的内容？', '撤销全部修改', { type: 'warning' })
  } catch {
    return
  }
  text.value = original.value
  editor.value?.setDoc(original.value)
  saveMsg.value = null
}

function find() {
  if (editor.value) openSearchPanel(editor.value.view)
}

function check() {
  if (tab.value === 'form') checkForm()
  else if (editor.value) forceLinting(editor.value.view)
}

async function beforeClose(done: () => void) {
  if (saving.value) return
  ;(document.activeElement as HTMLElement | null)?.blur?.()
  await nextTick()
  if (text.value !== original.value) {
    try {
      await ElMessageBox.confirm('有未保存的修改，确定关闭？', '关闭', { type: 'warning' })
    } catch {
      return
    }
  }
  done()
}
</script>

<template>
  <!-- Esc 留给编辑器（关闭补全、搜索）；关闭对话框用右上角，有修改时先确认 -->
  <el-dialog
    v-model="visible"
    append-to-body
    title="编辑配置"
    width="88%"
    top="3vh"
    :before-close="beforeClose"
    :close-on-press-escape="false"
    :close-on-click-modal="false"
    class="cfg-dialog"
    @opened="onOpened"
    @close="onClosing"
    @closed="onClosed"
  >
    <div v-show="!reviewing" class="cfg-content" @keydown="onKeydown">
      <div class="bar">
        <el-radio-group v-model="tab" size="small">
          <el-radio-button value="form">表单</el-radio-button>
          <el-radio-button value="yaml">YAML</el-radio-button>
        </el-radio-group>
        <span class="muted mono path" :title="path">{{ path }}</span>
        <span class="spacer" />
        <el-button v-if="tab === 'yaml'" size="small" :icon="Search" @click="find">查找替换</el-button>
        <el-button size="small" :icon="Finished" :loading="checking" @click="check">立即校验</el-button>
      </div>
      <el-alert v-if="loadError" type="error" :title="loadError" :closable="false" show-icon />
      <el-alert v-if="focusHint && tab === 'yaml'" type="info" :title="focusHint" show-icon class="hint" @close="focusHint = ''" />
      <div v-loading="loading" class="body">
        <ConfigForm v-if="loaded" v-show="tab === 'form'" ref="form" :text="text" :errors="formErrors" @update:text="onFormText" />
        <div v-show="tab === 'yaml'" class="editor-wrap">
          <div ref="host" class="editor-host" />
        </div>
      </div>
      <div class="status mono">
        <template v-if="tab === 'yaml'">
          <span>行 {{ cursor.line }}，列 {{ cursor.col }}</span>
          <span v-if="checking" class="muted">校验中…</span>
          <span v-else-if="problems > 0" class="ev-error">{{ problems }} 个问题</span>
          <span v-else class="ev-ok">没有问题</span>
        </template>
        <template v-else>
          <span v-if="checking" class="muted">校验中…</span>
          <span v-else-if="formProblems > 0" class="ev-error">{{ formProblems }} 个问题</span>
          <span v-else class="ev-ok">没有问题</span>
        </template>
        <span :class="text !== original ? 'ev-warn' : 'muted'">{{ text !== original ? '已修改' : '未修改' }}</span>
        <span class="spacer" />
        <span v-if="tab === 'yaml'" class="muted">YAML · 2 空格缩进 · Ctrl+Space 补全 · Ctrl+F 查找 · Ctrl+S 保存 · 悬停字段名查看说明</span>
        <span v-else class="muted">表单和 YAML 是同一份内容，哪边改都行 · Ctrl+S 保存</span>
      </div>
    </div>
    <template #footer>
      <template v-if="reviewing"><span class="muted foot-tip">审阅 {{ changeCount }} 行修改</span><el-button @click="reviewing = false">返回修改</el-button><el-button type="primary" :loading="saving" @click="doSave">确认保存</el-button></template>
      <template v-else>
      <span v-if="saveMsg" :class="saveMsg.ok ? 'ev-ok' : 'ev-error'" class="foot-msg">{{ saveMsg.text }}</span>
      <span v-else class="muted foot-tip">密码等敏感信息不要写进配置</span>
      <el-button :disabled="text === original" @click="restore">撤销全部修改</el-button>
      <el-button type="primary" :loading="saving" :disabled="!loaded || refreshing || !!loadError" @click="save">保存</el-button>
      </template>
    </template>

    <!-- 保存前看改了什么 -->
    <section v-if="reviewing" class="inline-review">
      <h3>审阅配置修改</h3>
      <p class="muted diff-tip">红色是删掉的行，绿色是新加的行；左边是行号。共 {{ changeCount }} 行改动。</p>
      <div class="diff mono">
        <template v-for="(h, i) in hunks" :key="i">
          <div v-if="i > 0" class="gap">⋯</div>
          <div v-for="(l, j) in h.lines" :key="j" class="dl" :class="l.t === '-' ? 'del' : l.t === '+' ? 'add' : ''">
            <span class="no">{{ l.no }}</span>
            <span class="sign">{{ l.t }}</span>
            <span class="txt">{{ l.text || ' ' }}</span>
          </div>
        </template>
      </div>
    </section>
  </el-dialog>
</template>

<style scoped>
:global(.cfg-dialog) {
  height: min(1000px, 94dvh);
}
:global(.cfg-dialog .el-dialog__body) {
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
.cfg-content {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.inline-review { height: clamp(180px, calc(100dvh - 250px), 55vh); min-height: 0; display: flex; flex-direction: column; }
.inline-review .diff { flex: 1; min-height: 0; max-height: none; }
.inline-review h3 { margin: 0 0 10px; font-size: 16px; }
.bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  margin: -6px 0 8px;
}
.path {
  flex: 1;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.hint {
  margin-bottom: 8px;
}
.spacer {
  flex: 1;
}
.body {
  flex: 1;
  min-height: 0;
}
.editor-wrap {
  height: 100%;
  border: 1px solid var(--el-border-color);
  border-radius: 6px;
  overflow: hidden;
}
.editor-host {
  height: 100%;
}
.status {
  display: flex;
  gap: 16px;
  align-items: center;
  font-size: 12px;
  padding: 6px 2px 0;
  flex-wrap: wrap;
}
@media (max-width: 900px), (max-height: 700px) { .status > .muted:last-child { display: none; } }
.foot-msg,
.foot-tip {
  font-size: 12px;
  margin-right: 12px;
}
.diff-tip {
  margin: -8px 0 8px;
  font-size: 12.5px;
}
.diff {
  max-height: 60vh;
  overflow: auto;
  border: 1px solid var(--el-border-color);
  border-radius: 6px;
  padding: 4px 0;
  line-height: 1.55;
}
.dl {
  display: flex;
  white-space: pre;
}
.dl.del {
  background: var(--el-color-danger-light-9);
}
.dl.add {
  background: var(--el-color-success-light-9);
}
.no {
  width: 44px;
  flex: none;
  text-align: right;
  padding-right: 8px;
  color: var(--el-text-color-secondary);
  user-select: none;
}
.sign {
  width: 16px;
  flex: none;
  color: var(--el-text-color-secondary);
  user-select: none;
}
.del .sign {
  color: var(--el-color-danger);
}
.add .sign {
  color: var(--el-color-success);
}
.gap {
  padding-left: 52px;
  color: var(--el-text-color-secondary);
}
</style>
