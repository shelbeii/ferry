import { describe, expect, it } from 'vitest'
import { validRemoteName } from './fileNames'

describe('远端文件名', () => {
  it.each(['', '   ', '.', '..', ' .. ', 'logs/error.log', 'a\0b'])('拒绝目录跳转或无效名字 %j', (name) => {
    expect(validRemoteName(name)).toBe(false)
  })
  it.each(['.env', '应用日志', 'my folder', 'archive.tar.gz'])('保留有效文件名 %j', (name) => {
    expect(validRemoteName(name)).toBe(true)
  })
})
