import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import 'element-plus'
import type { AppSettings } from './ferry'

const { rpc, setTheme, nativeTheme, syncIcon, waitIcon } = vi.hoisted(() => ({
  rpc: vi.fn(), setTheme: vi.fn(), nativeTheme: vi.fn(), syncIcon: vi.fn(), waitIcon: vi.fn(),
}))
vi.mock('./ferry', async original => ({ ...await original<typeof import('./ferry')>(), call: rpc }))
vi.mock('./platform', () => ({ isTauri: true, storeGet: vi.fn() }))
vi.mock('@tauri-apps/api/window', () => ({ getCurrentWindow: () => ({ setTheme, theme: nativeTheme }) }))
vi.mock('./appIcon', () => ({ syncAppIcon: syncIcon, waitForAppIcon: waitIcon }))
vi.mock('./glass/runtime', () => ({
  glass: { enabled: true, transparency: 90 }, GLASS_TRANSPARENCY_DEFAULT: 90, themeChanged: vi.fn(),
}))

let media: { matches: boolean; onchange: (() => void) | null }
let dark: boolean
let cache: Map<string, string>
beforeEach(() => {
  vi.resetModules()
  rpc.mockReset()
  setTheme.mockReset().mockResolvedValue(undefined)
  nativeTheme.mockReset().mockResolvedValue('light')
  syncIcon.mockReset().mockResolvedValue(undefined)
  waitIcon.mockReset().mockResolvedValue(undefined)
  dark = false
  cache = new Map()
  media = { matches: false, onchange: null }
  vi.stubGlobal('window', { matchMedia: () => media })
  vi.stubGlobal('document', { documentElement: { classList: {
    contains: () => dark, toggle: (_name: string, value: boolean) => { dark = value },
  } } })
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => cache.get(key), setItem: (key: string, value: string) => cache.set(key, value),
  })
})
afterEach(() => vi.unstubAllGlobals())

describe('主题初始化与系统跟随', () => {
  it('启动和项目初始化共用设置读取，预览不覆盖已保存的启动主题', async () => {
    const settings = { ui: { theme: 'light' } } as AppSettings
    rpc.mockResolvedValue({ settings })
    const { loadSettings, applyTheme, waitForTheme } = await import('./state')
    const startup = loadSettings()
    const projects = loadSettings()
    expect(startup).toBe(projects)
    await startup
    await waitForTheme()
    expect(rpc).toHaveBeenCalledOnce()
    applyTheme('dark')
    await waitForTheme()
    expect(dark).toBe(true)
    expect(cache.get('ferry.theme')).toBe('light')
    applyTheme('light') // 取消预览。
    await waitForTheme()
    expect(dark).toBe(false)
    expect(syncIcon).toHaveBeenLastCalledWith(false)
  })

  it('记着上次的主题时，不等后台读完设置就可以显示窗口', async () => {
    cache.set('ferry.theme', 'dark')
    let finish!: (value: unknown) => void
    rpc.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    const { startTheme, waitForTheme } = await import('./state')
    await startTheme()
    expect(dark).toBe(true)
    expect(setTheme).toHaveBeenLastCalledWith('dark')
    expect(syncIcon).toHaveBeenLastCalledWith(true)
    // 设置晚到、主题没变：界面不变
    finish({ settings: { ui: { theme: 'dark' } } })
    await vi.waitFor(() => expect(syncIcon).toHaveBeenCalledTimes(2))
    await waitForTheme()
    expect(dark).toBe(true)
    expect(syncIcon).toHaveBeenLastCalledWith(true)
  })

  it('没记过主题时等正式设置，读不到就跟随系统', async () => {
    let finish!: (value: unknown) => void
    let fail!: (error: Error) => void
    rpc.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const { startTheme } = await import('./state')
    let ready = false
    const waiting = startTheme().then(() => { ready = true })
    await Promise.resolve()
    expect(ready).toBe(false)
    expect(setTheme).not.toHaveBeenCalled()
    finish({ settings: { ui: { theme: 'dark' } } })
    await waiting
    expect(dark).toBe(true)
    expect(cache.get('ferry.theme')).toBe('dark')

    vi.resetModules()
    cache.clear()
    setTheme.mockClear()
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    rpc.mockImplementationOnce(() => new Promise((_, reject) => { fail = reject }))
    const again = await import('./state')
    const failed = again.startTheme()
    fail(new Error('ferry 没启动起来'))
    await failed
    expect(setTheme).toHaveBeenLastCalledWith(null)
  })

  it('切回跟随系统时采用原生解析结果，并响应后续系统主题变化', async () => {
    nativeTheme.mockResolvedValue('dark')
    const { applyTheme, waitForTheme } = await import('./state')
    applyTheme('system')
    await waitForTheme()
    expect(setTheme).toHaveBeenCalledWith(null)
    expect(dark).toBe(true)
    media.matches = false
    media.onchange!()
    expect(dark).toBe(false)
    expect(syncIcon).toHaveBeenLastCalledWith(false)
    applyTheme('dark')
    await waitForTheme()
    expect(media.onchange).toBeNull()
    expect(dark).toBe(true)
  })

  it('过期的原生系统主题查询不会覆盖后选的固定主题', async () => {
    let finish!: (theme: string) => void
    nativeTheme.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    const { applyTheme, waitForTheme } = await import('./state')
    applyTheme('system')
    await vi.waitFor(() => expect(nativeTheme).toHaveBeenCalledOnce())
    applyTheme('light')
    finish('dark')
    await waitForTheme()
    expect(dark).toBe(false)
    expect(setTheme).toHaveBeenLastCalledWith('light')
    expect(syncIcon).toHaveBeenLastCalledWith(false)
  })

  it('窗口显示的等待条件包含原生主题和图标完成', async () => {
    let finishNative!: () => void
    let finishIcon!: () => void
    setTheme.mockImplementationOnce(() => new Promise<void>(resolve => { finishNative = resolve }))
    waitIcon.mockImplementationOnce(() => new Promise<void>(resolve => { finishIcon = resolve }))
    const { applyTheme, waitForTheme } = await import('./state')
    applyTheme('dark')
    let ready = false
    const waiting = waitForTheme().then(() => { ready = true })
    await vi.waitFor(() => expect(setTheme).toHaveBeenCalledOnce())
    expect(ready).toBe(false)
    finishNative()
    await vi.waitFor(() => expect(waitIcon).toHaveBeenCalledOnce())
    expect(ready).toBe(false)
    finishIcon()
    await waiting
    expect(ready).toBe(true)
  })
})
