// 配置里的时长：90s、2m、1h、500ms 或纯数字（秒），和 internal/config 的 Duration 一致。
// 表单里拆成「数字 + 单位」两部分填。

export interface DurUnit {
  /** 写进配置的后缀 */
  v: string
  label: string
  ms: number
}

export const DUR_UNITS: DurUnit[] = [
  { v: 'ms', label: '毫秒', ms: 1 },
  { v: 's', label: '秒', ms: 1000 },
  { v: 'm', label: '分钟', ms: 60_000 },
  { v: 'h', label: '小时', ms: 3_600_000 },
]

const UNIT_MS: Record<string, number> = { ms: 1, s: 1000, m: 60_000, h: 3_600_000 }

/** 时长有多少毫秒；写法不认识时是 null。 */
export function durMs(v: unknown): number | null {
  if (typeof v === 'number') return Number.isFinite(v) && v >= 0 ? v * 1000 : null
  if (typeof v !== 'string') return null
  const s = v.trim()
  if (/^\d+$/.test(s)) return Number(s) * 1000
  if (!s || !/^(\d+(\.\d+)?(ms|s|m|h))+$/.test(s)) return null
  let ms = 0
  for (const m of s.matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h)/g)) ms += Number(m[1]) * UNIT_MS[m[2]]
  return Math.round(ms)
}

/** 拆成数字和单位：用能整除的最大单位（90s → 90 秒，1h30m → 90 分钟，120s → 2 分钟）。 */
export function splitDur(v: unknown): { n: number; u: string } | null {
  const ms = durMs(v)
  if (ms == null) return null
  if (ms === 0) return { n: 0, u: 's' }
  for (let i = DUR_UNITS.length - 1; i >= 0; i--) {
    const u = DUR_UNITS[i]
    if (ms % u.ms === 0) return { n: ms / u.ms, u: u.v }
  }
  return { n: ms, u: 'ms' }
}

/** 给人看：5 秒、10 分钟、1 小时 30 分钟；写法不认识时原样返回。 */
export function durText(v: unknown): string {
  const ms = durMs(v)
  if (ms == null) return String(v ?? '')
  if (ms === 0) return '0 秒'
  const parts: string[] = []
  let rest = ms
  for (let i = DUR_UNITS.length - 1; i >= 0; i--) {
    const u = DUR_UNITS[i]
    const n = Math.floor(rest / u.ms)
    if (n) parts.push(`${n} ${u.label}`)
    rest -= n * u.ms
  }
  return parts.join(' ')
}
