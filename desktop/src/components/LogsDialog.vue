<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Search, Close, Loading } from '@element-plus/icons-vue'
import { bytes, call, ferry, ferryRaw, time, type LogRun, type LogSearch, type Running } from '../ferry'

// release：打开时选中这个版本（从「版本/回滚」点进来）；不传 = 当前运行
const props = defineProps<{ config: string; target: string; app: string; release?: string }>()
const emit = defineEmits<{ close: [] }>()

const visible = ref(true)
let disposed = false
const expandedLogs = ref(false)
// 显示的行：日志刷得很快时（每秒几百上千行）不能每来一行就重画一次，
// 新来的先放进 incoming，每 100 毫秒一起加进来；只保留最近 MAX 行，整段作为一块文字显示
const lines = shallowRef<string[]>([])
let incoming: string[] = []
let flushTimer: ReturnType<typeof setTimeout> | undefined
const paused = ref(false) // 暂停：新来的先攒着（最多 MAX 行），继续时再显示
const dropped = ref(0) // 暂停期间丢掉的行数
const buffered = ref(0)
const rate = ref(0) // 最近一秒来了多少行
let rateCount = 0
let rateTimer: ReturnType<typeof setInterval> | undefined
const stickBottom = ref(true) // 用户没往上翻时才自动滚到底
const follow = ref(true)
const file = ref(false)
const path = ref('') // 日志文件：空 = diagLogs 指定的
const count = ref(300)
const error = ref('')
const box = ref<HTMLElement>()
let running: Running<void> | undefined
const MAX = 5000
// 读取中：从开始到最后几行都到了（持续输出时 ferry 给一个标记），期间显示在读什么、等了多久
const loading = ref(false)
const streaming = ref(false) // 还在持续输出（没有内容时显示「还没有新的输出」）
const waited = ref(0)
let waitTimer: ReturnType<typeof setInterval> | undefined
const LOADED = '\u0000ferry:loaded' // 与 ferry 的 logsLoadedMark 一致

function setLoading(on: boolean) {
  loading.value = on
  clearInterval(waitTimer)
  waitTimer = undefined
  if (on) {
    waited.value = 0
    waitTimer = setInterval(() => waited.value++, 1000)
  }
}

const loadingText = computed(() => {
  if (around.value) return `正在读取 ${path.value} 第 ${around.value} 行附近…`
  const what = file.value ? path.value || '日志文件' : '服务输出'
  const run = isCurrent.value ? '' : '这次运行的'
  return `正在读取${run}${what}的最后 ${count.value} 行…`
})

const text = computed(() => lines.value.join('\n'))

function queue(l: string) {
  incoming.push(l)
  rateCount++
  if (incoming.length > MAX) {
    dropped.value += incoming.length - MAX
    incoming = incoming.slice(-MAX)
  }
  buffered.value = incoming.length
  if (!flushTimer && !paused.value) flushTimer = setTimeout(flush, 100)
}

async function flush() {
  flushTimer = undefined
  if (paused.value || !incoming.length) return
  const next = lines.value.length + incoming.length > MAX ? [...lines.value, ...incoming].slice(-MAX) : [...lines.value, ...incoming]
  incoming = []
  buffered.value = 0
  lines.value = next
  await nextTick()
  if (around.value) {
    box.value?.querySelector('.hit')?.scrollIntoView({ block: 'center' })
  } else if (box.value && stickBottom.value) {
    box.value.scrollTop = box.value.scrollHeight
  }
}

function onScroll() {
  const b = box.value
  if (b) stickBottom.value = b.scrollHeight - b.scrollTop - b.clientHeight < 40
}

function togglePause() {
  paused.value = !paused.value
  if (!paused.value) {
    dropped.value = 0
    stickBottom.value = true
    flush()
  }
}
async function followLatest() {
  stickBottom.value = true
  if (paused.value) togglePause()
  await nextTick()
  if (box.value) box.value.scrollTop = box.value.scrollHeight
}
async function copyLog() {
  try { await navigator.clipboard.writeText(text.value); ElMessage.success('已复制当前已加载的日志') } catch { ElMessage.error('复制失败，请手动选择日志文本') }
}

// 看某一行附近（从搜索结果点进来）：around 为行号，firstLine 为显示的第一行的行号
const around = ref(0)
const firstLine = ref(0)

// 各次运行的日志：当前运行 + 每次切换版本时归档的
const runs = ref<LogRun[]>([])
const runsError = ref('')
const runKey = ref('current')
const keyOf = (r: LogRun) => (r.current ? 'current' : `${r.release}/${r.id}`)
const run = computed(() => runs.value.find((r) => keyOf(r) === runKey.value))
// 从「版本/回滚」点某个版本进来：只列这个版本的运行（它可能是当前运行，也可能归档过几次）
const shownRuns = computed(() => (props.release ? runs.value.filter((r) => r.release === props.release) : runs.value))
const runsLoaded = ref(false)
const noRuns = computed(() => !!props.release && !runsError.value && runsLoaded.value && shownRuns.value.length === 0)
const isCurrent = computed(() => runKey.value === 'current')

function shortTime(t?: string): string {
  if (!t || t.startsWith('0001')) return '?'
  const d = new Date(t)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

function runLabel(r: LogRun): string {
  const files = r.files.length ? ` · ${r.files.length} 个文件 · ${bytes(r.size)}` : ' · 没有日志文件'
  if (r.current) return `当前运行（版本 ${r.release}${r.start ? `，${shortTime(r.start)} 起` : ''}）${files}`
  return `版本 ${r.release} · ${shortTime(r.start)} ~ ${shortTime(r.end)}${files}`
}

// 文件列表：服务器已按修改时间从新到旧排好
function fileLabel(f: LogRun['files'][number]): string {
  const when = f.mtime && !f.mtime.startsWith('0001') ? ` · ${time(f.mtime)}` : ''
  return `${f.path}（${bytes(f.size)}${when}）`
}

async function loadRuns() {
  try {
    const data = await call<LogRun[]>(['logs', props.target, props.app, '-c', props.config, '--runs'])
    if (disposed) return
    runs.value = data
    runsError.value = ''
    runsLoaded.value = true
    if (props.release) {
      const r = runs.value.find((x) => x.release === props.release && x.current) ?? runs.value.find((x) => x.release === props.release)
      if (r) {
        runKey.value = keyOf(r)
        await start()
      }
    }
  } catch (e) {
    if (disposed) return
    runsError.value = (e as Error).message
  }
}

const reAround = /^==> .* 第 (\d+) 行起$/
let loadSeq = 0

async function start() {
  if (disposed || noRuns.value) return // 这个版本没有日志：不要拿别的版本的充数
  const seq = ++loadSeq
  await stop()
  if (seq !== loadSeq) return
  clearTimeout(flushTimer)
  flushTimer = undefined
  lines.value = []
  incoming = []
  buffered.value = 0
  dropped.value = 0
  paused.value = false
  stickBottom.value = true
  error.value = ''
  firstLine.value = 0
  const args = ['logs', props.target, props.app, '-c', props.config, '-n', String(count.value)]
  const r = run.value
  if (!isCurrent.value && r) args.push('--release', r.release, '--run', r.id!)
  if (isCurrent.value && follow.value && !around.value) args.push('-f', '--loaded-mark')
  if (file.value) args.push(...(path.value ? ['--log', path.value] : ['--file']))
  if (file.value && around.value) args.push('--line', String(around.value))
  let first = true
  setLoading(true)
  streaming.value = true
  const proc = ferryRaw(args, (l) => {
    if (running !== proc) return
    if (l === LOADED) {
      setLoading(false)
      return
    }
    if (first) {
      first = false
      const m = reAround.exec(l)
      if (m) {
        firstLine.value = Number(m[1])
        return
      }
    }
    if (loading.value) setLoading(false)
    queue(l)
  })
  running = proc
  const res = await proc.done
  if (running !== proc) return // 已经换成别的了
  flush()
  setLoading(false)
  streaming.value = false
  if (!res.ok) error.value = res.error ?? ''
}

async function stop() {
  const r = running
  running = undefined
  await r?.kill()
}

/** 换了运行、文件、方式：回到看末尾 */
function reload() {
  clearSearch()
  around.value = 0
  start()
}

function onRunChange() {
  // 换了运行：所选文件不在这次运行里就回到默认
  if (path.value && !run.value?.files.some((f) => f.path === path.value)) path.value = ''
  clearSearch()
  reload()
}

// ---------------------------------------------------------------- 搜索

const keyword = ref('')
const scope = ref<'file' | 'all'>('all')
const caseSensitive = ref(false)
const searching = ref(false)
const result = ref<LogSearch | null>(null)
const searchError = ref('')
const searchedFor = ref('')
const searchedCase = ref(false)
let searchSeq = 0
let searchProc: Running<LogSearch> | undefined

async function search() {
  const kw = keyword.value.trim()
  if (!kw || noRuns.value) return
  const seq = ++searchSeq
  searchProc?.kill()
  searching.value = true
  searchError.value = ''
  result.value = null
  const matchCase = caseSensitive.value
  const args = ['logs', props.target, props.app, '-c', props.config, '--grep', kw, '--context', '2', '-n', '300']
  const r = run.value
  if (!isCurrent.value && r) args.push('--release', r.release, '--run', r.id!)
  if (file.value) {
    if (scope.value === 'all') args.push('--all-files')
    else args.push(...(path.value ? ['--log', path.value] : ['--file']))
  }
  if (matchCase) args.push('--case')
  const proc = ferry<LogSearch>(args)
  searchProc = proc
  try {
    const r = await proc.done
    if (seq !== searchSeq) return
    if (!r.ok) throw new Error(r.error ?? '搜索失败')
    result.value = r.data ?? { matches: [], files: 0, truncated: false }
    searchedFor.value = kw
    searchedCase.value = matchCase
  } catch (e) {
    if (seq !== searchSeq) return
    searchError.value = (e as Error).message
    result.value = null
  } finally {
    if (seq === searchSeq) searching.value = false
  }
}

function clearSearch() {
  searchSeq++
  searchProc?.kill()
  searchProc = undefined
  searching.value = false
  result.value = null
  searchError.value = ''
}
watch([runKey, file, path, scope, caseSensitive], clearSearch)

/** 搜索结果点进去：打开那个文件的这一行附近 */
function openMatch(m: LogSearch['matches'][number]) {
  if (!m.file) return
  file.value = true
  path.value = m.file
  around.value = m.line
  clearSearch()
  // watch 清理旧结果后再开始读取，避免模式切换和搜索的响应交叉。
  nextTick(start)
}

// 把关键字标出来（先转义 HTML）
function mark(text: string): string {
  const esc = text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  const kw = searchedFor.value.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  if (!kw) return esc
  const re = new RegExp(kw.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), searchedCase.value ? 'g' : 'gi')
  return esc.replace(re, (x) => `<mark>${x}</mark>`)
}

const scopeTip = computed(() => {
  if (!file.value) return '在服务输出里找（这次运行期间的）'
  return scope.value === 'all' ? '在这次运行的全部日志文件里找（包括压缩的 .gz）' : `只在 ${path.value || 'diagLogs 指定的文件'} 里找`
})

onMounted(() => {
  if (!props.release) start() // 指定了版本：等知道它有哪些运行再开始
  loadRuns()
})
onMounted(() => {
  rateTimer = setInterval(() => {
    rate.value = rateCount
    rateCount = 0
  }, 1000)
})
onBeforeUnmount(() => {
  disposed = true
  loadSeq++
  clearSearch()
  clearInterval(rateTimer)
  clearInterval(waitTimer)
  clearTimeout(flushTimer)
  stop()
})
</script>

<template>
  <el-dialog v-model="visible" :title="release ? `日志：${app}（${target}）· 版本 ${release}` : `日志：${app}（${target}）`" :width="expandedLogs ? '96vw' : 'min(1180px, 94vw)'" top="4vh" @closed="emit('close')">
    <div class="logs-workspace">
    <div class="log-context"><span><i :class="{ live: isCurrent && follow && streaming && !paused }" />{{ loading ? '正在读取日志' : isCurrent && follow ? streaming ? paused ? '显示已暂停' : '持续接收日志' : '接收已结束' : '日志快照' }}</span><span class="muted small">{{ lines.length }} 行已加载<span v-if="paused && buffered"> · {{ buffered }} 行待显示</span></span><el-button v-if="!result && lines.length" size="small" text @click="copyLog">复制日志</el-button><el-button v-if="!result && !around && !stickBottom" size="small" text type="primary" @click="followLatest">回到最新输出</el-button><el-button size="small" text class="expand-logs" @click="expandedLogs = !expandedLogs">{{ expandedLogs ? '还原宽度' : '展开工作区' }}</el-button></div>
    <div class="bar">
      <el-select v-model="runKey" size="small" class="run" :loading="!runsLoaded && !runsError" :disabled="noRuns" @change="onRunChange">
        <el-option v-if="!release && !runs.some((r) => r.current)" value="current" label="当前运行" />
        <el-option v-for="r in shownRuns" :key="keyOf(r)" :value="keyOf(r)" :label="runLabel(r)">
          <span :class="{ cur: r.current }">{{ runLabel(r) }}</span>
        </el-option>
      </el-select>
      <el-radio-group v-model="file" size="small" @change="(clearSearch(), reload())">
        <el-tooltip :show-after="HOVER_DELAY" placement="bottom">
          <template #content>
            程序打印到控制台的内容（标准输出 / 错误输出），以及启动、停止、崩溃等服务事件。<br />
            由系统日志服务 journald 记录，相当于 journalctl -u 单元名
          </template>
          <el-radio-button :value="false">服务输出</el-radio-button>
        </el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" placement="bottom">
          <template #content>程序自己写到磁盘上的日志文件（如 logback 的 error.log），和这个版本的程序放在一起</template>
          <el-radio-button :value="true">日志文件</el-radio-button>
        </el-tooltip>
      </el-radio-group>
      <el-select v-if="file" v-model="path" size="small" class="file" placeholder="diagLogs 指定的文件" clearable @change="reload">
        <el-option v-for="(f, i) in run?.files ?? []" :key="f.path" :value="f.path" :label="fileLabel(f)">
          <span>{{ fileLabel(f) }}</span>
          <el-tag v-if="i === 0" size="small" type="success" class="newest">最新</el-tag>
        </el-option>
      </el-select>
      <div class="log-control-group">
      <el-tooltip :show-after="HOVER_DELAY" :disabled="isCurrent" content="以往的运行已经结束，没有新的输出" placement="top">
        <el-checkbox v-model="follow" :disabled="!isCurrent" @change="reload">持续输出</el-checkbox>
      </el-tooltip>
      <el-tooltip :show-after="HOVER_DELAY" content="暂停滚动：新的输出先攒着（最多 5000 行），方便看清楚；点「继续」接着显示" placement="top">
        <el-button v-if="isCurrent && follow && !around" size="small" :type="paused ? 'warning' : 'default'" @click="togglePause">{{ paused ? '继续' : '暂停' }}</el-button>
      </el-tooltip>
      </div>
      <div class="log-control-group">
      <span class="muted">最近</span>
      <el-input-number v-model="count" :min="50" :max="5000" :step="100" size="small" @change="reload" />
      <span class="muted">行</span>
      <el-button size="small" @click="reload">重新加载</el-button>
      </div>
    </div>
    <div class="bar">
      <el-input v-model="keyword" size="small" class="kw" :prefix-icon="Search" placeholder="搜索关键字，回车开始" clearable @keyup.enter="search" />
      <el-radio-group v-if="file" v-model="scope" size="small">
        <el-radio-button value="all">全部日志文件</el-radio-button>
        <el-radio-button value="file">只搜这个文件</el-radio-button>
      </el-radio-group>
      <el-checkbox v-model="caseSensitive" size="small">区分大小写</el-checkbox>
      <el-button size="small" type="primary" :loading="searching" :disabled="!keyword.trim() || noRuns" @click="search">搜索</el-button>
      <span class="muted small">{{ scopeTip }}</span>
    </div>
    <div v-if="run && file && run.dir" class="hint muted">日志文件在服务器上的 <span class="mono">{{ run.dir }}</span> 下</div>
    <div v-else-if="run && !isCurrent && !file" class="hint muted">这次运行期间（{{ shortTime(run.start) }} ~ {{ shortTime(run.end) }}）的服务输出</div>
    <el-alert v-if="noRuns" type="info" :title="`版本 ${release} 没有运行日志：没有运行过，或日志已随版本清理`" :closable="false" show-icon class="err" />
    <el-alert v-if="runsError" type="warning" :title="`读取以往的运行失败：${runsError}`" :closable="false" show-icon class="err" />
    <el-alert v-if="searchError" type="error" :title="`搜索失败：${searchError}`" :closable="false" show-icon class="err" />

    <!-- 搜索结果 -->
    <div v-if="searching" class="log busy"><div class="loading" role="status"><el-icon class="is-loading spin"><Loading /></el-icon><span>正在搜索「{{ keyword.trim() }}」…</span><el-button text class="res-close" @click="clearSearch">返回日志</el-button></div></div>
    <div v-else-if="result" class="log mono results">
      <div class="res-head">
        <span>
          「{{ searchedFor }}」：找到 {{ result.matches.length }} 处<template v-if="file">（搜了 {{ result.files }} 个文件）</template>
          <template v-if="result.truncated">，太多了只显示前 {{ result.matches.length }} 处：换个更具体的关键字</template>
        </span>
        <el-button :icon="Close" size="small" link class="res-close" @click="clearSearch">回到日志</el-button>
      </div>
      <div v-if="!result.matches.length" class="empty">没有找到</div>
      <div v-for="(m, i) in result.matches" :key="i" class="match">
        <div v-if="m.file" class="where" @click="openMatch(m)">{{ m.file }} · 第 {{ m.line }} 行（点击看前后）</div>
        <div v-for="(l, j) in m.before ?? []" :key="'b' + j" class="ctx">{{ l }}</div>
        <div class="hitline" v-html="mark(m.text)" />
        <div v-for="(l, j) in m.after ?? []" :key="'a' + j" class="ctx">{{ l }}</div>
      </div>
    </div>

    <template v-else>
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="err" />
      <div v-if="around" class="hint muted">
        {{ path }} 第 {{ around }} 行附近
        <el-button link size="small" type="primary" @click="reload">回到末尾</el-button>
      </div>
      <div v-if="isCurrent && follow && !around && (rate > 100 || paused)" class="hint fast">
        <template v-if="paused">已暂停：新的输出先攒着<template v-if="dropped">（已丢掉最早的 {{ dropped }} 行）</template>，点「继续」接着显示</template>
        <template v-else>输出很快（每秒约 {{ rate }} 行），只显示最近 {{ MAX }} 行；想找某个错误可以用上面的搜索</template>
      </div>
      <div ref="box" class="log mono" :class="{ busy: loading && !lines.length }" role="region" aria-label="日志内容" tabindex="0" @scroll="onScroll">
        <!-- 读取中：大日志、慢网络时要等一会儿，说清楚在读什么，免得以为没有日志 -->
        <div v-if="loading && !lines.length" class="loading">
          <el-icon class="is-loading spin"><Loading /></el-icon>
          <div>{{ loadingText }}</div>
          <div v-if="waited >= 2" class="loading-sub">
            已等 {{ waited }} 秒<template v-if="waited >= 8">：网络较慢或服务器忙，还在读</template>
          </div>
        </div>
        <template v-if="firstLine">
          <div v-for="(l, i) in lines" :key="i" :class="{ hit: firstLine + i === around }"><span class="ln">{{ firstLine + i }}</span>{{ l }}</div>
        </template>
        <pre v-else class="log-text">{{ text }}</pre>
        <div v-if="!lines.length && !error && !loading" class="empty">
          {{ streaming ? '（还没有输出，有新的会自动显示）' : file ? '（这个文件是空的）' : '（这次运行还没有输出）' }}
        </div>
      </div>
    </template>
    </div>
  </el-dialog>
</template>

<style scoped>
.logs-workspace { display: flex; flex-direction: column; height: min(680px, calc(100dvh - 180px)); min-height: 340px; }
.logs-workspace > :not(.log) { flex-shrink: 0; }
.log-context { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 16px; padding-bottom: 12px; margin-bottom: 12px; border-bottom: 1px solid var(--ferry-stroke); font-size: 12px; }
.log-context > span:first-child { display: flex; align-items: center; gap: 7px; }
.log-context .el-button { margin-left: 0; }
.log-context .expand-logs { margin-left: auto; }
.log-context i { width: 6px; height: 6px; border-radius: 50%; background: var(--el-text-color-placeholder); }
.log-context i.live { background: var(--el-color-success); box-shadow: 0 0 0 4px var(--el-color-success-light-9); }
.bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 6px;
  flex-wrap: wrap;
}
.run {
  flex: 1 1 300px;
  min-width: 220px;
  max-width: 100%;
}
.log-control-group { display: flex; align-items: center; gap: 8px; flex-shrink: 0; white-space: nowrap; }
.log-control-group .el-input-number { width: 100px; }
.log-control-group .el-button { margin-left: 0; }
.file {
  width: 420px;
}
.kw {
  width: 260px;
}
.newest {
  margin-left: 8px;
}
.cur {
  color: var(--el-color-success);
}
.hint {
  font-size: 12px;
  margin-bottom: 8px;
}
.small {
  font-size: 12px;
}
.err {
  margin-bottom: 8px;
}
.log {
  flex: 1;
  min-height: 180px;
  overflow: auto;
  background: #0f172a;
  color: #e2e8f0;
  border-radius: 6px;
  padding: 10px 12px;
  white-space: pre-wrap;
  word-break: break-all;
  line-height: 1.5;
}
.empty {
  color: #64748b;
}
.log.busy {
  display: flex;
  align-items: center;
  justify-content: center;
}
.loading {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  color: #cbd5e1;
  font-size: 13px;
  text-align: center;
}
.spin {
  font-size: 28px;
  color: #93c5fd;
}
.loading-sub {
  color: #64748b;
  font-size: 12px;
}
.log-text {
  margin: 0;
  font: inherit;
  white-space: pre-wrap;
  word-break: break-all;
}
.fast {
  color: var(--el-color-warning);
}
.ln {
  display: inline-block;
  min-width: 4em;
  margin-right: 10px;
  color: #64748b;
  text-align: right;
  user-select: none;
}
.hit {
  background: #713f12;
}
.res-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  color: #94a3b8;
  margin-bottom: 8px;
}
.res-close {
  color: #93c5fd;
}
.match {
  border-top: 1px solid #1e293b;
  padding: 6px 0;
}
.where {
  color: #93c5fd;
  cursor: pointer;
  margin-bottom: 2px;
}
.where:hover {
  text-decoration: underline;
}
.ctx {
  color: #64748b;
}
.hitline :deep(mark) {
  background: #facc15;
  color: #0f172a;
  border-radius: 2px;
}
</style>
