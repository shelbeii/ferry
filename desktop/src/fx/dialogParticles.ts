// 对话框的粒子动画，要细、要淡、要快：
// 打开时，窗口形状的一团细尘（比窗口大一圈、稀疏）往里收缩，落到哪里就在哪里淡掉，窗口同时从略大缩到原大、显出来，像是粒子凝成了窗口；
// 关闭时，窗口很快淡出，同时化成细尘往外散开、淡掉。
// 用法：main.ts 里 app.use(ElementPlus, { dialog: { transition: dialogTransition } })，所有 el-dialog 都用它，不用一个个改组件。
// 粒子画在一块全屏 canvas 上（不接收鼠标），只在有动画时显示；窗口本身只改 opacity（Web Animations；不缩放，缩放会让里面的字变大再缩回），
// 不改 CSS 变量（WebView2 131 频繁改 CSS 变量会崩溃，见 glass/runtime.ts 的 setVar）。
// 设置里关掉「粒子特效」（ui.particles，见 setParticles）、系统设置了「减少动态效果」、或者拿不到 canvas（测试环境）时，退回简单的淡入淡出。
import type { TransitionProps } from 'vue'
import { createEngine, type Box, type EffectSpec, type View } from './particleEngine'
import type { FromWorker, ToWorker } from './particleWorker'
import type { ToInk } from './inkWorker'
import { contentShapes, type Shape } from './contentShapes'

let enabled = true

function reducedMotion(): boolean {
  return typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches
}

/** 现在放不放粒子：设置里开着、系统没有要求减少动态效果 */
function particlesOn(): boolean {
  return enabled && !reducedMotion()
}

/** 设置里的「粒子特效」开关（state.ts 读到设置时、设置窗口里切换时调用）。
 * html 上的 fx-particles 表示开着：下拉框不再缩放弹出，交给粒子（见 style.css、popperParticles.ts） */
export function setParticles(on: boolean) {
  enabled = on
  if (typeof document !== 'undefined') document.documentElement.classList.toggle('fx-particles', particlesOn())
}

export const OPEN_MS = 300 // 打开：窗口显出来用的时间（粒子差不多同时落完）
export const CLOSE_MS = 110 // 关闭：窗口淡出用的时间

// 开发时调动画用：控制台里 __ferryFxSlow = 10 就慢放 10 倍
export function slow(): number {
  const v = import.meta.env.DEV ? Number((window as { __ferryFxSlow?: number }).__ferryFxSlow) : 1
  return v > 0 ? v : 1
}

// 粒子的计算和绘制在 particleEngine.ts：优先放进 Web Worker（画在 OffscreenCanvas 上），
// 打开窗口时主线程常常正忙着生成内容（「部署历史」的整张表格），粒子在主线程上画就会顿一下；放在 Worker 里不受影响。
// 主线程这边只管：canvas 显示 / 隐藏、取颜色、把窗口位置和里面的内容发过去。
// 粒子落到窗口里的字上，先要把字画出来取点：这一步放在另一个 Worker（inkWorker.ts）里，算好直接交给画粒子的 Worker。
interface Backend {
  start(spec: EffectSpec, view: View): void
  track(id: number, rect: Box, view: View): void
  retarget(id: number, shapes: Shape[], w: number, h: number): void
}
let canvas: HTMLCanvasElement | null = null
let backend: Backend | null = null
let noWorker = false // Worker 起不来（比如被安全策略拦了）：以后都在主线程里画
let gen = 0 // 每放一次加一：Worker 说「放完了」时，只有是最近这一次才把 canvas 藏起来
let nextId = 0

function hide() {
  if (canvas) canvas.style.display = 'none'
}

// 开发时看粒子有没有掉帧：控制台里 __ferryFxStats 是上一轮的帧数和最长的一帧间隔（毫秒）
function report(stats: { frames: number; maxGap: number; inkMs?: number }, where: string) {
  if (import.meta.env.DEV) (window as { __ferryFxStats?: unknown }).__ferryFxStats = { ...stats, where }
}

function newCanvas(): HTMLCanvasElement {
  canvas?.remove()
  const c = document.createElement('canvas')
  c.className = 'ferry-fx-canvas'
  c.setAttribute('aria-hidden', 'true')
  Object.assign(c.style, { position: 'fixed', inset: '0', width: '100vw', height: '100vh', pointerEvents: 'none', zIndex: '2147483000', display: 'none' })
  document.body.appendChild(c)
  canvas = c
  return c
}

function workerBackend(c: HTMLCanvasElement): Backend | null {
  if (noWorker || typeof Worker !== 'function' || typeof c.transferControlToOffscreen !== 'function') return null
  let w: Worker
  try {
    w = new Worker(new URL('./particleWorker.ts', import.meta.url), { type: 'module' })
    const off = c.transferControlToOffscreen()
    w.postMessage({ type: 'init', canvas: off } satisfies ToWorker, [off])
  } catch {
    noWorker = true
    return null
  }
  const fail = () => {
    // canvas 已经交给了 Worker，主线程不能再画：换一块新的，下次在主线程里画
    noWorker = true
    w.terminate()
    if (canvas === c) { c.remove(); canvas = null; backend = null }
  }
  w.onerror = fail
  // 取点的 Worker：起不来就让画粒子的 Worker 自己取点（会偶尔停一帧）
  let ink: Worker | null = null
  try {
    ink = new Worker(new URL('./inkWorker.ts', import.meta.url), { type: 'module' })
    const ch = new MessageChannel()
    ink.postMessage({ type: 'port', port: ch.port1 } satisfies ToInk, [ch.port1])
    w.postMessage({ type: 'inkPort', port: ch.port2 } satisfies ToWorker, [ch.port2])
    ink.onerror = () => { ink?.terminate(); ink = null }
  } catch {
    ink = null
  }
  w.onmessage = (e: MessageEvent<FromWorker>) => {
    const m = e.data
    if (m.type === 'ready' && !m.ok) fail()
    if (m.type === 'idle') {
      report(m, 'worker')
      if (m.gen === gen) hide()
    }
  }
  return {
    start: (spec, view) => {
      const shapes = spec.shapes
      if (ink && shapes) {
        w.postMessage({ type: 'start', gen, spec: { ...spec, shapes: undefined, inkLater: true }, view } satisfies ToWorker)
        ink.postMessage({ type: 'ink', id: spec.id, shapes, w: spec.rect.width, h: spec.rect.height } satisfies ToInk)
      } else w.postMessage({ type: 'start', gen, spec, view } satisfies ToWorker)
    },
    track: (id, rect, view) => w.postMessage({ type: 'track', id, rect, view } satisfies ToWorker),
    retarget: (id, shapes, width, height) => {
      if (ink) ink.postMessage({ type: 'ink', id, shapes, w: width, h: height } satisfies ToInk)
      else w.postMessage({ type: 'retarget', id, shapes } satisfies ToWorker)
    },
  }
}

function ensureBackend(): Backend | null {
  if (backend && canvas?.isConnected) return backend
  const c = newCanvas()
  backend = workerBackend(c)
  if (!backend) {
    const target = c.isConnected && !noWorker ? c : newCanvas()
    backend = createEngine(target, stats => { report(stats, 'main'); hide() })
  }
  return backend
}

const viewNow = (): View => ({ width: window.innerWidth, height: window.innerHeight, dpr: window.devicePixelRatio || 1 })
const boxOf = (r: DOMRect): Box => ({ left: r.left, top: r.top, width: r.width, height: r.height })

// 颜色跟着主题：深色主题用浅淡的蓝白，粒子最亮只到一半；浅色主题在白底上要深一点、浓一点才看得清
let scratch: CanvasRenderingContext2D | null | undefined
function parseColor(c: CanvasRenderingContext2D, color: string): [number, number, number] {
  c.fillStyle = '#000'
  c.fillStyle = color
  const v = String(c.fillStyle)
  if (v.startsWith('#') && v.length === 7) return [parseInt(v.slice(1, 3), 16), parseInt(v.slice(3, 5), 16), parseInt(v.slice(5, 7), 16)]
  const m = v.match(/\d+(\.\d+)?/g)
  return m && m.length >= 3 ? [+m[0], +m[1], +m[2]] : [200, 220, 255]
}
function pickPalette(): { colors: [number, number, number][]; maxAlpha: number } | null {
  if (scratch === undefined) {
    try { scratch = document.createElement('canvas').getContext('2d') } catch { scratch = null }
  }
  if (!scratch) return null
  const css = getComputedStyle(document.documentElement)
  const v = (name: string, fallback: string) => css.getPropertyValue(name).trim() || fallback
  const dark = document.documentElement.classList.contains('dark')
  const names = dark
    ? ['#e8efff', '#c9dafc', v('--el-color-primary-light-5', '#a0cfff'), '#d6d0ff']
    : [v('--el-color-primary', '#409eff'), v('--el-color-primary-dark-2', '#337ecc'), '#4a72d4', '#7b62e6']
  const sc = scratch
  return { colors: names.map(n => parseColor(sc, n)), maxAlpha: dark ? .5 : .78 }
}

// 窗口在布局里的位置：打开动画里窗口带着缩放，按布局尺寸换回来
function layoutRect(el: HTMLElement): DOMRect {
  const b = el.getBoundingClientRect()
  const w = el.offsetWidth || b.width, h = el.offsetHeight || b.height
  return new DOMRect(b.left + b.width / 2 - w / 2, b.top + b.height / 2 - h / 2, w, h)
}

// 打开时窗口内容加载后高度会变：动画期间每帧看一下窗口的位置，变了就发过去（主线程卡住的那几帧粒子按上次的位置继续）。
// 窗口里的内容也常常是打开后才出来的（设置的各页、异步加载的组件）：粒子飞的前一段时间里内容有变化，
// 就重新读一次文字的位置发过去，还没落定的粒子转向新的文字（最多每 40 毫秒一次、一共两次：Worker 里每次取点也要十来毫秒）
interface Tracked {
  el: HTMLElement
  until: number
  last: Box
  watch?: { observer: MutationObserver; dirty: boolean; at: number; until: number; left: number }
}
const tracked = new Map<number, Tracked>()
let trackFrame = 0
function untrack(id: number) {
  tracked.get(id)?.watch?.observer.disconnect()
  tracked.delete(id)
}
function trackLoop() {
  trackFrame = 0
  const now = performance.now()
  for (const [id, t] of tracked) {
    if (now > t.until || !t.el.isConnected) { untrack(id); continue }
    const r = boxOf(layoutRect(t.el))
    if (r.width > 0 && (r.left !== t.last.left || r.top !== t.last.top || r.width !== t.last.width || r.height !== t.last.height)) {
      t.last = r
      backend?.track(id, r, viewNow())
    }
    const w = t.watch
    if (w && now > w.until) { w.observer.disconnect(); t.watch = undefined }
    else if (w?.dirty && w.left > 0 && now - w.at >= 40 * slow() && r.width > 0) {
      w.dirty = false
      w.at = now
      w.left--
      try { backend?.retarget(id, contentShapes(t.el, r), r.width, r.height) } catch { /* 读不出来就按原来的落 */ }
    }
  }
  if (tracked.size) trackFrame = requestAnimationFrame(trackLoop)
}

function play(kind: 'open' | 'close', rect: DOMRect, radius: number, el?: HTMLElement, min = 1200, density = 50, pace = 1) {
  const pal = pickPalette()
  const b = pal && ensureBackend()
  if (!pal || !b || !canvas) return false
  gen++
  canvas.style.display = 'block'
  const id = ++nextId
  const box = boxOf(rect)
  // 打开时粒子收拢到窗口里的文字、图标、控件上，关闭时从它们上面散开：这里只读出它们的位置，画字、取点在 Worker 里
  let shapes: ReturnType<typeof contentShapes> | undefined
  if (el) {
    const t0 = performance.now()
    try { shapes = contentShapes(el, box) } catch { shapes = undefined }
    if (import.meta.env.DEV) (window as { __ferryFxShapes?: unknown }).__ferryFxShapes = { count: shapes?.length, ms: Math.round((performance.now() - t0) * 10) / 10 }
  }
  // 按窗口面积定数量（每 density 平方像素一颗，最少 min 颗），大窗口和小窗口一样密
  b.start({ id, kind, rect: box, radius, min, density, pace, colors: pal.colors, maxAlpha: pal.maxAlpha, slow: slow(), closeMs: CLOSE_MS, shapes }, viewNow())
  if (el && kind === 'open') {
    const now = performance.now()
    const t: Tracked = { el, until: now + 700 * pace * slow(), last: box }
    if (shapes) {
      const observer = new MutationObserver(() => { if (t.watch) t.watch.dirty = true })
      observer.observe(el, { childList: true, subtree: true, characterData: true })
      // 粒子大多在 250 毫秒左右落定：前 170 毫秒里内容变了才换落点
      t.watch = { observer, dirty: false, at: now, until: now + 170 * pace * slow(), left: 2 }
    }
    tracked.set(id, t)
    if (!trackFrame) trackFrame = requestAnimationFrame(trackLoop)
  }
  return true
}

// 窗口、遮罩正在放的动画（打开到一半又关掉时先停掉）
const running = new WeakMap<Element, Animation[]>()

function stop(el: Element) {
  for (const a of running.get(el) ?? []) a.cancel()
  running.delete(el)
  el.classList.remove('ferry-fx-mask')
}

function dialogOf(el: Element) {
  return el.querySelector<HTMLElement>('.el-dialog')
}

function radiusOf(dialog: HTMLElement) {
  return parseFloat(getComputedStyle(dialog).borderTopLeftRadius) || 12
}

// 遮罩淡入淡出：遮罩外层的透明度不能动（里面玻璃的磨砂会失效，见 style.css），渐变底色又是主线程每帧算的，
// 打开时主线程忙着生成窗口内容，遮罩就停在半截、再一下跳到底。
// 改成动画期间把底色挪到遮罩的 ::before 上（同一个颜色），只渐变它的透明度：透明度动画由合成线程做，主线程忙也照常。
// 颜色写在 --ferry-fx-mask 上，每次打开、关闭写一次（不是每帧，见 glass/runtime.ts 的 setVar）
function fadeMask(overlay: HTMLElement, from: number, to: number, duration: number): Animation | undefined {
  const mask = getComputedStyle(overlay).backgroundColor
  if (!mask || mask === 'transparent' || /^rgba\(.*,\s*0\)$/.test(mask)) return undefined // 没有遮罩（modal 为 false）
  overlay.style.setProperty('--ferry-fx-mask', mask)
  overlay.classList.add('ferry-fx-mask')
  return overlay.animate([{ opacity: from }, { opacity: to }], { duration, easing: 'ease-out', fill: 'forwards', pseudoElement: '::before' })
}

function onEnter(el: Element, done: () => void) {
  const overlay = el as HTMLElement
  stop(overlay)
  const dialog = dialogOf(overlay)
  const anims: Animation[] = []
  const mask = fadeMask(overlay, 0, 1, OPEN_MS)
  if (mask) anims.push(mask)
  const rect = dialog && layoutRect(dialog)
  const fancy = !!dialog && !!rect && rect.width > 0 && particlesOn() && play('open', rect, radiusOf(dialog), dialog)
  if (dialog) {
    anims.push(fancy
      // 只淡入、不缩放：前面四成全透明，等粒子先聚到字上，字再跟着显出来。
      // 以前从 1.035 倍缩到原大，里面的字会先变大再缩回、动画结束时还从缩放的模糊状态跳回清晰，看起来字的大小和样式在变
      ? dialog.animate([
        { opacity: 0, offset: 0 },
        { opacity: 0, offset: .4 },
        { opacity: 1, offset: 1 },
      ], { duration: OPEN_MS, easing: 'cubic-bezier(.25,.8,.3,1)', fill: 'backwards' })
      : dialog.animate([{ opacity: 0, translate: '0 -16px' }, { opacity: 1, translate: '0 0' }], { duration: 220, easing: 'ease-out', fill: 'backwards' }))
  }
  for (const a of anims) a.playbackRate = 1 / slow()
  running.set(overlay, anims)
  // 放完把遮罩的底色还给外层（同一个颜色，看不出切换）
  Promise.all(anims.map(a => a.finished)).then(() => { stop(overlay); done() }, () => {})
}

function onLeave(el: Element, done: () => void) {
  const overlay = el as HTMLElement
  stop(overlay)
  const dialog = dialogOf(overlay)
  const anims: Animation[] = []
  const rect = dialog?.getBoundingClientRect()
  const fancy = !!dialog && !!rect && rect.width > 0 && particlesOn() && play('close', rect, radiusOf(dialog), dialog)
  const duration = fancy ? CLOSE_MS : 200
  const mask = fadeMask(overlay, 1, 0, 200)
  if (mask) anims.push(mask)
  if (dialog) {
    anims.push(fancy
      ? dialog.animate([{ opacity: 1 }, { opacity: 0 }], { duration, easing: 'ease-out', fill: 'forwards' })
      : dialog.animate([{ opacity: 1, translate: '0 0' }, { opacity: 0, translate: '0 -16px' }], { duration, easing: 'ease-in', fill: 'forwards' }))
  }
  for (const a of anims) a.playbackRate = 1 / slow()
  running.set(overlay, anims)
  // 窗口淡出就算关完，粒子在 canvas 上自己飞完；遮罩不等它（下面的界面早一点能点）
  const last = anims[anims.length - 1]
  last.finished.then(() => {
    done()
    // 关完把 fill 留下的样式去掉，下次打开从干净的状态开始
    stop(overlay)
  }, () => {})
}

/** 粒子数量和快慢：min 最少几颗，density 每多少平方像素一颗（越小越密） */
export interface ParticleAmount {
  min?: number
  density?: number
  pace?: number // 打开时收拢用的时间是对话框的几倍（小于 1 更快）
}

/** 粒子收拢成 el（下拉框等别的弹出层用）：el 要已经显示出来。放不了（减少动态效果、没有 canvas）时返回 false */
export function particlesIn(el: HTMLElement, amount: ParticleAmount = {}): boolean {
  if (!particlesOn()) return false
  const rect = layoutRect(el)
  return rect.width > 0 && play('open', rect, radiusOf(el), el, amount.min, amount.density, amount.pace)
}

/** el 化成粒子散开（el 自己的淡出由调用方做） */
export function particlesOut(el: HTMLElement, amount: ParticleAmount = {}): boolean {
  if (!particlesOn()) return false
  const rect = el.getBoundingClientRect()
  return rect.width > 0 && play('close', rect, radiusOf(el), el, amount.min, amount.density)
}

/** 给 Element Plus 的全局对话框配置用：css 为 false，动画全由上面的 JS 做 */
export const dialogTransition: TransitionProps = {
  name: 'ferry-fx-dialog',
  css: false,
  onEnter,
  onLeave,
  onEnterCancelled: el => stop(el),
  onLeaveCancelled: el => stop(el),
}
