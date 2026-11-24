import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  isTauri: true,
  call: vi.fn(),
  ferry: vi.fn(),
  appInfo: vi.fn(),
  cleanupOldBinaries: vi.fn(),
  shutdownFerry: vi.fn(),
  exitApp: vi.fn(),
  notify: vi.fn(() => ({ close: vi.fn() })),
}))

vi.mock('element-plus', () => ({
  ElNotification: mocks.notify,
}))
vi.mock('./appIcon', () => ({
  appIconPath: () => '/icon.png',
}))
vi.mock('./ferry', () => ({
  call: (...args: unknown[]) => mocks.call(...args),
  ferry: (...args: unknown[]) => mocks.ferry(...args),
}))
vi.mock('./platform', () => ({
  get isTauri() {
    return mocks.isTauri
  },
  appInfo: () => mocks.appInfo(),
  cleanupOldBinaries: () => mocks.cleanupOldBinaries(),
  shutdownFerry: () => mocks.shutdownFerry(),
  exitApp: () => mocks.exitApp(),
}))
vi.mock('./state', () => ({
  appSettings: { value: { ui: { checkUpdate: true } } },
}))

import { baseVersion, checkInstallHealth, checkUpdate, detectVersionMismatch, hasUpdate, installUpdate, updater } from './update'

function resetUpdater() {
  Object.assign(updater, {
    info: undefined,
    checking: false,
    checkError: '',
    checkedAt: 0,
    mismatch: undefined,
    staleBinaries: 0,
    dialog: false,
    phase: '',
    done: 0,
    total: 0,
    error: '',
    message: '',
  })
}

describe('update version helpers', () => {
  it('extracts base semver and ignores dev/invalid strings', () => {
    expect(baseVersion('0.18.9')).toBe('0.18.9')
    expect(baseVersion('v0.18.9+abc123.dirty')).toBe('0.18.9')
    expect(baseVersion('0.18.9-rc1')).toBe('0.18.9')
    expect(baseVersion('dev')).toBe('')
    expect(baseVersion('')).toBe('')
    expect(baseVersion(undefined)).toBe('')
  })

  it('detects version mismatch only when both sides are valid and differ', () => {
    expect(detectVersionMismatch('0.18.9', '0.18.9+72335e7')).toBeUndefined()
    expect(detectVersionMismatch('0.18.9', 'dev')).toBeUndefined()
    expect(detectVersionMismatch('0.18.8', '0.18.9+72335e7')).toEqual({ desktop: '0.18.8', cli: '0.18.9' })
    expect(detectVersionMismatch('0.18.9', '0.18.8')).toEqual({ desktop: '0.18.9', cli: '0.18.8' })
  })
})

describe('update health check and install flow', () => {
  beforeEach(() => {
    resetUpdater()
    vi.clearAllMocks()
    mocks.isTauri = true
    mocks.appInfo.mockResolvedValue({ name: 'Ferry', version: '0.18.9', tauri: '2.0.0' })
    mocks.cleanupOldBinaries.mockResolvedValue([])
    mocks.shutdownFerry.mockResolvedValue(undefined)
    mocks.exitApp.mockResolvedValue(undefined)
  })

  it('checkInstallHealth detects mismatch and locked stale binaries locally without hitting GitHub', async () => {
    mocks.appInfo.mockResolvedValue({ name: 'Ferry', version: '0.18.8', tauri: '2.0.0' })
    mocks.call.mockResolvedValue({ version: '0.18.9+abc' })
    mocks.cleanupOldBinaries.mockResolvedValue(['ferry.exe.old-12345'])

    await checkInstallHealth()

    expect(mocks.call).toHaveBeenCalledWith(['version'])
    expect(updater.mismatch).toEqual({ desktop: '0.18.8', cli: '0.18.9' })
    expect(updater.staleBinaries).toBe(1)
    expect(hasUpdate()).toBe(true)
    expect(mocks.notify).toHaveBeenCalledTimes(2)
  })

  it('checkUpdate re-fetches with --force when desktop and cli mismatch on latest', async () => {
    mocks.appInfo.mockResolvedValue({ name: 'Ferry', version: '0.18.8', tauri: '2.0.0' })
    mocks.call
      .mockResolvedValueOnce({
        current: '0.18.9+abc',
        latest: '0.18.9',
        newer: false,
        page: 'https://github.com/shelbeii/ferry/releases/tag/v0.18.9',
        install: 'installer',
      })
      .mockResolvedValueOnce({
        current: '0.18.9+abc',
        latest: '0.18.9',
        newer: false,
        page: 'https://github.com/shelbeii/ferry/releases/tag/v0.18.9',
        notes: '## 0.18.9',
        size: 12345,
        install: 'installer',
      })

    const info = await checkUpdate(true)
    expect(mocks.call).toHaveBeenNthCalledWith(1, ['update', 'check'])
    expect(mocks.call).toHaveBeenNthCalledWith(2, ['update', 'check', '--force'])
    expect(updater.mismatch).toEqual({ desktop: '0.18.8', cli: '0.18.9' })
    expect(info?.notes).toBe('## 0.18.9')
    expect(info?.size).toBe(12345)
  })

  it('installUpdate shuts down ferry serve before exiting app without 1200ms delay', async () => {
    const order: string[] = []
    mocks.shutdownFerry.mockImplementation(async () => {
      order.push('shutdownFerry')
    })
    mocks.exitApp.mockImplementation(async () => {
      order.push('exitApp')
    })
    mocks.ferry.mockReturnValue({
      done: Promise.resolve({ ok: true, data: { message: '安装程序已启动' } }),
      kill: vi.fn(),
    })

    updater.info = {
      current: '0.18.8',
      latest: '0.18.9',
      newer: true,
      page: 'https://github.com/shelbeii/ferry/releases/tag/v0.18.9',
      install: 'installer',
    }

    const start = Date.now()
    await installUpdate()
    const elapsed = Date.now() - start

    expect(mocks.ferry).toHaveBeenCalledWith(['update', 'install', '--restart'], expect.any(Function))
    expect(order).toEqual(['shutdownFerry', 'exitApp'])
    expect(elapsed).toBeLessThan(800)
  })

  it('installUpdate passes --force in repair mode when desktop and cli mismatch', async () => {
    mocks.ferry.mockReturnValue({
      done: Promise.resolve({ ok: true, data: { message: '安装程序已启动' } }),
      kill: vi.fn(),
    })

    updater.mismatch = { desktop: '0.18.8', cli: '0.18.9' }
    updater.info = {
      current: '0.18.9',
      latest: '0.18.9',
      newer: false,
      page: 'https://github.com/shelbeii/ferry/releases/tag/v0.18.9',
      install: 'installer',
    }

    await installUpdate()

    expect(mocks.ferry).toHaveBeenCalledWith(['update', 'install', '--restart', '--force'], expect.any(Function))
    expect(mocks.shutdownFerry).toHaveBeenCalledTimes(1)
    expect(mocks.exitApp).toHaveBeenCalledTimes(1)
  })
})
