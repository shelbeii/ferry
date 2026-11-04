import { afterEach, describe, expect, it, vi } from 'vitest'
import { computed, effectScope, ref } from 'vue'
import { FerryError, type FerryResult, type Running } from './ferry'
import { readCall, ReadCanceled, useReadRequests } from './readRequests'

const { spawn } = vi.hoisted(() => ({ spawn: vi.fn() }))
vi.mock('./ferry', async (original) => ({ ...await original<typeof import('./ferry')>(), ferry: spawn }))

function deferred<T>() {
  let resolve!: (result: T) => void
  let reject!: (error: Error) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

function command<T>() {
  const result = deferred<FerryResult<T>>()
  // 模拟后台连 cancel 都未收尾，页面也应能立即解除等待。
  const kill = vi.fn(() => new Promise<void>(() => undefined))
  const proc: Running<T> = { done: result.promise, kill }
  spawn.mockReturnValueOnce(proc)
  return { ...result, kill }
}

afterEach(() => { vi.useRealTimers(); vi.clearAllMocks() })

describe('连接页面的只读请求', () => {
  it('组件销毁时自动取消后台读取，不等待后台取消收尾', async () => {
    const scope = effectScope()
    const reads = scope.run(useReadRequests)!
    const proc = command<string>()
    const pending = reads.run('details', s => readCall(['app-info', '162', 'api'], s))
    const canceled = expect(pending).rejects.toBeInstanceOf(ReadCanceled)
    await Promise.resolve()
    scope.stop()
    await canceled
    expect(proc.kill).toHaveBeenCalledOnce()
    expect(reads.loading.value).toEqual({})
  })
  it('162 刷新卡住时，切换到已缓存的 41 不继承刷新遮罩或等待状态', async () => {
    const reads = useReadRequests()
    const selected = ref('162')
    const refreshing = computed(() => !!reads.loading.value[selected.value])
    const old = command<string>()
    const cached = { '41': '41 的已有数据', '162': '162 的已有数据' }
    const pending = reads.run('162', async (signal) => { cached['162'] = await readCall(['status', '162'], signal) })
    const canceled = expect(pending).rejects.toBeInstanceOf(ReadCanceled)
    await Promise.resolve()
    expect(refreshing.value).toBe(true)
    selected.value = '41'
    reads.cancel('162')
    expect(refreshing.value).toBe(false)
    expect(cached['41']).toBe('41 的已有数据')
    await canceled
    expect(old.kill).toHaveBeenCalledOnce()
    old.resolve({ ok: true, data: '迟到的162数据' })
    await Promise.resolve()
    expect(cached['162']).toBe('162 的已有数据')
  })

  it('同一连接的重复读取共用请求，不叠加加载计数', async () => {
    const reads = useReadRequests()
    const proc = command<string>()
    const first = reads.run('41', (s) => readCall<string>(['status', '41'], s))
    const next = reads.run('41', () => { throw new Error('不应重复启动') })
    expect(next).toBe(first)
    await Promise.resolve()
    expect(spawn).toHaveBeenCalledOnce()
    proc.resolve({ ok: true, data: '数据' })
    await expect(first).resolves.toBe('数据')
    expect(reads.loading.value).toEqual({})
  })

  it('超时后停止等待、取消该读取并允许重试，旧结果不能覆盖新结果', async () => {
    vi.useFakeTimers()
    const reads = useReadRequests()
    let cached = '已有数据'
    const old = command<string>()
    const run = () => reads.run('162', async (s) => { cached = await readCall<string>(['status', '162'], s) })
    const pending = run()
    const failed = expect(pending).rejects.toThrow('读取超时（60 秒）')
    await vi.advanceTimersByTimeAsync(60_000)
    await failed
    expect(old.kill).toHaveBeenCalledOnce()
    expect(reads.loading.value).toEqual({})
    expect(cached).toBe('已有数据')
    const fresh = command<string>()
    const retry = run()
    await Promise.resolve()
    old.resolve({ ok: true, data: '旧结果' })
    fresh.resolve({ ok: true, data: '新结果' })
    await retry
    expect(cached).toBe('新结果')
  })

  it('旧请求收尾不能清除同一连接新请求的加载状态', async () => {
    const reads = useReadRequests()
    const old = deferred<string>()
    const pending = reads.run('162', () => old.promise)
    const canceled = expect(pending).rejects.toBeInstanceOf(ReadCanceled)
    reads.cancel('162')
    const fresh = deferred<string>()
    const next = reads.run('162', () => fresh.promise)
    await canceled
    expect(reads.loading.value['162']).toBe(true)
    old.resolve('旧数据')
    await Promise.resolve()
    expect(reads.loading.value['162']).toBe(true)
    fresh.resolve('新数据')
    await expect(next).resolves.toBe('新数据')
  })

  it('父刷新取消时同时取消状态和面板读取，其他连接的请求继续', async () => {
    const refresh = useReadRequests()
    const status = useReadRequests()
    const panel = useReadRequests()
    const parent = refresh.run('162', (signal) => Promise.all([
      status.run('162', (s) => readCall(['status', '162'], s), { signal }),
      panel.run('162', (s) => readCall(['software', '162'], s), { signal }),
    ]))
    const canceled = expect(parent).rejects.toBeInstanceOf(ReadCanceled)
    const statusProc = command<string>()
    const panelProc = command<string>()
    await Promise.resolve()
    await Promise.resolve()
    const other = deferred<string>()
    const pendingOther = status.run('41', () => other.promise)
    refresh.cancel('162')
    await canceled
    expect(statusProc.kill).toHaveBeenCalledOnce()
    expect(panelProc.kill).toHaveBeenCalledOnce()
    expect(status.loading.value['41']).toBe(true)
    other.resolve('41')
    await pendingOther
  })

  it('请求失败保留认证的结构化错误；结束后清理超时定时器', async () => {
    vi.useFakeTimers()
    const reads = useReadRequests()
    const proc = command<string>()
    const pending = reads.run('41', (s) => readCall(['status', '41'], s))
    await Promise.resolve()
    proc.resolve({ ok: false, error: '需要密码', detail: { kind: 'password-required', target: '41' } })
    await expect(pending).rejects.toBeInstanceOf(FerryError)
    await expect(pending).rejects.toMatchObject({ name: 'Error', message: '需要密码', detail: { kind: 'password-required', target: '41' } })
    expect(reads.loading.value).toEqual({})
    expect(vi.getTimerCount()).toBe(0)
    expect(proc.kill).not.toHaveBeenCalled()
  })

  it('离开项目时取消全部读取，即使后台没有任何响应', async () => {
    const reads = useReadRequests()
    const a = reads.run('162', () => new Promise(() => undefined))
    const b = reads.run('41', () => new Promise(() => undefined))
    const results = Promise.allSettled([a, b])
    reads.cancelAll()
    expect(reads.loading.value).toEqual({})
    expect((await results).every((r) => r.status === 'rejected' && r.reason instanceof ReadCanceled)).toBe(true)
  })

  it('已取消的读取不发送后台命令', async () => {
    const reads = useReadRequests()
    const controller = new AbortController()
    controller.abort()
    await expect(reads.run('162', (s) => readCall(['status', '162'], s), { signal: controller.signal })).rejects.toBeInstanceOf(ReadCanceled)
    const pending = reads.run('162', (s) => readCall(['status', '162'], s))
    const canceled = expect(pending).rejects.toBeInstanceOf(ReadCanceled)
    reads.cancel('162')
    await canceled
    expect(spawn).not.toHaveBeenCalled()
  })
})
