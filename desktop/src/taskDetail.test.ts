import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Task } from './state'

vi.mock('./ferry', async (original) => ({ ...(await original<typeof import('./ferry')>()), call: vi.fn() }))
const { call } = await import('./ferry')
const { loadTaskDetail } = await import('./state')
const mockedCall = vi.mocked(call)
const record = (): Task => ({ id: 1, hid: 'saved-task', title: '部署 prod：api', status: 'failed', error: '应用启动超时', lines: [], startedAt: 0, lastAt: 0, loaded: false })

describe('历史任务输出读取', () => {
  beforeEach(() => mockedCall.mockReset())

  it('读取失败保留原任务错误，并允许重试恢复输出', async () => {
    const task = record()
    mockedCall.mockRejectedValueOnce(new Error('暂时不可用'))
    await loadTaskDetail(task)
    expect(task.error).toBe('应用启动超时')
    expect(task.loaded).toBe(false)
    expect(task.detailLoadError).toContain('暂时不可用')

    mockedCall.mockResolvedValueOnce({ lines: [{ kind: 'error', msg: '健康检查超时' }] })
    await loadTaskDetail(task)
    expect(task.loaded).toBe(true)
    expect(task.detailLoadError).toBe('')
    expect(task.lines[0].msg).toBe('健康检查超时')
    expect(task.error).toBe('应用启动超时')
  })

  it('加载中重复选中不会重复请求，已加载记录复用结果', async () => {
    const task = record()
    let finish!: (value: unknown) => void
    mockedCall.mockImplementationOnce(() => new Promise((resolve) => { finish = resolve }) as ReturnType<typeof call>)
    const first = loadTaskDetail(task)
    await loadTaskDetail(task)
    expect(mockedCall).toHaveBeenCalledTimes(1)
    finish({ lines: [] })
    await first
    await loadTaskDetail(task)
    expect(mockedCall).toHaveBeenCalledTimes(1)
    expect(task.loadingDetail).toBe(false)
  })
})
