// 粒子的计算和绘制（对话框、下拉框共用，见 dialogParticles.ts）。不碰 DOM，只用一块 canvas：
// 平时在 Web Worker 里跑（particleWorker.ts，画在 OffscreenCanvas 上），主线程忙着生成界面（比如打开「部署历史」时生成整张表格）也不卡；
// 浏览器不支持时在主线程里跑，画法一样。
import type { Shape } from './contentShapes'

/** 窗口在屏幕上的位置（CSS 像素） */
export interface Box { left: number; top: number; width: number; height: number }
/** 屏幕大小（CSS 像素）和缩放比例 */
export interface View { width: number; height: number; dpr: number }

export interface EffectSpec {
  id: number
  kind: 'open' | 'close'
  rect: Box
  radius: number
  min: number // 最少几颗
  density: number // 每多少平方像素一颗
  pace: number // 打开时收拢用的时间是对话框的几倍
  colors: [number, number, number][]
  maxAlpha: number // 粒子最亮到多少
  slow: number // 开发时慢放的倍数
  closeMs: number // 关闭：窗口淡出用的时间（粒子同时淡淡地显出来）
  shapes?: Shape[] // 打开：窗口里的文字、图标、控件（相对窗口左上角，见 contentShapes.ts），粒子收拢到它们上面
  inkLater?: boolean // 落点由取点的 Worker 算好后再送过来（applyInk）：打开时先朝窗口里均匀的位置飞；关闭时等落点到了才开始（最多等 100 毫秒）
}

interface Particle {
  x: number; y: number // 现在的位置
  px: number; py: number // 上一帧的位置（画很短的拖尾）
  u: number; v: number // 在窗口里的相对位置（0～1）；边上的 u 是沿圆角边一圈的比例
  edge: boolean // 在窗口边上：收拢后勾出窗口的轮廓
  text: boolean // 落在窗口里的文字、图标、控件上（tx、ty 是相对窗口左上角的落点）
  tx: number; ty: number
  ox: number; oy: number // 落在内容上的：起点离落点多远（位置 = 落点 + o × 剩下的路程比例）；中途换落点时重算，保证不跳
  spread: number // 离窗口中心的倍数：打开时从这么远收进来，关闭时散到这么远
  jx: number; jy: number // 起点（打开）或终点（关闭）再随机偏一点，不是整整齐齐放大
  delay: number; dur: number // 毫秒
  size: 0 | 1 // 细的、稍粗的两种
  color: number // 调色板下标
  a: number // 这一帧的透明度
}

interface Effect {
  spec: EffectSpec
  waiting?: number // 关闭：在等取点的结果（开始等的时间）；等到前粒子不显示
  start: number
  end: number
  parts: Particle[]
  rect: Box
}

const TRAIL = 5 // 拖尾最长几个像素

const rand = (a: number, b: number) => a + Math.random() * (b - a)
const clamp01 = (t: number) => (t < 0 ? 0 : t > 1 ? 1 : t)
const easeOut = (t: number) => 1 - Math.pow(1 - t, 3)
const easeInOut = (t: number) => (t < .5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2)

// 按窗口面积定数量，大窗口和小窗口一样密；全屏那么大的最多 18000 颗（再多一帧画不完，反而卡）
function countFor(r: Box, min: number, density: number) {
  return Math.round(Math.min(18000, Math.max(min, r.width * r.height / density)))
}

// 沿圆角矩形的边走 u（0～1）一圈，取那一点；圆角处沿弧走，不会在直角上戳出去
function edgePoint(r: Box, radius: number, u: number): [number, number] {
  const rr = Math.max(0, Math.min(radius, r.width / 2, r.height / 2))
  const right = r.left + r.width, bottom = r.top + r.height
  const sw = r.width - 2 * rr, sh = r.height - 2 * rr, arc = Math.PI * rr / 2
  let d = u * (2 * sw + 2 * sh + 4 * arc)
  const corner = (cx: number, cy: number, from: number): [number, number] => {
    const a = from + (d / (arc || 1)) * Math.PI / 2
    return [cx + Math.cos(a) * rr, cy + Math.sin(a) * rr]
  }
  if (d < sw) return [r.left + rr + d, r.top]
  d -= sw
  if (d < arc) return corner(right - rr, r.top + rr, -Math.PI / 2)
  d -= arc
  if (d < sh) return [right, r.top + rr + d]
  d -= sh
  if (d < arc) return corner(right - rr, bottom - rr, 0)
  d -= arc
  if (d < sw) return [right - rr - d, bottom]
  d -= sw
  if (d < arc) return corner(r.left + rr, bottom - rr, Math.PI / 2)
  d -= arc
  if (d < sh) return [r.left, bottom - rr - d]
  d -= sh
  return corner(r.left + rr, r.top + rr, Math.PI)
}

function homeOf(r: Box, radius: number, p: Particle): [number, number] {
  if (p.text) return [r.left + p.tx, r.top + p.ty]
  return p.edge ? edgePoint(r, radius, p.u) : [r.left + 3 + p.u * Math.max(0, r.width - 6), r.top + 3 + p.v * Math.max(0, r.height - 6)]
}

// 把窗口里的文字、图标、控件按原来的位置画到一块看不见的画布上，取有墨的像素当粒子的落点（相对窗口左上角）。
// 按原大小画（落点再在像素里随机偏一点，不会排成整齐的格子）。字按页面上量到的宽度横向拉齐（字体回退、字距不同时也对得上位置）。
// 在 Worker 里画，但这一步花的时间也会让粒子停一帧：只读有内容的那块区域，按 32 位一次读一个像素，只扫一遍、只记有墨像素的序号，
// 粒子要落点时（pick）再换算成坐标
const INK_SCALE = 1
let inkMs = 0 // 最近一轮里取点最长用了多久（开发时看）
/** 有墨的像素：idx 是在 (bx, by) 起、宽 bw 的那块区域里的序号 */
export interface Ink { idx: Uint32Array; bx: number; by: number; bw: number }
let inkCtx: OffscreenCanvasRenderingContext2D | null = null
let inkIdx = new Uint32Array(0)
export function inkPoints(shapes: Shape[], w: number, h: number): Ink | null {
  const t0 = performance.now()
  const out = inkPointsRaw(shapes, w, h)
  inkMs = Math.max(inkMs, performance.now() - t0)
  return out
}
function inkPointsRaw(shapes: Shape[], w: number, h: number): Ink | null {
  const W = Math.ceil(w * INK_SCALE), H = Math.ceil(h * INK_SCALE)
  if (!shapes.length || !W || !H || W * H > 6e6 || typeof OffscreenCanvas !== 'function') return null
  // 画布和序号数组一直复用：每次新建，读像素、扫描都会慢好几倍（还要回收内存）
  if (!inkCtx) {
    try { inkCtx = new OffscreenCanvas(W, H).getContext('2d', { willReadFrequently: true }) } catch { inkCtx = null }
  }
  const g = inkCtx
  if (!g) return null
  if (g.canvas.width < W || g.canvas.height < H) { g.canvas.width = Math.max(W, g.canvas.width); g.canvas.height = Math.max(H, g.canvas.height) }
  g.setTransform(1, 0, 0, 1, 0, 0)
  g.clearRect(0, 0, W, H)
  g.scale(INK_SCALE, INK_SCALE)
  g.fillStyle = g.strokeStyle = '#000'
  g.lineWidth = 1
  g.textBaseline = 'middle'
  for (const sh of shapes) {
    g.save()
    if (sh.c) { g.beginPath(); g.rect(sh.c[0], sh.c[1], sh.c[2], sh.c[3]); g.clip() }
    try {
      if (sh.k === 't') {
        g.font = sh.font
        const m = g.measureText(sh.s).width
        if (sh.a) {
          // 输入框里的字：按原宽度居中 / 靠右
          g.translate(sh.x + (sh.a === 'center' ? (sh.w - m) / 2 : sh.w - m), sh.y + sh.h / 2)
        } else {
          g.translate(sh.x, sh.y + sh.h / 2)
          if (m > 0) g.scale(Math.min(1.4, Math.max(.6, sh.w / m)), 1)
        }
        g.fillText(sh.s, 0, 0)
      } else if (sh.k === 'b') {
        g.beginPath()
        if (sh.fill) g.roundRect(sh.x, sh.y, sh.w, sh.h, sh.r)
        else g.roundRect(sh.x + .5, sh.y + .5, Math.max(0, sh.w - 1), Math.max(0, sh.h - 1), sh.r)
        if (sh.fill) g.fill()
        else g.stroke()
      } else {
        const [vx, vy, vw, vh] = sh.vb
        g.translate(sh.x, sh.y)
        g.scale(sh.w / vw, sh.h / vh)
        g.translate(-vx, -vy)
        for (const d of sh.d) g.fill(new Path2D(d))
      }
    } catch { /* 画不了的跳过 */ }
    g.restore()
  }
  // 只读画了东西的范围
  let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity
  for (const sh of shapes) {
    x0 = Math.min(x0, sh.x); y0 = Math.min(y0, sh.y)
    x1 = Math.max(x1, sh.x + sh.w); y1 = Math.max(y1, sh.y + sh.h)
  }
  const bx = Math.max(0, Math.floor(x0 * INK_SCALE) - 2), by = Math.max(0, Math.floor(y0 * INK_SCALE) - 2)
  const bw = Math.min(W, Math.ceil(x1 * INK_SCALE) + 2) - bx, bh = Math.min(H, Math.ceil(y1 * INK_SCALE) + 2) - by
  if (bw <= 0 || bh <= 0) return null
  const px = new Uint32Array(g.getImageData(bx, by, bw, bh).data.buffer)
  const LIMIT = 110 << 24 >>> 0 // 透明度大于 110 算有墨（小端：最高字节是透明度）
  if (inkIdx.length < px.length) inkIdx = new Uint32Array(px.length)
  let n = 0
  for (let i = 0; i < px.length; i++) if (px[i] > LIMIT) inkIdx[n++] = i
  return n >= 40 ? { idx: inkIdx.slice(0, n), bx, by, bw } : null
}

/** Worker 刚起来时空跑一次取点：建好画布、让扫描像素的代码先被优化，第一次打开窗口时不慢 */
export function warmUp() {
  const shapes: Shape[] = []
  for (let i = 0; i < 24; i++) shapes.push({ k: 't', s: '粒子 Ferry 预热 0123456789', x: 10, y: 10 + i * 24, w: 360, h: 20, font: '14px sans-serif' })
  // 画布按常见的大窗口大小建好，之后不用再变大（变大要重新分配，很慢）
  for (let i = 0; i < 3; i++) inkPoints(shapes, 1600, 1100)
  inkMs = 0
}

// 落在文字上的粒子：飞出来时很快显出来，越靠近字越透明，落到字上时正好消失。
// 窗口里的字这时同时显出来，粒子不会停在字上挡住它（以前落定后在字上停 0.1 秒再淡，会短暂盖住内容）
const smooth = (t: number) => t * t * (3 - 2 * t)

// 均匀铺满窗口：按网格每格随机放一颗（比纯随机均匀，不会一块密一块空），再取一部分放在边上
function makeParticles(s: EffectSpec): Particle[] {
  const r = s.rect, kind = s.kind
  const n = countFor(r, s.min, s.density)
  const ink = s.shapes ? inkPoints(s.shapes, r.width, r.height) : null
  // 有内容时：打开时大部分落到文字、图标、控件上，关闭时从它们上面散开；一部分勾出窗口的边，剩下一点细尘铺在窗口里。
  // 打开时内容还没出来（异步加载）就先均匀铺开，等内容出来后主线程发 retarget 过来，飞在半路的粒子转向文字
  if (s.shapes || s.inkLater) return textParticles(s, n, ink)
  const edges = Math.round(n * (kind === 'open' ? .2 : .14))
  const inner = n - edges
  const cols = Math.max(1, Math.round(Math.sqrt(inner * r.width / Math.max(1, r.height))))
  const rows = Math.max(1, Math.ceil(inner / cols))
  const colors = Math.max(1, s.colors.length)
  const parts: Particle[] = []
  const add = (u: number, v: number, edge: boolean) => {
    // 离中心越远的越晚落下 / 越先散开，整体是从里到外的一层层
    const k = edge ? 1 : Math.min(1, Math.hypot(u - .5, v - .5) * 1.42)
    parts.push({
      x: 0, y: 0, px: 0, py: 0, u, v, edge, text: false, tx: 0, ty: 0, ox: 0, oy: 0,
      spread: kind === 'open' ? rand(1.18, 1.5) : rand(1.12, 1.45),
      jx: rand(-18, 18), jy: rand(-18, 18) + (kind === 'close' ? -rand(4, 22) : 0),
      delay: kind === 'open' ? (k * 45 + rand(0, 35)) * s.pace : (1 - k) * 25 + rand(0, 30),
      dur: kind === 'open' ? rand(190, 250) * s.pace : rand(750, 1000),
      size: Math.random() < (edge ? .45 : .2) ? 1 : 0,
      color: edge ? 0 : Math.floor(Math.random() * colors),
      a: 0,
    })
  }
  for (let i = 0; i < inner; i++) add(((i % cols) + Math.random()) / cols, (Math.floor(i / cols) + Math.random()) / rows, false)
  for (let i = 0; i < edges; i++) add((i + Math.random()) / edges, 0, true)
  return parts
}

// 在有墨的点里随机取一个；没有内容时在窗口里随机取
function pick(ink: Ink | null, r: Box): [number, number] {
  if (!ink) return [3 + Math.random() * Math.max(0, r.width - 6), 3 + Math.random() * Math.max(0, r.height - 6)]
  const i = ink.idx[Math.floor(Math.random() * ink.idx.length)]
  return [(ink.bx + (i % ink.bw) + Math.random()) / INK_SCALE, (ink.by + Math.floor(i / ink.bw) + Math.random()) / INK_SCALE]
}

function textParticles(s: EffectSpec, n: number, ink: Ink | null): Particle[] {
  const r = s.rect, open = s.kind === 'open'
  const edges = Math.round(n * (open ? .15 : .12))
  const dust = Math.round(n * (open ? .08 : .05))
  const onText = Math.max(0, n - edges - dust)
  const colors = Math.max(1, s.colors.length)
  const parts: Particle[] = []
  // 快慢、散开多远和原来一样（打开 / 关闭各自调好的）
  const push = (p: Partial<Particle> & { u: number; v: number; k: number }) => {
    const { k, ...rest } = p
    parts.push({
      x: 0, y: 0, px: 0, py: 0, edge: false, text: false, tx: 0, ty: 0, ox: 0, oy: 0,
      spread: open ? rand(1.18, 1.5) : rand(1.12, 1.45),
      jx: rand(-18, 18), jy: rand(-18, 18) + (open ? 0 : -rand(4, 22)),
      delay: open ? (k * 45 + rand(0, 35)) * s.pace : (1 - k) * 25 + rand(0, 30),
      dur: open ? rand(190, 250) * s.pace : rand(750, 1000),
      size: 0, color: Math.floor(Math.random() * colors), a: 0,
      ...rest,
    })
  }
  for (let i = 0; i < onText; i++) {
    const [tx, ty] = pick(ink, r)
    const u = tx / Math.max(1, r.width), v = ty / Math.max(1, r.height)
    const k = Math.min(1, Math.hypot(u - .5, v - .5) * 1.42)
    if (!open) {
      // 关闭：从字上散开
      push({ u, v, k, text: true, tx, ty, size: Math.random() < .08 ? 1 : 0 })
      continue
    }
    // 起点更散一些（不是把整排字放大），收拢时才看出字形
    const spread = rand(1.15, 1.6), jx = rand(-40, 40), jy = rand(-40, 40)
    push({ u, v, k, text: true, tx, ty, spread, jx, jy, size: Math.random() < .08 ? 1 : 0,
      ox: (tx - r.width / 2) * (spread - 1) + jx, oy: (ty - r.height / 2) * (spread - 1) + jy })
  }
  for (let i = 0; i < dust; i++) {
    const u = Math.random(), v = Math.random()
    push({ u, v, k: Math.min(1, Math.hypot(u - .5, v - .5) * 1.42) })
  }
  for (let i = 0; i < edges; i++) push({ u: (i + Math.random()) / edges, v: 0, k: 1, edge: true, color: 0, size: Math.random() < .45 ? 1 : 0 })
  return parts
}

function stepOpen(fx: Effect, t: number) {
  const r = fx.rect, s = fx.spec
  const cx = r.left + r.width / 2, cy = r.top + r.height / 2
  for (const p of fx.parts) {
    const [hx, hy] = homeOf(r, s.radius, p)
    const k = clamp01((t - p.delay) / p.dur)
    // 从放大的位置收进来：离落点越近越慢，轻轻落定
    const e = 1 - easeOut(k)
    const x = hx + (p.text ? p.ox : (hx - cx) * (p.spread - 1) + p.jx) * e
    const y = hy + (p.text ? p.oy : (hy - cy) * (p.spread - 1) + p.jy) * e
    p.px = k <= 0 ? x : p.x; p.py = k <= 0 ? y : p.y
    p.x = x; p.y = y
    // 一出现就淡淡地显出来，快落定时淡掉（窗口这时正好显出来，接上）；边上的多留一会儿，勾一下轮廓
    const fadeIn = clamp01(k / .25)
    if (p.text) {
      // 落到字上：前两成路程显出来，之后越近越淡，落定时为 0
      p.a = s.maxAlpha * clamp01(k / .2) * (1 - smooth(clamp01((k - .25) / .75)))
      continue
    }
    const fadeOut = 1 - clamp01((k - (p.edge ? .8 : .55)) / (p.edge ? .2 : .45))
    const linger = p.edge && k >= 1 ? 1 - clamp01((t - p.delay - p.dur) / 70) : 0
    p.a = s.maxAlpha * (k >= 1 ? linger * .6 : fadeIn * fadeOut)
  }
}

function stepClose(fx: Effect, t: number) {
  const r = fx.rect, s = fx.spec
  const cx = r.left + r.width / 2, cy = r.top + r.height / 2
  for (const p of fx.parts) {
    const [hx, hy] = homeOf(r, s.radius, p)
    const k = clamp01((t - p.delay) / p.dur)
    const m = easeOut(k)
    const x = hx + ((hx - cx) * (p.spread - 1) + p.jx) * m
    const y = hy + ((hy - cy) * (p.spread - 1) + p.jy) * m
    p.px = k <= 0 ? x : p.x; p.py = k <= 0 ? y : p.y
    p.x = x; p.y = y
    // 窗口淡出的同时粒子淡淡地显出来，然后边散边淡
    const fadeIn = clamp01(t / (s.closeMs * .6))
    p.a = s.maxAlpha * fadeIn * Math.pow(1 - easeInOut(k), 1.3)
  }
}

type Canvas2D = CanvasRenderingContext2D | OffscreenCanvasRenderingContext2D

export interface Engine {
  start(spec: EffectSpec, view: View): void
  /** 打开的窗口内容加载后高度会变：主线程每帧把新位置发过来 */
  track(id: number, rect: Box, view: View): void
  /** 窗口里的内容变了（异步加载出来）：还没落定的粒子改落到新内容上 */
  retarget(id: number, shapes: Shape[]): void
  /** 取点的 Worker 算好的落点（inkLater）：还没落定的粒子改落到这些点上 */
  applyInk(id: number, ink: Ink): void
}

/** onIdle：动画都放完了（canvas 已经缩成 0），可以把 canvas 藏起来；stats 是这一轮的帧数和最长的一帧间隔（毫秒） */
export function createEngine(canvas: HTMLCanvasElement | OffscreenCanvas, onIdle: (stats: { frames: number; maxGap: number; inkMs: number }) => void): Engine | null {
  let c: Canvas2D | null = null
  try { c = canvas.getContext('2d') as Canvas2D | null } catch { c = null }
  if (!c) return null
  const ctx = c
  const effects: Effect[] = []
  let view: View = { width: 0, height: 0, dpr: 1 }
  let dpr = 1
  let frame = 0
  let img: ImageData | null = null
  let prevBox: [number, number, number, number] | null = null
  let frames = 0, lastAt = 0, maxGap = 0
  let lastStarted = 0
  const pendingInk = new Map<number, Ink>()
  const raf: (f: (t: number) => void) => number = typeof requestAnimationFrame === 'function'
    ? f => requestAnimationFrame(f)
    : f => setTimeout(() => f(performance.now()), 16) as unknown as number

  function sizeCanvas() {
    dpr = Math.min(view.dpr || 1, 2)
    const w = Math.round(view.width * dpr), h = Math.round(view.height * dpr)
    if (canvas.width !== w) canvas.width = w
    if (canvas.height !== h) canvas.height = h
    if (!img || img.width !== canvas.width || img.height !== canvas.height) {
      img = canvas.width && canvas.height ? ctx.createImageData(canvas.width, canvas.height) : null
      prevBox = null
    }
  }

  function draw(now: number) {
    frame = 0
    frames++
    if (lastAt) maxGap = Math.max(maxGap, now - lastAt)
    lastAt = now
    sizeCanvas()
    for (let e = effects.length - 1; e >= 0; e--) {
      const fx = effects[e]
      if (fx.waiting !== undefined) {
        // 关闭：取点的结果还没到，粒子先不出现；等太久（取点的 Worker 出了问题）就按均匀的位置散开
        if (now - fx.waiting < 100 * fx.spec.slow) continue
        fx.waiting = undefined
        fx.start = now
      }
      const t = (now - fx.start) / fx.spec.slow
      if (t >= fx.end) { effects.splice(e, 1); continue }
      if (fx.spec.kind === 'open') stepOpen(fx, t)
      else stepClose(fx, t)
    }
    if (img) drawParticles(img)
    if (effects.length) frame = raf(draw)
    else idle()
  }

  // 画法：粒子多（大窗口上万颗），canvas 一颗颗描线太慢（1.6 万颗时一帧要 40 毫秒），
  // 改成直接往像素缓冲里写：每颗按亚像素位置分摊到相邻 4 个像素（边缘柔和、移动不跳格），带一小段渐淡的拖尾；
  // 只清空、回写粒子所在的那块区域。
  function drawParticles(im: ImageData) {
    const W = im.width, H = im.height, data = im.data
    // 这一帧粒子占的范围（CSS 像素）
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity
    for (const fx of effects) for (const p of fx.parts) {
      if (p.a <= .01) continue
      const ax = Math.min(p.x, p.px), bx = Math.max(p.x, p.px), ay = Math.min(p.y, p.py), by = Math.max(p.y, p.py)
      if (ax < x0) x0 = ax
      if (bx > x1) x1 = bx
      if (ay < y0) y0 = ay
      if (by > y1) y1 = by
    }
    const box: [number, number, number, number] | null = x0 <= x1
      ? [Math.max(0, Math.floor((x0 - TRAIL) * dpr) - 2), Math.max(0, Math.floor((y0 - TRAIL) * dpr) - 2), Math.min(W, Math.ceil((x1 + TRAIL) * dpr) + 3), Math.min(H, Math.ceil((y1 + TRAIL) * dpr) + 3)]
      : null
    // 要清空、回写的区域 = 上一帧的 ∪ 这一帧的
    const all = [prevBox, box].filter((b): b is [number, number, number, number] => !!b && b[2] > b[0] && b[3] > b[1])
    if (!all.length) { prevBox = null; return }
    const rx = Math.min(...all.map(b => b[0])), ry = Math.min(...all.map(b => b[1]))
    const rx1 = Math.max(...all.map(b => b[2])), ry1 = Math.max(...all.map(b => b[3]))
    for (let y = ry; y < ry1; y++) data.fill(0, (y * W + rx) * 4, (y * W + rx1) * 4)

    const blend = (i: number, r: number, g: number, b: number, sa: number) => {
      if (sa < .003) return
      const da = data[i + 3] / 255
      const oa = sa + da * (1 - sa)
      const k = sa / oa
      data[i] += (r - data[i]) * k
      data[i + 1] += (g - data[i + 1]) * k
      data[i + 2] += (b - data[i + 2]) * k
      data[i + 3] = oa * 255
    }
    // 亚像素位置分摊到相邻 4 个像素
    const plot = (x: number, y: number, r: number, g: number, b: number, a: number) => {
      x -= .5; y -= .5
      const ix = Math.floor(x), iy = Math.floor(y)
      if (ix < 0 || iy < 0 || ix + 1 >= W || iy + 1 >= H) return
      const fx = x - ix, fy = y - iy
      const i = (iy * W + ix) * 4, j = i + W * 4
      blend(i, r, g, b, a * (1 - fx) * (1 - fy))
      blend(i + 4, r, g, b, a * fx * (1 - fy))
      blend(j, r, g, b, a * (1 - fx) * fy)
      blend(j + 4, r, g, b, a * fx * fy)
    }
    // 一颗粒子 1 个 CSS 像素见方（缩放 125% 时是 1.25×1.25 个设备像素），分摊到像素后按面积补回亮度
    const gain = 1.3 * dpr * dpr
    for (const fx of effects) for (const p of fx.parts) {
      if (p.a <= .01) continue
      const [r, g, b] = fx.spec.colors[p.color] ?? [200, 220, 255]
      const x = p.x * dpr, y = p.y * dpr
      let dx = (p.x - p.px) * dpr, dy = (p.y - p.py) * dpr
      let len = Math.hypot(dx, dy)
      const max = TRAIL * dpr
      if (len > max) { dx *= max / len; dy *= max / len; len = max }
      // 拖尾：往回一个像素一个点，越往后越淡
      const steps = Math.max(1, Math.ceil(len))
      // 稍粗的那种：再错开半个像素多点一个
      const a = Math.min(.9, (p.size ? p.a * .85 : p.a) * gain)
      for (let s = 0; s < steps; s++) {
        const f = s / steps
        const px = x - dx * f, py = y - dy * f, pa = a * (1 - .7 * f)
        plot(px, py, r, g, b, pa)
        if (p.size) plot(px + .55 * dpr, py + .45 * dpr, r, g, b, pa)
      }
    }
    ctx.putImageData(im, 0, 0, rx, ry, rx1 - rx, ry1 - ry)
    prevBox = box
  }

  function idle() {
    frame = 0
    // 不用时缩成 0，免得一直占着一整屏的显存
    canvas.width = 0; canvas.height = 0
    img = null; prevBox = null
    const stats = { frames, maxGap: Math.round(maxGap), inkMs: Math.round(inkMs) }
    inkMs = 0
    frames = 0; lastAt = 0; maxGap = 0
    onIdle(stats)
  }

  return {
    start(spec, v) {
      view = v
      const parts = makeParticles(spec)
      const longest = parts.reduce((m, p) => Math.max(m, p.delay + p.dur), 0)
      const now = performance.now()
      effects.push({ spec, start: now, end: longest + (spec.kind === 'open' ? 90 : 20), parts, rect: spec.rect, waiting: spec.kind === 'close' && spec.inkLater ? now : undefined })
      lastStarted = Math.max(lastStarted, spec.id)
      const early = pendingInk.get(spec.id)
      pendingInk.clear()
      if (early) this.applyInk(spec.id, early)
      if (!frame) frame = raf(draw)
    },
    track(id, rect, v) {
      view = v
      for (const fx of effects) if (fx.spec.id === id && rect.width > 0) fx.rect = rect
    },
    retarget(id, shapes) {
      const fx = effects.find((f) => f.spec.id === id && f.spec.kind === 'open')
      const ink = fx && inkPoints(shapes, fx.rect.width, fx.rect.height)
      if (ink) this.applyInk(id, ink)
    },
    applyInk(id, ink) {
      const fx = effects.find((f) => f.spec.id === id)
      if (!fx) {
        // 比 start 先到（两条消息走的路不同）：先存着
        if (id > lastStarted) pendingInk.set(id, ink)
        return
      }
      const r = fx.rect
      if (fx.spec.kind === 'close') {
        // 关闭：粒子从字上出发，落点到了才开始散
        for (const p of fx.parts) if (p.text) [p.tx, p.ty] = pick(ink, r)
        if (fx.waiting !== undefined) { fx.waiting = undefined; fx.start = performance.now() }
        return
      }
      const t = (performance.now() - fx.start) / fx.spec.slow
      let longest = fx.end
      for (const p of fx.parts) {
        if (!p.text) continue
        const k = clamp01((t - p.delay) / p.dur)
        if (k >= 1) continue // 已经落定的不动
        const [tx, ty] = pick(ink, r)
        if (k <= 0) {
          // 还没出发：直接换落点，起点按新落点算
          p.ox = (tx - r.width / 2) * (p.spread - 1) + p.jx
          p.oy = (ty - r.height / 2) * (p.spread - 1) + p.jy
        } else {
          // 飞在半路：从现在的位置接着飞向新落点（剩下的路程比例不变，不跳）
          const e = Math.max(.05, 1 - easeOut(k))
          p.ox = (p.x - (r.left + tx)) / e
          p.oy = (p.y - (r.top + ty)) / e
        }
        p.tx = tx; p.ty = ty
        longest = Math.max(longest, p.delay + p.dur + 90)
      }
      fx.end = longest
    },
  }
}
