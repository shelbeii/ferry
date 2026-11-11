import { beforeEach, describe, expect, it, vi } from 'vitest'

const { setIcon, fetchIcon, invoke } = vi.hoisted(() => ({ setIcon: vi.fn(), fetchIcon: vi.fn(), invoke: vi.fn() }))
vi.mock('./platform', () => ({ isTauri: true }))
vi.mock('@tauri-apps/api/window', () => ({ getCurrentWindow: () => ({ setIcon }) }))
vi.mock('@tauri-apps/api/core', () => ({ invoke }))

let favicon: { href: string }
beforeEach(() => {
  vi.resetModules()
  setIcon.mockReset().mockResolvedValue(undefined)
  fetchIcon.mockReset()
  invoke.mockReset().mockResolvedValue({ updated: 0, warnings: [] })
  favicon = { href: '' }
  vi.stubGlobal('document', { getElementById: () => favicon })
  vi.stubGlobal('fetch', fetchIcon)
})

describe('主题图标', () => {
  it('较慢的旧主题下载不会覆盖新主题，favicon 立即更新', async () => {
    let finishDark!: (response: Response) => void
    const darkBytes = new ArrayBuffer(1)
    const lightBytes = new ArrayBuffer(2)
    fetchIcon.mockImplementation((path: string) => path.includes('dark')
      ? new Promise<Response>(resolve => { finishDark = resolve })
      : Promise.resolve({ ok: true, arrayBuffer: async () => lightBytes }))
    const { syncAppIcon } = await import('./appIcon')
    const dark = syncAppIcon(true)
    await Promise.resolve()
    const light = syncAppIcon(false)
    expect(favicon.href).toBe('/branding/ferry-light.png')
    finishDark({ ok: true, arrayBuffer: async () => darkBytes } as Response)
    await Promise.all([dark, light])
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(lightBytes)
    expect(invoke).toHaveBeenCalledExactlyOnceWith('sync_shell_icon', { dark: false })
  })

  it('原生窗口更新串行执行，最终使用最近一次选择的主题', async () => {
    let finishDark!: () => void
    const darkBytes = new ArrayBuffer(1)
    const lightBytes = new ArrayBuffer(2)
    fetchIcon.mockImplementation(async (path: string) => ({ ok: true, arrayBuffer: async () => path.includes('dark') ? darkBytes : lightBytes }))
    setIcon.mockImplementationOnce(() => new Promise<void>(resolve => { finishDark = resolve }))
    const { syncAppIcon } = await import('./appIcon')
    const dark = syncAppIcon(true)
    await vi.waitFor(() => expect(setIcon).toHaveBeenCalledOnce())
    const light = syncAppIcon(false)
    expect(setIcon).toHaveBeenCalledOnce()
    finishDark()
    await Promise.all([dark, light])
    expect(setIcon.mock.calls.map(([bytes]) => bytes)).toEqual([darkBytes, lightBytes])
    expect(invoke.mock.calls.map(([, theme]) => theme.dark)).toEqual([true, false])
    await syncAppIcon(false)
    expect(setIcon).toHaveBeenCalledTimes(2)
  })

  it('图片读取失败后允许重试，不阻塞后续主题切换', async () => {
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
    const bytes = new ArrayBuffer(2)
    fetchIcon.mockResolvedValueOnce({ ok: false, status: 503 })
      .mockResolvedValue({ ok: true, arrayBuffer: async () => bytes })
    const { syncAppIcon } = await import('./appIcon')
    await syncAppIcon(false)
    expect(setIcon).not.toHaveBeenCalled()
    await syncAppIcon(false)
    expect(fetchIcon).toHaveBeenCalledTimes(2)
    expect(setIcon).toHaveBeenCalledExactlyOnceWith(bytes)
    warning.mockRestore()
  })

  it('桌面图标同步失败后，同一主题仍能重试，不能只记住窗口图标已经更新', async () => {
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
    fetchIcon.mockResolvedValue({ ok: true, arrayBuffer: async () => new ArrayBuffer(2) })
    invoke.mockRejectedValueOnce(new Error('shell unavailable'))
    const { syncAppIcon } = await import('./appIcon')
    await syncAppIcon(true)
    await syncAppIcon(true)
    expect(invoke).toHaveBeenCalledTimes(2)
    expect(setIcon).toHaveBeenCalledTimes(2)
    expect(fetchIcon).toHaveBeenCalledOnce()
    warning.mockRestore()
  })

  it('正在更新固定快捷方式时，后续窗口和桌面主题也按顺序切换', async () => {
    const warning = vi.spyOn(console, 'warn').mockImplementation(() => {})
    fetchIcon.mockResolvedValue({ ok: true, arrayBuffer: async () => new ArrayBuffer(2) })
    let finishShell!: (value: { updated: number; warnings: string[] }) => void
    invoke.mockImplementationOnce(() => new Promise(resolve => { finishShell = resolve }))
    const { syncAppIcon } = await import('./appIcon')
    const dark = syncAppIcon(true)
    await vi.waitFor(() => expect(invoke).toHaveBeenCalledOnce())
    const light = syncAppIcon(false)
    // 快捷方式先改好再换窗口图标（任务栏看到窗口图标变了会去记这一组的图标）
    expect(setIcon).not.toHaveBeenCalled()
    finishShell({ updated: 1, warnings: ['one shortcut is temporarily locked'] })
    await Promise.all([dark, light])
    expect(invoke.mock.calls.map(([, theme]) => theme.dark)).toEqual([true, false])
    expect(setIcon).toHaveBeenCalledTimes(2)
    expect(invoke.mock.invocationCallOrder[0]).toBeLessThan(setIcon.mock.invocationCallOrder[0])
    expect(invoke.mock.invocationCallOrder[1]).toBeLessThan(setIcon.mock.invocationCallOrder[1])
    warning.mockRestore()
  })
})
