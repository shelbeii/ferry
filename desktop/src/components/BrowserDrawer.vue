<script setup lang="ts">
// 内置浏览器：右侧抽屉。标签、地址栏在这里；网页和开发者工具是 Rust 那边的网页视图（browser.rs），
// 盖在下面 .page / .devtools 留出的位置上。网页视图总在最上层，所以抽屉没完全打开、上面有别的对话框、
// 当前是新标签页时，让它们藏起来。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { invoke } from '@tauri-apps/api/core'
import { listen, type UnlistenFn } from '@tauri-apps/api/event'
import { ArrowLeft, ArrowRight, RefreshRight, Close, Plus } from '@element-plus/icons-vue'
import { isTauri } from '../platform'
import { browserOpen, uiPref, setUiPref } from '../state'
import { toUrl, history, addVisit, removeVisit, clearHistory, siteOf, visitTime } from '../browser/address'

const props = defineProps<{ side: number }>()

interface Tab {
  id: number
  url: string
  title: string
  loading: boolean
  devtools: boolean
  /** 网页图标（data: 地址），网页没有图标时为空 */
  icon: string
}
interface Snapshot {
  tabs: Tab[]
  active: number
}
interface Bounds {
  x: number
  y: number
  width: number
  height: number
}

const tabs = ref<Tab[]>([])
const active = ref(0)
const current = computed(() => tabs.value.find((t) => t.id === active.value))
const blank = computed(() => !current.value?.url)
const address = ref('')
const editing = ref(false)
const input = ref<HTMLInputElement>()
const error = ref('')

// 每个标签上次记进「最近打开」的网址和标题：变了才再记
const recorded = new Map<number, string>()
function apply(s: Snapshot) {
  // 网页加载完、有了标题：记进最近打开（标题常在加载完之后才出来，跳转页没有标题，addVisit 里会跳过）
  for (const t of s.tabs) {
    if (t.loading || !t.title) continue
    const k = `${t.url}\n${t.title}`
    if (recorded.get(t.id) === k) continue
    recorded.set(t.id, k)
    addVisit({ url: t.url, title: t.title })
  }
  const switched = s.active !== active.value
  tabs.value = s.tabs
  active.value = s.active
  if (switched || !editing.value) address.value = current.value?.url ?? ''
  if (switched) error.value = ''
}

const call = (cmd: string, args: Record<string, unknown> = {}) =>
  invoke(cmd, args).catch((e) => {
    error.value = String((e as Error)?.message ?? e)
  })

function go(text = address.value) {
  const url = toUrl(text)
  if (!url) return
  error.value = ''
  editing.value = false
  input.value?.blur()
  address.value = url
  if (!tabs.value.length) call('browser_new_tab', { url })
  else call('browser_navigate', { url })
}
type Action = 'back' | 'forward' | 'reload' | 'stop' | 'devtools'
const action = (a: Action) => call('browser_action', { action: a })
const newTab = () => call('browser_new_tab')
const closeTab = (id: number) => call('browser_close_tab', { id })
const select = (id: number) => call('browser_select_tab', { id })
function focusAddress() {
  input.value?.focus()
  input.value?.select()
}

// 起始页「最近打开」：站点首字母 + 按站点固定的颜色，好区分
const BADGE_COLORS = ['#4f46e5', '#0891b2', '#059669', '#d97706', '#db2777', '#7c3aed', '#2563eb', '#dc2626']
function badgeText(url: string) {
  return (siteOf(url).replace(/^[^a-z0-9一-龥]+/i, '')[0] ?? '?').toUpperCase()
}
function badgeColor(url: string) {
  let h = 0
  for (const ch of siteOf(url)) h = (h * 31 + ch.charCodeAt(0)) >>> 0
  return BADGE_COLORS[h % BADGE_COLORS.length]
}
// 「几分钟前」按显示起始页时的时间算
const now = ref(Date.now())
watch(blank, (b) => b && (now.value = Date.now()))

function label(t: Tab) {
  if (t.title) return t.title
  if (!t.url) return '新标签页'
  try {
    return new URL(t.url).host || t.url
  } catch {
    return t.url
  }
}

// ---------- 网页视图的位置 ----------

const opened = ref(false) // 抽屉滑出来的动画做完了
const covered = ref(false) // 上面有别的对话框（网页视图会盖住它）
const pageBox = ref<HTMLElement>()
const devtoolsBox = ref<HTMLElement>()
const viewport = ref<HTMLElement>()
const DEVTOOLS_MIN = 260
const PAGE_MIN = 240
const devtoolsWidth = ref(Number(uiPref('browserDevtoolsWidth')) || 520)
const showDevtools = computed(() => !!current.value?.devtools && !blank.value)

function bounds(el?: HTMLElement): Bounds | null {
  if (!el) return null
  const r = el.getBoundingClientRect()
  if (r.width < 1 || r.height < 1) return null
  return { x: Math.round(r.left), y: Math.round(r.top), width: Math.round(r.width), height: Math.round(r.height) }
}

let frame = 0
let last = ''
function place() {
  if (frame) return
  frame = requestAnimationFrame(() => {
    frame = 0
    const visible = browserOpen.value && opened.value && !covered.value && !blank.value
    const page = visible ? bounds(pageBox.value) : null
    const devtools = page && showDevtools.value ? bounds(devtoolsBox.value) : null
    const key = JSON.stringify([page, devtools])
    if (key === last || !isTauri) return
    last = key
    invoke('browser_layout', { page, devtools }).catch(() => (last = ''))
  })
}
watch([opened, covered, blank, showDevtools, active, devtoolsWidth, browserOpen], () => nextTick(place))

let resize: ResizeObserver | undefined
watch(viewport, (el) => {
  resize?.disconnect()
  if (el) {
    resize = new ResizeObserver(place)
    resize.observe(el)
  }
})

// 开发者工具的宽度：拖中间的分隔条
function clampWidth(w: number) {
  const total = viewport.value?.clientWidth ?? 1200
  return Math.round(Math.max(DEVTOOLS_MIN, Math.min(w, total - PAGE_MIN)))
}
function startDrag(e: PointerEvent) {
  const bar = e.currentTarget as HTMLElement
  bar.setPointerCapture(e.pointerId)
  const x0 = e.clientX
  const w0 = devtoolsWidth.value
  const move = (ev: PointerEvent) => (devtoolsWidth.value = clampWidth(w0 + x0 - ev.clientX))
  const up = () => {
    bar.removeEventListener('pointermove', move)
    bar.removeEventListener('pointerup', up)
    bar.removeEventListener('pointercancel', up)
    setUiPref('browserDevtoolsWidth', String(devtoolsWidth.value))
  }
  bar.addEventListener('pointermove', move)
  bar.addEventListener('pointerup', up)
  bar.addEventListener('pointercancel', up)
}

// 别的对话框、确认框（都在 body 下的 .el-overlay 里）打开时网页视图让开
function checkCovered() {
  covered.value = [...document.querySelectorAll<HTMLElement>('body > .el-overlay')].some(
    (el) => !el.classList.contains('browser-overlay') && el.getClientRects().length > 0 && getComputedStyle(el).display !== 'none',
  )
}

// ---------- 打开、关上 ----------

function onOpened() {
  opened.value = true
  if (blank.value) focusAddress()
  else call('browser_action', { action: 'focus' })
}
function onClose() {
  opened.value = false
}
// 抽屉收起这么久后释放网页（浏览器的进程退出，不占内存）；标签、地址留着，再打开时重新加载
const RELEASE_AFTER = 10 * 60 * 1000
let release = 0
watch(browserOpen, async (open) => {
  clearTimeout(release)
  if (!open) {
    stopTimers()
    covered.value = false
    if (isTauri) release = window.setTimeout(() => invoke('browser_release').catch(() => {}), RELEASE_AFTER)
    return
  }
  startTimers()
  // 第一次打开：开一个新标签页；网页释放过：重新打开当前标签
  if (isTauri && !tabs.value.length) await newTab()
  else if (current.value?.url) await select(active.value)
})

// 快捷键：焦点在抽屉里时在这里处理；焦点在网页里时 Rust 那边拦下来发 browser-key
function onKey(e: KeyboardEvent) {
  if (!browserOpen.value || covered.value) return
  const mod = e.ctrlKey || e.metaKey
  const k = e.key.toLowerCase()
  let handled = true
  if (e.key === 'F12' || (mod && e.shiftKey && k === 'i')) action('devtools')
  else if (e.key === 'F5' || (mod && !e.shiftKey && k === 'r')) action('reload')
  else if (mod && k === 't') newTab()
  else if (mod && k === 'w') closeTab(active.value)
  else if (mod && k === 'l') focusAddress()
  else if (e.altKey && e.key === 'ArrowLeft') action('back')
  else if (e.altKey && e.key === 'ArrowRight') action('forward')
  else if (mod && e.key === 'Tab' && tabs.value.length > 1) {
    const i = tabs.value.findIndex((t) => t.id === active.value)
    select(tabs.value[(i + (e.shiftKey ? -1 : 1) + tabs.value.length) % tabs.value.length].id)
  } else handled = false
  if (handled) {
    e.preventDefault()
    e.stopPropagation()
  }
}
function onPageKey(key: string) {
  if (key === 'devtools') action('devtools')
  else if (key === 'new-tab') newTab()
  else if (key === 'close-tab') closeTab(active.value)
  else if (key === 'address') focusAddress()
}

// 当前标签换了：新标签页把光标放到地址栏
watch(active, () => {
  if (blank.value && opened.value) nextTick(focusAddress)
})

// 对话框开关、单页网站换页面（不触发加载事件）：只在抽屉开着时定时看一下
let timer = 0
let poll = 0
function startTimers() {
  stopTimers()
  poll = window.setInterval(checkCovered, 250)
  timer = window.setInterval(() => {
    if (opened.value && !blank.value && !document.hidden) invoke('browser_sync').catch(() => {})
  }, 1000)
}
function stopTimers() {
  clearInterval(timer)
  clearInterval(poll)
}

const unlisten: UnlistenFn[] = []
onMounted(async () => {
  window.addEventListener('keydown', onKey, true)
  window.addEventListener('resize', place)
  if (browserOpen.value) startTimers()
  if (!isTauri) return
  unlisten.push(await listen<Snapshot>('browser-tabs', (e) => apply(e.payload)))
  unlisten.push(await listen<string>('browser-key', (e) => onPageKey(e.payload)))
  apply(await invoke<Snapshot>('browser_tabs'))
  if (browserOpen.value && !tabs.value.length) await newTab()
})
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKey, true)
  window.removeEventListener('resize', place)
  stopTimers()
  clearTimeout(release)
  resize?.disconnect()
  unlisten.forEach((u) => u())
})
</script>

<template>
  <el-drawer
    v-model="browserOpen"
    :size="`calc(100vw - ${props.side}px)`"
    direction="rtl"
    :with-header="false"
    append-to-body
    class="browser-drawer"
    modal-class="browser-overlay"
    @opened="onOpened"
    @close="onClose"
  >
    <div class="browser">
      <div class="tabs" role="tablist">
        <div
          v-for="t in tabs"
          :key="t.id"
          class="tab"
          :class="{ on: t.id === active }"
          role="tab"
          :aria-selected="t.id === active"
          :title="t.url ? `${label(t)}\n${t.url}` : '新标签页'"
          @mousedown.left="select(t.id)"
          @mousedown.middle.prevent="closeTab(t.id)"
        >
          <span v-if="t.loading" class="spin" aria-label="正在加载" />
          <img v-else-if="t.icon" class="fav" :src="t.icon" alt="" draggable="false" />
          <svg v-else-if="t.url" class="fav globe" viewBox="0 0 16 16" aria-hidden="true">
            <circle cx="8" cy="8" r="6.25" />
            <path d="M1.75 8h12.5M8 1.75c-1.8 1.7-2.6 3.8-2.6 6.25S6.2 12.55 8 14.25M8 1.75c1.8 1.7 2.6 3.8 2.6 6.25S9.8 12.55 8 14.25" />
          </svg>
          <span class="tab-text">{{ label(t) }}</span>
          <button class="tab-close" aria-label="关闭标签" title="关闭标签（Ctrl+W）" @mousedown.stop @click.stop="closeTab(t.id)">
            <el-icon><Close /></el-icon>
          </button>
        </div>
        <button class="icon new-tab" aria-label="新标签页" title="新标签页（Ctrl+T）" @click="newTab"><el-icon><Plus /></el-icon></button>
        <span class="gap" />
        <button class="icon" aria-label="收起浏览器" title="收起浏览器（网页留着，再打开接着用）" @click="browserOpen = false"><el-icon><Close /></el-icon></button>
      </div>
      <div class="nav">
        <button class="icon" :disabled="blank" aria-label="后退" title="后退（Alt+←）" @click="action('back')"><el-icon><ArrowLeft /></el-icon></button>
        <button class="icon" :disabled="blank" aria-label="前进" title="前进（Alt+→）" @click="action('forward')"><el-icon><ArrowRight /></el-icon></button>
        <button v-if="current?.loading" class="icon" aria-label="停止" title="停止加载" @click="action('stop')"><el-icon><Close /></el-icon></button>
        <button v-else class="icon" :disabled="blank" aria-label="刷新" title="刷新（F5）" @click="action('reload')"><el-icon><RefreshRight /></el-icon></button>
        <input
          ref="input"
          v-model="address"
          class="address"
          spellcheck="false"
          autocomplete="off"
          placeholder="输入网址，或者要搜索的内容"
          aria-label="地址"
          @focus="editing = true; ($event.target as HTMLInputElement).select()"
          @blur="editing = false"
          @keydown.enter="go()"
          @keydown.esc.stop="address = current?.url ?? ''; input?.blur()"
        />
        <span v-if="error" class="error" role="alert" :title="error">{{ error }}</span>
        <button class="tool" :class="{ on: showDevtools }" :disabled="blank" title="开发者工具（F12）" @click="action('devtools')">
          <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8 7l-5 5 5 5M16 7l5 5-5 5M14 4l-4 16" /></svg>
          <span>开发者工具</span>
        </button>
      </div>
      <div ref="viewport" class="viewport">
        <div ref="pageBox" class="page">
          <div v-if="blank" class="start">
            <h1>打开网页</h1>
            <p v-if="!isTauri" class="hint">浏览器只能在桌面版里用。</p>
            <p v-else class="hint">在上面输入网址（如 192.168.1.10:8080、example.com），或者要搜索的内容。打开网页后按 F12 或点「开发者工具」调试，开发者工具显示在网页右边。</p>
            <section v-if="history.length" class="recent">
              <header>
                <h2>最近打开</h2>
                <button class="clear" title="清空最近打开的网页（不影响登录状态和缓存）" @click="clearHistory()">清空</button>
              </header>
              <ul>
                <li v-for="v in history.slice(0, 12)" :key="v.url">
                  <button class="visit" :title="`${v.title}\n${v.url}`" @click="go(v.url)">
                    <span class="badge" :style="{ background: badgeColor(v.url) }" aria-hidden="true">{{ badgeText(v.url) }}</span>
                    <span class="v-text">
                      <span class="v-title">{{ v.title }}</span>
                      <span class="v-meta">{{ siteOf(v.url) }}<template v-if="v.at"> · {{ visitTime(v.at, now) }}</template></span>
                    </span>
                  </button>
                  <button class="v-remove" aria-label="从最近打开里删掉" title="删掉这一条" @click="removeVisit(v.url)"><el-icon><Close /></el-icon></button>
                </li>
              </ul>
            </section>
          </div>
        </div>
        <template v-if="showDevtools">
          <div class="splitter" role="separator" aria-label="拖动调整开发者工具的宽度" @pointerdown.prevent="startDrag" />
          <div ref="devtoolsBox" class="devtools" :style="{ width: devtoolsWidth + 'px' }" />
        </template>
      </div>
    </div>
  </el-drawer>
</template>

<style>
.browser-drawer .el-drawer__body {
  padding: 0;
  overflow: hidden;
}
</style>

<style scoped>
/* 网页视图盖在 .page / .devtools 上：这两块里不要放会被挡住的东西（起始页只在新标签页时显示，那时网页视图藏着） */
.browser {
  height: 100%;
  display: flex;
  flex-direction: column;
  background: var(--el-bg-color);
}
.tabs {
  height: 38px;
  flex-shrink: 0;
  display: flex;
  align-items: flex-end;
  gap: 2px;
  padding: 0 8px;
  background: var(--el-fill-color-light);
  overflow: hidden;
}
.gap {
  flex: 1;
}
.tabs > .icon:last-child {
  margin-bottom: 4px;
}
.tab {
  flex: 0 1 220px;
  min-width: 60px;
  height: 32px;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 0 6px 0 12px;
  border-radius: 8px 8px 0 0;
  font-size: 13px;
  color: var(--el-text-color-regular);
  cursor: default;
  user-select: none;
}
.tab:hover {
  background: var(--el-fill-color);
}
.tab.on {
  background: var(--el-bg-color);
  color: var(--el-text-color-primary);
}
.tab-text {
  flex: 1;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}
.tab-close {
  width: 20px;
  height: 20px;
  padding: 0;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: var(--el-text-color-secondary);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  font-size: 12px;
}
.tab-close:hover {
  background: var(--el-fill-color-dark);
  color: var(--el-text-color-primary);
}
.fav {
  width: 16px;
  height: 16px;
  flex-shrink: 0;
  object-fit: contain;
}
.globe {
  fill: none;
  stroke: var(--el-text-color-secondary);
  stroke-width: 1.2;
}
.spin {
  width: 10px;
  height: 10px;
  flex-shrink: 0;
  border: 2px solid var(--el-border-color);
  border-top-color: var(--el-color-primary);
  border-radius: 50%;
  animation: spin 0.8s linear infinite;
}
@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}
.nav {
  height: 44px;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 0 8px;
  border-bottom: 1px solid var(--el-border-color-lighter);
  box-sizing: border-box;
}
button {
  font: inherit;
  color: var(--el-text-color-regular);
  cursor: pointer;
}
button:disabled {
  opacity: 0.4;
  cursor: default;
}
.icon {
  width: 30px;
  height: 30px;
  padding: 0;
  border: 0;
  border-radius: 6px;
  background: transparent;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  font-size: 16px;
}
.new-tab {
  width: 26px;
  height: 26px;
  margin: 0 0 4px 4px;
  font-size: 14px;
}
.icon:not(:disabled):hover,
.tool:not(:disabled):hover {
  background: var(--el-fill-color);
}
.address {
  flex: 1;
  min-width: 0;
  height: 30px;
  padding: 0 12px;
  border: 1px solid var(--el-border-color);
  border-radius: 15px;
  background: var(--el-fill-color-blank);
  color: var(--el-text-color-primary);
  font: inherit;
  font-size: 13px;
  outline: none;
}
.address:focus {
  border-color: var(--el-color-primary);
}
.error {
  max-width: 40%;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
  font-size: 12px;
  color: var(--el-color-danger);
}
.tool {
  height: 30px;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 0 10px;
  border: 0;
  border-radius: 6px;
  background: transparent;
  white-space: nowrap;
  flex-shrink: 0;
  font-size: 13px;
}
.tool.on {
  background: var(--el-color-primary-light-9);
  color: var(--el-color-primary);
}
.tool svg {
  width: 16px;
  height: 16px;
  fill: none;
  stroke: currentColor;
  stroke-width: 2;
  stroke-linecap: round;
  stroke-linejoin: round;
}
.viewport {
  flex: 1;
  min-height: 0;
  display: flex;
}
.page {
  flex: 1;
  min-width: 0;
  overflow: hidden;
}
.splitter {
  width: 6px;
  flex-shrink: 0;
  cursor: col-resize;
  background: var(--el-border-color-lighter);
  touch-action: none;
}
.splitter:hover {
  background: var(--el-color-primary-light-5);
}
.devtools {
  flex-shrink: 0;
  background: var(--el-fill-color-light);
}
.start {
  height: 100%;
  overflow-y: auto;
  box-sizing: border-box;
  padding: 48px max(24px, calc(50% - 440px)) 32px;
}
.start h1 {
  margin: 0 0 8px;
  font-size: 22px;
  font-weight: 600;
}
.hint {
  margin: 0;
  color: var(--el-text-color-secondary);
  line-height: 1.7;
}
.recent {
  margin-top: 36px;
}
.recent header {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  margin-bottom: 8px;
}
.recent h2 {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
  color: var(--el-text-color-secondary);
}
.clear {
  border: 0;
  padding: 2px 6px;
  border-radius: 4px;
  background: transparent;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.clear:hover {
  color: var(--el-color-primary);
  background: var(--el-fill-color-light);
}
/* 两列（窄时一列），每行一条：站点首字母、标题一行、站点和时间一行；悬停显示完整网址和删除按钮 */
.recent ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(340px, 100%), 1fr));
  gap: 2px 16px;
}
.recent li {
  position: relative;
  min-width: 0;
  border-radius: 8px;
}
.recent li:hover {
  background: var(--el-fill-color-light);
}
.visit {
  width: 100%;
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 36px 8px 10px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  text-align: left;
}
.visit:focus-visible {
  outline: 2px solid var(--el-color-primary);
  outline-offset: -2px;
}
.badge {
  width: 28px;
  height: 28px;
  flex-shrink: 0;
  border-radius: 7px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
  font-size: 13px;
  font-weight: 600;
}
.v-text {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.v-title,
.v-meta {
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}
.v-title {
  font-size: 13px;
  color: var(--el-text-color-primary);
}
.v-meta {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.v-remove {
  position: absolute;
  top: 50%;
  right: 6px;
  width: 24px;
  height: 24px;
  margin-top: -12px;
  padding: 0;
  border: 0;
  border-radius: 50%;
  background: transparent;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  color: var(--el-text-color-secondary);
  opacity: 0;
}
.recent li:hover .v-remove,
.v-remove:focus-visible {
  opacity: 1;
}
.v-remove:hover {
  background: var(--el-fill-color-dark);
  color: var(--el-text-color-primary);
}
</style>
