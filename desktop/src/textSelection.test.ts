import { describe, expect, it, vi } from 'vitest'
import { installTextSelectionGuard } from './textSelection'

class SelectionDocument extends EventTarget {
  defaultView = new EventTarget()
  text = { nodeType: 3, textContent: 'hello world' }
  clear = vi.fn()
  offset: number | undefined
  ranges: string[] = []
  caretPositionFromPoint: ((x: number) => { offsetNode: { nodeType: number; textContent: string }; offset: number }) | undefined = (x) => ({ offsetNode: this.text, offset: this.offset ?? Math.min(this.text.textContent.length, Math.max(0, Math.round(x / 10))) })
  caretRangeFromPoint: ((x: number) => { startContainer: { nodeType: number; textContent: string }; startOffset: number }) | undefined = (x) => ({ startContainer: this.text, startOffset: this.offset ?? Math.min(this.text.textContent.length, Math.max(0, Math.round(x / 10))) })
  getSelection = () => ({ removeAllRanges: this.clear })
  bounds = (start: number, end: number) => ({ left: start * 10, right: end * 10, top: 10, bottom: 30, width: (end - start) * 10 })
  createRange() {
    let start = 0, end = 0
    return {
      setStart: (_node: unknown, offset: number) => { start = offset },
      setEnd: (_node: unknown, offset: number) => { end = offset },
      getClientRects: () => { this.ranges.push(this.text.textContent.slice(start, end)); return [this.bounds(start, end)] },
    }
  }
  down(x: number, y = 20, kind = '', button = 0, pointerType = 'mouse') {
    const target = {
      closest: (selector: string) => kind && selector.includes(kind) ? target : null,
      contains: (node: unknown) => node === this.text,
    }
    const event = new Event('pointerdown', { cancelable: true })
    Object.defineProperties(event, { target: { value: target }, clientX: { value: x }, clientY: { value: y }, button: { value: button }, pointerType: { value: pointerType } })
    this.dispatchEvent(event)
    return event
  }
  select() {
    const event = new Event('selectstart', { cancelable: true })
    this.dispatchEvent(event)
    return event
  }
}

const setup = () => {
  const doc = new SelectionDocument()
  const cleanup = installTextSelectionGuard(doc as unknown as Document)
  return { doc, cleanup }
}

describe('文字选中起点', () => {
  it('空白处即使被浏览器映射到最近的文字，也不开始选区；鼠标默认行为仍保留', () => {
    const { doc } = setup()
    expect(doc.down(300).defaultPrevented).toBe(false)
    expect(doc.select().defaultPrevented).toBe(true)
    expect(doc.clear).toHaveBeenCalledOnce()
  })

  it('直接从文字开始可以正常选中，不清空已有选区', () => {
    const { doc } = setup()
    doc.down(23)
    expect(doc.select().defaultPrevented).toBe(false)
    expect(doc.clear).not.toHaveBeenCalled()
  })

  it('文字下方的留白、文字之间的空格不会误选', () => {
    const { doc } = setup()
    doc.down(23, 80)
    expect(doc.select().defaultPrevented).toBe(true)
    doc.down(55)
    expect(doc.select().defaultPrevented).toBe(true)
  })

  it('操作按钮上的拖动不选择按钮文字', () => {
    const { doc } = setup()
    doc.down(23, 20, 'button')
    expect(doc.select().defaultPrevented).toBe(true)
    expect(doc.clear).not.toHaveBeenCalled()
  })

  it.each(['input', 'textarea', '[contenteditable]', '.cm-editor', '.xterm'])('%s 保留自身选择逻辑，允许在空白处定位', (kind) => {
    const { doc } = setup()
    doc.down(300, 80, kind)
    expect(doc.select().defaultPrevented).toBe(false)
    expect(doc.clear).not.toHaveBeenCalled()
  })

  it('键盘选择，以及鼠标抬起、取消或窗口失焦后的选择不受限制', () => {
    const { doc } = setup()
    expect(doc.select().defaultPrevented).toBe(false)
    for (const event of ['pointerup', 'pointercancel', 'keydown', 'blur']) {
      doc.down(300)
      if (event === 'blur') doc.defaultView.dispatchEvent(new Event(event))
      else doc.dispatchEvent(new Event(event))
      expect(doc.select().defaultPrevented).toBe(false)
    }
  })

  it('右键和触摸不触发鼠标文字选择限制', () => {
    const { doc } = setup()
    doc.down(300, 80, '', 2)
    expect(doc.select().defaultPrevented).toBe(false)
    doc.down(300, 80, '', 0, 'touch')
    expect(doc.select().defaultPrevented).toBe(false)
  })

  it('emoji 和组合字符作为完整字素命中', () => {
    const { doc } = setup()
    doc.text.textContent = 'e\u0301😀字'
    doc.bounds = (start, end) => {
      const left = start === 0 ? 0 : start === 2 ? 10 : 30
      const right = end === 2 ? 10 : end === 4 ? 30 : 40
      return { left, right, top: 10, bottom: 30, width: right - left }
    }
    doc.offset = 2
    doc.down(5)
    expect(doc.select().defaultPrevented).toBe(false)
    doc.offset = 4
    doc.down(20)
    expect(doc.select().defaultPrevented).toBe(false)
    expect(doc.ranges).toEqual(['e\u0301', '😀'])
  })

  it('兼容旧 WebView 的 caretRangeFromPoint；没有命中接口时保留原生选择', () => {
    const { doc } = setup()
    doc.caretPositionFromPoint = undefined
    doc.down(23)
    expect(doc.select().defaultPrevented).toBe(false)
    doc.down(300)
    expect(doc.select().defaultPrevented).toBe(true)
    doc.caretRangeFromPoint = undefined
    doc.down(23)
    expect(doc.select().defaultPrevented).toBe(false)
  })

  it('重复安装不重复监听，卸载后恢复原生行为', () => {
    const { doc, cleanup } = setup()
    expect(installTextSelectionGuard(doc as unknown as Document)).toBe(cleanup)
    doc.down(300)
    expect(doc.clear).toHaveBeenCalledOnce()
    cleanup()
    expect(doc.select().defaultPrevented).toBe(false)
  })
})
