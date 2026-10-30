import { describe, expect, it } from 'vitest'
import { glassFactors } from './runtime'

describe('玻璃透明度', () => {
  it('50 是原来的样子', () => {
    expect(glassFactors(50)).toEqual({ tint: 1, blur: 1 })
  })
  it('往磨砂那边每差 25 翻一倍，模糊最多 2 倍', () => {
    expect(glassFactors(25)).toEqual({ tint: 2, blur: 2 })
    expect(glassFactors(0)).toEqual({ tint: 4, blur: 2 })
  })
  it('往通透那边模糊退得更快，最右边完全不模糊、底色很淡，看得清后面的字', () => {
    const at75 = glassFactors(75), at90 = glassFactors(90), at100 = glassFactors(100)
    expect(at75.blur).toBeLessThan(at75.tint)
    expect(at90.blur).toBeLessThan(.1)
    expect(at100.blur).toBe(0)
    expect(at100.tint).toBeCloseTo(.1)
  })
  it('超出范围按两端算', () => {
    expect(glassFactors(-20)).toEqual(glassFactors(0))
    expect(glassFactors(180)).toEqual(glassFactors(100))
  })
})
