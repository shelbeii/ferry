import { describe, expect, it } from 'vitest'
import { defaultEffort, defaultLevels, effortLabel, effortOptions, modelEffort, validEffortValue } from './ai'

const m = (o: Record<string, unknown>) => ({ provider: 'openai' as const, thinking: '', baseUrl: '', model: '', ...o })

describe('思考档位', () => {
  it('没设置时按接口给默认档位', () => {
    expect(defaultLevels(m({ baseUrl: 'https://api.deepseek.com', model: 'deepseek-flash' })).map((l) => l.value)).toEqual(['off', 'low', 'high', 'max'])
    expect(defaultLevels(m({ provider: 'anthropic' })).map((l) => l.value)).toEqual(['low', 'medium', 'high', 'xhigh', 'max'])
    expect(defaultLevels(m({ model: 'gpt-5' })).map((l) => l.value)).toEqual(['low', 'medium', 'high'])
    expect(defaultLevels(m({ thinking: 'none' }))).toEqual([])
  })
  it('设置了就用设置的', () => {
    const x = m({ model: 'deepseek-flash', levels: [{ label: '快', value: 'low' }, { label: '超高', value: 'max' }] })
    expect(effortOptions(x)).toEqual([
      { value: 'low', label: '快' },
      { value: 'max', label: '超高' },
    ])
    expect(effortLabel(x, 'max')).toBe('超高')
    expect(effortLabel(x, 'weird')).toBe('weird')
  })
  it('没选过默认档位时挑一个具体的档位', () => {
    const ds = m({ model: 'deepseek-flash' })
    expect(modelEffort({ ...ds, effort: '' })).toBe('high')
    expect(modelEffort({ ...ds, effort: 'off' })).toBe('off')
    expect(modelEffort({ ...ds, effort: 'on' })).toBe('high')
    expect(defaultEffort([{ value: 'off' }, { value: 'low' }, { value: 'max' }])).toBe('low')
    expect(defaultEffort([{ value: 'off' }])).toBe('off')
    expect(defaultEffort([])).toBe('')
  })
  it('档位的值', () => {
    expect(validEffortValue('high')).toBe(true)
    expect(validEffortValue('8192')).toBe(true)
    expect(validEffortValue('none')).toBe(false)
    expect(validEffortValue('a b')).toBe(false)
    expect(validEffortValue('')).toBe(false)
  })
})
