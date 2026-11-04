import { describe, expect, it, vi } from 'vitest'
import { saveDraft } from './saveDraft'

describe('配置保存冲突与快照', () => {
  it('运行环境切换等操作修改了磁盘文件后，不覆盖其改动', async () => {
    const write = vi.fn()
    await expect(saveDraft('java: old\n', 'java: draft\n', { read: async () => 'java: switched\n', write })).rejects.toThrow('本次未保存')
    expect(write).not.toHaveBeenCalled()
  })
  it('保存期间继续输入时，保存基线只包含真正写入的内容，并保持 CRLF', async () => {
    let text = 'project: draft\n'
    let finish!: () => void
    let written = ''
    const pending = saveDraft('project: old\n', text.replace(/\n/g, '\r\n'), {
      read: async () => 'project: old\r\n',
      write: async (content) => { written = content; await new Promise<void>((resolve) => { finish = resolve }) },
    })
    await Promise.resolve()
    text += 'version: 1\n'
    finish()
    const baseline = await pending
    expect(written).toBe('project: draft\r\n')
    expect(baseline).toBe('project: draft\n')
    expect(text).not.toBe(baseline)
  })
})
