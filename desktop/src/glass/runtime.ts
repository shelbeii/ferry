// 液态玻璃在浏览器里的部分：全局参数、位移图缓存、跟随指针的高光、自适应明暗。
import { reactive, watchEffect } from 'vue'
import { lensField, type LensSpec } from './optics'

/** 新装时玻璃的透明度：比较通透，列表从工具栏下面滑过时看得见后面的字（和 Go 的 settings.DefaultGlassTransparency 一致） */
export const GLASS_TRANSPARENCY_DEFAULT = 90

/**
 * 全局参数。enabled 跟着设置里的「液态玻璃效果」，关掉后玻璃控件变回普通的实心样式、没有动效；
 * transparency 跟着设置里的「玻璃透明度」（0～100，新装默认 GLASS_TRANSPARENCY_DEFAULT）；
 * refraction 是位移倍数（1 为默认），frost 是磨砂倍数，这两个和 dispersion 只在实验页上调。
 */
export const glass = reactive({
  enabled: true,
  transparency: GLASS_TRANSPARENCY_DEFAULT,
  refraction: 1,
  frost: 1,
  dispersion: true,
})

/**
 * 透明度换成两个全局倍数，写在 html 上，所有玻璃（包括只用 CSS 磨砂的工具栏、提示条）一起用：
 * --cg-k（tint）乘到底色的不透明度上，--cg-blur-k（blur）乘到磨砂的模糊半径上。50 时都是 1（原来的样子）。
 * 往磨砂那边：每差 25 翻一倍，0 时底色 4 倍（基本不透）；模糊最多 2 倍，再大拖动窗口、滚动时会明显变慢。
 * 往通透那边：模糊比底色退得快，100 时完全不模糊、底色只剩 1/10，能看清玻璃后面的字（90 时模糊只剩 1/25，也基本看得清）。
 * 只在设置改变时写一次，不是每帧（见 GlassPane 的样式说明）。html 上的 cg-on 表示液态玻璃开着，全局样式（提示条）按它切换
 */
export function glassFactors(transparency: number): { tint: number; blur: number } {
  const v = Math.max(0, Math.min(100, transparency))
  if (v <= 50) {
    const k = 2 ** ((50 - v) / 25)
    return { tint: k, blur: Math.min(2, k) }
  }
  const t = (v - 50) / 50
  return { tint: 1 - .9 * t, blur: (1 - t) ** 2 }
}
if (typeof document !== 'undefined') {
  watchEffect(() => {
    const root = document.documentElement
    const { tint, blur } = glassFactors(glass.transparency)
    setVar(root, '--cg-k', tint.toFixed(3))
    setVar(root, '--cg-blur-k', blur.toFixed(3))
    root.classList.toggle('cg-on', glass.enabled)
  })
}

/**
 * 拖动窗口改大小时：玻璃的折射（SVG 位移滤镜）每帧都要按新的背后内容重算，是拖动时最费的
 * （GPU 进程几乎占满一个核，内容跟不上窗口边缘）。拖动期间 html 上加 cg-resizing，只留磨砂；
 * 停下 0.2 秒后恢复折射、重新生成位移图、重看明暗
 */
export const viewport = reactive({ resizing: false })
let resizeTimer: number | undefined
function onResize() {
  if (!viewport.resizing) {
    viewport.resizing = true
    document.documentElement.classList.add('cg-resizing')
  }
  clearTimeout(resizeTimer)
  resizeTimer = window.setTimeout(() => {
    viewport.resizing = false
    document.documentElement.classList.remove('cg-resizing')
    if (toned.size) queueProbe()
  }, 200)
}
if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') window.addEventListener('resize', onResize)

export interface LensImage { url: string; scale: number; width: number; height: number }

// 同样尺寸的控件共用一张位移图；只缓存最近用到的若干张，免得拖动窗口时无限增长。
const cache = new Map<string, LensImage>()
const LIMIT = 48

const lensKey = (spec: LensSpec) => [spec.width, spec.height, spec.radius, spec.bezel, spec.edge, spec.magnify ?? 0].map((n) => Math.round(n * 10) / 10).join(':')

/** 已经生成过的位移图（没有就是 undefined，不现算） */
export function cachedLensImage(spec: LensSpec): LensImage | undefined {
  return cache.get(lensKey(spec))
}

export function lensImage(spec: LensSpec): LensImage | null {
  const key = lensKey(spec)
  const hit = cache.get(key)
  if (hit) {
    cache.delete(key)
    cache.set(key, hit)
    return hit
  }
  // 位移图在面板上会被拉伸铺满：大面板用较低的分辨率生成就够了（偏移按同样比例换回到面板的像素）
  const ratio = Math.min(1, 320 / Math.max(spec.width, spec.height))
  const field = lensField(ratio === 1 ? spec : {
    width: spec.width * ratio, height: spec.height * ratio, radius: spec.radius * ratio,
    bezel: spec.bezel * ratio, edge: spec.edge * ratio, magnify: spec.magnify,
  })
  const canvas = document.createElement('canvas')
  canvas.width = field.width
  canvas.height = field.height
  // 画布默认放在显卡上，toDataURL 要等显卡画完手头的帧再读回来：抽屉滑出时一张小图要等 7～19 毫秒（平时不到 1 毫秒），
  // 滑出动画因此掉帧。willReadFrequently 让画布留在内存里，编码不用等显卡
  const context = canvas.getContext('2d', { willReadFrequently: true })
  if (!context) return null
  const image = context.createImageData(field.width, field.height)
  image.data.set(field.pixels)
  context.putImageData(image, 0, 0)
  const result = { url: canvas.toDataURL(), scale: field.scale / ratio, width: field.width, height: field.height }
  cache.set(key, result)
  if (cache.size > LIMIT) cache.delete(cache.keys().next().value!)
  return result
}

// 高光方向跟着指针：玻璃边缘朝向指针的一侧最亮，指针靠近时整体更亮。
// 所有玻璃共用一个 pointermove 监听，按帧合并更新。只改 transform 和 opacity，
// 不改 CSS 变量（WebView2 131 每帧改 CSS 变量、同时拖动窗口时会崩溃，见 GlassPane 的样式说明）。
// 高光一变，浏览器就要把这块玻璃的磨砂、折射整个重算一遍（实测指针移动时 GPU 进程占满一个多核，关掉高光后不到十分之一）。
// 所以只有指针附近（220px 内）的玻璃实时跟着转；远处的等指针停下 0.25 秒后一起转过去（有 0.4 秒过渡）；
// 指针在弹窗、抽屉上时，被它们盖住的玻璃不动
const lit = new Map<HTMLElement, { angle: number; near: number; sheen?: HTMLElement; beam?: HTMLElement }>()
let pointer: { x: number; y: number; cover?: Element } | undefined
let frame: number | undefined
let rest: number | undefined
const REACH = 220
const REST = 250

function paint(all = false) {
  frame = undefined
  if (!pointer) return
  let waiting = false
  for (const [element, state] of lit) {
    // 被指针所在的弹窗盖住：只把亮度退回去，方向不变
    const covered = !!pointer.cover && !pointer.cover.contains(element)
    if (covered && !state.near) continue
    const box = element.getBoundingClientRect()
    if (!box.width) continue
    const dx = Math.max(box.left - pointer.x, 0, pointer.x - box.right)
    const dy = Math.max(box.top - pointer.y, 0, pointer.y - box.bottom)
    const near = covered ? 0 : Math.round(Math.max(0, 1 - Math.hypot(dx, dy) / REACH) * 50) / 50
    if (!near && !state.near && !all) { waiting = true; continue }
    state.sheen ??= element.querySelector<HTMLElement>(':scope > .cg-rim > .cg-sheen') ?? undefined
    state.beam ??= state.sheen?.querySelector<HTMLElement>('.cg-beam') ?? undefined
    if (!state.sheen || !state.beam) continue
    if (!covered) {
      const cx = box.left + box.width / 2, cy = box.top + box.height / 2
      let angle = Math.atan2(pointer.x - cx, cy - pointer.y) * 180 / Math.PI
      // 角度连续变化，过渡时不绕远路
      while (angle - state.angle > 180) angle -= 360
      while (angle - state.angle < -180) angle += 360
      // 变化很小时不动，免得一直重绘
      if (Math.abs(angle - state.angle) >= 1) {
        state.angle = angle
        state.beam.style.transform = `rotate(${angle.toFixed(1)}deg)`
      }
    }
    if (near !== state.near) {
      state.near = near
      state.sheen.style.opacity = (.62 + .38 * near).toFixed(2)
    }
  }
  clearTimeout(rest)
  rest = waiting ? window.setTimeout(() => paint(true), REST) : undefined
}

function move(event: PointerEvent) {
  pointer = { x: event.clientX, y: event.clientY, cover: (event.target as Element | null)?.closest?.('.el-overlay') ?? undefined }
  if (frame === undefined) frame = requestAnimationFrame(() => paint())
}

export function trackLight(element: HTMLElement): () => void {
  if (!lit.size) window.addEventListener('pointermove', move, { passive: true })
  lit.set(element, { angle: -45, near: 0 })
  return () => {
    lit.delete(element)
    if (lit.size) return
    window.removeEventListener('pointermove', move)
    if (frame !== undefined) cancelAnimationFrame(frame)
    clearTimeout(rest)
    frame = rest = undefined
  }
}

// 自适应明暗：看玻璃背后内容的亮度，深色内容上换成深色玻璃、浅色字，反之亦然（苹果原生也这样做）。
// 取玻璃上几个点下面的元素，向上找第一个有背景色或渐变的，取颜色平均亮度。滚动、尺寸变化时重新看。
const toned = new Set<HTMLElement>()
let toneTimer: number | undefined

function luminance(rgb: number[]): number {
  const [r, g, b] = rgb.map((c) => { const v = c / 255; return v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4 })
  return .2126 * r + .7152 * g + .0722 * b
}
function colors(text: string): { rgb: number[]; alpha: number }[] {
  return Array.from(text.matchAll(/rgba?\(([^)]+)\)/g), (m) => {
    const parts = m[1].split(/[\s,/]+/).filter(Boolean).map(Number)
    return { rgb: parts.slice(0, 3), alpha: parts[3] ?? 1 }
  })
}
function backdropLuminance(glassElement: HTMLElement, x: number, y: number): number | undefined {
  // 只看叠在玻璃下面的：盖在上面的（对话框遮罩、弹出菜单）不算
  const stack = document.elementsFromPoint(x, y)
  const own = stack.findIndex((element) => glassElement.contains(element))
  if (own < 0) return undefined
  const hit = stack.slice(own + 1).find((element) => !element.closest('.cg'))
  for (let element: Element | null = hit ?? null; element; element = element.parentElement) {
    const style = getComputedStyle(element)
    const found = [...colors(style.backgroundImage), ...colors(style.backgroundColor)].filter((c) => c.alpha >= .5)
    if (found.length) return found.reduce((sum, c) => sum + luminance(c.rgb), 0) / found.length
  }
  return undefined
}
// 滚动时只重看和滚动区域重叠的玻璃（打开 AI 助手滚对话时，不用重看项目页上的按钮）；undefined 表示全部重看
let scrolled: Set<Element> | undefined = new Set()
function overlaps(a: DOMRect, b: DOMRect) {
  return a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom
}
function probe() {
  toneTimer = undefined
  const areas = scrolled && [...scrolled].map((element) => element.getBoundingClientRect())
  scrolled = new Set()
  for (const element of toned) {
    const box = element.getBoundingClientRect()
    if (!box.width || (areas && !areas.some((area) => overlaps(area, box)))) continue
    const samples = [.2, .5, .8].map((fx) => backdropLuminance(element, box.left + box.width * fx, box.top + box.height / 2)).filter((v): v is number => v !== undefined)
    if (!samples.length) continue
    const value = samples.reduce((a, b) => a + b, 0) / samples.length
    // 留一段滞后，亮度在中间来回时不闪
    const current = element.dataset.tone
    if (!current) element.dataset.tone = value < .28 ? 'dark' : 'light'
    else if (value < .22 && current !== 'dark') element.dataset.tone = 'dark'
    else if (value > .34 && current !== 'light') element.dataset.tone = 'light'
  }
}
function queueProbe() {
  scrolled = undefined
  if (toneTimer === undefined) toneTimer = window.setTimeout(probe, 90)
}
function onScroll(event: Event) {
  const target = event.target
  if (target instanceof Element) scrolled?.add(target)
  else scrolled = undefined
  if (toneTimer === undefined) toneTimer = window.setTimeout(probe, 90)
}
// 页面内容变了（切换视图、对话框关掉）不一定有滚动事件：另外每 1.5 秒看一次，窗口隐藏、正在拖动窗口时不看
// （窗口大小变了，拖动停下时会重看，见上面的 onResize）
let slow: number | undefined

export function trackTone(element: HTMLElement): () => void {
  if (!toned.size) {
    document.addEventListener('scroll', onScroll, { capture: true, passive: true })
    slow = window.setInterval(() => { if (!document.hidden && !viewport.resizing) queueProbe() }, 1500)
  }
  toned.add(element)
  queueProbe()
  return () => {
    toned.delete(element)
    if (toned.size) return
    document.removeEventListener('scroll', onScroll, { capture: true })
    clearInterval(slow)
  }
}
export function retone() { queueProbe() }

/** 写 CSS 变量，值没变就不写：拖动窗口时尺寸回调很频繁，WebView2 131 在频繁改 CSS 变量、同时重算样式时会崩溃 */
export function setVar(element: HTMLElement, name: string, value: string) {
  if (element.style.getPropertyValue(name) !== value) element.style.setProperty(name, value)
}

export function reducedMotion(): boolean {
  return matchMedia('(prefers-reduced-motion: reduce)').matches
}
