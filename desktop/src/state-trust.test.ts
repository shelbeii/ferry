import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { FerryResult } from './ferry'

// 只替换启动 ferry 的部分：每次调用记下参数、环境变量，结果按顺序取
const runs: { args: string[]; env: Record<string, string> }[] = []
const results: FerryResult<unknown>[] = []
vi.mock('./ferry', async (orig) => ({
  ...(await orig<typeof import('./ferry')>()),
  ferry: (args: string[], _onLine: unknown, env: Record<string, string> = {}) => {
    runs.push({ args, env })
    return { done: Promise.resolve(results.shift() ?? { ok: true }), kill: async () => {} }
  },
  call: async () => ({ removed: [] }),
}))
vi.stubGlobal('window', { setInterval: () => 0, clearInterval: () => {} })

const { startTask, trustRequest } = await import('./state')

const settle = () => new Promise((r) => setTimeout(r, 0))

describe('任务碰到没记录的主机公钥', () => {
  beforeEach(() => {
    runs.length = 0
    results.length = 0
    trustRequest.value = null
  })

  it('弹出核对，记录后用同样的参数和这次输入的密码接着做', async () => {
    results.push({ ok: false, error: '未知主机', detail: { kind: 'unknown-host', target: 'prod-a' } }, { ok: true, data: { localUpdated: true } })
    const done: string[] = []
    const env = { FERRY_PASSWORD_TUNNEL_41: 'pw' }
    startTask('配置免密登录：prod-a', ['keygen', 'prod-a', '-c', 'D:/p/ferry.yaml', '--local'], (t) => done.push(t.status), env)
    await settle()
    expect(done).toEqual(['failed'])
    expect(trustRequest.value).toMatchObject({ target: 'prod-a', config: 'D:/p/ferry.yaml' })
    expect(trustRequest.value?.retry).toBeTypeOf('function')

    trustRequest.value!.retry!()
    await settle()
    expect(runs).toHaveLength(2)
    expect(runs[1].args).toEqual(runs[0].args)
    expect(runs[1].env).toEqual(env)
    // 完成回调也接上了（对话框据此提示成功、页面刷新）
    expect(done).toEqual(['failed', 'ok'])
  })

  it('其他任务（部署）也一样接着做', async () => {
    results.push({ ok: false, error: '未知主机 1.2.3.4:22：先执行 ferry trust' })
    startTask('部署 prod：api', ['deploy', 'prod', 'api', '-c', 'D:/p/ferry.yaml'])
    await settle()
    expect(trustRequest.value?.target).toBe('prod')
    trustRequest.value!.retry!()
    await settle()
    expect(runs.map((r) => r.args[0])).toEqual(['deploy', 'deploy'])
  })
})
