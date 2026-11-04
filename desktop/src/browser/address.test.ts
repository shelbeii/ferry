import { describe, expect, it } from 'vitest'
import { toUrl, cleanHistory, visitTime, siteOf } from './address'

describe('toUrl', () => {
  it('带协议的原样打开', () => {
    expect(toUrl(' https://example.com/a ')).toBe('https://example.com/a')
    expect(toUrl('http://10.0.0.1:8080')).toBe('http://10.0.0.1:8080')
  })
  it('本机、IP、主机名加端口用 http', () => {
    expect(toUrl('localhost:5173')).toBe('http://localhost:5173')
    expect(toUrl('192.168.1.10:8080/api?a=1')).toBe('http://192.168.1.10:8080/api?a=1')
    expect(toUrl('devbox:3000')).toBe('http://devbox:3000')
  })
  it('域名用 https', () => {
    expect(toUrl('example.com')).toBe('https://example.com')
    expect(toUrl('docs.example.com/path#x')).toBe('https://docs.example.com/path#x')
  })
  it('其他的搜索', () => {
    expect(toUrl('nginx 配置')).toBe('https://www.bing.com/search?q=' + encodeURIComponent('nginx 配置'))
    expect(toUrl('hello')).toBe('https://www.bing.com/search?q=hello')
  })
  it('空的不打开', () => {
    expect(toUrl('   ')).toBe('')
  })
})

describe('cleanHistory', () => {
  it('不记没有标题的、跳转页、标题就是网址的', () => {
    expect(
      cleanHistory([
        { url: 'https://www.baidu.com/link?url=abc', title: '' },
        { url: 'https://a.com/', title: 'https://a.com/' },
        { url: 'about:blank', title: 'blank' },
        { url: 'https://www.baidu.com/link?url=0t02kaPZ', title: 'baidu.com/link?url=0t02kaPZ' },
        { url: 'https://www.baidu.com/link?url=xCNthexh0eEiV8ZzJGNOKy26Rn5K', title: 'www.baidu.com/link?url=xCNthexh0e' },
        { url: 'https://example.com/', title: 'example.com' },
        { url: 'https://github.com/x', title: ' GitHub ' },
      ]),
    ).toEqual([{ url: 'https://github.com/x', title: 'GitHub' }])
  })
  it('同一网址、同一站点同样标题只留最新的', () => {
    const out = cleanHistory([
      { url: 'https://cn.bing.com/search?q=a&form=1', title: 'a - 搜索', at: 3 },
      { url: 'https://cn.bing.com/search?q=a', title: 'a - 搜索', at: 2 },
      { url: 'https://github.com/', title: 'GitHub', at: 1 },
      { url: 'https://github.com/', title: 'GitHub 旧', at: 0 },
    ])
    expect(out.map((v) => v.at)).toEqual([3, 1])
  })
})

describe('visitTime', () => {
  const now = new Date(2026, 10, 4, 15, 30).getTime()
  it('按远近显示', () => {
    expect(visitTime(undefined, now)).toBe('')
    expect(visitTime(now - 20_000, now)).toBe('刚刚')
    expect(visitTime(now - 5 * 60_000, now)).toBe('5 分钟前')
    expect(visitTime(new Date(2026, 10, 4, 9, 5).getTime(), now)).toBe('今天 09:05')
    expect(visitTime(new Date(2026, 10, 3, 22, 0).getTime(), now)).toBe('昨天 22:00')
    expect(visitTime(new Date(2026, 9, 1).getTime(), now)).toBe('10月1日')
    expect(visitTime(new Date(2025, 0, 2).getTime(), now)).toBe('2025-01-02')
  })
})

describe('siteOf', () => {
  it('去掉 www.', () => {
    expect(siteOf('https://www.baidu.com/s?wd=1')).toBe('baidu.com')
    expect(siteOf('http://192.168.1.10:8080/a')).toBe('192.168.1.10:8080')
  })
})
