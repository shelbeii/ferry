import type { SwExtra } from './ferry'

/** 界面上的一行附加配置 */
export interface ExtraRow {
  key: string
  value: string
}

const MAX_VALUE = 4096
const KEY = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/
const PROPS_KEY = /^[A-Za-z0-9][A-Za-z0-9_.\-[\]]{0,191}$/
const ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/
const SECTION = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/

/** 比较名称时用的写法（和 Go 的 Extra.norm 一致）：MySQL 的 - 和 _ 等价；redis、PostgreSQL 不分大小写 */
export function normKey(x: SwExtra, key: string): string {
  const k = key.trim()
  switch (x.format) {
    case 'ini': {
      if (x.section) return k.replaceAll('-', '_').toLowerCase()
      const i = k.indexOf('.')
      if (i < 0) return k.toLowerCase()
      return k.slice(0, i).toLowerCase() + '.' + k.slice(i + 1).replaceAll('-', '_').toLowerCase()
    }
    case 'keyvalue':
    case 'args':
      return k.toLowerCase()
  }
  return k
}

/** 名称有什么问题；没问题返回空 */
export function keyError(x: SwExtra, key: string): string {
  const k = key.trim()
  if (!k) return '写名称'
  let name = k
  if (x.format === 'ini' && !x.section) {
    const i = k.indexOf('.')
    if (i < 0 || !SECTION.test(k.slice(0, i))) return '写成「段.名称」，如 hook.enable'
    name = k.slice(i + 1)
  }
  const re = x.format === 'env' ? ENV_KEY : x.format === 'properties' ? PROPS_KEY : KEY
  if (!re.test(name)) return x.format === 'env' ? '只能用字母、数字、下划线，不能以数字开头' : '只能用字母、数字和 _ . -'
  const n = normKey(x, k)
  if ((x.locked ?? []).some((l) => normKey(x, l) === n)) return '由 Ferry 管理（端口、目录、密码等用上面的参数设置），不能在这里改'
  return ''
}

/** 值有什么问题；没问题返回空 */
export function valueError(x: SwExtra, value: string): string {
  const v = value.trim()
  if (/[\r\n\0]/.test(v)) return '不能换行'
  if (new TextEncoder().encode(v).length > MAX_VALUE) return `太长（最多 ${MAX_VALUE} 字节）`
  if (!v && x.format === 'keyvalue') return '要写值'
  return ''
}

/** 每一行的问题（名称、值、重复）；空行不算 */
export function rowErrors(x: SwExtra, rows: ExtraRow[]): { key: string; value: string }[] {
  const seen = new Map<string, number>()
  return rows.map((r, i) => {
    if (!r.key.trim() && !r.value.trim()) return { key: '', value: '' }
    let key = keyError(x, r.key)
    if (!key) {
      const n = normKey(x, r.key)
      if (seen.has(n)) key = `和第 ${seen.get(n)! + 1} 行是同一项`
      else seen.set(n, i)
    }
    return { key, value: key ? '' : valueError(x, r.value) }
  })
}

/** 交给 ferry 的 FERRY_SOFTWARE_EXTRA：{附加配置ID: {名称: 值}}；没有填的返回空串 */
export function extrasPayload(extras: SwExtra[], rows: Record<string, ExtraRow[]>): string {
  const out: Record<string, Record<string, string>> = {}
  for (const x of extras) {
    for (const r of rows[x.id] ?? []) {
      if (!r.key.trim() && !r.value.trim()) continue
      ;(out[x.id] ??= {})[r.key.trim()] = r.value.trim()
    }
  }
  return Object.keys(out).length ? JSON.stringify(out) : ''
}

/** 填了几项 */
export function extrasCount(rows: Record<string, ExtraRow[]>): number {
  return Object.values(rows).reduce((n, rs) => n + rs.filter((r) => r.key.trim() || r.value.trim()).length, 0)
}

/** 写到哪里（给人看） */
export function extraWhere(x: Pick<SwExtra, 'format' | 'file' | 'section' | 'prefix'>): string {
  const file = x.file ? x.file.split('/').pop() : ''
  switch (x.format) {
    case 'ini':
      return x.section ? `${file} 的 [${x.section}]` : `${file}（名称写成「段.名称」）`
    case 'keyvalue':
    case 'properties':
      return file ?? ''
    case 'env':
      return '服务的环境变量'
    case 'args':
      return `启动参数 ${(x.prefix ?? '').trim()} 名称=值`
  }
}

/** 一项的写法（给人看），和写进文件的格式一致 */
export function extraLine(format: SwExtra['format'], it: { section?: string; key: string; value: string }): string {
  const key = format === 'ini' && it.section ? `[${it.section}] ${it.key}` : it.key
  if (format === 'keyvalue') return `${key} ${it.value}`
  if (format === 'ini' && it.value === '') return key
  return `${key}=${it.value}`
}
