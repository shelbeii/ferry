// 桌面界面的空白拖动不应由浏览器推算成跨区域文字选区。
// 只拦截原生 selectstart，不取消 pointerdown，避免影响按钮、焦点、滚动条和拖放。
const managedSelection = 'input, textarea, select, [contenteditable]:not([contenteditable="false"]), .cm-editor, .xterm'
const controls = 'button, [role="button"], [role="tab"], [role="switch"], [role="checkbox"], [role="radio"]'
const graphemes = typeof Intl.Segmenter === 'function' ? new Intl.Segmenter(undefined, { granularity: 'grapheme' }) : undefined
const installed = new WeakMap<Document, () => void>()

function hitsText(doc: Document, target: Element, x: number, y: number): boolean {
  // 老版本 WebView 没有命中测试接口时沿用原生行为，不能因此禁用正常文字选择。
  if (!doc.caretPositionFromPoint && !doc.caretRangeFromPoint) return true
  const caret = doc.caretPositionFromPoint?.(x, y)
  const fallback = caret ? undefined : doc.caretRangeFromPoint?.(x, y)
  const node = caret?.offsetNode ?? fallback?.startContainer
  const offset = caret?.offset ?? fallback?.startOffset ?? 0
  if (!node || node.nodeType !== 3 || !target.contains(node)) return false
  const text = node.textContent ?? ''
  const range = doc.createRange()
  const hit = (start: number, end: number) => {
    if (start < 0 || end > text.length || start >= end || !/\S/u.test(text.slice(start, end))) return false
    range.setStart(node, start)
    range.setEnd(node, end)
    return Array.from(range.getClientRects()).some((rect) => rect.width > 0 && x >= rect.left && x <= rect.right && y >= rect.top && y <= rect.bottom)
  }
  if (!graphemes) return hit(offset - 1, offset) || hit(offset, offset + 1)
  // 只检查光标附近的一小段，长日志不需要整段分词；按字素处理中文、emoji 和组合字符。
  const from = Math.max(0, offset - 32)
  for (const part of graphemes.segment(text.slice(from, offset + 32))) {
    const start = from + part.index, end = start + part.segment.length
    if (start > offset) break
    if (end >= offset && hit(start, end)) return true
  }
  return false
}

export function installTextSelectionGuard(doc: Document = document): () => void {
  const existing = installed.get(doc)
  if (existing) return existing
  let active = false
  let allowed = true
  const reset = () => { active = false; allowed = true }
  const down = (event: PointerEvent) => {
    reset()
    if (event.button !== 0 || event.pointerType !== 'mouse') return
    const target = event.target as Element | null
    if (!target?.closest) return
    active = true
    const managed = target.closest(managedSelection)
    const control = target.closest(controls)
    allowed = !!managed || (!control && hitsText(doc, target, event.clientX, event.clientY))
    // 空白点击清掉旧的原生选区；工具栏按钮可能需要读取已有选区，交给原生焦点逻辑处理。
    if (!allowed && !control) doc.getSelection()?.removeAllRanges()
  }
  const select = (event: Event) => {
    if (active && !allowed) event.preventDefault()
  }
  const capture = { capture: true }
  doc.addEventListener('pointerdown', down, capture)
  doc.addEventListener('pointerup', reset, capture)
  doc.addEventListener('pointercancel', reset, capture)
  doc.addEventListener('keydown', reset, capture)
  doc.addEventListener('selectstart', select, capture)
  doc.defaultView?.addEventListener('blur', reset)
  const cleanup = () => {
    reset()
    doc.removeEventListener('pointerdown', down, capture)
    doc.removeEventListener('pointerup', reset, capture)
    doc.removeEventListener('pointercancel', reset, capture)
    doc.removeEventListener('keydown', reset, capture)
    doc.removeEventListener('selectstart', select, capture)
    doc.defaultView?.removeEventListener('blur', reset)
    installed.delete(doc)
  }
  installed.set(doc, cleanup)
  return cleanup
}
