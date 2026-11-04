import { describe, expect, it, vi } from 'vitest'
import { completeRequest, mergeRetries, queuedRequest, requestKey } from './requestQueue'

describe('并发任务的确认请求', () => {
  it('相同登录请求只询问一次，所有任务都重试；重试失败的新请求仍独立排队', () => {
    const request = queuedRequest<{ target: string; retry: () => void }>({ key: r => r.target, merge: mergeRetries })
    const a = vi.fn(), b = vi.fn()
    const first = { target: '41', retry: a }
    request.value = first
    request.value = { target: '41', retry: b }
    expect(request.value).toBe(first)
    completeRequest(first)
    request.value!.retry()
    expect(a).toHaveBeenCalledOnce()
    expect(b).toHaveBeenCalledOnce()
    const failedAgain = { target: '41', retry: vi.fn() }
    request.value = failedAgain
    request.value = null
    expect(request.value).toBe(failedAgain)
  })
  it('同一台服务器连续请求也会创建不同的弹窗实例，避免沿用已关闭状态或旧密码', () => {
    const first = { target: '41' }, second = { target: '41' }
    expect(requestKey(first)).toBe(requestKey(first))
    expect(requestKey(second)).not.toBe(requestKey(first))
  })
  it('41 与 162 同时需要密码时，保留各自的目标与重试回调', () => {
    const request = queuedRequest<{ target: string; retry: () => void }>()
    const first = { target: '162', retry: vi.fn() }, second = { target: '41', retry: vi.fn() }
    request.value = first
    request.value = second
    expect(request.value).toBe(first)
    request.value!.retry()
    request.value = null
    expect(request.value).toBe(second)
    expect(first.retry).toHaveBeenCalledOnce()
    expect(second.retry).not.toHaveBeenCalled()
    request.value!.retry()
    request.value = null
    expect(request.value).toBeNull()
    expect(second.retry).toHaveBeenCalledOnce()
  })
  it('取消第一项保留后续项，重试过程中到达的新请求不会被关闭事件清掉', () => {
    const request = queuedRequest<{ id: number }>()
    request.value = { id: 1 }
    request.value = { id: 2 }
    request.value = null
    expect(request.value).toEqual({ id: 2 })
    request.value = { id: 3 }
    request.value = null
    expect(request.value).toEqual({ id: 3 })
    request.value = null
    request.value = null
    expect(request.value).toBeNull()
  })
})
