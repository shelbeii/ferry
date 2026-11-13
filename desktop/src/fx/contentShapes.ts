// 打开窗口时粒子收拢到窗口里的内容上（文字、图标、输入框和按钮的边框、加载中的骨架条），看起来像是粒子拼成了里面的字。
// 这里（主线程）只读出这些东西在哪、什么字体：每段文字的位置和字、图标的路径、控件的方框，坐标相对窗口左上角；
// 真正把字画出来、在字形上取点在 Worker 里做（particleEngine.ts 的 inkPoints），主线程不多花时间。

/** t：一行文字（按 w 横向拉齐到页面上的宽度；a 为 center / right 时按原宽度在 w 里居中、靠右，输入框里的字）；b：方框（fill 为 true 时填满，否则描边）；p：图标（SVG 路径，按 viewBox 缩放到 w×h）。
 *  c：裁剪框（在滚动区域里、只露出一部分时），都相对窗口左上角，单位 CSS 像素 */
export type Shape =
  | { k: 't'; s: string; x: number; y: number; w: number; h: number; font: string; a?: 'center' | 'right'; c?: Clip }
  | { k: 'b'; x: number; y: number; w: number; h: number; r: number; fill: boolean; c?: Clip }
  | { k: 'p'; d: string[]; x: number; y: number; w: number; h: number; vb: [number, number, number, number]; c?: Clip }
export type Clip = [number, number, number, number]

const MAX_SHAPES = 1500
const MAX_CHARS = 400 // 一段文字折成几行时逐字找换行，太长的只取前面这些字
// 描边的控件（输入框、按钮、标签……）、填满的（骨架条、开关）
const OUTLINED = '.el-input__wrapper, .el-select__wrapper, .el-textarea__inner, .el-button:not(.is-link):not(.is-text), .el-tag, .el-radio-button__inner, .el-input-number, .el-table__inner-wrapper, .el-alert, .el-pagination .number, .el-checkbox__inner, .el-radio__inner'
const FILLED = '.el-skeleton__item, .el-switch__core'

type Box = { left: number; top: number; width: number; height: number }

/** 窗口 el（布局位置 box）里看得见的内容。
 *  打开动画里窗口带着缩放（从 1.035 倍缩到原大），量到的位置也是放大的：按窗口现在的大小和原大小换算回去，
 *  不然内容晚出来、动画中途重新读位置时，粒子会落歪（边上能差十几像素） */
export function contentShapes(el: HTMLElement, box: Box): Shape[] {
  const shapes: Shape[] = []
  const b = el.getBoundingClientRect()
  const sx = b.width ? box.width / b.width : 1, sy = b.height ? box.height / b.height : 1
  // 屏幕上量到的位置 → 相对窗口左上角、原大小时的位置
  const local = (r: { left: number; top: number; width: number; height: number }) => ({ x: (r.left - b.left) * sx, y: (r.top - b.top) * sy, w: r.width * sx, h: r.height * sy })
  const styles = new WeakMap<Element, CSSStyleDeclaration>()
  const style = (e: Element) => { let s = styles.get(e); if (!s) { s = getComputedStyle(e); styles.set(e, s) } return s }
  const clips = new WeakMap<Element, DOMRect | null>()
  // 往上找滚动区域、overflow 裁剪的容器（到窗口为止），取它们的交集：滚出去的表格行、折叠起来的内容不算
  const clipOf = (e: Element | null): DOMRect | null => {
    if (!e || e === el || !el.contains(e)) return null
    if (clips.has(e)) return clips.get(e)!
    const up = clipOf(e.parentElement)
    const s = style(e)
    let mine: DOMRect | null = up
    if (s.overflowX !== 'visible' || s.overflowY !== 'visible') {
      const r = e.getBoundingClientRect()
      mine = up ? intersect(up, r) : r
    }
    clips.set(e, mine)
    return mine
  }
  const shown = (e: Element) => {
    const s = style(e)
    return s.visibility !== 'hidden' && s.opacity !== '0'
  }
  const rel = (c: DOMRect | null): Clip | undefined => { if (!c) return undefined; const l = local(c); return [l.x, l.y, l.w, l.h] }
  const inside = (r: DOMRect, c: DOMRect | null) => r.width > 0 && r.height > 0 && r.right > b.left && r.left < b.right && r.bottom > b.top && r.top < b.bottom && (!c || (r.right > c.left && r.left < c.right && r.bottom > c.top && r.top < c.bottom))
  const fontOf = (s: CSSStyleDeclaration) => `${s.fontStyle} ${s.fontWeight} ${s.fontSize} ${s.fontFamily}`
  const addText = (s: string, r: DOMRect, font: string, c: DOMRect | null, a?: 'center' | 'right') => {
    if (shapes.length < MAX_SHAPES && inside(r, c)) shapes.push({ k: 't', s, ...local(r), font, a, c: rel(c) })
  }

  // 文字
  const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT)
  const range = document.createRange()
  for (let n = walker.nextNode(); n && shapes.length < MAX_SHAPES; n = walker.nextNode()) {
    const text = n.nodeValue ?? ''
    if (!text.trim()) continue
    const parent = n.parentElement
    if (!parent || !shown(parent)) continue
    const c = clipOf(parent)
    range.selectNodeContents(n)
    const rects = range.getClientRects()
    if (!rects.length) continue
    const font = fontOf(style(parent))
    if (rects.length === 1) { addText(text.trim(), rects[0], font, c); continue }
    // 折成几行：逐字看每个字在哪一行，同一行的字拼成一段
    let line = '', lineRect: DOMRect | null = null
    const flush = () => { if (line.trim() && lineRect) addText(line.trim(), lineRect, font, c); line = ''; lineRect = null }
    const len = Math.min(text.length, MAX_CHARS)
    for (let i = 0; i < len; i++) {
      range.setStart(n, i); range.setEnd(n, i + 1)
      const r = range.getBoundingClientRect()
      if (!r.width && !r.height) { line += text[i]; continue }
      if (lineRect && Math.abs(r.top - lineRect.top) > r.height / 2) flush()
      lineRect = lineRect ? union(lineRect, r) : r
      line += text[i]
    }
    flush()
  }
  range.detach()

  // 输入框里的字（不是文字节点）：填了的内容，没填时的提示
  for (const input of el.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>('input:not([type=hidden]):not([type=checkbox]):not([type=radio]), textarea')) {
    const s = input.value || input.placeholder
    if (!s || !shown(input) || shapes.length >= MAX_SHAPES) continue
    const r = input.getBoundingClientRect()
    const st = style(input)
    const pl = parseFloat(st.paddingLeft) || 0, pr = parseFloat(st.paddingRight) || 0
    const one = s.split('\n')[0].slice(0, 200)
    // 单行输入框的字上下居中；多行的从第一行开始。左右按输入框的对齐方式（数字框是居中的）
    const lh = parseFloat(st.lineHeight) || parseFloat(st.fontSize) * 1.4 || 20
    const top = input instanceof HTMLTextAreaElement ? r.top + (parseFloat(st.paddingTop) || 0) : r.top
    const h = input instanceof HTMLTextAreaElement ? lh : r.height
    const align = st.textAlign === 'center' ? 'center' : st.textAlign === 'right' || st.textAlign === 'end' ? 'right' : undefined
    addText(one, new DOMRect(r.left + pl, top, Math.max(0, r.width - pl - pr), h), fontOf(st), clipOf(input), align)
  }

  // 图标
  for (const svg of el.querySelectorAll<SVGSVGElement>('svg')) {
    if (shapes.length >= MAX_SHAPES) break
    const r = svg.getBoundingClientRect()
    const c = clipOf(svg.parentElement)
    if (!inside(r, c) || !shown(svg) || r.width > 64 || r.height > 64) continue
    const vb = svg.viewBox?.baseVal
    const d = Array.from(svg.querySelectorAll('path'), (p) => p.getAttribute('d') ?? '').filter(Boolean)
    if (!d.length || !vb?.width || !vb.height) continue
    shapes.push({ k: 'p', d, ...local(r), vb: [vb.x, vb.y, vb.width, vb.height], c: rel(c) })
  }

  // 控件的方框、骨架条
  for (const [selector, fill] of [[OUTLINED, false], [FILLED, true]] as const) {
    for (const e of el.querySelectorAll(selector)) {
      if (shapes.length >= MAX_SHAPES) break
      const r = e.getBoundingClientRect()
      const c = clipOf(e.parentElement)
      if (!inside(r, c) || !shown(e)) continue
      const l = local(r)
      shapes.push({ k: 'b', ...l, r: Math.min(parseFloat(style(e).borderTopLeftRadius) || 0, l.h / 2, l.w / 2), fill, c: rel(c) })
    }
  }
  return shapes
}

function intersect(a: DOMRect, b: DOMRect): DOMRect {
  const l = Math.max(a.left, b.left), t = Math.max(a.top, b.top)
  return new DOMRect(l, t, Math.max(0, Math.min(a.right, b.right) - l), Math.max(0, Math.min(a.bottom, b.bottom) - t))
}
function union(a: DOMRect, b: DOMRect): DOMRect {
  const l = Math.min(a.left, b.left), t = Math.min(a.top, b.top)
  return new DOMRect(l, t, Math.max(a.right, b.right) - l, Math.max(a.bottom, b.bottom) - t)
}
