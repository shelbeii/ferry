import { describe, expect, it } from 'vitest'
import { appLocation, softwareLocations } from './fileLocations'
import type { Software } from './ferry'

const software = (fields: Partial<Software>): Software => ({ name: 'Redis', state: 'running', via: 'systemd', ...fields })

describe('查看服务器位置', () => {
  it('优先定位 Ferry 安装目录，并保留程序文件路径供服务器识别', () => {
    expect(softwareLocations(software({ dir: '/opt/redis', path: '/usr/bin/redis-server' }))).toEqual([{ label: '安装目录', path: '/opt/redis' }])
    expect(softwareLocations(software({ path: '/usr/bin/redis-server' }))).toEqual([{ label: '程序位置', path: '/usr/bin/redis-server' }])
    expect(softwareLocations(software({ path: '/opt/nacos' }))[0].path).toBe('/opt/nacos')
  })

  it('容器只使用主机位置，不能打开容器内程序路径或镜像名', () => {
    const row = software({ via: 'docker', image: 'redis:8', path: '/usr/bin/redis-server' })
    expect(softwareLocations(row)).toEqual([])
    row.locations = [{ label: '挂载 /data', path: '/srv/redis/data' }]
    expect(softwareLocations(row)).toEqual(row.locations)
  })

  it('未知或相对路径不猜测为主机路径', () => {
    expect(softwareLocations(software({}))).toEqual([])
    expect(softwareLocations(software({ path: 'redis-server' }))).toEqual([])
  })

  it('应用位置使用当前服务器的部署目录和应用单独设置的目录', () => {
    const target = { root: '/opt/shop/', appRoots: { api: '/data/服务/' } }
    expect(appLocation(target, 'web')).toBe('/opt/shop/apps/web')
    expect(appLocation(target, 'api')).toBe('/data/服务/apps/api')
    expect(appLocation({ root: '/' }, 'web')).toBe('/apps/web')
    expect(appLocation(undefined, 'api')).toBe('')
  })
})
