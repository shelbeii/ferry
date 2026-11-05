import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { AppSettings, SettingsResponse } from './ferry'

const { rpc } = vi.hoisted(() => ({ rpc: vi.fn() }))
vi.mock('./ferry', async original => ({ ...await original<typeof import('./ferry')>(), call: rpc }))
vi.mock('./glass/runtime', () => ({ glass: { enabled: true, transparency: 90 }, GLASS_TRANSPARENCY_DEFAULT: 90 }))
const { appSettings, settingsInfo, saveSettings, selectProject, projects, currentPath } = await import('./state')
vi.stubGlobal('window', { matchMedia: () => ({ matches: false, onchange: null }) })
vi.stubGlobal('document', { documentElement: { classList: { toggle: vi.fn() } } })
vi.stubGlobal('localStorage', { setItem: vi.fn() })

function settings(): AppSettings {
  return {
    connect: { timeoutSec: 15, acceptNewHostKey: false, keyDir: '', uploadStreams: 6, idleMinutes: 15, statusMinutes: 1 },
    build: { env: {}, path: [], verbose: false }, logs: { buildLogDays: 30 }, deploy: { skipBuild: false, full: false, keepPerms: false },
    runtime: { source: '', cacheDir: '', nginxSource: '', githubMirror: '', mysqlSource: '' },
    ui: { theme: 'system', taskKeep: 50, taskKeepDays: 30, taskMaxMB: 100, confirmClose: true, notify: true, glass: true, glassTransparency: 90, checkUpdate: true },
    ai: { models: [], default: '', keepDays: 30, maxMB: 200 }, mcp: { allowChanges: false, targets: [], logKeepDays: 30, logMaxMB: 20 },
    notify: { channels: [] }, desktop: { projects: [{ path: 'a.yaml', name: 'demo' }], current: 'a.yaml' }, team: { name: '' },
  }
}
function response(s: AppSettings): SettingsResponse {
  return { settings: s, dataDir: { dir: '/demo', defaultDir: '/demo', mode: 'env' }, file: '/demo/settings.json', keyDir: '/demo/keys', legacyDir: '', secretsTransaction: 1 }
}
beforeEach(() => {
  rpc.mockReset()
  appSettings.value = settings()
  settingsInfo.value = response(settings())
  projects.value = settings().desktop.projects ?? []
  currentPath.value = 'a.yaml'
})

describe('设置保存的一次提交', () => {
  it('设置与凭据只发一次 RPC，使用返回值刷新缓存，不再额外读取', async () => {
    const draft = settings(); draft.ui.theme = 'dark'
    rpc.mockImplementation(async (_args, env) => response(JSON.parse(env.FERRY_SETTINGS_VALUE)))
    await saveSettings(draft, [{ kind: 'ai', id: 'm1', value: 'test-key-private' }])
    expect(rpc).toHaveBeenCalledOnce()
    const [args, env] = rpc.mock.calls[0]
    expect(args).toEqual(['settings', 'save', '--with-secrets'])
    expect(JSON.parse(env.FERRY_SETTINGS_SECRETS)).toEqual([{ kind: 'ai', id: 'm1', value: 'test-key-private' }])
    expect(env.FERRY_SETTINGS_VALUE).not.toContain('test-key-private')
    expect(appSettings.value?.ui.theme).toBe('dark')
  })
  it('失败不改变缓存与草稿，可再次提交同一批凭据', async () => {
    const draft = settings(); draft.ui.theme = 'dark'
    const changes = [{ kind: 'dns' as const, id: 'cloudflare', value: 'test-token-private' }]
    rpc.mockRejectedValueOnce(new Error('保存失败，已恢复原有设置和凭据'))
    await expect(saveSettings(draft, changes)).rejects.toThrow('已恢复')
    expect(appSettings.value?.ui.theme).toBe('system')
    expect(changes[0].value).toBe('test-token-private')
    rpc.mockImplementation(async (_args, env) => response(JSON.parse(env.FERRY_SETTINGS_VALUE)))
    await saveSettings(draft, changes)
    expect(appSettings.value?.ui.theme).toBe('dark')
    expect(JSON.parse(rpc.mock.calls[1][1].FERRY_SETTINGS_SECRETS)).toEqual(changes)
  })
  it('旧后台缺少批量提交能力时，在任何写入前拦住带凭据的保存', async () => {
    settingsInfo.value!.secretsTransaction = undefined
    await expect(saveSettings(settings(), [{ kind: 'ai', id: 'm1', value: 'test-private' }])).rejects.toThrow('本次未保存')
    expect(rpc).not.toHaveBeenCalled()
  })
  it('保存期间切换项目会排队，导航保存使用新设置，不用旧设置覆盖刚保存的内容', async () => {
    let complete!: (r: SettingsResponse) => void
    rpc.mockImplementationOnce(() => new Promise<SettingsResponse>(resolve => { complete = resolve }))
    const draft = settings(); draft.ui.theme = 'dark'
    const first = saveSettings(draft)
    await Promise.resolve()
    const navigation = selectProject('b.yaml')
    await Promise.resolve()
    expect(rpc).toHaveBeenCalledOnce()
    rpc.mockImplementation(async (_args, env) => response(JSON.parse(env.FERRY_SETTINGS_VALUE)))
    complete(response(draft))
    await first; await navigation
    const body = JSON.parse(rpc.mock.calls[1][1].FERRY_SETTINGS_VALUE)
    expect(body.ui.theme).toBe('dark')
    expect(body.desktop.current).toBe('b.yaml')
    expect(appSettings.value?.desktop.current).toBe('b.yaml')
  })
})
