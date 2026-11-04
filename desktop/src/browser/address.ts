// 内置浏览器地址栏：把输入变成网址。像浏览器一样：省略 http(s):// 也能打开，不像网址的就搜索。
import { ref } from 'vue'

const SEARCH = 'https://www.bing.com/search?q='

/** 本机、内网常用的写法（localhost、IP、不带点的主机名加端口）默认 http，域名默认 https */
export function toUrl(input: string): string {
  const s = input.trim()
  if (!s) return ''
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(s) || /^about:/i.test(s)) return s
  const m = /^([^\s/?#]+)([/?#]\S*)?$/.exec(s)
  if (m) {
    const host = m[1]
    const port = /:\d{1,5}$/.test(host)
    const name = host.replace(/:\d{1,5}$/, '')
    if (/^(localhost|\d{1,3}(\.\d{1,3}){3}|\[[0-9a-f:.]+\])$/i.test(name)) return 'http://' + s
    if (/^[a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,}$/i.test(name)) return 'https://' + s
    if (port && /^[a-z0-9-]+$/i.test(name)) return 'http://' + s
  }
  return SEARCH + encodeURIComponent(s)
}

export interface Visit {
  url: string
  title: string
  /** 打开的时间（毫秒）；旧记录没有 */
  at?: number
}

const HISTORY = 'ferry.browserHistory'
const KEEP = 30

/** 网址里显示的站点名：去掉 www. */
export function siteOf(url: string): string {
  try {
    return new URL(url).host.replace(/^www\./, '')
  } catch {
    return url
  }
}

/** 值得记的访问：http(s) 网页，有自己的标题。跳转页（如 baidu.com/link?url=…）、还没加载出标题的不记：
 *  网页没有标题时 WebView2 拿去掉 https:// 的网址当标题 */
function meaningful(v: Visit): boolean {
  const title = v.title.trim().replace(/^www\./, '')
  if (!/^https?:\/\//i.test(v.url) || !title || /^https?:\/\//i.test(title)) return false
  const bare = v.url.replace(/^https?:\/\//i, '').replace(/^www\./, '')
  return !bare.startsWith(title) && !title.startsWith(bare.split('/')[0] + '/')
}

/** 同一网址、或同一站点同样标题（同一搜索换了参数等）只留最新的一条 */
const key = (v: Visit) => `${siteOf(v.url)}\n${v.title.trim()}`

/** 整理记录：去掉不值得记的，合并重复的，最多留 KEEP 条（新的在前） */
export function cleanHistory(list: Visit[]): Visit[] {
  const urls = new Set<string>()
  const keys = new Set<string>()
  const out: Visit[] = []
  for (const v of list) {
    if (!v || typeof v.url !== 'string' || typeof v.title !== 'string' || !meaningful(v)) continue
    if (urls.has(v.url) || keys.has(key(v))) continue
    urls.add(v.url)
    keys.add(key(v))
    out.push({ url: v.url, title: v.title.trim(), ...(v.at ? { at: v.at } : {}) })
    if (out.length >= KEEP) break
  }
  return out
}

/** 起始页上的时间：刚刚、几分钟前、今天 / 昨天 几点、更早的写日期 */
export function visitTime(at: number | undefined, now = Date.now()): string {
  if (!at) return ''
  const p = (n: number) => String(n).padStart(2, '0')
  const d = new Date(at)
  const mins = Math.floor((now - at) / 60000)
  if (mins < 1) return '刚刚'
  if (mins < 60) return `${mins} 分钟前`
  const day = (t: Date) => new Date(t.getFullYear(), t.getMonth(), t.getDate()).getTime()
  const days = Math.round((day(new Date(now)) - day(d)) / 86400000)
  const hm = `${p(d.getHours())}:${p(d.getMinutes())}`
  if (days === 0) return `今天 ${hm}`
  if (days === 1) return `昨天 ${hm}`
  if (d.getFullYear() === new Date(now).getFullYear()) return `${d.getMonth() + 1}月${d.getDate()}日`
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

function load(): Visit[] {
  try {
    const v = JSON.parse(localStorage.getItem(HISTORY) ?? '[]')
    return Array.isArray(v) ? cleanHistory(v) : []
  } catch {
    return []
  }
}

/** 最近打开的网页（起始页上列出来），只记在本机 */
export const history = ref<Visit[]>(load())

function save() {
  try {
    if (history.value.length) localStorage.setItem(HISTORY, JSON.stringify(history.value))
    else localStorage.removeItem(HISTORY)
  } catch {
    // 存不了就算了
  }
}

/** 记一条访问（排到最前）；不值得记的不记 */
export function addVisit(v: Visit) {
  const next = cleanHistory([{ ...v, at: v.at ?? Date.now() }, ...history.value])
  if (JSON.stringify(next) === JSON.stringify(history.value)) return
  history.value = next
  save()
}

export function removeVisit(url: string) {
  history.value = history.value.filter((x) => x.url !== url)
  save()
}

export function clearHistory() {
  history.value = []
  save()
}
