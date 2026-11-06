// 「软件」页连数据库、打开网页：用服务器地址直接连，还是经服务器（SSH 转发）连；各自连哪个地址、端口。
import type { Software, WebPanel } from './ferry'

export interface ListenAddr { host: string; port: number }

/** 监听地址（*:80、127.0.0.1:6379、[::1]:8080；UDP 的带 /udp，不算） */
export function tcpListens(sw: Software): ListenAddr[] {
  const out: ListenAddr[] = []
  for (const l of sw.listen ?? []) {
    if (l.includes('/')) continue
    const i = l.lastIndexOf(':')
    const port = Number(l.slice(i + 1))
    if (i < 0 || !(port > 0)) continue
    out.push({ host: l.slice(0, i).replace(/^\[|\]$/g, ''), port })
  }
  return out
}

export function isLoopback(host: string): boolean {
  return host.startsWith('127.') || host === '::1' || host === 'localhost'
}
const isWildcard = (h: string) => !h || h === '*' || h === '0.0.0.0' || h === '::'

/** 经服务器连时从服务器上看的地址：监听所有地址的用 127.0.0.1，只监听某个 IP 的用那个 IP */
export function sshHost(sw: Software, port: number): string {
  const l = tcpListens(sw).filter((a) => a.port === port)
  if (!l.length || l.some((a) => isWildcard(a.host) || a.host.startsWith('127.'))) return '127.0.0.1'
  return l.find((a) => a.host === '::1')?.host ?? l[0].host
}

export interface Endpoint {
  /** 用服务器地址直接连的端口；没有 = 直接连不上（why 说为什么） */
  direct?: number
  why?: string
  /** 经服务器连：从服务器上看的地址，或容器（端口没映射到主机时） */
  ssh: { host: string; port: number; container?: string }
}

/** 数据库：连哪个端口 */
export function dbEndpoint(sw: Software, defaultPort: number): Endpoint {
  const ls = tcpListens(sw)
  const l = ls.find((a) => a.port === defaultPort && !isLoopback(a.host)) ?? ls.find((a) => !isLoopback(a.host)) ?? ls[0]
  if (l) {
    if (isLoopback(l.host)) return { why: `只在服务器本机监听（${l.host}:${l.port}），这台电脑直接连不上`, ssh: { host: l.host === '::1' ? '::1' : '127.0.0.1', port: l.port } }
    return { direct: l.port, ssh: { host: sshHost(sw, l.port), port: l.port } }
  }
  const inner = sw.inner?.map((x) => Number(x.slice(x.lastIndexOf(':') + 1))).find((p) => p > 0)
  if (sw.container && inner) return { why: '端口没有映射到服务器上，只能经服务器连容器', ssh: { host: '', port: inner, container: sw.container } }
  return { direct: defaultPort, ssh: { host: '127.0.0.1', port: defaultPort } }
}

/** 网页：连哪个端口 */
export function webEndpoint(sw: Software, w: WebPanel): Endpoint {
  if (!w.port) {
    return { why: '端口没有映射到服务器上，只能经服务器连容器', ssh: { host: '', port: w.inner ?? 0, container: sw.container } }
  }
  if (w.local) return { why: '只在服务器本机监听（127.0.0.1），这台电脑直接连不上', ssh: { host: sshHost(sw, w.port), port: w.port } }
  return { direct: w.port, ssh: { host: sshHost(sw, w.port), port: w.port } }
}

/** 网址：http 的 80、https 的 443 不写端口；IPv6 加方括号 */
export function webUrl(w: Pick<WebPanel, 'scheme' | 'path'>, host: string, port: number): string {
  const h = host.includes(':') && !host.startsWith('[') ? `[${host}]` : host
  const p = (w.scheme === 'http' && port === 80) || (w.scheme === 'https' && port === 443) ? '' : `:${port}`
  const path = !w.path ? '/' : w.path.startsWith('/') ? w.path : '/' + w.path
  return `${w.scheme}://${h}${p}${path}`
}

/** 「更多」里的名字：打开控制台、打开面板；几个网页或网站时「打开网页」 */
export function webActionLabel(web: WebPanel[]): string {
  return web.length === 1 && !web[0].label.startsWith('网站') ? `打开${web[0].label}` : '打开网页'
}
