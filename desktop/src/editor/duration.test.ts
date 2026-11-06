import { describe, expect, it } from 'vitest'
import { durMs, durText, splitDur } from './duration'

describe('duration', () => {
  it('读出配置里的写法', () => {
    expect(durMs('90s')).toBe(90_000)
    expect(durMs(30)).toBe(30_000)
    expect(durMs('30')).toBe(30_000)
    expect(durMs('1h30m')).toBe(5_400_000)
    expect(durMs('500ms')).toBe(500)
    expect(durMs('1.5h')).toBe(5_400_000)
    expect(durMs('abc')).toBeNull()
    expect(durMs('5 s')).toBeNull()
    expect(durMs('')).toBeNull()
  })

  it('拆成数字和单位', () => {
    expect(splitDur('5s')).toEqual({ n: 5, u: 's' })
    expect(splitDur('120s')).toEqual({ n: 2, u: 'm' })
    expect(splitDur('1h30m')).toEqual({ n: 90, u: 'm' })
    expect(splitDur('10m')).toEqual({ n: 10, u: 'm' })
    expect(splitDur('500ms')).toEqual({ n: 500, u: 'ms' })
    expect(splitDur(0)).toEqual({ n: 0, u: 's' })
    expect(splitDur('x')).toBeNull()
  })

  it('给人看', () => {
    expect(durText('5s')).toBe('5 秒')
    expect(durText('180s')).toBe('3 分钟')
    expect(durText('1h30m')).toBe('1 小时 30 分钟')
    expect(durText(600)).toBe('10 分钟')
    expect(durText('没有 tcp 时 10 秒')).toBe('没有 tcp 时 10 秒')
  })
})
