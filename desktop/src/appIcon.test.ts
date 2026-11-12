import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { setIcon, isVisible, fetchIcon, syncShell, refreshShell } = vi.hoisted(() => ({
  setIcon: vi.fn(), isVisible: vi.fn(), fetchIcon: vi.fn(), syncShell: vi.fn(), refreshShell: vi.fn(),
}))
vi.mock('./platform', () => ({ isTauri: true }))
vi.mock('@tauri-apps/api/window', () => ({ getCurrentWindow: () => ({ setIcon, isVisible }) }))
vi.mock('@tauri-apps/api/core', () => ({
  invoke: (command: string, args: unknown) => command === 'sync_shell_icon' ? syncShell(args) : refreshShell(args),
}))

const darkBytes = new ArrayBuffer(1)
const lightBytes = new ArrayBuffer(2)
// report：启动时（窗口还没显示）快捷方式本来就对；shown：窗口显示着切主题，改了快捷方式
const report = { updated: 0, warnings: [] as string[], generation: 1, refresh: false }
const shown = { ...report, updated: 1, refresh: true }
let favicon: { href: string }

beforeEach(() => {
  vi.resetModules()
  vi.useFakeTimers()
  setIcon.mockReset().mockResolvedValue(undefined)
  isVisible.mockReset().mockResolvedValue(true)
  fetchIcon.mockReset().mockImplementation(async (path: string) => ({
    ok: true, arrayBuffer: async () => path.includes('dark') ? darkBytes : lightBytes,
  }))
  syncShell.mockReset().mockResolvedValue(report)
  refreshShell.mockReset().mockResolvedValue(true)
  favicon = { href: '' }
  vi.stubGlobal('document', { getElementById: () => favicon })
  vi.stubGlobal('fetch', fetchIcon)
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('主题图标', () => {
  it('较慢的旧主题下载不会覆盖新主题，favicon 立即更新', async () => {
    let finishDark!: (value: unknown) => void
    fetchIcon.mockImplementationOnce(() => new Promise(resolve => { finishDark = resolve }))
    const { syncAppIcon } = await import('./appIcon')
    const dark = syncAppIcon(true)
    await Promise.resolve()
    const light = syncAppIcon(false)
    expect(favicon.href).toBe('/branding/ferry-light.png')
    finishDark({ ok: true, arrayBuffer: async () => darkBytes })
    await Promise.all([dark, light])
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(lightBytes)
    expect(syncShell).toHaveBeenCalledExactlyOnceWith({ dark: false })
  })

  it('原生窗口写入串行，只有最后一次更新确认完成后才跳过同主题请求', async () => {
    let finishDark!: () => void
    setIcon.mockImplementationOnce(() => new Promise<void>(resolve => { finishDark = resolve }))
    syncShell.mockResolvedValue(shown)
    const { syncAppIcon } = await import('./appIcon')
    const dark = syncAppIcon(true)
    await vi.waitFor(() => expect(setIcon).toHaveBeenCalledOnce())
    const light = syncAppIcon(false)
    expect(setIcon).toHaveBeenCalledOnce()
    finishDark()
    await vi.runAllTimersAsync()
    await Promise.all([dark, light])
    expect(setIcon.mock.calls.map(([bytes]) => bytes)).toEqual([darkBytes, lightBytes])
    expect(refreshShell).toHaveBeenCalledOnce()
    await syncAppIcon(false)
    expect(setIcon).toHaveBeenCalledTimes(2)
  })

  it('快捷方式慢请求完成时跳过已经过期的窗口图标', async () => {
    let finishShell!: (value: typeof report) => void
    syncShell.mockImplementationOnce(() => new Promise(resolve => { finishShell = resolve })).mockResolvedValue(shown)
    const { syncAppIcon } = await import('./appIcon')
    const dark = syncAppIcon(true)
    await vi.waitFor(() => expect(syncShell).toHaveBeenCalledOnce())
    const light = syncAppIcon(false)
    finishShell(shown)
    await vi.runAllTimersAsync()
    await Promise.all([dark, light])
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(lightBytes)
    expect(syncShell.mock.invocationCallOrder[1]).toBeLessThan(setIcon.mock.invocationCallOrder[0])
    expect(setIcon.mock.invocationCallOrder[0]).toBeLessThan(refreshShell.mock.invocationCallOrder[0])
  })

  it.each(['warning', 'rejection'])('部分更新连续失败后切回原主题仍重新同步（%s）', async failure => {
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    const { syncAppIcon } = await import('./appIcon')
    await syncAppIcon(true)
    syncShell.mockImplementation(async ({ dark }: { dark: boolean }) => {
      if (!dark && failure === 'rejection') throw new Error('shell unavailable')
      return { ...report, warnings: dark ? [] : ['shortcut busy'] }
    })
    const light = syncAppIcon(false)
    await vi.runAllTimersAsync()
    await light
    expect(syncShell).toHaveBeenCalledTimes(4)
    await syncAppIcon(true)
    expect(setIcon).toHaveBeenLastCalledWith(darkBytes)
    expect(syncShell).toHaveBeenLastCalledWith({ dark: true })
    expect(syncShell).toHaveBeenCalledTimes(5)
  })

  it('个别快捷方式一直改不了时任务栏只刷新一次，之后同主题不再反复试', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    syncShell.mockResolvedValueOnce({ updated: 2, warnings: ['shortcut read-only'], generation: 1, refresh: true })
      .mockResolvedValueOnce({ updated: 0, warnings: ['shortcut read-only'], generation: 2, refresh: true })
      .mockResolvedValueOnce({ updated: 0, warnings: ['shortcut read-only'], generation: 3, refresh: true })
    const { syncAppIcon } = await import('./appIcon')
    const update = syncAppIcon(true)
    await vi.runAllTimersAsync()
    await update
    expect(syncShell).toHaveBeenCalledTimes(3)
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(darkBytes)
    expect(refreshShell).toHaveBeenCalledExactlyOnceWith({ generation: 1 })
    await syncAppIcon(true) // 保存设置、切换项目时会再要求一次同样的主题
    expect(syncShell).toHaveBeenCalledTimes(3)
    expect(refreshShell).toHaveBeenCalledOnce()
  })

  it('被占用的快捷方式后来改成了，任务栏补一次刷新', async () => {
    syncShell.mockResolvedValueOnce({ updated: 1, warnings: ['shortcut busy'], generation: 1, refresh: true })
      .mockResolvedValueOnce({ updated: 1, warnings: [], generation: 2, refresh: true })
    const { syncAppIcon } = await import('./appIcon')
    const update = syncAppIcon(false)
    await vi.runAllTimersAsync()
    await update
    expect(syncShell).toHaveBeenCalledTimes(2)
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(lightBytes)
    expect(refreshShell.mock.calls).toEqual([[{ generation: 1 }], [{ generation: 2 }]])
  })

  it('同一个主题正在做时再要求一次，不重新来一遍', async () => {
    syncShell.mockResolvedValue({ ...report, refresh: true })
    const { syncAppIcon, waitForAppIcon } = await import('./appIcon')
    const first = syncAppIcon(true)
    await vi.waitFor(() => expect(setIcon).toHaveBeenCalledOnce())
    const second = syncAppIcon(true)
    expect(second).toBe(first)
    await vi.runAllTimersAsync()
    await waitForAppIcon()
    expect(syncShell).toHaveBeenCalledOnce()
    expect(setIcon).toHaveBeenCalledOnce()
    expect(refreshShell).toHaveBeenCalledOnce()
  })

  it('图片读取瞬时失败自动重试，失败的图片不会留在缓存中', async () => {
    fetchIcon.mockResolvedValueOnce({ ok: false, status: 503 })
    const { syncAppIcon } = await import('./appIcon')
    const update = syncAppIcon(false)
    await vi.runAllTimersAsync()
    await update
    expect(fetchIcon).toHaveBeenCalledTimes(2)
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(lightBytes)
  })

  it('任务栏失败自动补偿，连续失败后同主题仍能重试', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    refreshShell.mockRejectedValue(new Error('native refresh failed'))
    syncShell.mockResolvedValue(shown)
    const { syncAppIcon } = await import('./appIcon')
    const update = syncAppIcon(true)
    await vi.runAllTimersAsync()
    await update
    expect(refreshShell).toHaveBeenCalledTimes(3)
    refreshShell.mockResolvedValue(true)
    const again = syncAppIcon(true)
    await vi.runAllTimersAsync()
    await again
    expect(refreshShell).toHaveBeenCalledTimes(4)
    await syncAppIcon(true)
    expect(refreshShell).toHaveBeenCalledTimes(4)
  })

  it('延迟刷新期间新主题可以继续写入，过期主题不再刷新任务栏', async () => {
    syncShell.mockResolvedValueOnce({ ...report, refresh: true })
      .mockResolvedValueOnce({ ...report, generation: 2, refresh: true })
    const { syncAppIcon } = await import('./appIcon')
    const dark = syncAppIcon(true)
    await vi.waitFor(() => expect(setIcon).toHaveBeenCalledOnce())
    const light = syncAppIcon(false)
    await vi.waitFor(() => expect(setIcon).toHaveBeenCalledTimes(2))
    expect(refreshShell).not.toHaveBeenCalled()
    await vi.runAllTimersAsync()
    await Promise.all([dark, light])
    expect(refreshShell).toHaveBeenCalledExactlyOnceWith({ generation: 2 })
  })

  it('模态交互结束后继续刷新，等待者收到实际完成结果', async () => {
    refreshShell.mockResolvedValueOnce(false)
    syncShell.mockResolvedValue(shown)
    const { syncAppIcon, waitForAppIcon } = await import('./appIcon')
    let done = false
    const update = syncAppIcon(true)
    const waiting = waitForAppIcon().then(() => { done = true })
    await vi.advanceTimersByTimeAsync(2500)
    expect(refreshShell).toHaveBeenCalledOnce()
    expect(done).toBe(false)
    await vi.advanceTimersByTimeAsync(500)
    await Promise.all([update, waiting])
    expect(done).toBe(true)
    expect(syncShell).toHaveBeenCalledOnce()
    expect(refreshShell).toHaveBeenCalledTimes(2)
  })

  it('启动等待会跟进正式设置替换的主题，窗口图标换好就可以显示窗口', async () => {
    let finishFetch!: (value: unknown) => void
    fetchIcon.mockImplementationOnce(() => new Promise(resolve => { finishFetch = resolve }))
    const { syncAppIcon, waitForAppIcon } = await import('./appIcon')
    const initial = syncAppIcon(true)
    let ready = false
    const waiting = waitForAppIcon().then(() => { ready = true })
    await Promise.resolve()
    expect(ready).toBe(false)
    const settings = syncAppIcon(false)
    finishFetch({ ok: true, arrayBuffer: async () => darkBytes })
    await Promise.all([initial, settings, waiting])
    expect(ready).toBe(true)
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(lightBytes)
    expect(refreshShell).not.toHaveBeenCalled()
    await syncAppIcon(false)
    expect(syncShell).toHaveBeenCalledOnce()
  })

  it('启动时不拦着窗口显示，也不刷新任务栏', async () => {
    syncShell.mockResolvedValue({ ...report, updated: 2 })
    const { syncAppIcon } = await import('./appIcon')
    await syncAppIcon(false)
    await vi.runAllTimersAsync()
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(lightBytes)
    expect(syncShell).toHaveBeenCalledOnce()
    expect(refreshShell).not.toHaveBeenCalled()
    await syncAppIcon(false)
    expect(syncShell).toHaveBeenCalledOnce()
  })

  it('启动后马上换了主题：按新主题来，任务栏只刷新一次', async () => {
    syncShell.mockResolvedValueOnce(report).mockResolvedValueOnce({ ...shown, generation: 2 })
    const { syncAppIcon } = await import('./appIcon')
    await syncAppIcon(true)
    const light = syncAppIcon(false)
    await vi.runAllTimersAsync()
    await light
    expect(syncShell).toHaveBeenCalledTimes(2)
    expect(setIcon.mock.calls.map(([bytes]) => bytes)).toEqual([darkBytes, lightBytes])
    expect(refreshShell).toHaveBeenCalledExactlyOnceWith({ generation: 2 })
  })

  it('启动时快捷方式要改：先不改、不换窗口图标，窗口显示出来后再走平时切主题的那一套', async () => {
    syncShell.mockResolvedValueOnce({ ...report, pending: 2 }).mockResolvedValueOnce({ ...shown, updated: 2, generation: 2 })
    isVisible.mockResolvedValueOnce(false).mockResolvedValueOnce(false)
    const { syncAppIcon } = await import('./appIcon')
    await syncAppIcon(true) // 马上返回，main.ts 这时显示窗口
    expect(syncShell).toHaveBeenCalledOnce()
    expect(setIcon).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(400) // 窗口显示出来了
    expect(isVisible).toHaveBeenCalledTimes(3)
    expect(syncShell).toHaveBeenCalledOnce()
    await vi.advanceTimersByTimeAsync(500)
    expect(syncShell).toHaveBeenCalledTimes(2)
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(darkBytes)
    expect(syncShell.mock.invocationCallOrder[1]).toBeLessThan(setIcon.mock.invocationCallOrder[0])
    await vi.advanceTimersByTimeAsync(2400)
    expect(refreshShell).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(100)
    expect(refreshShell).toHaveBeenCalledExactlyOnceWith({ generation: 2 })
    await syncAppIcon(true)
    expect(syncShell).toHaveBeenCalledTimes(2)
  })

  it('启动时快捷方式要改、等窗口显示的时候换了主题：只按新主题做一遍', async () => {
    syncShell.mockResolvedValueOnce({ ...report, pending: 2 }).mockResolvedValueOnce({ ...shown, generation: 2 })
    isVisible.mockResolvedValue(false)
    const { syncAppIcon } = await import('./appIcon')
    await syncAppIcon(true)
    await vi.advanceTimersByTimeAsync(300)
    const light = syncAppIcon(false)
    await vi.runAllTimersAsync()
    await light
    expect(syncShell).toHaveBeenCalledTimes(2)
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(lightBytes)
    expect(refreshShell).toHaveBeenCalledExactlyOnceWith({ generation: 2 })
  })

  it('启动时个别快捷方式没改成：不拦着窗口显示，之后再试', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    syncShell.mockResolvedValue({ ...report, warnings: ['shortcut read-only'] })
    const { syncAppIcon } = await import('./appIcon')
    await syncAppIcon(true)
    expect(syncShell).toHaveBeenCalledOnce()
    await vi.runAllTimersAsync()
    expect(syncShell).toHaveBeenCalledTimes(3)
    expect(setIcon).toHaveBeenCalledOnce()
    expect(refreshShell).not.toHaveBeenCalled()
  })
})
