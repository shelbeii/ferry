import { beforeEach, describe, expect, it, vi } from 'vitest'
import { reactive, ref } from 'vue'
import { useAiAccess } from './aiAccess'

const storage = vi.hoisted(() => new Map<string, string>())
vi.mock('./state', () => ({
  uiPref: (key: string) => storage.get(key) ?? null,
  setUiPref: (key: string, value: string) => storage.set(key, value),
}))

const target = () => reactive({
  config: 'D:/project/ferry.yaml',
  target: 'server',
  server: { host: 'test-server', port: 22, user: 'tester', hostKey: '' },
  production: false,
  noChanges: false,
})

describe('AI 助手开关偏好', () => {
  beforeEach(() => storage.clear())

  it('按服务器保存两个自动确认等级，忽略旧版没有的值和未知选项', () => {
    const server = target()
    const access = useAiAccess(server, () => true)
    expect(access.autoConfirm.value).toEqual([])
    access.autoConfirm.value = ['danger', 'warning', 'danger']
    expect(useAiAccess(server, () => true).autoConfirm.value).toEqual(['warning', 'danger'])

    server.target = 'another'
    expect(access.autoConfirm.value).toEqual([])
    server.target = 'server'
    expect(access.autoConfirm.value).toEqual(['warning', 'danger'])
    access.autoConfirm.value = []
    expect(useAiAccess(server, () => true).autoConfirm.value).toEqual([])

    for (const key of storage.keys()) storage.set(key, '{"autoConfirm":["danger","unknown"]}')
    expect(useAiAccess(server, () => true).autoConfirm.value).toEqual(['danger'])
  })

  it('首次关闭，重开、重新挂载时恢复开启与关闭的选择', () => {
    const opened = ref(true)
    const server = target()
    const access = useAiAccess(server, () => opened.value)
    expect(access.allowChange.value).toBe(false)
    expect(access.shareData.value).toBe(false)

    access.allowChange.value = true
    access.shareData.value = true
    opened.value = false
    opened.value = true
    expect(access.allowChange.value).toBe(true)
    expect(access.shareData.value).toBe(true)

    const reopened = useAiAccess(server, () => true)
    expect(reopened.allowChange.value).toBe(true)
    expect(reopened.shareData.value).toBe(true)

    reopened.allowChange.value = false
    reopened.shareData.value = false
    const reloaded = useAiAccess(server, () => true)
    expect(reloaded.allowChange.value).toBe(false)
    expect(reloaded.shareData.value).toBe(false)
  })

  it('隔离项目、目标和不同的服务器连接', () => {
    const server = target()
    const access = useAiAccess(server, () => true)
    access.allowChange.value = true
    access.shareData.value = true

    for (const changes of [
      { config: 'D:/another/ferry.yaml' },
      { target: 'second' },
      { server: { ...server.server, host: 'replacement' } },
      { server: { ...server.server, user: 'different-user' } },
      { server: { ...server.server, hostKey: 'changed-key' } },
    ]) {
      const original = { ...server }
      Object.assign(server, changes)
      expect(access.allowChange.value).toBe(false)
      expect(access.shareData.value).toBe(false)
      Object.assign(server, original)
      expect(access.allowChange.value).toBe(true)
      expect(access.shareData.value).toBe(true)
    }
  })

  it('生产服务器重新打开保持只读，两个开关互不干扰', () => {
    const opened = ref(true)
    const server = target()
    const access = useAiAccess(server, () => opened.value)
    access.allowChange.value = true
    access.shareData.value = true
    server.production = true
    expect(access.allowChange.value).toBe(false)
    access.allowChange.value = true
    expect(access.allowChange.value).toBe(true)
    opened.value = false
    opened.value = true
    expect(access.allowChange.value).toBe(false)
    expect(access.shareData.value).toBe(true)
    expect(useAiAccess(server, () => true).allowChange.value).toBe(false)
  })

  it('目标禁用 AI 改动时强制只读，损坏的偏好用安全默认值', () => {
    const server = target()
    const access = useAiAccess(server, () => true)
    access.allowChange.value = true
    access.shareData.value = true
    server.noChanges = true
    expect(access.allowChange.value).toBe(false)
    access.allowChange.value = true
    expect(access.allowChange.value).toBe(false)
    expect(access.shareData.value).toBe(true)

    for (const key of storage.keys()) storage.set(key, '{bad JSON')
    const restored = useAiAccess({ ...server, noChanges: false }, () => true)
    expect(restored.allowChange.value).toBe(false)
    expect(restored.shareData.value).toBe(false)
  })
})
