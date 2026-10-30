// AI 助手：思考强度的选项、token 用量与费用估算（设置里的模型列表、对话时共用）。
// 设置里「不设置」存成空字符串；下拉框里用 'none'（空值会被当成没选，显示成「请选择」）。
import type { AiModel } from './ferry'

export const EFFORT_LABELS: Record<string, string> = {
  none: '不设置',
  off: '关闭',
  on: '开启',
  low: '低',
  medium: '中',
  high: '高',
  xhigh: '很高',
  max: '最高',
}

export const THINKING_STYLES: { value: string; label: string }[] = [
  { value: '', label: '自动识别' },
  { value: 'effort', label: 'reasoning_effort（OpenAI 等）' },
  { value: 'deepseek', label: 'DeepSeek（开 / 关）' },
  { value: 'qwen', label: '通义千问（enable_thinking）' },
  { value: 'none', label: '不传思考参数' },
]

/** OpenAI 兼容接口传思考设置的方式（和 ferry 里的识别规则一致） */
export function thinkingStyle(m: Pick<AiModel, 'provider' | 'thinking' | 'baseUrl' | 'model'>): string {
  if (m.provider !== 'openai') return 'anthropic'
  if (m.thinking) return m.thinking
  const b = (m.baseUrl || '').toLowerCase()
  const n = (m.model || '').toLowerCase()
  if (b.includes('deepseek') || n.startsWith('deepseek')) return 'deepseek'
  if (b.includes('dashscope') || b.includes('aliyuncs') || n.startsWith('qwen') || n.startsWith('qwq')) return 'qwen'
  return 'effort'
}

/** 这个模型能选的思考强度 */
export function effortOptions(m: Pick<AiModel, 'provider' | 'thinking' | 'baseUrl' | 'model'>): { value: string; label: string }[] {
  const keys =
    {
      anthropic: ['none', 'low', 'medium', 'high', 'xhigh', 'max'],
      effort: ['none', 'low', 'medium', 'high'],
      deepseek: ['none', 'off', 'on'],
      qwen: ['none', 'off', 'low', 'medium', 'high'],
      none: ['none'],
    }[thinkingStyle(m)] ?? ['none']
  return keys.map((value) => ({ value, label: EFFORT_LABELS[value] }))
}

// ---------------------------------------------------------------- 用量与费用

export interface Usage {
  input: number
  cacheRead?: number
  cacheWrite?: number
  output: number
}

export function addUsage(a: Usage, b?: Usage): Usage {
  if (!b) return a
  return {
    input: a.input + b.input,
    cacheRead: (a.cacheRead ?? 0) + (b.cacheRead ?? 0),
    cacheWrite: (a.cacheWrite ?? 0) + (b.cacheWrite ?? 0),
    output: a.output + b.output,
  }
}

export const zeroUsage = (): Usage => ({ input: 0, cacheRead: 0, cacheWrite: 0, output: 0 })

export function tokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1000) return (n / 1000).toFixed(1) + 'k'
  return String(n)
}

/** 回复里记的模型名（如 deepseek-chat）对应设置里的哪个模型 */
export function findModelByName(models: AiModel[], name: string): AiModel | undefined {
  return models.find((m) => m.model === name) ?? models.find((m) => !m.model && m.provider === 'anthropic' && name.startsWith('claude-'))
}

/** 估算费用：没填单价时返回 undefined。命中缓存的输入没填单价时按输入单价算。 */
export function cost(m: AiModel | undefined, u: Usage): number | undefined {
  if (!m || (!m.priceIn && !m.priceOut)) return undefined
  const cache = m.priceCache || m.priceIn
  return (u.input * m.priceIn + (u.cacheRead ?? 0) * cache + (u.cacheWrite ?? 0) * m.priceIn + u.output * m.priceOut) / 1e6
}

export function money(v: number, currency: string): string {
  return currency + (v < 0.01 ? v.toFixed(4) : v.toFixed(2))
}

/** 一段用量的说明，如「输入 12.3k · 缓存 8k · 输出 1.2k tokens · 约 ¥0.02」 */
export function usageText(u: Usage, m?: AiModel): string {
  const parts = [`输入 ${tokens(u.input)}`]
  const cached = (u.cacheRead ?? 0) + (u.cacheWrite ?? 0)
  if (cached) parts.push(`缓存 ${tokens(cached)}`)
  parts.push(`输出 ${tokens(u.output)} tokens`)
  const c = cost(m, u)
  if (c !== undefined) parts.push('约 ' + money(c, m!.currency || '¥'))
  return parts.join(' · ')
}

/** 接口地址是 http（不加密）而且不是本机、内网：API Key 会明文传出去 */
export function plainHttpOutside(url: string): boolean {
  let u: URL
  try {
    u = new URL(url.trim())
  } catch {
    return false
  }
  if (u.protocol !== 'http:') return false
  const h = u.hostname.replace(/^\[|\]$/g, '').toLowerCase()
  if (h === 'localhost' || h.endsWith('.localhost') || h === '::1' || !h.includes('.')) return false
  const m = /^(\d+)\.(\d+)\.\d+\.\d+$/.exec(h)
  if (m) {
    const [a, b] = [Number(m[1]), Number(m[2])]
    if (a === 127 || a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) || (a === 169 && b === 254)) return false
  }
  return true
}

// ---------------------------------------------------------------- 图片

/** 能看图片的模型名里常见的字样（和 ferry 里 internal/ai 的 visionHints 一致） */
const VISION_HINTS = ['vl', 'vision', '4o', 'gpt-4.1', 'gpt-5', 'omni', 'qvq', 'gemini', 'claude', '4v', '5v', 'llava', 'pixtral']

/** 模型能不能看图片：设置里选了就按设置，没选时 Anthropic 都能，OpenAI 兼容接口按模型名识别 */
export function supportsImages(m: Pick<AiModel, 'provider' | 'model' | 'images'> | undefined): boolean {
  if (!m) return false
  if (m.images === 'on') return true
  if (m.images === 'off') return false
  if (m.provider !== 'openai') return true
  const n = (m.model || '').toLowerCase()
  return VISION_HINTS.some((k) => n.includes(k))
}

/** 附在消息里的一张图片（base64，不带 data: 前缀） */
export interface AiImage {
  media: string
  data: string
}

export const MAX_IMAGES = 8
const MAX_SIDE = 1568 // 长边超过这么多的缩小（再大模型也会先缩小，白花上传和 token）
const MAX_BYTES = 1.5 * 1024 * 1024

export const imageUrl = (im: AiImage) => `data:${im.media};base64,${im.data}`

/**
 * 把图片（粘贴的截图、选的文件）转成要发的格式：太大的缩小、转成 JPEG；小的 PNG、JPEG 原样用。
 * 读不了（不是图片）时抛出错误。
 */
export async function prepareImage(blob: Blob): Promise<AiImage> {
  // 用 data: 地址加载（桌面版的内容安全策略只允许 data: 图片，不允许 blob:）
  const dataUrl = await toDataUrl(blob)
  const img = new Image()
  img.src = dataUrl
  await img.decode().catch(() => {
    throw new Error('不是能识别的图片')
  })
  const w = img.naturalWidth
  const h = img.naturalHeight
  const scale = Math.min(1, MAX_SIDE / Math.max(w, h))
  const keep = ['image/png', 'image/jpeg', 'image/webp', 'image/gif'].includes(blob.type)
  if (keep && scale === 1 && blob.size <= MAX_BYTES) {
    return { media: blob.type, data: dataUrl.replace(/^data:[^,]*,/, '') }
  }
  const c = document.createElement('canvas')
  c.width = Math.max(1, Math.round(w * scale))
  c.height = Math.max(1, Math.round(h * scale))
  const ctx = c.getContext('2d')!
  // 透明的地方填白色（JPEG 没有透明）
  ctx.fillStyle = '#fff'
  ctx.fillRect(0, 0, c.width, c.height)
  ctx.drawImage(img, 0, 0, c.width, c.height)
  // 截图大多是文字、线条，PNG 更清楚；PNG 太大时才用 JPEG
  let out = await new Promise<Blob | null>((r) => c.toBlob(r, 'image/png'))
  if (!out || out.size > MAX_BYTES) out = await new Promise<Blob | null>((r) => c.toBlob(r, 'image/jpeg', 0.88))
  if (!out) throw new Error('图片转换失败')
  return { media: out.type, data: (await toDataUrl(out)).replace(/^data:[^,]*,/, '') }
}

function toDataUrl(b: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(String(r.result))
    r.onerror = () => reject(r.error ?? new Error('读取图片失败'))
    r.readAsDataURL(b)
  })
}

/** base64 的图片 → Blob（拖进来的文件由 ferry 读出来，再走一遍 prepareImage 缩小） */
export function base64Blob(im: AiImage): Blob {
  const bin = atob(im.data)
  const bytes = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
  return new Blob([bytes], { type: im.media })
}
