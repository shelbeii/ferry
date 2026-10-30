import { describe, expect, it } from 'vitest'
import { filePermissions } from './filePermissions'

describe('文件权限的中文说明', () => {
  it('解释常见文件权限，并合并相同权限的同组用户和其他人', () => {
    const p = filePermissions({ mode: '-rw-r--r--', dir: false })
    expect(p.octal).toBe('644')
    expect(p.summary).toEqual([{ role: '所有者', text: '读写' }, { role: '同组及其他人', text: '只读' }])
  })
  it('文件夹的 x 表示进入，文件的 x 表示执行', () => {
    expect(filePermissions({ mode: 'drwxr-xr-x', dir: true }).roles[1].text).toBe('读取、进入')
    expect(filePermissions({ mode: '-rwxr-xr-x', dir: false }).roles[1].text).toBe('读取、执行')
  })
  it('私有、完全开放和三类人权限不同的情况不混淆', () => {
    expect(filePermissions({ mode: '-rw-------', dir: false }).summary[1].text).toBe('无权限')
    expect(filePermissions({ mode: 'drwxrwxrwx', dir: true }).summary).toEqual([{ role: '所有人', text: '读写、进入' }])
    const p = filePermissions({ mode: '-rwxr-x---', dir: false })
    expect(p.summary).toEqual(p.roles)
    expect(p.roles.map((r) => r.text)).toEqual(['读写、执行', '读取、执行', '无权限'])
  })
  it('识别 Go 格式的多字符前缀及特殊权限', () => {
    const p = filePermissions({ mode: 'dugtrwxrwxrwx', dir: true })
    expect(p.octal).toBe('7777')
    expect(p.special).toHaveLength(3)
    expect(filePermissions({ mode: 'urwxr-xr-x', dir: false }).octal).toBe('4755')
    expect(filePermissions({ mode: 'dtrwxrwxrwx', dir: true }).octal).toBe('1777')
  })
  it('符号链接不把自身权限当作目标的实际权限', () => {
    for (const dir of [true, false]) {
      const p = filePermissions({ mode: 'Lrwxrwxrwx', dir, link: true })
      expect(p.summary[0].text).toBe('以目标权限为准')
      expect(p.roles).toEqual([])
      expect(p.octal).toBeUndefined()
    }
  })
  it.each(['', 'unknown', 'drwxr-x', '-rwxr-zr-x'])('未知格式保留为未知，不猜测 %j', (mode) => {
    expect(filePermissions({ mode, dir: false }).summary[0].text).toBe('权限信息未知')
  })
})
