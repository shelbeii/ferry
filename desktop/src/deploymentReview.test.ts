import { describe, expect, it } from 'vitest'
import type { Inspect, ReleaseInfo } from './ferry'
import { deploymentTargets, previousRelease } from './deploymentReview'

const info = { apps: [{ name: 'api' }, { name: 'web' }], targets: [{ name: 'prod', apps: ['api'] }, { name: 'stage', apps: ['api', 'web'] }] } as Inspect

describe('部署目标审阅', () => {
  it('未指定应用时按各目标实际允许的应用展示，保留目标执行顺序', () => {
    expect(deploymentTargets(info, ['stage', 'prod'], []).map((p) => [p.target.name, p.apps])).toEqual([['stage', ['api', 'web']], ['prod', ['api']]])
  })
  it('显式选择不允许的应用时列出冲突，不能把它静默排除后上线', () => {
    expect(deploymentTargets(info, ['prod'], ['api', 'web'])[0]).toMatchObject({ apps: ['api'], unavailable: ['web'] })
  })
  it('空选择不回退到最初目标', () => {
    expect(deploymentTargets(info, [], ['api'])).toEqual([])
  })
})

describe('上一个可用版本', () => {
  const release = (id: string, current = false, failed = false) => ({ id, current, failed }) as ReleaseInfo
  it('跳过失败版本和当前版本之后的记录', () => {
    expect(previousRelease([release('004'), release('003', true), release('002', false, true), release('001')])?.id).toBe('001')
  })
  it('没有当前版本时选最新的可用版本', () => {
    expect(previousRelease([release('001'), release('003', false, true), release('002')])?.id).toBe('002')
  })
  it('没有可用版本时不提供错误的回滚目标', () => {
    expect(previousRelease([release('002', true), release('001', false, true)])).toBeUndefined()
  })
})
