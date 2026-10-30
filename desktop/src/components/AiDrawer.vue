<script setup lang="ts">
// AI 助手：在一台服务器上和大模型对话。只读的查看自动执行；打开「允许改动」后，模型也能改动服务器
// （执行命令、写文件、启停应用、装软件、保存软件模板），但每个改动都先停下来给用户看，用户同意后才执行。
// 每句话（或每次确认）调用一次 ferry ai，对话的上下文和待确认的操作保存在 ferry 那边（按对话 ID 接着来）。
// 能看图片的模型可以附图片：粘贴截图、选文件、拖进输入框。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Promotion, VideoPause, Plus, Setting, CaretRight, CaretBottom, Loading, WarningFilled, Clock, Delete, Cpu, Opportunity, ArrowDown, EditPen, Download, Close, Picture, FullScreen } from '@element-plus/icons-vue'
import MarkdownIt from 'markdown-it'
import { call, ferry, isSecretKind, type FerryLine } from '../ferry'
import { onFileDrop, openUrl, pickSaveFile } from '../platform'
import { appSettings, secretRequest, trustRequest, settingsRequest } from '../state'
import { EFFORT_LABELS, effortOptions, addUsage, zeroUsage, usageText, findModelByName, cost, money, tokens, type Usage } from '../ai'
import { supportsImages, prepareImage, base64Blob, imageUrl, MAX_IMAGES, type AiImage } from '../ai'
import GlassPane from './glass/GlassPane.vue'
import { glass, setVar } from '../glass/runtime'
import GlassSwitch from './glass/GlassSwitch.vue'

const props = defineProps<{
  config: string
  target: string
  title: string
  plain?: boolean
  /** 生产服务器：每次打开都从「只读」开始，每个改动都要确认 */
  production?: boolean
  /** 配置里不允许 AI 改动：只能查看 */
  noChanges?: boolean
}>()
const expandedPanel = ref(false)
const open = defineModel<boolean>({ required: true })

interface Call {
  id: string
  name: string
  input: Record<string, unknown>
}
interface Approval {
  kind: string
  title: string
  detail?: string
  danger?: string
  once?: string // 每次都要确认的原因
  asRoot?: boolean
}
interface Item {
  kind: 'user' | 'text' | 'thinking' | 'usage' | 'tool' | 'error'
  text?: string
  images?: AiImage[] // user：附的图片
  streaming?: boolean // 回复还在逐段生成
  call?: Call
  result?: string
  live?: string // 执行中的实时输出
  isError?: boolean
  done?: boolean
  expanded?: boolean
  approval?: Approval // 要确认的改动
  waiting?: boolean // 等用户确认
  decision?: 'approve' | 'always' | 'reject' | 'skipped' | 'allowed'
}

const items = ref<Item[]>([])
const input = ref('')
const running = ref(false)
const conv = ref('')
const box = ref<HTMLElement>()
const following = ref(true)
const stopping = ref(false)
const loadingConversation = ref(false)
let conversationSeq = 0
let disposed = false
let kill: (() => Promise<void>) | undefined

// 允许改动：记住上次的选择（只在本机）
const CHANGE_KEY = 'ferry.ai.allowChange'
function loadAllow(): boolean {
  try {
    return localStorage.getItem(CHANGE_KEY) === '1'
  } catch {
    return false
  }
}
// 生产服务器不沿用上次的选择，每次从「只读」开始；不允许改动的服务器一直是只读
const allowChange = ref(props.production || props.noChanges ? false : loadAllow())
watch(allowChange, (v) => {
  if (props.production || props.noChanges) return
  try {
    localStorage.setItem(CHANGE_KEY, v ? '1' : '0')
  } catch {
    /* 存不了就算了 */
  }
})

const waitingItem = computed(() => items.value.find((x) => x.waiting))

// 用哪个模型、思考强度（设置 → AI 里的模型列表；记住上次选的模型，思考强度默认用模型设置的）
const MODEL_KEY = 'ferry.ai.model'
function loadModelPref(): string {
  try {
    return localStorage.getItem(MODEL_KEY) ?? ''
  } catch {
    return ''
  }
}
const models = computed(() => appSettings.value?.ai?.models ?? [])
const modelId = ref(loadModelPref())
const curModel = computed(
  () => models.value.find((m) => m.id === modelId.value) ?? models.value.find((m) => m.id === appSettings.value?.ai?.default) ?? models.value[0],
)
const effort = ref(curModel.value?.effort || 'none')
const efforts = computed(() => effortOptions(curModel.value ?? { provider: 'anthropic', thinking: '', baseUrl: '', model: '' }))
watch(
  () => curModel.value?.id,
  () => {
    effort.value = curModel.value?.effort || 'none'
  },
)
// 设置里改了思考参数，原来选的这一档没有了
watch(efforts, (opts) => {
  if (!opts.some((o) => o.value === effort.value)) effort.value = 'none'
})

// ---------------------------------------------------------------- 图片

const attachments = ref<AiImage[]>([]) // 输入框里附的图片，发送时一起发
const canImages = computed(() => supportsImages(curModel.value))
const fileInput = ref<HTMLInputElement>()
const composerEl = ref<HTMLElement>()
const dropOver = ref(false)
let adding = 0 // 正在处理的图片

function cannotSee() {
  ElMessage.warning(`${curModel.value?.name ?? '当前模型'} 不能看图片：换一个能看图片的模型，或在「模型设置」里改`)
}

async function addImages(blobs: Blob[]) {
  if (!blobs.length || running.value) return
  if (!canImages.value) return cannotSee()
  const room = MAX_IMAGES - attachments.value.length - adding
  if (room <= 0) {
    ElMessage.warning(`一次最多附 ${MAX_IMAGES} 张图片`)
    return
  }
  if (blobs.length > room) ElMessage.warning(`一次最多附 ${MAX_IMAGES} 张图片，多的没有加上`)
  for (const b of blobs.slice(0, room)) {
    adding++
    try {
      attachments.value.push(await prepareImage(b))
    } catch (e) {
      ElMessage.error((e as Error).message)
    } finally {
      adding--
    }
  }
}

// 粘贴：剪贴板里有图片（截图）就附上；同时有文字的照常粘贴文字
function onPaste(e: ClipboardEvent) {
  const files = [...(e.clipboardData?.items ?? [])].filter((x) => x.kind === 'file' && x.type.startsWith('image/')).map((x) => x.getAsFile())
  const blobs = files.filter((f): f is File => !!f)
  if (!blobs.length) return
  if (!e.clipboardData?.types.includes('text/plain')) e.preventDefault()
  addImages(blobs)
}

function pickImages() {
  fileInput.value?.click()
}

function onFilePicked(e: Event) {
  const el = e.target as HTMLInputElement
  addImages([...(el.files ?? [])])
  el.value = ''
}

// 浏览器里拖进来的文件（桌面版的拖放由下面的 onFileDrop 处理）
function onDrop(e: DragEvent) {
  dropOver.value = false
  addImages([...(e.dataTransfer?.files ?? [])].filter((f) => f.type.startsWith('image/')))
}

// 桌面版：从资源管理器 / Finder 拖进输入框的图片文件，由 ferry 读出来
let unlisten: (() => void) | undefined
onMounted(async () => {
  unlisten = await onFileDrop(
    (x, y) => {
      const r = composerEl.value?.getBoundingClientRect()
      return open.value && !!r && r.width > 0 && x >= r.left && x <= r.right && y >= r.top && y <= r.bottom
    },
    async (paths) => {
      if (running.value) return
      if (!canImages.value) return cannotSee()
      try {
        const r = await call<{ images: AiImage[] }>(['ai-image', ...paths])
        await addImages((r.images ?? []).map(base64Blob))
      } catch (e) {
        ElMessage.error((e as Error).message)
      }
    },
    (over) => (dropOver.value = over),
  )
  if (disposed) unlisten?.()
})
onBeforeUnmount(() => { disposed = true; unlisten?.(); conversationSeq++; historySeq++; kill?.(); layoutObserver?.disconnect() })

// 液态玻璃打开时，标题栏（和等待确认的提示条）浮在对话上方、输入框浮在下方，对话从它们下面滚过。
// 量出它们的高度写到抽屉上，对话上下各留出这么多，最早和最新的内容不会被挡住。
let layoutObserver: ResizeObserver | undefined
function measureLayout() {
  const panel = box.value?.closest<HTMLElement>('.el-drawer')
  if (!panel) return
  const height = (selector: string) => `${panel.querySelector<HTMLElement>(selector)?.offsetHeight ?? 0}px`
  setVar(panel, '--ai-head', height('.el-drawer__header'))
  setVar(panel, '--ai-banner', height('.pending-banner'))
  setVar(panel, '--ai-input', height('.ai-input'))
}
function observeLayout() {
  layoutObserver?.disconnect()
  const panel = box.value?.closest<HTMLElement>('.el-drawer')
  if (!panel || !glass.enabled) return
  layoutObserver = new ResizeObserver(() => {
    const atBottom = following.value
    measureLayout()
    if (atBottom) scroll()
  })
  for (const selector of ['.el-drawer__header', '.pending-banner', '.ai-input']) {
    const element = panel.querySelector(selector)
    if (element) layoutObserver.observe(element)
  }
  measureLayout()
}
watch([open, () => !!waitingItem.value, () => glass.enabled], () => nextTick(observeLayout), { immediate: true })

function removeImage(i: number) {
  attachments.value.splice(i, 1)
}

// ---------------------------------------------------------------- 用量与费用

let turnUsage: Usage = zeroUsage() // 这一轮（一句话或一次确认）
let turnModel = ''
const convUsage = ref<Record<string, Usage>>({}) // 这个对话各模型一共用了多少
const totalText = computed(() => {
  let all = zeroUsage()
  let money_ = 0
  let currency = ''
  let priced = false
  for (const [name, u] of Object.entries(convUsage.value)) {
    all = addUsage(all, u)
    const m = findModelByName(models.value, name)
    const c = cost(m, u)
    if (c !== undefined) {
      money_ += c
      priced = true
      currency = currency || m!.currency || '¥'
    }
  }
  const n = all.input + (all.cacheRead ?? 0) + (all.cacheWrite ?? 0) + all.output
  if (!n) return ''
  return `本对话 ${tokens(n)} tokens` + (priced ? ` · 约 ${money(money_, currency)}` : '')
})
function pickModel(id: string) {
  modelId.value = id
  try {
    localStorage.setItem(MODEL_KEY, id)
  } catch {
    /* 存不了就算了 */
  }
}

const examples = computed(() => {
  if (props.plain) {
    return allowChange.value
      ? ['看看这台服务器的磁盘、内存和负载', '帮我装一个 Redis，只允许本机访问', '清理一下 journal 日志，只保留 500M']
      : ['看看这台服务器的磁盘、内存和负载', '哪些端口在监听，分别是什么程序', '最近的系统日志里有没有报错']
  }
  return allowChange.value
    ? ['各应用现在运行得怎么样，有没有问题', '帮我装一个 Redis，只允许本机访问', '重启有问题的应用，再看看日志']
    : ['各应用现在运行得怎么样，有没有问题', '看看最近的错误日志，分析一下原因', '磁盘、内存够不够用']
})

function onConversationScroll() {
  const el = box.value
  if (el) following.value = el.scrollHeight - el.scrollTop - el.clientHeight < 60
}
async function scroll(force = false) {
  await nextTick()
  if (force) following.value = true
  if (box.value && following.value) box.value.scrollTop = box.value.scrollHeight
}
function showApproval() {
  box.value?.querySelector('.tool.ask')?.scrollIntoView({ block: 'center' })
}

function findTool(id: string) {
  return [...items.value].reverse().find((x) => x.kind === 'tool' && x.call?.id === id)
}

const LIVE_MAX = 200_000

// 正在生成的回答、思考都结束了
function finishStreaming() {
  for (const it of items.value) {
    if (it.streaming) {
      it.streaming = false
      // 只剩空白的半段回答不留
      if (it.kind === 'text' && !it.text?.trim()) it.text = ''
    }
  }
  items.value = items.value.filter((it) => !(it.kind === 'text' && !it.text))
}

function onLine(l: FerryLine) {
  if (disposed || l.kind !== 'ai') return
  const ev = l as FerryLine & {
    type: string
    text?: string
    call?: Call
    result?: string
    isError?: boolean
    approval?: Approval
    allowed?: boolean
    usage?: Usage
    model?: string
  }
  const last = items.value[items.value.length - 1]
  if (ev.type === 'delta' && ev.text) {
    // 回答逐段到达：接在正在生成的那段后面
    if (last?.kind === 'text' && last.streaming) last.text = (last.text ?? '') + ev.text
    else {
      finishStreaming()
      items.value.push({ kind: 'text', text: ev.text, streaming: true })
    }
  } else if (ev.type === 'thinking' && ev.text) {
    if (last?.kind === 'thinking' && last.streaming) last.text = (last.text ?? '') + ev.text
    else items.value.push({ kind: 'thinking', text: ev.text, streaming: true })
  } else if (ev.type === 'text' && ev.text) {
    // 完整的回答：替换逐段拼起来的那段（以它为准）
    if (last?.kind === 'text' && last.streaming) {
      last.text = ev.text
      last.streaming = false
    } else items.value.push({ kind: 'text', text: ev.text })
    finishStreaming()
  } else if (ev.type === 'usage' && ev.usage) {
    turnUsage = addUsage(turnUsage, ev.usage)
    turnModel = ev.model ?? turnModel
    const name = ev.model ?? ''
    convUsage.value = { ...convUsage.value, [name]: addUsage(convUsage.value[name] ?? zeroUsage(), ev.usage) }
    finishStreaming()
  } else if (ev.type === 'call' && ev.call) {
    finishStreaming()
    // 确认后接着执行时不会再发 call（卡片已经在了）
    if (!findTool(ev.call.id)) {
      items.value.push({
        kind: 'tool',
        call: ev.call,
        approval: ev.allowed ? ev.approval : undefined,
        decision: ev.allowed ? 'allowed' : undefined,
      })
    }
  } else if (ev.type === 'confirm' && ev.call) {
    const it = findTool(ev.call.id)
    if (it) {
      it.approval = ev.approval
      it.waiting = true
    }
  } else if (ev.type === 'output' && ev.call) {
    const it = findTool(ev.call.id)
    if (it) {
      let s = (it.live ?? '') + (ev.text ?? '') + '\n'
      if (s.length > LIVE_MAX) s = '…\n' + s.slice(s.length - LIVE_MAX)
      it.live = s
      it.expanded = true
    }
  } else if (ev.type === 'result' && ev.call) {
    const it = findTool(ev.call.id)
    if (it) {
      it.result = ev.result
      it.isError = ev.isError
      it.done = true
      it.waiting = false
      // 改动的结果默认展开（看得到执行了什么），查看的出错才展开
      it.expanded = !!ev.isError || (!!it.approval && it.decision !== 'reject')
    }
  }
  scroll()
}

// 调用一次 ferry ai：说一句话（message，可以附图片），或对待确认的操作做决定（decide）
async function run(message: string, decide?: string, retry?: () => void, rewind = 0, images: AiImage[] = []) {
  running.value = true
  turnUsage = zeroUsage()
  turnModel = ''
  scroll()
  // 对等确认的操作做决定时总按「允许改动」执行（用户已经明确同意或拒绝了）
  const mode = decide || allowChange.value ? 'confirm' : 'readonly'
  const args = ['-c', props.config, 'ai', props.target, '--mode', mode]
  if (curModel.value) args.push('--model', curModel.value.id, '--effort', effort.value || 'none')
  if (conv.value) args.push('--conv', conv.value)
  if (decide) args.push('--decide', decide)
  if (rewind > 0) args.push('--rewind', String(rewind))
  const env: Record<string, string> = {}
  if (!decide) {
    env.FERRY_AI_MESSAGE = message
    if (images.length) env.FERRY_AI_IMAGES = JSON.stringify(images)
  }
  const r0 = ferry<{ conv?: string; pending?: boolean }>(args, onLine, env)
  kill = r0.kill
  const r = await r0.done
  if (disposed) return
  running.value = false
  kill = undefined
  finishStreaming()
  if (r.data?.conv) conv.value = r.data.conv
  // 这一轮用了多少（每次请求都会带上前面的对话，所以对话越长每轮越多）
  if (turnUsage.input + turnUsage.output > 0) {
    items.value.push({ kind: 'usage', text: usageText(turnUsage, findModelByName(models.value, turnModel)) })
  }
  if (!r.ok) {
    const d = r.detail
    if (d?.kind === 'unknown-host') {
      trustRequest.value = { target: d.target || props.target, config: props.config, retry }
    } else if (d && isSecretKind(d.kind) && retry) {
      secretRequest.value = { detail: d, config: props.config, retry }
    }
    items.value.push({ kind: 'error', text: r.error ?? '出错了' })
    // 进行到一半的工具调用不会再有结果
    for (const it of items.value) {
      if (it.kind === 'tool' && !it.done && !it.waiting) it.done = true
    }
  }
  scroll()
}

// 改第几句话（从 1 数）重新发；0 = 没有在改
const editing = ref(0)

function startEdit(i: number) {
  if (running.value) return
  editing.value = items.value.slice(0, i + 1).filter((x) => x.kind === 'user').length
  input.value = items.value[i].text ?? ''
  attachments.value = [...(items.value[i].images ?? [])]
}

function cancelEdit() {
  editing.value = 0
  input.value = ''
  attachments.value = []
}

async function send(text = input.value, retry = false, images = attachments.value) {
  text = text.trim()
  if ((!text && !images.length) || running.value || loadingConversation.value || adding) return
  if (!models.value.length) { ElMessage.warning('请先在模型设置里添加一个模型'); return }
  let rewind = 0
  if (!retry && editing.value > 0 && conv.value) {
    // 去掉这句和它后面的，换成新的这句
    rewind = editing.value
    let n = 0
    const at = items.value.findIndex((x) => x.kind === 'user' && ++n === rewind)
    if (at >= 0) items.value = items.value.slice(0, at)
  }
  editing.value = 0
  if (!retry) {
    // 有等确认的操作时接着说了别的：那个操作不执行了
    const w = waitingItem.value
    if (w) {
      w.waiting = false
      w.done = true
      w.decision = 'skipped'
    }
    items.value.push({ kind: 'user', text, images: images.length ? images : undefined })
    input.value = ''
    attachments.value = []
  }
  following.value = true
  await run(text, undefined, () => send(text, true, images), rewind, images)
}

async function decide(it: Item, choice: 'approve' | 'always' | 'reject') {
  if (running.value) return
  it.waiting = false
  it.decision = choice
  if (choice === 'reject') it.done = true
  await run('', choice, () => decide(it, choice))
}

async function stop() {
  if (stopping.value) return
  stopping.value = true
  try { await kill?.() } finally { stopping.value = false }
}

/** 「让 AI 看看」：新开一个对话，直接问这句话（应用异常、任务失败时从别处点进来） */
let asking = false
async function ask(text: string) {
  if (running.value) {
    ElMessage.warning('AI 助手正在回答，等它说完再问')
    return
  }
  asking = true
  restored = true
  newConv()
  await nextTick()
  send(text)
}
defineExpose({ ask })

function newConv() {
  if (running.value) return
  conversationSeq++
  loadingConversation.value = false
  following.value = true
  items.value = []
  conv.value = ''
  convUsage.value = {}
  editing.value = 0
  attachments.value = []
  input.value = ''
}

// 导出成 Markdown 文件
async function exportConv() {
  if (!conv.value || running.value) return
  const file = await pickSaveFile('导出对话', `AI助手-${props.title}-${conv.value.slice(0, 15)}.md`, 'Markdown', ['md'])
  if (!file) return
  try {
    await call(['ai-history', 'export', conv.value, '--out', file])
    ElMessage.success('已导出')
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// ---------------------------------------------------------------- 历史对话

interface ConvSummary {
  id: string
  title: string
  updated: string
  messages: number
  pending?: boolean
}
const history = ref<ConvSummary[]>([])
const historyOpen = ref(false)
const historyLoading = ref(false)
const historyError = ref('')
const historyQuery = ref('')
const matchingHistory = computed(() => history.value.filter((h) => (h.title ?? '').toLowerCase().includes(historyQuery.value.trim().toLowerCase())))
let historySeq = 0

async function loadHistory() {
  const seq = ++historySeq
  historyLoading.value = true
  historyError.value = ''
  try {
    const r = await call<{ items: ConvSummary[] }>(['ai-history', 'list', '-c', props.config, props.target])
    if (seq !== historySeq) return false
    history.value = r.items ?? []
    return true
  } catch (e) {
    if (seq === historySeq) historyError.value = (e as Error).message
    return false
  } finally {
    if (seq === historySeq) historyLoading.value = false
  }
}

async function openConv(id: string) {
  if (running.value) return
  const seq = ++conversationSeq
  loadingConversation.value = true
  try {
    const r = await call<{ id: string; items: Item[]; usage?: Record<string, Usage> }>(['ai-history', 'show', id])
    if (seq !== conversationSeq) return
    items.value = (r.items ?? []).map((it) => ({ ...it, expanded: false }))
    conv.value = r.id
    convUsage.value = r.usage ?? {}
    editing.value = 0
    input.value = ''
    attachments.value = []
    historyOpen.value = false
    scroll(true)
  } catch (e) {
    if (seq !== conversationSeq) return
    ElMessage.error((e as Error).message)
  } finally {
    if (seq === conversationSeq) loadingConversation.value = false
  }
}

async function deleteConv(id: string) {
  try {
    await call(['ai-history', 'delete', id])
    if (id === conv.value) newConv()
    await loadHistory()
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

function when(t: string) {
  const d = new Date(t)
  const p = (n: number) => String(n).padStart(2, '0')
  const sameDay = d.toDateString() === new Date().toDateString()
  return sameDay ? `今天 ${p(d.getHours())}:${p(d.getMinutes())}` : `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

// 第一次打开时接着上次的对话
let restored = false
watch(
  open,
  async (v) => {
    if (!v || restored) return
    restored = true
    const available = await loadHistory()
    if (available && !items.value.length && !conv.value && history.value.length && !asking) await openConv(history.value[0].id)
  },
  { immediate: true },
)

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape' && editing.value) { e.preventDefault(); cancelEdit(); return }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault()
    send()
  }
}

const verbs: Record<string, string> = { start: '启动', stop: '停止', restart: '重启' }

function callTitle(c: Call): string {
  const i = c.input as Record<string, string>
  switch (c.name) {
    case 'run_command':
      return i.command ?? ''
    case 'app_status':
      return '查看各应用的状态'
    case 'app_logs':
      return `看 ${i.app} 的${i.source === 'file' ? '日志文件' : '服务输出'}${i.path ? '（' + i.path + '）' : ''}${i.keyword ? '，搜索「' + i.keyword + '」' : ''}`
    case 'list_software':
      return '列出服务器上已有的软件'
    case 'app_releases':
      return `查看${i.app ? ' ' + i.app + ' ' : '应用'}保留的版本`
    case 'software_templates':
      return '查看软件模板库'
    case 'template_guide':
      return '查看软件模板的格式'
    case 'write_file':
      return `写文件 ${i.path ?? ''}`
    case 'app_control': {
      const apps = (c.input.apps as string[] | undefined) ?? []
      return `${verbs[i.action] ?? i.action}应用 ${apps.join('、')}`
    }
    case 'app_rollback':
      return `回滚 ${i.app}${i.to ? ' 到 ' + i.to : ''}`
    case 'software_install':
      return `安装软件 ${i.template || '（自定义模板）'}${i.version ? ' ' + i.version : ''}`
    case 'software_control':
      return `${verbs[i.action] ?? i.action}软件 ${i.name}`
    case 'save_software_template':
      return '保存软件模板'
  }
  return c.name
}

const decisionText: Record<string, string> = {
  approve: '已同意执行',
  always: '已同意，本次对话同类不再询问',
  reject: '没有执行（你拒绝了）',
  skipped: '没有执行（你接着说了别的）',
  allowed: '同类已允许，直接执行',
}

// 差异里的行上色：+ 加上的、- 删掉的
function detailLines(d: string) {
  return d.split('\n').map((t) => ({ t, c: t.startsWith('+ ') ? 'add' : t.startsWith('- ') ? 'del' : '' }))
}

// 模型的回答按 Markdown 显示（表格、列表、引用、代码块、链接等）。不放行 HTML，链接只认 http(s)。
const markdown = new MarkdownIt({ html: false, linkify: true, breaks: true })
markdown.validateLink = (url) => /^https?:\/\//i.test(url.trim())
// 表格放进可横向滚动的框里，宽表格不撑破抽屉
markdown.renderer.rules.table_open = () => '<div class="table-wrap"><table>'
markdown.renderer.rules.table_close = () => '</table></div>'
function md(text: string): string {
  return markdown.render(text)
}

// 链接用系统浏览器打开（不在应用窗口里跳转）
function onBotClick(e: MouseEvent) {
  const a = (e.target as HTMLElement).closest('a')
  if (!a) return
  e.preventDefault()
  const href = a.getAttribute('href') ?? ''
  if (/^https?:\/\//i.test(href)) openUrl(href).catch((err) => ElMessage.error(String(err)))
}

function openSettings() {
  settingsRequest.value = 'ai'
}

function copy(text: string) {
  navigator.clipboard?.writeText(text).then(() => ElMessage.success('已复制'))
}
</script>

<template>
  <el-drawer v-model="open" direction="rtl" :size="expandedPanel ? 'min(1120px, 94vw)' : 'min(720px, 92vw)'" append-to-body class="ai-drawer" :class="{ 'ai-glass': glass.enabled }">
    <template #header>
      <div class="ai-head">
        <span class="ai-title">AI 助手 · {{ title }}</span>
        <el-tag v-if="production" size="small" type="danger" effect="dark">生产</el-tag>
        <el-tooltip
          :show-after="HOVER_DELAY"
          :content="
            noChanges
              ? '配置里设成了不允许 AI 改动这台服务器（aiChanges: false），只能查看'
              : allowChange
                ? '助手可以改动服务器，每个改动都先给你看，你同意后才执行'
                : '助手只查看服务器，不做改动'
          "
          placement="bottom"
        >
          <GlassSwitch v-model="allowChange" size="small" active-text="允许改动" inactive-text="只读" :disabled="running || noChanges" aria-label="允许 AI 改动服务器" />
        </el-tooltip>
        <span class="spacer" />
        <el-popover v-model:visible="historyOpen" placement="bottom-end" :width="380" trigger="click" :hide-after="0" @show="loadHistory">
          <template #reference>
            <el-button text size="small" :icon="Clock" :disabled="running">历史</el-button>
          </template>
          <el-input v-model="historyQuery" class="hist-search" placeholder="搜索历史对话" clearable size="small" aria-label="搜索历史对话" />
          <div v-if="historyLoading" class="muted small hist-empty" role="status">正在读取历史对话…</div>
          <div v-else-if="historyError" class="small hist-empty"><span class="warn">{{ historyError }}</span><el-button link type="primary" @click="loadHistory">重试</el-button></div>
          <div v-else-if="!matchingHistory.length" class="muted small hist-empty">{{ historyQuery.trim() ? '没有匹配的对话' : '还没有历史对话' }}</div>
          <div v-else class="hist">
            <div v-for="h in matchingHistory" :key="h.id" class="hist-row" :class="{ cur: h.id === conv }" role="button" tabindex="0" @click="openConv(h.id)" @keydown.enter="openConv(h.id)" @keydown.space.prevent="openConv(h.id)">
              <div class="hist-main">
                <div class="hist-title">{{ h.title || '（空）' }}</div>
                <div class="muted small">
                  {{ when(h.updated) }} · {{ h.messages }} 句<span v-if="h.pending" class="hist-pending"> · 有等确认的操作</span>
                </div>
              </div>
              <el-popconfirm title="删除这段历史对话？此操作无法撤销。" confirm-button-text="删除" cancel-button-text="取消" @confirm="deleteConv(h.id)"><template #reference><el-button text size="small" :icon="Delete" :aria-label="`删除对话 ${h.title}`" @click.stop @keydown.stop /></template></el-popconfirm>
            </div>
          </div>
        </el-popover>
        <el-tooltip :show-after="HOVER_DELAY" content="把这个对话导出成 Markdown 文件" placement="bottom">
          <el-button text size="small" :icon="Download" :disabled="running || !conv" @click="exportConv">导出</el-button>
        </el-tooltip>
        <el-button text size="small" :icon="Setting" aria-label="模型设置" @click="openSettings" />
        <el-button text size="small" :icon="FullScreen" :aria-label="expandedPanel ? '收窄 AI 面板' : '展开 AI 面板'" :aria-pressed="expandedPanel" @click="expandedPanel = !expandedPanel" />
        <el-button text size="small" :icon="Plus" :disabled="running" @click="newConv">新对话</el-button>
      </div>
      <!-- 放在后面：抽屉标题栏让第一个子元素占满宽度，那得是上面这一行。
           抽屉自己的底色是固定的，不跟着背后内容换深浅（深色代码块滚过时也不会闪） -->
      <GlassPane v-if="glass.enabled" class="ai-head-glass" :radius="0" :adaptive="false" aria-hidden="true" />
    </template>

    <div v-if="waitingItem" class="pending-banner" role="status"><el-icon><WarningFilled /></el-icon><span>有一项操作等待确认 · {{ waitingItem.approval?.title }}</span><el-button size="small" text type="warning" @click="showApproval">查看操作</el-button></div>
    <div ref="box" class="ai-body" role="region" aria-label="对话内容" tabindex="0" @scroll.passive="onConversationScroll">
      <div v-if="loadingConversation" class="ai-empty muted" role="status"><el-icon class="is-loading"><Loading /></el-icon> 正在读取对话…</div>
      <div v-else-if="!items.length" class="ai-empty">
        <div class="muted">
          {{ allowChange ? `说说要做什么。助手会先在 ${title} 上查看，要改动时停下来给你确认。` : `说说要查什么，助手会在 ${title} 上执行只读命令查看，然后告诉你结果。` }}
        </div>
        <div class="examples">
          <el-button v-for="e in examples" :key="e" size="small" round @click="send(e)">{{ e }}</el-button>
        </div>
      </div>
      <template v-for="(it, i) in items" :key="i">
        <div v-if="it.kind === 'user'" class="user-row">
          <el-tooltip v-if="!running" :show-after="HOVER_DELAY" content="改这句话重新发（它后面的对话会被替换）" placement="left">
            <el-button class="edit-btn" text size="small" :icon="EditPen" @click="startEdit(i)" />
          </el-tooltip>
          <div class="msg user" :class="{ editing: editing > 0 && items.slice(0, i + 1).filter((x) => x.kind === 'user').length === editing }">
            <div v-if="it.images?.length" class="msg-imgs">
              <el-image
                v-for="(im, k) in it.images"
                :key="k"
                :src="imageUrl(im)"
                :preview-src-list="it.images.map(imageUrl)"
                :initial-index="k"
                preview-teleported
                hide-on-click-modal
                fit="cover"
                class="msg-img"
              />
            </div>
            <div v-if="it.text">{{ it.text }}</div>
          </div>
        </div>
        <div v-else-if="it.kind === 'thinking'" class="think">
          <div class="think-head" @click="it.expanded = !it.expanded">
            <el-icon v-if="it.streaming" class="is-loading"><Loading /></el-icon>
            <el-icon v-else><component :is="it.expanded ? CaretBottom : CaretRight" /></el-icon>
            <span>{{ it.streaming ? '正在思考…' : `思考过程（${it.text?.length ?? 0} 字）` }}</span>
          </div>
          <!-- 思考中：固定高度、总是显示最新的几行（上面渐隐），不截断文字 -->
          <div v-if="it.streaming && !it.expanded" class="think-body live">
            <div>{{ it.text }}</div>
          </div>
          <div v-else-if="it.expanded" class="think-body">{{ it.text }}</div>
        </div>
        <div v-else-if="it.kind === 'usage'" class="usage muted">{{ it.text }}</div>
        <div v-else-if="it.kind === 'text'" class="bot-answer"><div class="msg bot" :class="{ streaming: it.streaming }" @click="onBotClick" v-html="md(it.text ?? '')" /><el-button v-if="!it.streaming" size="small" text class="copy-answer" @click="copy(it.text ?? '')">复制回复</el-button></div>
        <div v-else-if="it.kind === 'error'" class="msg err">{{ it.text }}</div>
        <div v-else class="tool" :class="{ bad: it.isError, change: !!it.approval, danger: !!it.approval?.danger, ask: it.waiting }">
          <div class="tool-head" @click="(it.done || it.live) && (it.expanded = !it.expanded)">
            <el-icon v-if="it.waiting" class="warn"><WarningFilled /></el-icon>
            <el-icon v-else-if="!it.done" class="is-loading"><Loading /></el-icon>
            <el-icon v-else><component :is="it.expanded ? CaretBottom : CaretRight" /></el-icon>
            <span v-if="it.call?.name === 'run_command'" class="mono cmd">$ {{ callTitle(it.call!) }}</span>
            <span v-else class="cmd">{{ callTitle(it.call!) }}</span>
          </div>
          <div v-if="it.call?.input?.purpose" class="muted small purpose">{{ it.call.input.purpose }}</div>

          <!-- 要确认的改动 -->
          <div v-if="it.waiting && it.approval" class="confirm">
            <div class="confirm-title">
              <span>{{ it.approval.title }}</span>
              <el-tag v-if="it.approval.asRoot" size="small" type="warning" effect="plain">以 root 执行</el-tag>
              <el-tag v-if="it.approval.danger" size="small" type="danger" effect="dark">危险：{{ it.approval.danger }}</el-tag>
            </div>
            <div v-if="it.approval.once && !it.approval.danger" class="muted small once">每次都要确认：{{ it.approval.once }}</div>
            <pre v-if="it.approval.detail" class="tool-out mono detail"><span v-for="(ln, k) in detailLines(it.approval.detail)" :key="k" :class="ln.c">{{ ln.t }}
</span></pre>
            <div class="confirm-btns">
              <el-button size="small" :type="it.approval.danger ? 'danger' : 'primary'" :disabled="running" @click="decide(it, 'approve')">执行</el-button>
              <el-tooltip v-if="!it.approval.danger && !it.approval.once" :show-after="HOVER_DELAY" :content="`本次对话里「${it.approval.kind}」不再询问`" placement="top">
                <el-button size="small" :disabled="running" @click="decide(it, 'always')">执行，同类不再询问</el-button>
              </el-tooltip>
              <el-button size="small" :disabled="running" @click="decide(it, 'reject')">不执行</el-button>
            </div>
            <div v-if="!it.approval.danger && !it.approval.once" class="muted small scope">
              同类指：{{ it.approval.kind.replace(/^命令：/, '') }}
            </div>
          </div>
          <div v-else-if="it.decision" class="muted small decision" :class="{ no: it.decision === 'reject' || it.decision === 'skipped' }">
            {{ decisionText[it.decision] }}
          </div>

          <pre v-if="it.expanded && !it.done && it.live" class="tool-out mono">{{ it.live }}</pre>
          <pre v-else-if="it.expanded && it.result" class="tool-out mono" @dblclick="copy(it.result)">{{ it.result }}</pre>
        </div>
      </template>
      <div v-if="running && !items[items.length - 1]?.streaming" class="thinking muted"><el-icon class="is-loading"><Loading /></el-icon> {{ allowChange ? '正在处理…' : '正在查看…' }}</div>
    </div>

    <div class="ai-input">
      <div v-if="!following && items.length" class="conversation-position">
        <component :is="glass.enabled ? GlassPane : 'div'" class="position-pill" :radius="glass.enabled ? 'pill' : undefined"><span class="muted small">正在查看较早的内容</span><el-button size="small" text type="primary" @click="scroll(true)">回到最新内容</el-button></component>
      </div>
      <!-- 输入框和模型、思考强度、发送放在一个框里 -->
      <component :is="glass.enabled ? GlassPane : 'div'" v-if="editing" class="edit-bar" :radius="glass.enabled ? 10 : undefined">
        <el-icon><EditPen /></el-icon>
        <span>正在改第 {{ editing }} 句，发送后它后面的对话会被替换（已经在服务器上做了的改动不会撤销）</span>
        <span class="spacer" />
        <el-button text size="small" :icon="Close" @click="cancelEdit">取消</el-button>
      </component>
      <div
        ref="composerEl"
        class="composer"
        :class="{ disabled: running, over: dropOver }"
        @dragover.prevent="dropOver = true"
        @dragleave="dropOver = false"
        @drop.prevent="onDrop"
      >
        <GlassPane v-if="glass.enabled" class="composer-glass" :radius="12" :adaptive="false" aria-hidden="true" />
        <div v-if="attachments.length || adding" class="attach">
          <div v-for="(im, k) in attachments" :key="k" class="attach-item">
            <el-image :src="imageUrl(im)" :preview-src-list="attachments.map(imageUrl)" :initial-index="k" preview-teleported hide-on-click-modal fit="cover" class="attach-img" />
            <span class="attach-x" title="去掉这张" @click="removeImage(k)"><el-icon><Close /></el-icon></span>
          </div>
          <div v-if="adding" class="attach-item attach-loading"><el-icon class="is-loading"><Loading /></el-icon></div>
          <span v-if="attachments.length && !canImages" class="warn-line small">{{ curModel?.name }} 不能看图片，图片不会给它看</span>
        </div>
        <el-input
          v-model="input"
          type="textarea"
          class="composer-text"
          :autosize="{ minRows: 2, maxRows: 8 }"
          resize="none"
          :placeholder="waitingItem ? '上面有等你确认的操作；也可以直接说别的（那个操作就不执行了）' : '说说要查什么、做什么…'"
          :disabled="running || loadingConversation"
          aria-label="给 AI 助手的消息"
          @keydown="onKey"
          @paste="onPaste"
        />
        <input ref="fileInput" type="file" accept="image/png,image/jpeg,image/gif,image/webp" multiple hidden @change="onFilePicked" />
        <div class="composer-bar">
          <template v-if="models.length">
            <el-dropdown trigger="click" :disabled="running" placement="top-start" @command="pickModel">
              <span class="chip" :class="{ off: running }">
                <el-icon><Cpu /></el-icon>
                <span class="chip-text">{{ curModel?.name }}</span>
                <el-icon class="chip-arrow"><ArrowDown /></el-icon>
              </span>
              <template #dropdown>
                <el-dropdown-menu>
                  <el-dropdown-item v-for="m in models" :key="m.id" :command="m.id" :class="{ 'chip-cur': m.id === curModel?.id }">
                    {{ m.name }}
                  </el-dropdown-item>
                </el-dropdown-menu>
              </template>
            </el-dropdown>
            <el-dropdown trigger="click" :disabled="running" placement="top-start" @command="(v: string) => (effort = v)">
              <span class="chip" :class="{ off: running }">
                <el-icon><Opportunity /></el-icon>
                <span class="chip-text">思考：{{ EFFORT_LABELS[effort] ?? effort }}</span>
                <el-icon class="chip-arrow"><ArrowDown /></el-icon>
              </span>
              <template #dropdown>
                <el-dropdown-menu>
                  <div class="muted chip-tip">越高想得越久、越仔细，也越慢、越贵</div>
                  <el-dropdown-item v-for="o in efforts" :key="o.value" :command="o.value" :class="{ 'chip-cur': o.value === effort }">
                    {{ o.label }}
                  </el-dropdown-item>
                </el-dropdown-menu>
              </template>
            </el-dropdown>
          </template>
          <el-button v-else text size="small" type="primary" @click="openSettings">还没有模型，去设置里添加</el-button>
          <el-tooltip
            v-if="models.length"
            :show-after="HOVER_DELAY"
            :content="canImages ? '附图片（也可以直接粘贴截图，或把图片拖进来）' : `${curModel?.name ?? '当前模型'} 不能看图片（在「模型设置」里可以改）`"
            placement="top"
          >
            <span class="chip" :class="{ off: running || !canImages }" @click="!running && canImages && pickImages()">
              <el-icon><Picture /></el-icon>
            </span>
          </el-tooltip>
          <span class="spacer" />
          <span v-if="totalText" class="muted hint total">{{ totalText }}</span>
          <span v-else class="muted hint">Enter 发送 · Shift+Enter 换行</span>
          <el-tooltip v-if="running" :show-after="HOVER_DELAY" content="停止" placement="top">
            <el-button circle type="danger" plain :icon="VideoPause" :loading="stopping" aria-label="停止 AI 回答" @click="stop" />
          </el-tooltip>
          <el-button v-else circle type="primary" :icon="Promotion" aria-label="发送消息" :disabled="(!input.trim() && !attachments.length) || !!adding || !models.length || loadingConversation" @click="send()" />
        </div>
      </div>
    </div>
  </el-drawer>
</template>

<style>
.pending-banner { display: flex; align-items: center; gap: 8px; padding: 8px 16px; border-bottom: 1px solid var(--el-color-warning-light-8); background: var(--el-color-warning-light-9); color: var(--el-color-warning-dark-2); font-size: 12px; }
.pending-banner > span { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.conversation-position { display: flex; align-items: center; justify-content: center; gap: 12px; padding-bottom: 6px; }
.copy-answer { opacity: .65; padding: 2px 0; height: 24px; }
.bot-answer:hover .copy-answer, .copy-answer:focus-visible { opacity: 1; }
/* 下拉菜单里当前选中的一项（菜单挂在 body 下，不能用 scoped） */
.el-dropdown-menu__item.chip-cur {
  color: var(--el-color-primary);
  font-weight: 600;
}
.chip-tip {
  font-size: 12px;
  padding: 4px 16px 6px;
}
.ai-drawer .el-drawer__header {
  margin-bottom: 0;
  padding: 10px 16px;
  border-bottom: 1px solid var(--el-border-color-lighter);
}
.ai-drawer .el-drawer__body {
  padding: 0;
  display: flex;
  flex-direction: column;
}
/* 液态玻璃：标题栏、提示条、输入框浮在对话上，对话占满整个抽屉、从它们下面滚过（比下面 scoped 的规则多一个类，才能盖过它们） */
.ai-drawer.ai-glass .el-drawer__header { position: absolute; top: 0; left: 0; right: 0; z-index: 3; border-bottom: 0; }
.ai-drawer.ai-glass .el-drawer__body { position: relative; }
/* 标题栏和输入框上字小、要打字：玻璃比按钮厚（磨砂更重、底色更实），滚过去的对话只透出颜色和轮廓 */
.cg.ai-head-glass { position: absolute; inset: 0; z-index: -1; --cg-frost: 18px; --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.72 * var(--cg-k, 1))), rgb(255 255 255 / calc(.58 * var(--cg-k, 1)))); box-shadow: 0 1px 0 rgb(16 24 40 / .06); }
html.dark .cg.ai-head-glass { --cg-fill: linear-gradient(180deg, rgb(24 30 44 / calc(.78 * var(--cg-k, 1))), rgb(20 26 38 / calc(.66 * var(--cg-k, 1)))); box-shadow: 0 1px 0 rgb(0 0 0 / .4); }
.ai-drawer.ai-glass .pending-banner { position: absolute; top: var(--ai-head, 0px); left: 0; right: 0; z-index: 2; }
.ai-drawer.ai-glass .ai-body { padding-top: calc(var(--ai-head, 0px) + var(--ai-banner, 0px) + 14px); padding-bottom: calc(var(--ai-input, 0px) + 4px); scroll-padding-top: calc(var(--ai-head, 0px) + var(--ai-banner, 0px)); }
.ai-drawer.ai-glass .ai-input { position: absolute; left: 0; right: 0; bottom: 0; z-index: 2; pointer-events: none; }
.ai-drawer.ai-glass .ai-input > * { pointer-events: auto; }
/* 输入框下面和两侧的空隙：对话渐隐成抽屉底色，不让文字从输入框下面露出来 */
.ai-drawer.ai-glass .ai-input::before { content: ''; position: absolute; inset: 45% 0 0; z-index: -1; pointer-events: none; background: linear-gradient(to bottom, transparent, var(--el-drawer-bg-color, var(--el-bg-color)) 55%); }
.ai-drawer.ai-glass .composer { position: relative; isolation: isolate; background: transparent; border-color: transparent; }
.ai-drawer.ai-glass .composer:focus-within { border-color: color-mix(in srgb, var(--el-color-primary) 45%, transparent); }
.cg.composer-glass { position: absolute; inset: -1px; z-index: -1; --cg-frost: 16px; --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.74 * var(--cg-k, 1))), rgb(255 255 255 / calc(.62 * var(--cg-k, 1)))); }
html.dark .cg.composer-glass { --cg-fill: linear-gradient(180deg, rgb(28 34 50 / calc(.8 * var(--cg-k, 1))), rgb(22 28 42 / calc(.7 * var(--cg-k, 1)))); }
.ai-drawer.ai-glass .conversation-position { pointer-events: none; }
.position-pill { display: inline-flex; align-items: center; gap: 12px; }
.ai-drawer.ai-glass .position-pill { gap: 10px; padding: 2px 6px 2px 14px; pointer-events: auto; }
.ai-drawer.ai-glass .edit-bar { padding: 6px 8px 6px 12px; }
</style>

<style scoped>
.ai-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.ai-title {
  font-weight: 600;
  color: var(--el-text-color-primary);
}
.spacer {
  flex: 1;
}
.small {
  font-size: 12px;
}
.ai-body {
  flex: 1;
  overflow: auto;
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.ai-empty {
  margin: auto 0;
  text-align: center;
}
.examples {
  margin-top: 12px;
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  justify-content: center;
}
.examples .el-button {
  margin: 0;
}
.msg {
  line-height: 1.7;
  word-break: break-word;
}
.msg.user {
  align-self: flex-end;
  max-width: 85%;
  background: var(--el-color-primary-light-9);
  border-radius: 8px;
  padding: 8px 12px;
  white-space: pre-wrap;
}
.msg.err {
  color: var(--el-color-danger);
  white-space: pre-wrap;
}
.msg.bot {
  font-family: 'Segoe UI', 'Microsoft YaHei UI', 'PingFang SC', -apple-system, sans-serif;
  font-size: 14px;
  line-height: 1.8;
  text-rendering: optimizeLegibility;
}
.msg.bot > :deep(:first-child) {
  margin-top: 0;
}
.msg.bot > :deep(:last-child) {
  margin-bottom: 0;
}
.msg.bot :deep(p) {
  margin: 6px 0;
}
.msg.bot :deep(h1),
.msg.bot :deep(h2),
.msg.bot :deep(h3),
.msg.bot :deep(h4),
.msg.bot :deep(h5),
.msg.bot :deep(h6) {
  margin: 12px 0 6px;
  font-weight: 600;
  line-height: 1.4;
}
.msg.bot :deep(h1) {
  font-size: 18px;
}
.msg.bot :deep(h2) {
  font-size: 16px;
}
.msg.bot :deep(h3),
.msg.bot :deep(h4),
.msg.bot :deep(h5),
.msg.bot :deep(h6) {
  font-size: 14px;
}
.msg.bot :deep(ul),
.msg.bot :deep(ol) {
  margin: 6px 0;
  padding-left: 22px;
}
.msg.bot :deep(li) {
  margin: 2px 0;
}
.msg.bot :deep(li > p) {
  margin: 2px 0;
}
.msg.bot :deep(blockquote) {
  margin: 6px 0;
  padding: 2px 12px;
  border-left: 3px solid var(--el-border-color);
  color: var(--el-text-color-secondary);
}
.msg.bot :deep(hr) {
  border: none;
  border-top: 1px solid var(--el-border-color-lighter);
  margin: 10px 0;
}
.msg.bot :deep(a) {
  color: var(--el-color-primary);
  text-decoration: none;
}
.msg.bot :deep(a:hover) {
  text-decoration: underline;
}
.msg.bot :deep(code) {
  /* 显式选择屏幕等宽字体，避免通用 monospace 在 Windows 上回退到细碎的字形。 */
  font-family: 'Cascadia Mono', Consolas, 'SFMono-Regular', Menlo, Monaco, 'Liberation Mono', 'Microsoft YaHei UI', monospace;
  font-size: 13px;
  font-weight: 400;
  font-variant-ligatures: none;
  letter-spacing: 0;
  color: var(--el-text-color-primary);
  background: var(--el-fill-color-light);
  padding: 2px 5px;
  border-radius: 4px;
  -webkit-box-decoration-break: clone;
  box-decoration-break: clone;
}
.msg.bot :deep(pre) {
  background: var(--el-fill-color-light);
  padding: 8px 10px;
  border-radius: 6px;
  overflow: auto;
  margin: 6px 0;
  line-height: 1.65;
}
.msg.bot :deep(pre code) {
  background: none;
  padding: 0;
  font-size: 13px;
  white-space: pre;
}
.msg.bot :deep(.table-wrap) {
  overflow-x: auto;
  margin: 6px 0;
}
.msg.bot :deep(table) {
  border-collapse: collapse;
  font-size: inherit;
  line-height: 1.7;
}
.msg.bot :deep(th),
.msg.bot :deep(td) {
  border: 1px solid var(--el-border-color-lighter);
  padding: 4px 10px;
  text-align: left;
  vertical-align: top;
}
.msg.bot :deep(th) {
  background: var(--el-fill-color-light);
  font-weight: 600;
  white-space: nowrap;
}
.msg.bot :deep(tr:nth-child(even) td) {
  background: var(--el-fill-color-lighter);
}
.user-row {
  align-self: flex-end;
  max-width: 85%;
  display: flex;
  align-items: flex-start;
  gap: 2px;
}
.user-row .msg.user {
  max-width: none;
}
.edit-btn {
  visibility: hidden;
  margin-top: 4px;
}
.user-row:hover .edit-btn {
  visibility: visible;
}
.msg.user.editing {
  outline: 2px solid var(--el-color-primary-light-5);
}
.msg.bot.streaming::after {
  content: '▍';
  color: var(--el-color-primary);
  animation: blink 1s steps(2) infinite;
}
@keyframes blink {
  50% {
    opacity: 0;
  }
}
.think {
  font-size: 13px;
  color: var(--el-text-color-secondary);
}
.think-head {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: pointer;
}
.think-body {
  margin: 4px 0 0 6px;
  padding-left: 10px;
  border-left: 2px solid var(--el-border-color);
  white-space: pre-wrap;
  max-height: 240px;
  overflow: auto;
  line-height: 1.6;
}
.think-body.live {
  max-height: 96px;
  overflow: hidden;
  display: flex;
  flex-direction: column;
  justify-content: flex-end;
  -webkit-mask-image: linear-gradient(to bottom, transparent 0, #000 36px);
  mask-image: linear-gradient(to bottom, transparent 0, #000 36px);
}
.think-body.live > div {
  flex-shrink: 0;
}
.usage {
  font-size: 12px;
  align-self: flex-start;
  margin-top: -4px;
}
.edit-bar {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--el-color-primary);
  margin-bottom: 6px;
}
.hint.total {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.tool {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 6px 10px;
  font-size: 13px;
  background: var(--el-fill-color-blank);
}
.tool.bad {
  border-color: var(--el-color-danger-light-5);
}
.tool-head {
  display: flex;
  align-items: center;
  gap: 6px;
  cursor: pointer;
  color: var(--el-text-color-regular);
}
.tool .cmd {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.purpose {
  margin: 2px 0 0 20px;
}
.tool-out {
  margin: 6px 0 2px;
  max-height: 320px;
  overflow: auto;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
  background: var(--el-fill-color-light);
  padding: 6px 8px;
  border-radius: 4px;
}
.tool.change {
  border-color: var(--el-color-primary-light-7);
}
.tool.ask {
  border-color: var(--el-color-warning);
  background: var(--el-color-warning-light-9);
}
.tool.danger.ask {
  border-color: var(--el-color-danger);
  background: var(--el-color-danger-light-9);
}
.tool-head .cmd {
  min-width: 0;
}
.warn {
  color: var(--el-color-warning);
}
.tool.danger .warn {
  color: var(--el-color-danger);
}
.confirm {
  margin: 8px 0 2px 20px;
}
.confirm-title {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}
.once {
  margin-top: 4px;
}
.confirm .detail {
  max-height: 360px;
}
.detail .add {
  color: var(--el-color-success);
}
.detail .del {
  color: var(--el-color-danger);
}
.confirm-btns {
  margin-top: 8px;
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.confirm-btns .el-button {
  margin: 0;
}
.scope {
  margin-top: 4px;
}
.decision {
  margin: 2px 0 0 20px;
}
.decision.no {
  color: var(--el-color-danger);
}
.hist-search {
  display: flex;
  margin-bottom: 8px;
}
.hist-empty {
  padding: 8px 4px;
}
.hist {
  max-height: 360px;
  overflow: auto;
  margin: 0;
  padding: 4px;
}
.hist-row {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 8px;
  border-radius: 4px;
  cursor: pointer;
}
.hist-row:hover {
  background: var(--el-fill-color-light);
}
.hist-row.cur {
  background: var(--el-color-primary-light-9);
}
.hist-main {
  flex: 1;
  min-width: 0;
}
.hist-title {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--el-text-color-primary);
}
.hist-pending {
  color: var(--el-color-warning);
}
.thinking {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 13px;
}
.ai-input {
  padding: 8px 16px 14px;
}
.composer {
  border: 1px solid var(--el-border-color);
  border-radius: 12px;
  background: var(--el-fill-color-blank);
  padding: 8px 10px 8px 12px;
  transition:
    border-color 0.2s,
    box-shadow 0.2s;
}
.composer:focus-within {
  border-color: var(--el-color-primary);
  box-shadow: 0 0 0 3px var(--el-color-primary-light-9);
}
.composer.disabled {
  background: var(--el-fill-color-lighter);
}
/* 输入框本身不要边框和背景，融进外面的框 */
.composer-text :deep(.el-textarea__inner) {
  box-shadow: none !important;
  background: transparent !important;
  padding: 2px 0;
  line-height: 1.6;
}
.composer-bar {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-top: 6px;
}
/* 模型、思考强度：小圆角的「标签」，宽度随文字 */
.chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 26px;
  padding: 0 8px 0 10px;
  border-radius: 13px;
  background: var(--el-fill-color-light);
  color: var(--el-text-color-regular);
  font-size: 12px;
  cursor: pointer;
  outline: none;
  transition: background 0.2s;
}
.chip:hover {
  background: var(--el-fill-color);
}
.chip.off {
  opacity: 0.6;
  cursor: not-allowed;
}
.chip .el-icon {
  color: var(--el-text-color-secondary);
}
.chip-text {
  max-width: 150px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.chip-arrow {
  font-size: 11px;
}
.hint {
  font-size: 12px;
  white-space: nowrap;
}
.composer-bar .el-button {
  margin: 0;
}
@media (max-width: 900px) {
  .hint {
    display: none;
  }
}
/* 附的图片：输入框上面一排缩略图；消息里的显示在文字上面 */
.composer.over {
  border-color: var(--el-color-primary);
  border-style: dashed;
}
.attach {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.attach-item {
  position: relative;
  width: 56px;
  height: 56px;
}
.attach-img {
  width: 56px;
  height: 56px;
  border-radius: 6px;
  border: 1px solid var(--el-border-color-lighter);
  display: block;
}
.attach-x {
  position: absolute;
  top: -6px;
  right: -6px;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  background: var(--el-text-color-secondary);
  color: var(--el-bg-color);
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 11px;
  cursor: pointer;
}
.attach-x:hover {
  background: var(--el-color-danger);
}
.attach-loading {
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 6px;
  background: var(--el-fill-color-light);
}
.warn-line {
  color: var(--el-color-warning);
}
.msg-imgs {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 4px;
}
.msg-img {
  width: 96px;
  height: 72px;
  border-radius: 6px;
  display: block;
  cursor: zoom-in;
}
</style>
