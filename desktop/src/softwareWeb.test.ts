import { describe, expect, it } from 'vitest'
import type { Software } from './ferry'
import { dbEndpoint, sshHost, webActionLabel, webEndpoint, webUrl } from './softwareWeb'

const sw = (p: Partial<Software>): Software => ({ name: 'x', state: 'running', via: 'systemd', ...p })

describe('dbEndpoint', () => {
  it('监听所有地址：直接连服务器的端口，经服务器连 127.0.0.1', () => {
    expect(dbEndpoint(sw({ listen: ['*:3307'] }), 3306)).toEqual({ direct: 3307, ssh: { host: '127.0.0.1', port: 3307 } })
  })
  it('只在本机监听：不能直接连', () => {
    const e = dbEndpoint(sw({ listen: ['127.0.0.1:6379'] }), 6379)
    expect(e.direct).toBeUndefined()
    expect(e.why).toContain('本机')
    expect(e.ssh).toEqual({ host: '127.0.0.1', port: 6379 })
  })
  it('既有对外的又有本机的：用对外的', () => {
    expect(dbEndpoint(sw({ listen: ['127.0.0.1:33060', '*:3306'] }), 3306).direct).toBe(3306)
  })
  it('只在容器里：经服务器连容器', () => {
    const e = dbEndpoint(sw({ via: 'docker', container: 'pg', inner: ['*:5432'] }), 5432)
    expect(e.direct).toBeUndefined()
    expect(e.ssh).toEqual({ host: '', port: 5432, container: 'pg' })
  })
  it('不知道端口：用默认端口', () => {
    expect(dbEndpoint(sw({}), 5432)).toEqual({ direct: 5432, ssh: { host: '127.0.0.1', port: 5432 } })
  })
})

describe('webEndpoint', () => {
  it('对外的、本机的、容器里的', () => {
    expect(webEndpoint(sw({ listen: ['*:8848'] }), { label: '控制台', scheme: 'http', port: 8848 }).direct).toBe(8848)
    expect(webEndpoint(sw({ listen: ['127.0.0.1:9001'] }), { label: '控制台', scheme: 'http', port: 9001, local: true }).direct).toBeUndefined()
    const c = webEndpoint(sw({ container: 'nacos' }), { label: '控制台', scheme: 'http', inner: 8080 })
    expect(c.ssh).toEqual({ host: '', port: 8080, container: 'nacos' })
  })
  it('只监听某个 IP：经服务器时连那个 IP', () => {
    expect(sshHost(sw({ listen: ['10.0.0.5:8080'] }), 8080)).toBe('10.0.0.5')
    expect(sshHost(sw({ listen: ['[::1]:8080'] }), 8080)).toBe('::1')
  })
})

describe('webUrl', () => {
  it('拼网址', () => {
    expect(webUrl({ scheme: 'http', path: '/nacos' }, '203.0.113.10', 8848)).toBe('http://203.0.113.10:8848/nacos')
    expect(webUrl({ scheme: 'http' }, 'a.example.com', 80)).toBe('http://a.example.com/')
    expect(webUrl({ scheme: 'https', path: 'ab12' }, 'fe80::1', 10001)).toBe('https://[fe80::1]:10001/ab12')
  })
  it('「更多」里的名字', () => {
    expect(webActionLabel([{ label: '控制台', scheme: 'http' }])).toBe('打开控制台')
    expect(webActionLabel([{ label: '网站 :80', scheme: 'http' }])).toBe('打开网页')
    expect(webActionLabel([{ label: '面板', scheme: 'http' }, { label: 'b', scheme: 'http' }])).toBe('打开网页')
  })
})
