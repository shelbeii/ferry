import { describe, expect, it } from 'vitest'
import { prefixOffsets, visibleRange } from './virtualRows'

describe('prefixOffsets', () => {
  it('累计每行的顶部位置，最后是总高度', () => {
    expect(prefixOffsets([10, 20, 30])).toEqual([0, 10, 30, 60])
    expect(prefixOffsets([])).toEqual([0])
  })
})

describe('visibleRange', () => {
  const offsets = prefixOffsets(Array.from({ length: 1000 }, (_, i) => (i % 3 === 0 ? 57 : 37)))

  it('只返回和视口相交的行，加上前后余量', () => {
    const [start, end] = visibleRange(offsets, 0, 400, 0)
    expect(start).toBe(0)
    expect(offsets[end - 1]).toBeLessThan(400)
    expect(offsets[end]).toBeGreaterThanOrEqual(400)
    expect(visibleRange(offsets, 0, 400, 5)).toEqual([0, end + 5])
  })

  it('滚到中间：第一行是底部越过视口顶部的那行', () => {
    const top = offsets[500] + 5
    const [start, end] = visibleRange(offsets, top, top + 300, 0)
    expect(start).toBe(500)
    expect(offsets[end]).toBeGreaterThanOrEqual(top + 300)
    expect(offsets[end - 1]).toBeLessThan(top + 300)
  })

  it('视口正好落在行的边界上', () => {
    expect(visibleRange(offsets, offsets[10], offsets[20], 0)).toEqual([10, 20])
  })

  it('视口在表头上方（负数）和列表末尾之外都不越界', () => {
    expect(visibleRange(offsets, -40, 100, 3)[0]).toBe(0)
    const total = offsets[offsets.length - 1]
    const [first, last] = visibleRange(offsets, total - 50, total + 500, 0)
    expect(last).toBe(1000)
    expect(offsets[first]).toBeLessThanOrEqual(total - 50)
    expect(visibleRange(offsets, total - 50, total + 500, 3)).toEqual([first - 3, 1000])
  })

  it('没有行时是空的', () => {
    expect(visibleRange([0], 0, 500)).toEqual([0, 0])
  })
})
