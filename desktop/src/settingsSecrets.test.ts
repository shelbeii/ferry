import { describe, expect, it } from 'vitest'
import { aiSecretChanges, dnsSecretChanges, notifySecretChanges } from './settingsSecrets'

describe('设置凭据草稿', () => {
  it('AI 新值覆盖删除意图，收集草稿不会清空输入', () => {
    const pending = { m1: ' new-key ' }, removed = new Set(['m1', 'm2'])
    expect(aiSecretChanges(pending, removed)).toEqual([{ kind: 'ai', id: 'm2', remove: true }, { kind: 'ai', id: 'm1', value: 'new-key' }])
    expect(pending.m1).toBe(' new-key ')
    expect(removed.size).toBe(2)
  })
  it('删除通知方式同时删除地址和加签，显式新签名优先，避免重复项', () => {
    expect(notifySecretChanges({}, new Set(['url:n1', 'sign:n1']))).toEqual([{ kind: 'notify-url', id: 'n1', remove: true }, { kind: 'notify-sign', id: 'n1', remove: true }])
    expect(notifySecretChanges({ 'sign:n1': 'new-sign' }, new Set(['url:n1']))).toEqual([{ kind: 'notify-url', id: 'n1', remove: true }, { kind: 'notify-sign', id: 'n1', value: 'new-sign' }])
  })
  it('DNS 全部校验后生成配对修改，失败仍保留所有草稿', () => {
    const draft = { aliyun: { id: 'id-a', secret: 'secret-a' }, dnspod: { id: '', secret: 'incomplete' }, cloudflare: { id: '', secret: '' } }
    expect(() => dnsSecretChanges(draft, new Set())).toThrow('访问 ID 和密钥要一起填写')
    expect(draft.aliyun.secret).toBe('secret-a')
    draft.dnspod.id = 'id-b'
    expect(dnsSecretChanges(draft, new Set(['cloudflare']))).toEqual([
      { kind: 'dns', id: 'aliyun', value: 'secret-a', accessId: 'id-a' },
      { kind: 'dns', id: 'dnspod', value: 'incomplete', accessId: 'id-b' },
      { kind: 'dns', id: 'cloudflare', remove: true },
    ])
  })
  it('只填 DNS ID 也必须提示，不能在关闭设置时悄悄丢失', () => {
    expect(() => dnsSecretChanges({ aliyun: { id: 'new-id', secret: '' }, dnspod: { id: '', secret: '' }, cloudflare: { id: '', secret: '' } }, new Set())).toThrow('一起填写')
  })
})
