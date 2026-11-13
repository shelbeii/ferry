import { describe, expect, it, vi } from 'vitest'
import type { FerryLine } from './ferry'

// 部署中途 ferry 问用户（kind=ask）：弹窗「继续部署 / 停下」，回答交回 ferry
let onLine: ((l: FerryLine) => void) | undefined
const answers: boolean[] = []
let finish: (() => void) | undefined
vi.mock('./ferry', async (orig) => ({
  ...(await orig<typeof import('./ferry')>()),
  ferry: (_args: string[], cb: (l: FerryLine) => void) => {
    onLine = cb
    return {
      done: new Promise((r) => (finish = () => r({ ok: true }))),
      kill: async () => {},
      answer: (yes: boolean) => answers.push(yes),
    }
  },
  call: async () => ({ removed: [] }),
}))
const confirm = vi.fn()
vi.mock('element-plus', async (orig) => ({
  ...(await orig<typeof import('element-plus')>()),
  ElMessageBox: { confirm: (...a: unknown[]) => confirm(...a) },
}))
vi.stubGlobal('window', { setInterval: () => 0, clearInterval: () => {} })

const { startTask } = await import('./state')
const settle = () => new Promise((r) => setTimeout(r, 0))

describe('部署中途问用户', () => {
  it('弹窗，点「继续部署」交回 yes、点「停下」交回 no', async () => {
    const task = startTask('部署 a、b：api', ['deploy', 'a,b', 'api', '-c', 'D:/p/ferry.yaml', '--canary', '1'])
    confirm.mockResolvedValueOnce('confirm')
    onLine!({ kind: 'ask', msg: '先发的 a 已上线。继续部署剩下的 b？' })
    await settle()
    expect(confirm).toHaveBeenCalledWith('先发的 a 已上线。继续部署剩下的 b？', '继续部署？', expect.objectContaining({ confirmButtonText: '继续部署', cancelButtonText: '停下' }))
    expect(answers).toEqual([true])
    expect(task.step?.text).toBe('等你确认：继续部署剩下的？')
    expect(task.lines.at(-1)?.msg).toContain('继续部署剩下的 b？')

    confirm.mockRejectedValueOnce('cancel')
    onLine!({ kind: 'ask', msg: '再问一次？' })
    await settle()
    expect(answers).toEqual([true, false])
    finish!()
  })
})
