import { describe, expect, it } from 'vitest'
import { createSprings, lensField, refractionProfile, rubber, stepSpring } from './optics'

// 从位移图里读回某个像素的取样偏移（像素）
function offsetAt(field: ReturnType<typeof lensField>, x: number, y: number) {
  const i = (y * field.width + x) * 4
  return [(field.pixels[i] / 255 - .5) * field.scale, (field.pixels[i + 1] / 255 - .5) * field.scale]
}

describe('折射轮廓', () => {
  it('边缘附近最强，进入平顶后为零', () => {
    expect(refractionProfile(1)).toBe(0)
    expect(refractionProfile(.6)).toBeLessThan(.1)
    expect(Math.max(...[0, .01, .02, .05, .1].map(refractionProfile))).toBeCloseTo(1, 1)
    expect(refractionProfile(.05)).toBeGreaterThan(refractionProfile(.4))
  })
})

describe('位移图', () => {
  const field = lensField({ width: 120, height: 40, radius: 20, bezel: 12, edge: 10 })

  it('中间平顶不偏移', () => {
    const [x, y] = offsetAt(field, 60, 20)
    expect(Math.abs(x)).toBeLessThan(.2)
    expect(Math.abs(y)).toBeLessThan(.2)
  })

  it('边缘往里取背景，左右对称', () => {
    const [left] = offsetAt(field, 1, 20)
    const [right] = offsetAt(field, 118, 20)
    expect(left).toBeGreaterThan(2)
    expect(right).toBeLessThan(-2)
    expect(left).toBeCloseTo(-right, 0)
    const [, top] = offsetAt(field, 60, 1)
    expect(top).toBeGreaterThan(2)
  })

  it('圆角外面保持中性', () => {
    const [x, y] = offsetAt(field, 0, 0)
    expect(Math.abs(x)).toBeLessThan(.2)
    expect(Math.abs(y)).toBeLessThan(.2)
  })

  it('放大时整体往中心取', () => {
    const lens = lensField({ width: 100, height: 100, radius: 50, bezel: 10, edge: 0, magnify: .2 })
    const [x] = offsetAt(lens, 70, 50)
    expect(x).toBeCloseTo(-.2 * 20.5, 0)
  })
})

describe('弹簧', () => {
  it('慢帧也收敛，欠阻尼时有回弹', () => {
    let state = { value: 0, velocity: 0 }
    let peak = 0
    for (let i = 0; i < 120; i++) {
      state = stepSpring(state, 1, i % 10 === 0 ? .2 : 1 / 60, { stiffness: 400, damping: 18 })
      peak = Math.max(peak, state.value)
    }
    expect(peak).toBeGreaterThan(1.05)
    expect(state.value).toBeCloseTo(1, 2)
  })

  it('共用一个帧循环，停下后不再申请帧，写回调里改目标不会重复申请', () => {
    const queue: FrameRequestCallback[] = []
    let requests = 0
    const clock = { request: (fn: FrameRequestCallback) => { requests++; queue.push(fn); return requests }, cancel: () => {} }
    let retargeted = false
    const springs = createSprings([{ stiffness: 400, damping: 40 }, { stiffness: 400, damping: 40 }], ([a]) => {
      if (!retargeted && a > .5) { retargeted = true; springs.to(1, 3) }
    }, clock)
    springs.to(0, 1)
    springs.to(0, 1)
    expect(queue.length).toBe(1)
    let time = 0
    while (queue.length) {
      expect(queue.length).toBe(1)
      queue.shift()!(time += 16)
    }
    expect(springs.value(0)).toBe(1)
    expect(springs.value(1)).toBe(3)
    expect(springs.running).toBe(false)
  })

  it('中途换弹性参数：位置和速度接着走，之后按新参数变慢', () => {
    const queue: FrameRequestCallback[] = []
    const clock = { request: (fn: FrameRequestCallback) => { queue.push(fn); return queue.length }, cancel: () => {} }
    let seen = 0
    const springs = createSprings([{ stiffness: 500, damping: 30, precision: .002 }], ([v]) => { seen = v }, clock)
    springs.jump(0, 1)
    springs.tune(0, { stiffness: 100, damping: 14, precision: .002 })
    springs.to(0, 0)
    let time = 0
    for (let i = 0; i < 6; i++) queue.shift()!(time += 1000 / 60)
    // 100 / 14 的弹簧 0.1 秒内还落不到一半；500 / 30 的早就过半了
    expect(seen).toBeGreaterThan(.5)
    while (queue.length) queue.shift()!(time += 1000 / 60)
    expect(springs.value(0)).toBe(0)
  })

  it('橡皮筋不超过上限', () => {
    expect(rubber(1000, 8)).toBeLessThanOrEqual(8)
    expect(rubber(-1000, 8)).toBeGreaterThanOrEqual(-8)
    expect(rubber(10, 8)).toBeLessThan(10)
    expect(rubber(0, 8)).toBe(0)
  })
})
