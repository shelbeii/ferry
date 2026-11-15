/** 敏感值只放在本次保存请求中，不进入设置文件、任务记录或保存结果。 */
export interface SettingsSecretChange {
  kind: 'ai' | 'notify-url' | 'notify-sign' | 'dns'
  id: string
  value?: string
  accessId?: string
  remove?: boolean
}

export function aiSecretChanges(pending: Record<string, string>, removed: Iterable<string>): SettingsSecretChange[] {
  return [
    ...[...removed].filter(id => !pending[id]).map(id => ({ kind: 'ai' as const, id, remove: true })),
    ...Object.entries(pending).map(([id, value]) => ({ kind: 'ai' as const, id, value: value.trim() })),
  ]
}

export function notifySecretChanges(pending: Record<string, string>, removed: Iterable<string>): SettingsSecretChange[] {
  const changes = new Map<string, SettingsSecretChange>()
  const change = (key: string, value?: string) => {
    const [what, id] = key.split(':')
    if (!id || (what !== 'url' && what !== 'sign')) throw new Error('通知凭据的标识无效')
    const kind = what === 'url' ? 'notify-url' : 'notify-sign'
    changes.set(key, value === undefined ? { kind, id, remove: true } : { kind, id, value: value.trim() })
  }
  for (const key of removed) {
    if (pending[key]) continue
    change(key)
    // 旧的 notify forget 删除地址时也会清理加签密钥，保留这一语义。
    if (key.startsWith('url:') && !pending[`sign:${key.slice(4)}`]) change(`sign:${key.slice(4)}`)
  }
  for (const [key, value] of Object.entries(pending)) change(key, value)
  return [...changes.values()]
}

export const dnsProviderNames = { aliyun: '阿里云 DNS', dnspod: '腾讯云 DNSPod', cloudflare: 'Cloudflare', huaweicloud: '华为云 DNS', route53: 'AWS Route 53' }
export type DnsProvider = keyof typeof dnsProviderNames
export function dnsSecretChanges(draft: Partial<Record<DnsProvider, { id: string; secret: string }>>, removed: ReadonlySet<string>): SettingsSecretChange[] {
  const changes: SettingsSecretChange[] = []
  for (const id of Object.keys(dnsProviderNames) as DnsProvider[]) {
    const d = draft[id], value = d?.secret.trim(), accessId = d?.id.trim()
    if (id !== 'cloudflare' && !!value !== !!accessId) throw new Error(`${dnsProviderNames[id]}：访问 ID 和密钥要一起填写`)
    if (value) changes.push({ kind: 'dns', id, value, ...(id === 'cloudflare' ? {} : { accessId }) })
    else if (removed.has(id)) changes.push({ kind: 'dns', id, remove: true })
  }
  return changes
}
