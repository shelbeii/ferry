// 液态玻璃（Claude 版原型）的光学和动效计算。只做纯计算，不碰 DOM，便于测试。
//
// 模型：一块平顶、边缘是凸圆弧（超椭圆轮廓）的厚玻璃平放在内容上，视线竖直向下。
// 平顶部分不偏折；边缘越陡，按斯涅尔定律偏折越大，看到的是更靠里的内容——
// 所以边缘一圈会把背景「拉伸、弯曲」，中间保持清晰。液滴状态再叠加整体放大。

const IOR = 1.5 // 玻璃折射率
const THICKNESS = .6 // 玻璃厚度相对边缘宽度
const BASE = .15 // 边缘弧面以下的平板厚度

/** 边缘弧面的高度：t=0 在外沿，t=1 进入平顶。超椭圆比圆弧过渡更柔，接近苹果的连续曲率。 */
export function bezelHeight(t: number): number {
  const u = 1 - Math.min(1, Math.max(0, t))
  return Math.pow(1 - u ** 4, 1 / 4)
}

function bezelSlope(t: number): number {
  const u = 1 - Math.min(1, Math.max(0, t))
  const rest = 1 - u ** 4
  return rest <= 1e-9 ? 1e4 : u ** 3 / Math.pow(rest, 3 / 4)
}

/** 竖直视线穿过 t 处弧面后落到底面的横向偏移（以边缘宽度为单位，未归一）。 */
function rawRefraction(t: number): number {
  if (t >= 1) return 0
  const incidence = Math.atan(bezelSlope(t) * THICKNESS)
  const refracted = Math.asin(Math.sin(incidence) / IOR)
  return Math.tan(incidence - refracted) * (bezelHeight(t) * THICKNESS + BASE)
}

const PEAK = (() => {
  let peak = 0
  for (let i = 0; i <= 400; i++) peak = Math.max(peak, rawRefraction(i / 400))
  return peak
})()

/** 归一化的折射强度轮廓：边缘附近为 1，进入平顶后为 0。 */
export function refractionProfile(t: number): number {
  return rawRefraction(t) / PEAK
}

// 生成位移图时逐像素查表，不再每个像素算一遍三角函数（大面板第一次打开时这一步要几十毫秒）
const STEPS = 1024
const PROFILE = Float32Array.from({ length: STEPS + 1 }, (_, i) => refractionProfile(i / STEPS))

export interface LensSpec {
  width: number
  height: number
  /** 圆角半径，胶囊形传高度的一半 */
  radius: number
  /** 边缘弧面的宽度（像素） */
  bezel: number
  /** 最强处的折射偏移（像素） */
  edge: number
  /** 整体放大：0 不放大，.2 约放大 1.25 倍。液滴、开关滑块按下时用 */
  magnify?: number
}

export interface LensField {
  width: number
  height: number
  /** feDisplacementMap 的 scale：像素值 128 表示不偏移，0/255 表示 ∓scale/2 */
  scale: number
  /** RGBA，R、G 分别是 x、y 方向的取样偏移 */
  pixels: Uint8ClampedArray
}

/** 圆角矩形的位移场。每个像素记录「从哪里取背景」：边缘往里取，放大时整体往中心取。 */
export function lensField(spec: LensSpec): LensField {
  const width = Math.max(1, Math.round(spec.width))
  const height = Math.max(1, Math.round(spec.height))
  const radius = Math.min(Math.max(0, spec.radius), width / 2, height / 2)
  const bezel = Math.max(1, spec.bezel)
  const magnify = spec.magnify ?? 0
  const offsets = new Float32Array(width * height * 2)
  let largest = 0
  const hx = width / 2, hy = height / 2
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      const px = x + .5 - hx, py = y + .5 - hy
      // 圆角矩形的有向距离和外法线
      const qx = Math.abs(px) - (hx - radius), qy = Math.abs(py) - (hy - radius)
      let nx: number, ny: number, distance: number
      if (qx > 0 && qy > 0) {
        const length = Math.hypot(qx, qy)
        nx = qx / length; ny = qy / length
        distance = length - radius
      } else if (qx > qy) {
        nx = 1; ny = 0; distance = qx - radius
      } else {
        nx = 0; ny = 1; distance = qy - radius
      }
      if (distance > 0) continue // 圆角外面，保持中性
      const t = -distance / bezel
      if (t >= 1 && !magnify) continue // 平顶部分不偏折
      nx *= Math.sign(px) || 1
      ny *= Math.sign(py) || 1
      const bend = t >= 1 ? 0 : PROFILE[(t * STEPS) | 0] * spec.edge
      const ox = -nx * bend - px * magnify
      const oy = -ny * bend - py * magnify
      const i = (y * width + x) * 2
      offsets[i] = ox; offsets[i + 1] = oy
      largest = Math.max(largest, Math.abs(ox), Math.abs(oy))
    }
  }
  const scale = Math.max(1, largest * 2)
  const pixels = new Uint8ClampedArray(width * height * 4)
  for (let i = 0, j = 0; i < offsets.length; i += 2, j += 4) {
    pixels[j] = Math.round((offsets[i] / scale + .5) * 255)
    pixels[j + 1] = Math.round((offsets[i + 1] / scale + .5) * 255)
    pixels[j + 2] = 128
    pixels[j + 3] = 255
  }
  return { width, height, scale, pixels }
}

export interface SpringConfig {
  stiffness: number
  damping: number
  /** 离目标多近、速度多小算停下 */
  precision?: number
}

export interface SpringState { value: number; velocity: number }

/** 半隐式欧拉，按 1/240 秒细分，慢帧也不会发散；改目标时保留速度，动作连贯。 */
export function stepSpring(state: SpringState, target: number, seconds: number, config: SpringConfig): SpringState {
  let { value, velocity } = state
  let left = Math.min(.05, Math.max(0, seconds))
  while (left > 0) {
    const dt = Math.min(left, 1 / 240)
    velocity += (-(value - target) * config.stiffness - velocity * config.damping) * dt
    value += velocity * dt
    left -= dt
  }
  return { value, velocity }
}

export interface FrameClock {
  request: (callback: FrameRequestCallback) => number
  cancel: (id: number) => void
}

const browserClock: FrameClock = { request: (fn) => requestAnimationFrame(fn), cancel: (id) => cancelAnimationFrame(id) }

/** 一组弹簧共用一个 RAF；全部停下后不再申请帧。write 拿到当前值和速度。 */
export function createSprings(
  configs: SpringConfig[],
  write: (values: number[], velocities: number[]) => void,
  clock: FrameClock = browserClock,
) {
  const states: SpringState[] = configs.map(() => ({ value: 0, velocity: 0 }))
  const targets = configs.map(() => 0)
  let frame: number | undefined
  let previous: number | undefined
  const emit = () => write(states.map((s) => s.value), states.map((s) => s.velocity))
  function tick(now: number) {
    frame = undefined
    const dt = previous === undefined ? 1 / 60 : (now - previous) / 1000
    previous = now
    let moving = false
    states.forEach((state, i) => {
      const next = stepSpring(state, targets[i], dt, configs[i])
      const precision = configs[i].precision ?? .01
      if (Math.abs(next.value - targets[i]) < precision && Math.abs(next.velocity) < precision * 10) {
        state.value = targets[i]; state.velocity = 0
      } else {
        state.value = next.value; state.velocity = next.velocity
        moving = true
      }
    })
    emit() // write 里可能改了目标并已经申请了下一帧
    if (frame !== undefined) return
    if (moving) frame = clock.request(tick)
    else previous = undefined
  }
  function wake() { if (frame === undefined) frame = clock.request(tick) }
  return {
    to(index: number, target: number) { if (targets[index] !== target) { targets[index] = target; wake() } },
    jump(index: number, value: number) { targets[index] = value; states[index] = { value, velocity: 0 }; emit() },
    /** 换一组弹性参数（比如浮起快、落下慢），当前位置和速度不变 */
    tune(index: number, config: SpringConfig) { configs[index] = config },
    value: (index: number) => states[index].value,
    velocity: (index: number) => states[index].velocity,
    target: (index: number) => targets[index],
    stop() {
      if (frame !== undefined) clock.cancel(frame)
      frame = undefined
      previous = undefined
    },
    get running() { return frame !== undefined },
  }
}

/** 橡皮筋：越拉越费劲，最多到 limit。 */
export function rubber(offset: number, limit: number): number {
  return limit * Math.tanh(offset / (limit * 2.5))
}
