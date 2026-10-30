import { describe, expect, it } from 'vitest'
import type { ServerInfo, ServerMount } from './ferry'
import { capacity, percentText, serverInfoCards, uptimeText, usagePercent, visibleMounts } from './serverinfo'

const mount = (path: string, temporary: boolean, usage = 50): ServerMount => ({ path, temporary, usage, device: '/dev/vda1', fsType: temporary ? 'tmpfs' : 'xfs', total: 100, used: 50, available: 50, inodes: 1000, inodesUsed: 100, inodesUsage: 10 })

describe('服务器资源展示', () => {
  it('缺失指标用占位符、零使用率正常显示', () => {
    expect(percentText(null)).toBe('—')
    expect(percentText(0)).toBe('0.0%')
    expect(usagePercent(0, 0)).toBeUndefined()
    expect(usagePercent(0, 1024)).toBe(0)
    expect(serverInfoCards().every((c) => c.num === '—')).toBe(true)
  })

  it('临时文件系统可切换，根目录与同一设备的多个挂载点保留', () => {
    const mounts = [mount('/', true), mount('/run', true), mount('/data', false), mount('/srv/bind', false)]
    expect(visibleMounts(mounts, false).map((m) => m.path)).toEqual(['/', '/data', '/srv/bind'])
    expect(visibleMounts(mounts, true)).toHaveLength(4)
  })

  it('概览取最高磁盘使用率，不加总挂载容量；无 Swap 显示未启用', () => {
    const r = { cpu: { cores: 4, usage: 0 }, memory: { total: 8192, used: 2048 }, swap: { total: 0, used: 0 }, mounts: [mount('/', false, 40), mount('/data', false, 95), mount('/run', true, 100)] } as ServerInfo
    const cards = serverInfoCards(r)
    expect(cards.map((c) => c.num)).toEqual(['0.0%', '25.0%', '未启用', '95.0%'])
    expect(cards[3].sub).toBe('/data')
    expect(cards[3].cls).toBe('bad')
  })

  it('容量使用二进制单位，支持大容量磁盘与较长运行时间', () => {
    expect(capacity(2 ** 40)).toBe('1.00 TiB')
    expect(capacity(2 ** 30)).toBe('1.00 GiB')
    expect(uptimeText(90061)).toBe('1 天 1 小时')
  })
})
