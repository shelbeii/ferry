import {
  App,
  applyDocumentTheme,
  applyHostStyleVariables,
  type McpUiHostContext,
} from '@modelcontextprotocol/ext-apps'

export interface ConfirmCardData {
  kind: 'ferry-confirm-card'
  approvalId: string
  appToken: string
  opId: string
  title: string
  action: string
  actionLabel: string
  project?: string
  target?: string
  host?: string
  targetSummary?: string
  dbName?: string
  dbKind?: string
  database?: string
  dbEstimate?: string
  dbEstimateMax?: number
  config?: string
  danger?: string
  detail?: string[]
  content?: string
  reason?: string
  client?: string
  desktopOnline: boolean
  expiresAt: string
}

export interface OpSnapshot {
  state: 'waiting' | 'running' | 'done' | 'rejected' | 'error'
  id?: string
  title?: string
  elapsed?: string
  modelListening?: boolean
  progress?: string[]
  approval?: string
  result?: unknown
  log?: string[]
  error?: string
  message?: string
}

export function formatOutcomeMessage(card: ConfirmCardData, snap: OpSnapshot): string {
  if (snap.state === 'rejected') {
    const why = snap.message && !snap.message.startsWith('已拒绝，未执行') ? `（原因：${snap.message.replace(/^已拒绝：/, '')}）` : ''
    return `我已在 Ferry 确认卡片中拒绝执行「${card.title}」（ID: ${card.opId}）${why}。不要换方式重试，请根据需要询问我接下来的处理方式。`
  }
  if (snap.state === 'done') {
    const parts = [`我已在 Ferry 确认卡片中同意执行「${card.title}」（ID: ${card.opId}），操作已完成。`]
    if (snap.result !== undefined) {
      const txt = typeof snap.result === 'string' ? snap.result : JSON.stringify(snap.result)
      if (txt) parts.push(`结果：${txt.length > 1200 ? txt.slice(0, 1200) + '…' : txt}`)
    }
    parts.push('请基于这个结果继续汇报或执行后续步骤。')
    return parts.join('\n')
  }
  const detail = snap.error || snap.message || '执行失败'
  const parts = [`我已在 Ferry 确认卡片中同意执行「${card.title}」（ID: ${card.opId}），但执行失败：${detail}`]
  if (snap.log && snap.log.length > 0) {
    parts.push('过程日志：\n' + snap.log.slice(-15).join('\n'))
  }
  parts.push('请帮我分析原因并说明建议的处理方式。')
  return parts.join('\n')
}

const DANGER_ACTIONS = new Set([
  'stop',
  'rollback',
  'sw-stop',
  'port-close',
  'port-open',
  'fw-enable',
  'ssh-kick',
  'db-exec',
])

const PRIMARY_ACTIONS = new Set(['create', 'backup-run', 'backup-fetch'])

export function actionTone(action: string, danger?: string): 'danger' | 'primary' | 'warning' {
  if (danger || DANGER_ACTIONS.has(action)) return 'danger'
  if (PRIMARY_ACTIONS.has(action)) return 'primary'
  return 'warning'
}

export function formatCountdown(expiresAt: string, nowMs = Date.now()): { text: string; expired: boolean } {
  const ms = new Date(expiresAt).getTime() - nowMs
  if (!Number.isFinite(ms) || ms <= 0) return { text: '已过期，请重新发起', expired: true }
  const s = Math.ceil(ms / 1000)
  return {
    text: `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')} 后过期`,
    expired: false,
  }
}

export function classifyCodeLine(line: string): 'add' | 'del' | 'hunk' | '' {
  if (line.startsWith('+ ') || (line.startsWith('+') && !line.startsWith('+++'))) return 'add'
  if (line.startsWith('- ') || (line.startsWith('-') && !line.startsWith('---'))) return 'del'
  if (line.startsWith('@@')) return 'hunk'
  return ''
}

export function extractStructuredData(result: unknown): Record<string, unknown> | null {
  if (!result || typeof result !== 'object') return null
  const r = result as { structuredContent?: unknown; content?: { type?: string; text?: string }[] }
  if (r.structuredContent && typeof r.structuredContent === 'object') {
    return r.structuredContent as Record<string, unknown>
  }
  const text = r.content?.find((c) => c.type === 'text' && typeof c.text === 'string')?.text
  if (!text) return null
  try {
    const parsed = JSON.parse(text)
    if (parsed && typeof parsed === 'object') return parsed as Record<string, unknown>
  } catch {
    return { kind: 'text', text }
  }
  return null
}

function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs?: Record<string, string>,
  children?: (Node | string)[],
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag)
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'class') node.className = v
      else node.setAttribute(k, v)
    }
  }
  if (children) {
    for (const c of children) {
      node.append(typeof c === 'string' ? document.createTextNode(c) : c)
    }
  }
  return node
}

function renderCodeBlock(
  title: string,
  rawText: string,
  expanded: boolean,
  onToggle?: () => void,
): HTMLElement {
  const allLines = rawText.split('\n')
  const collapsible = allLines.length > 14 && !!onToggle
  const visibleLines = collapsible && !expanded ? allLines.slice(0, 14) : allLines
  const pre = el('pre', { class: 'code-pre' })
  for (const line of visibleLines) {
    const cls = classifyCodeLine(line)
    pre.append(el('span', { class: cls ? `line ${cls}` : 'line' }, [line + '\n']))
  }
  if (collapsible && !expanded) {
    pre.append(el('span', { class: 'line muted' }, [`… 已折叠其余 ${allLines.length - 14} 行`]))
  }
  const headChildren: HTMLElement[] = [el('div', { class: 'code-title' }, [title])]
  if (collapsible) {
    const btn = el('button', { type: 'button', class: 'link-btn' }, [
      expanded ? '收起' : `展开全部（共 ${allLines.length} 行）`,
    ])
    btn.addEventListener('click', onToggle!)
    headChildren.push(btn)
  }
  return el('div', { class: 'code-wrap' }, [el('div', { class: 'code-head' }, headChildren), pre])
}

function applyContext(ctx?: McpUiHostContext) {
  if (!ctx) return
  if (ctx.theme) applyDocumentTheme(ctx.theme)
  if (ctx.styles?.variables) applyHostStyleVariables(ctx.styles.variables)
}

function mount() {
  const root = document.getElementById('app')
  if (!root) return

  const app = new App({ name: 'ferry-confirm', version: '1.0.0' }, {})

  let card: ConfirmCardData | null = null
  let snapshot: OpSnapshot | null = null
  let busy = false
  let cancelling = false
  let notified = false
  let showRejectReason = false
  let rejectReason = ''
  const expandedBlocks = new Set<string>()
  let countdownTimer: number | undefined
  let pollTimer: number | undefined
  let notifyTimer: number | undefined

  function clearTimers() {
    if (countdownTimer !== undefined) {
      window.clearInterval(countdownTimer)
      countdownTimer = undefined
    }
    if (pollTimer !== undefined) {
      window.clearInterval(pollTimer)
      pollTimer = undefined
    }
    if (notifyTimer !== undefined) {
      window.clearTimeout(notifyTimer)
      notifyTimer = undefined
    }
  }

  function renderLoading(msg: string) {
    root!.replaceChildren(
      el('div', { class: 'card' }, [
        el('div', { class: 'head' }, [
          el('span', { class: 'badge primary' }, ['Ferry']),
          el('span', { class: 'title' }, [msg]),
        ]),
      ]),
    )
  }

  function renderGenericResult(data: Record<string, unknown>, isError?: boolean) {
    clearTimers()
    const body =
      data.kind === 'text' && typeof data.text === 'string'
        ? data.text
        : JSON.stringify(data, null, 2)
    const state = typeof data.state === 'string' ? data.state : isError ? '执行失败' : '完成'
    const title = typeof data.title === 'string' ? data.title : 'Ferry 操作结果'
    const tone = isError ? 'danger' : state.includes('完成') ? 'success' : 'primary'
    root!.replaceChildren(
      el('div', { class: 'card' }, [
        el('div', { class: 'head' }, [
          el('span', { class: `badge ${tone}` }, [state]),
          el('span', { class: 'title' }, [title]),
        ]),
        el('pre', { class: 'code-pre compact' }, [body]),
      ]),
    )
  }

  async function sendOutcomeToModel(currentCard: ConfirmCardData, snap: OpSnapshot) {
    const text = formatOutcomeMessage(currentCard, snap)
    try {
      const caps = app.getHostCapabilities()
      // 优先使用 sendMessage 自动唤醒模型继续处理结果；宿主不支持时退回到 updateModelContext
      if (caps?.message) {
        const res = await app.sendMessage({ role: 'user', content: [{ type: 'text', text }] })
        if (!res?.isError) return
      }
      if (caps?.updateModelContext) {
        await app.updateModelContext({ content: [{ type: 'text', text }] })
      }
    } catch {
      // 宿主不支持或当前视图无对话上下文时忽略
    }
  }

  function scheduleNotifyModel(currentCard: ConfirmCardData, snap: OpSnapshot) {
    if (notified) return
    notified = true
    // 模型正在通过 task_status 实时等待或已拿到终态结果时，不再重复发消息打断当前轮次
    if (snap.modelListening) return
    // 短暂延迟后再确认一次 modelListening，避免模型刚好在两轮 task_status 之间间隙触发重复唤醒
    notifyTimer = window.setTimeout(() => {
      notifyTimer = undefined
      void (async () => {
        try {
          const check = await app.callServerTool({
            name: 'mcp_app_status',
            arguments: { approvalId: currentCard.approvalId, appToken: currentCard.appToken },
          })
          const latest = extractStructuredData(check) as OpSnapshot | null
          if (latest?.modelListening) return
        } catch {
          // 查询失败时继续尝试唤醒模型
        }
        await sendOutcomeToModel(currentCard, snap)
      })()
    }, 1500)
  }

  async function pollStatus() {
    if (!card) return
    const currentCard = card
    try {
      const res = await app.callServerTool({
        name: 'mcp_app_status',
        arguments: { approvalId: currentCard.approvalId, appToken: currentCard.appToken },
      })
      const data = extractStructuredData(res) as OpSnapshot | null
      if (!data) return
      snapshot = data
      renderCard()
      if (data.state === 'done' || data.state === 'error' || data.state === 'rejected') {
        if (pollTimer !== undefined) {
          window.clearInterval(pollTimer)
          pollTimer = undefined
        }
        scheduleNotifyModel(currentCard, data)
      }
    } catch {
      // 单次轮询失败时等待下一次
    }
  }

  async function decide(decision: 'approve' | 'reject') {
    if (!card || busy) return
    const currentCard = card
    busy = true
    renderCard()
    try {
      const args: Record<string, unknown> = {
        approvalId: currentCard.approvalId,
        appToken: currentCard.appToken,
        decision,
      }
      if (decision === 'reject' && rejectReason.trim()) {
        args.reason = rejectReason.trim()
      }
      const res = await app.callServerTool({
        name: 'mcp_app_decide',
        arguments: args,
      })
      const data = extractStructuredData(res) as OpSnapshot | null
      snapshot = data ?? {
        state: decision === 'reject' ? 'rejected' : 'error',
        error: '未收到服务端响应',
      }
      if (countdownTimer !== undefined) {
        window.clearInterval(countdownTimer)
        countdownTimer = undefined
      }
      if (snapshot.state === 'running') {
        pollTimer = window.setInterval(() => void pollStatus(), 1500)
      } else {
        scheduleNotifyModel(currentCard, snapshot)
      }
    } catch (e) {
      snapshot = { state: 'error', error: e instanceof Error ? e.message : String(e) }
    } finally {
      busy = false
      renderCard()
    }
  }

  async function cancelRunning() {
    if (!card || cancelling) return
    const currentCard = card
    cancelling = true
    renderCard()
    try {
      const res = await app.callServerTool({
        name: 'mcp_app_cancel',
        arguments: { approvalId: currentCard.approvalId, appToken: currentCard.appToken },
      })
      const data = extractStructuredData(res) as OpSnapshot | null
      if (data) snapshot = data
    } catch (e) {
      if (snapshot) {
        snapshot = { ...snapshot, message: e instanceof Error ? e.message : String(e) }
      }
    } finally {
      cancelling = false
      renderCard()
    }
  }

  function renderCard() {
    if (!card) return
    const tone = actionTone(card.action, card.danger)
    const countdown = formatCountdown(card.expiresAt)

    const headChildren: HTMLElement[] = [
      el('span', { class: `badge ${tone}` }, [card.title]),
    ]
    if (card.danger) {
      headChildren.push(el('span', { class: 'badge danger solid' }, [`危险：${card.danger}`]))
    }
    headChildren.push(
      el('span', { class: 'pill muted' }, [
        card.desktopOnline ? 'Ferry 桌面端后台执行' : '桌面端未打开 · 直接执行',
      ]),
    )

    const rows: HTMLElement[] = []
    const addRow = (label: string, value: string, cls = '') => {
      if (!value) return
      rows.push(
        el('div', { class: 'meta-row' }, [
          el('span', { class: 'meta-label' }, [label]),
          el('span', { class: `meta-val ${cls}`.trim() }, [value]),
        ]),
      )
    }
    if (card.project) addRow('项目', card.project)
    if (card.targetSummary) addRow('目标', card.targetSummary, 'mono')
    if (card.dbEstimate) {
      const big = (card.dbEstimateMax ?? 0) >= 1000
      addRow('影响行数', `${card.dbEstimate}（EXPLAIN 估算）`, big ? 'danger' : 'warn')
    }
    addRow('理由', card.reason || '（未说明）', card.reason ? 'reason' : 'muted')

    const bodyNodes: HTMLElement[] = [
      el('div', { class: 'head' }, headChildren),
      el('div', { class: 'meta' }, rows),
    ]

    if (card.detail && card.detail.length > 0) {
      const hasMultiline = card.detail.some((d) => d.includes('\n'))
      if (hasMultiline) {
        const header = card.detail[0]
        const rest = card.detail.slice(1).join('\n')
        const key = 'detail'
        bodyNodes.push(
          renderCodeBlock(header, rest || header, expandedBlocks.has(key), () => {
            if (expandedBlocks.has(key)) expandedBlocks.delete(key)
            else expandedBlocks.add(key)
            renderCard()
          }),
        )
      } else {
        const ul = el('ul', { class: 'detail-list' })
        for (const d of card.detail) ul.append(el('li', {}, [d]))
        bodyNodes.push(ul)
      }
    }

    if (card.content) {
      const key = 'content'
      bodyNodes.push(
        renderCodeBlock('将写入的 ferry.yaml（已有文件不会覆盖）：', card.content, expandedBlocks.has(key), () => {
          if (expandedBlocks.has(key)) expandedBlocks.delete(key)
          else expandedBlocks.add(key)
          renderCard()
        }),
      )
    }

    if (snapshot) {
      const statusCls =
        snapshot.state === 'done'
          ? 'status ok'
          : snapshot.state === 'running' || snapshot.state === 'waiting'
            ? 'status running'
            : 'status fail'
      const statusTitle =
        snapshot.state === 'done'
          ? '执行完成'
          : snapshot.state === 'running'
            ? `正在执行…${snapshot.elapsed ? `（${snapshot.elapsed}）` : ''}`
            : snapshot.state === 'rejected'
              ? '已拒绝'
              : '执行失败'
      const statusHeadRow = el('div', { class: 'status-head-row' }, [
        el('div', { class: 'status-head' }, [statusTitle]),
      ])
      if (snapshot.state === 'running') {
        const stopBtn = el('button', { type: 'button', class: 'btn danger sm' }, [cancelling ? '正在停止…' : '停止执行'])
        stopBtn.disabled = cancelling
        stopBtn.addEventListener('click', () => void cancelRunning())
        statusHeadRow.append(stopBtn)
      }
      const statusBox = el('div', { class: statusCls }, [statusHeadRow])
      if (snapshot.message) statusBox.append(el('div', { class: 'status-msg' }, [snapshot.message]))
      if (snapshot.error) statusBox.append(el('div', { class: 'status-msg' }, [snapshot.error]))
      if (snapshot.progress && snapshot.progress.length > 0) {
        statusBox.append(el('pre', { class: 'code-pre compact' }, [snapshot.progress.slice(-12).join('\n')]))
      } else if (snapshot.log && snapshot.log.length > 0) {
        statusBox.append(el('pre', { class: 'code-pre compact' }, [snapshot.log.slice(-20).join('\n')]))
      }
      if (snapshot.result !== undefined) {
        const txt = typeof snapshot.result === 'string' ? snapshot.result : JSON.stringify(snapshot.result, null, 2)
        statusBox.append(el('pre', { class: 'code-pre compact' }, [txt]))
      }
      bodyNodes.push(statusBox)
    } else {
      if (showRejectReason) {
        const input = el('input', {
          type: 'text',
          class: 'reason-input',
          placeholder: '可选：说明拒绝原因（将发给 AI），按 Enter 直接拒绝',
        })
        input.value = rejectReason
        input.disabled = busy || countdown.expired
        input.addEventListener('input', () => {
          rejectReason = input.value
        })
        input.addEventListener('keydown', (e) => {
          if (e.key === 'Enter' && !busy && !countdown.expired) {
            void decide('reject')
          }
        })
        bodyNodes.push(el('div', { class: 'reason-row' }, [input]))
      }

      const reasonToggle = el('button', { type: 'button', class: 'link-btn' }, [
        showRejectReason ? '收起拒因' : '带理由拒绝',
      ])
      reasonToggle.disabled = busy || countdown.expired
      reasonToggle.addEventListener('click', () => {
        showRejectReason = !showRejectReason
        renderCard()
      })
      const rejectBtn = el('button', { type: 'button', class: 'btn secondary' }, ['拒绝'])
      const approveBtn = el(
        'button',
        { type: 'button', class: `btn ${tone === 'danger' ? 'danger' : 'primary'}` },
        [busy ? '正在提交…' : '同意执行'],
      )
      rejectBtn.disabled = busy || countdown.expired
      approveBtn.disabled = busy || countdown.expired
      rejectBtn.addEventListener('click', () => void decide('reject'))
      approveBtn.addEventListener('click', () => void decide('approve'))

      bodyNodes.push(
        el('div', { class: 'foot' }, [
          el('span', { class: `countdown ${countdown.expired ? 'fail-text' : 'muted'}` }, [countdown.text]),
          el('div', { class: 'actions' }, [reasonToggle, rejectBtn, approveBtn]),
        ]),
      )
    }

    root!.replaceChildren(el('div', { class: 'card' }, bodyNodes))
  }

  app.onhostcontextchanged = (ctx) => applyContext(ctx)

  app.ontoolinput = () => {
    renderLoading('正在预检操作并生成确认预览…')
  }

  app.ontoolcancelled = () => {
    clearTimers()
    if (!snapshot) {
      snapshot = { state: 'rejected', message: '操作已取消' }
      renderCard()
    }
  }

  app.ontoolresult = (result) => {
    clearTimers()
    const data = extractStructuredData(result)
    if (data && data.kind === 'ferry-confirm-card') {
      card = data as unknown as ConfirmCardData
      snapshot = null
      busy = false
      cancelling = false
      notified = false
      showRejectReason = false
      rejectReason = ''
      expandedBlocks.clear()
      renderCard()
      countdownTimer = window.setInterval(() => {
        if (!snapshot && !showRejectReason) renderCard()
      }, 1000)
      return
    }
    if (data) {
      renderGenericResult(data, result.isError)
    } else {
      renderLoading(result.isError ? '操作未执行' : '操作已完成')
    }
  }

  void app.connect().then(() => {
    applyContext(app.getHostContext())
  })
}

if (typeof document !== 'undefined') {
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', mount)
  } else {
    mount()
  }
}
