import type { ServerInfo, ServerMount } from './ferry'

export const usagePercent = (used: number, total: number): number | undefined => total > 0 ? Math.min(100, Math.max(0, used / total * 100)) : undefined
export const percentText = (value: number | null | undefined): string => value == null ? '—' : `${value.toFixed(1)}%`
export const usageClass = (value: number | null | undefined): string => value == null ? 'off' : value >= 90 ? 'bad' : value >= 80 ? 'warn' : 'ok'

export function capacity(n: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++ }
  return `${n.toFixed(i > 1 ? 2 : 0)} ${units[i]}`
}

export function uptimeText(seconds: number): string {
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor(seconds % 86400 / 3600)
  const minutes = Math.floor(seconds % 3600 / 60)
  return days ? `${days} 天 ${hours} 小时` : hours ? `${hours} 小时 ${minutes} 分钟` : `${minutes} 分钟`
}

export function visibleMounts(mounts: ServerMount[], temporary: boolean): ServerMount[] {
  return mounts.filter((m) => temporary || !m.temporary || m.path === '/')
}

export function serverInfoCards(r?: ServerInfo) {
  const mem = r ? usagePercent(r.memory.used, r.memory.total) : undefined
  const swap = r ? usagePercent(r.swap.used, r.swap.total) : undefined
  const disks = r ? visibleMounts(r.mounts, false).filter((m) => m.usage != null) : []
  const highest = disks.reduce<ServerMount | undefined>((a, b) => !a || b.usage! > a.usage! ? b : a, undefined)
  return [
    { label: 'CPU 使用率', num: percentText(r?.cpu.usage), cls: usageClass(r?.cpu.usage), sub: r?.cpu.cores ? `${r.cpu.cores} 个逻辑 CPU · 约 1 秒采样` : 'CPU 信息未读取' },
    { label: '内存使用率', num: percentText(mem), cls: usageClass(mem), sub: r?.memory.total ? `${capacity(r.memory.used)} / ${capacity(r.memory.total)}` : '内存信息未读取' },
    { label: 'Swap 使用率', num: r?.memory.total && !r.swap.total ? '未启用' : percentText(swap), cls: usageClass(swap), sub: r?.swap.total ? `${capacity(r.swap.used)} / ${capacity(r.swap.total)}` : '交换空间' },
    { label: '磁盘最高使用率', num: percentText(highest?.usage), cls: usageClass(highest?.usage), sub: highest?.path ?? '挂载点信息未读取', title: highest?.path },
  ]
}
