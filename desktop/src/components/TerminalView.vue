<script setup lang="ts">
// 一个服务器终端：xterm.js 显示，经 ferry serve 连到服务器上登录用户的 shell。
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebglAddon } from '@xterm/addon-webgl'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import '@xterm/xterm/css/xterm.css'
import { FolderOpened, RefreshRight } from '@element-plus/icons-vue'
import FilesDrawer from './FilesDrawer.vue'
import { isSecretKind, latencyInfo, latencyTip, openTerminal, type FerryLine, type FerryResult, type TerminalProc } from '../ferry'
import { secretRequest, trustRequest, trustedTick, type TermTab } from '../state'

// noFiles：所在页面自己有「文件」按钮（连接页），状态栏里不再放
const props = defineProps<{ tab: TermTab; active: boolean; noFiles?: boolean }>()

const box = ref<HTMLDivElement>()
const status = ref('')
const label = ref('')
const flash = ref('')
// 到服务器的往返时间（ferry 每 5 秒量一次）：打字卡不卡一眼看得出来
const latency = ref<ReturnType<typeof latencyInfo>>()
/** 上次断开是因为还没核对主机公钥：核对后自动重连 */
let waitTrust = false

let term: Terminal
let fit: FitAddon
let proc: TerminalProc | undefined
let observer: ResizeObserver | undefined

// 输出太快时（cat 大文件等）先让 xterm 显示完：积压超过 HIGH 暂停服务器的输出，降到 LOW 以下再继续
const HIGH = 512 * 1024
const LOW = 64 * 1024
let backlog = 0
let paused = false

const gray = (s: string) => `\x1b[90m${s.replace(/\r?\n/g, '\r\n')}\x1b[0m\r\n`
const red = (s: string) => `\x1b[31m${s.replace(/\r?\n/g, '\r\n')}\x1b[0m\r\n`

onMounted(() => {
  term = new Terminal({
    allowProposedApi: true,
    cursorBlink: true,
    fontSize: 14,
    lineHeight: 1.1,
    fontFamily: '"Cascadia Mono", Consolas, "Sarasa Mono SC", "Microsoft YaHei Mono", Menlo, Monaco, "PingFang SC", "Microsoft YaHei", monospace',
    scrollback: 5000,
    theme: { background: '#1e1e1e', foreground: '#d4d4d4', cursor: '#d4d4d4', selectionBackground: '#264f78' },
  })
  fit = new FitAddon()
  term.loadAddon(fit)
  // 中文、表情等宽字符按两格算
  term.loadAddon(new Unicode11Addon())
  term.unicode.activeVersion = '11'
  term.open(box.value!)
  try {
    const gl = new WebglAddon()
    gl.onContextLoss(() => gl.dispose())
    term.loadAddon(gl)
  } catch {
    // 没有 WebGL：用默认的 DOM 渲染
  }

  term.onData((d) => {
    if (proc) proc.input(d)
    else if (props.tab.state === 'closed' && d === '\r') connect()
  })
  term.onBinary((d) => proc?.inputBinary(d))
  term.onResize(({ cols, rows }) => proc?.resize(cols, rows))

  // 复制粘贴：选中文字时 Ctrl+C 复制（没选中时照常是中断）；Ctrl+Shift+C 复制；Ctrl+V / Ctrl+Shift+V 粘贴
  term.attachCustomKeyEventHandler((e) => {
    if (e.type !== 'keydown' || !e.ctrlKey || e.altKey || e.metaKey) return true
    const k = e.key.toLowerCase()
    if (k === 'c' && (e.shiftKey || term.hasSelection())) {
      // 交给浏览器：触发复制事件，由 xterm 把选中的文字放进剪贴板（不需要剪贴板权限）
      afterCopy()
      return false
    }
    // 交给浏览器：触发粘贴事件，由 xterm 送给服务器
    if (k === 'v') return false
    return true
  })

  observer = new ResizeObserver(() => refit())
  observer.observe(box.value!)
  refit()
  connect()
})

onBeforeUnmount(() => {
  observer?.disconnect()
  const p = proc
  proc = undefined
  p?.close()
  term.dispose()
})

watch(
  () => props.active,
  async (a) => {
    if (!a) return
    await nextTick()
    refit()
    term.focus()
  },
)

watch(trustedTick, () => {
  if (waitTrust && props.tab.state === 'closed') connect()
})

// 手动断开（连接列表、连接页的「断开」）
watch(
  () => props.tab.hangup,
  () => {
    const p = proc
    if (!p) return
    proc = undefined
    waitTrust = false
    p.close()
    props.tab.state = 'closed'
    latency.value = undefined
    status.value = '已断开'
    term.write('\r\n' + gray('已断开。按回车重新连接'))
  },
)

let fitQueued = false
function refit() {
  if (fitQueued) return
  fitQueued = true
  requestAnimationFrame(() => {
    fitQueued = false
    // 隐藏的标签没有大小：切回来时再算
    if (!box.value || box.value.offsetWidth === 0 || box.value.offsetHeight === 0) return
    try {
      fit.fit()
    } catch {
      // 刚卸载
    }
  })
}

function connect() {
  waitTrust = false
  props.tab.state = 'connecting'
  label.value = ''
  latency.value = undefined
  status.value = `正在连接 ${props.tab.target}…`
  backlog = 0
  paused = false
  term.write(gray(`正在连接 ${props.tab.target}…`))
  const p = openTerminal(props.tab.config, props.tab.target, { cols: term.cols, rows: term.rows }, onLine, onData)
  proc = p
  p.done.then((r) => {
    if (proc !== p) return // 已关闭或已重连
    proc = undefined
    ended(r)
  })
  if (props.active) term.focus()
}

function onLine(l: FerryLine) {
  if (l.kind === 'terminal-open') {
    props.tab.state = 'open'
    label.value = String((l as { label?: string }).label ?? '')
    status.value = `已连接 ${label.value}`
  } else if (l.kind === 'latency') {
    const x = l as { ms?: number; slow?: boolean }
    latency.value = latencyInfo(x.ms, x.slow)
  } else if (l.kind === 'step') {
    status.value = l.msg ?? ''
  } else if ((l.kind === 'info' || l.kind === 'warn') && l.msg) {
    term.write(gray(l.msg))
  }
}

function onData(d: string) {
  if (props.tab.state === 'connecting') props.tab.state = 'open'
  backlog += d.length
  term.write(d, () => {
    backlog -= d.length
    if (paused && backlog < LOW) {
      paused = false
      proc?.flow(false)
    }
  })
  if (!paused && backlog > HIGH) {
    paused = true
    proc?.flow(true)
  }
}

function ended(r: FerryResult<{ exit: number }>) {
  props.tab.state = 'closed'
  latency.value = undefined
  if (r.ok) {
    status.value = '已断开（shell 已退出）'
    term.write('\r\n' + gray('已断开：shell 已退出。按回车重新连接'))
    return
  }
  const d = r.detail
  const err = r.error ?? '未知错误'
  status.value = '已断开'
  term.write('\r\n' + red(err))
  if (d?.kind === 'unknown-host' || (!d && err.includes('未知主机'))) {
    waitTrust = true
    trustRequest.value = { target: props.tab.target, config: props.tab.config }
    term.write(gray('核对并记录主机公钥后会自动重新连接；也可以按回车重新连接'))
  } else if (d && isSecretKind(d.kind)) {
    secretRequest.value = { detail: d, config: props.tab.config, retry: connect }
    term.write(gray('填好密码后会自动重新连接；也可以按回车重新连接'))
  } else {
    term.write(gray('按回车重新连接'))
  }
}

function afterCopy() {
  if (!term.hasSelection()) return
  setTimeout(() => {
    term.clearSelection()
    showFlash('已复制')
  })
}

// 右键复制：先用复制命令（xterm 处理复制事件），不行再用剪贴板接口
async function copy() {
  const text = term.getSelection()
  if (!text) return
  term.focus()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch {
    ok = false
  }
  if (!ok) {
    try {
      await navigator.clipboard.writeText(text)
      ok = true
    } catch {
      ok = false
    }
  }
  if (ok) {
    term.clearSelection()
    showFlash('已复制')
  } else showFlash('复制失败，请用 Ctrl+C')
}

async function paste() {
  try {
    const text = await navigator.clipboard.readText()
    if (text) term.paste(text)
  } catch {
    showFlash('不能读取剪贴板，请用 Ctrl+V 粘贴')
  }
  term.focus()
}

// 右键：选中了文字就复制，否则粘贴（和 Xshell、PuTTY 一样）
function onContextMenu(e: MouseEvent) {
  e.preventDefault()
  if (term.hasSelection()) copy()
  else paste()
}

let flashTimer: ReturnType<typeof setTimeout> | undefined
function showFlash(s: string) {
  flash.value = s
  clearTimeout(flashTimer)
  flashTimer = setTimeout(() => (flash.value = ''), 2000)
}

function reconnect() {
  const p = proc
  proc = undefined
  p?.close()
  term.write('\r\n')
  connect()
}

function toggleFiles() {
  props.tab.files = !props.tab.files
}

defineExpose({ focus: () => term?.focus() })
</script>

<template>
  <div class="term-view">
    <div class="term-main">
      <div ref="box" class="term-box" @contextmenu.capture="onContextMenu" />
      <FilesDrawer v-if="!noFiles" v-model="tab.files" :config="tab.config" :target="tab.target" :title="tab.title" />
    </div>
    <div class="term-status">
      <span class="led" :class="tab.state" />
      <span class="mono">{{ status }}</span>
      <el-tooltip v-if="latency && tab.state === 'open'" :show-after="HOVER_DELAY" :content="latencyTip(latency.level)" placement="top">
        <span class="latency" :class="latency.level">延迟 {{ latency.text }}</span>
      </el-tooltip>
      <span v-if="flash" class="flash">{{ flash }}</span>
      <span class="spacer" />
      <el-tooltip :show-after="HOVER_DELAY" placement="top">
        <template #content>
          复制：选中文字后按 Ctrl+C（没选中时 Ctrl+C 是中断命令），或点右键<br />
          粘贴：Ctrl+V，或在没选中文字时点右键
        </template>
        <span class="hint">复制粘贴</span>
      </el-tooltip>
      <el-tooltip v-if="!noFiles" :show-after="HOVER_DELAY" content="打开服务器上的文件：上传、下载、新建文件夹、改名、删除" placement="top">
        <el-button link size="small" :icon="FolderOpened" :class="{ on: tab.files }" @click="toggleFiles">文件</el-button>
      </el-tooltip>
      <el-button v-if="tab.state !== 'connecting'" link size="small" :icon="RefreshRight" @click="reconnect">重新连接</el-button>
    </div>
  </div>
</template>

<style scoped>
.term-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  background: #1e1e1e;
}
.term-main {
  flex: 1;
  min-height: 0;
  display: flex;
}
.term-status .el-button.on {
  color: #4caf50;
}
.term-box {
  flex: 1;
  min-width: 0;
  min-height: 0;
  padding: 4px 0 0 6px;
  overflow: hidden;
}
.term-status {
  display: flex;
  align-items: center;
  gap: 8px;
  height: 26px;
  padding: 0 10px;
  font-size: 12px;
  color: #a0a0a0;
  background: #252526;
  border-top: 1px solid #333;
}
.term-status .el-button {
  color: #a0a0a0;
}
.led {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #777;
}
.led.open {
  background: #4caf50;
}
.led.connecting {
  background: #e6a23c;
}
.flash {
  color: #4caf50;
}
.latency {
  font-variant-numeric: tabular-nums;
  cursor: default;
}
.latency.good {
  color: #4caf50;
}
.latency.fair {
  color: #e6a23c;
}
.latency.poor {
  color: #f56c6c;
}
.hint {
  cursor: default;
  text-decoration: underline dotted;
}
.spacer {
  flex: 1;
}
</style>
