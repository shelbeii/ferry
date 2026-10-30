import { describe, expect, it } from 'vitest'
import { trendSegments } from './resourceTrend'

describe('真实资源采样趋势', () => {
  it('不足两次采样时不伪造趋势', () => {
    expect(trendSegments([], 120000)).toEqual([])
    expect(trendSegments([{ at: 1, value: 50 }], 120000)).toEqual([])
    expect(trendSegments([{ at: 1, value: 50 }, { at: 1, value: 60 }], 120000)).toEqual([])
  })
  it('横轴反映时间间隔，纵轴始终是 0–100%', () => {
    expect(trendSegments([{ at: 0, value: 0 }, { at: 1000, value: 50 }, { at: 4000, value: 100 }], 5000)).toEqual(['0.00,64.00 75.00,36.00 300.00,8.00'])
  })
  it('缺失值及断开连接后的长间隔不会连成连续曲线', () => {
    const points = [{ at: 0, value: 10 }, { at: 1000, value: 20 }, { at: 2000, value: null }, { at: 3000, value: 30 }, { at: 4000, value: 40 }, { at: 100000, value: 50 }]
    expect(trendSegments(points, 5000)).toHaveLength(2)
  })
})
