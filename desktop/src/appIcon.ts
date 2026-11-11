import { getCurrentWindow } from '@tauri-apps/api/window'
import { invoke } from '@tauri-apps/api/core'
import { isTauri } from './platform'

export function appIconPath(dark: boolean): string {
  return `/branding/ferry-${dark ? 'dark' : 'light'}.png`
}

const iconBytes = new Map<string, Promise<ArrayBuffer>>()
let revision = 0
let appliedTheme: boolean | undefined
let nativeUpdate = Promise.resolve()

/** 同步窗口、桌面和任务栏图标，包含「跟随系统」和设置预览；原生更新串行，避免旧请求覆盖新主题。 */
export function syncAppIcon(dark: boolean): Promise<void> {
  const path = appIconPath(dark)
  const favicon = document.getElementById?.('ferry-favicon') as HTMLLinkElement | null | undefined
  if (favicon) favicon.href = path
  if (!isTauri) return Promise.resolve()

  const current = ++revision
  nativeUpdate = nativeUpdate.then(async () => {
    if (current !== revision || appliedTheme === dark) return
    let bytes = iconBytes.get(path)
    if (!bytes) {
      bytes = fetch(path).then(response => {
        if (!response.ok) throw new Error(`读取 Ferry 图标失败：${response.status}`)
        return response.arrayBuffer()
      })
      iconBytes.set(path, bytes)
      // 读取失败时允许后续切换重试。
      void bytes.catch(() => { iconBytes.delete(path) })
    }
    const icon = await bytes
    if (current !== revision) return
    // 先改快捷方式、再换窗口图标：任务栏看到窗口图标变了会重新取这一组（合并的按钮）的图标并记下来，
    // 快捷方式还没改好就记成旧的，之后任务栏按钮一直是旧图标（见 shellicon.rs 的 sync）
    const shell = await invoke<{ updated: number; warnings: string[] }>('sync_shell_icon', { dark })
      .finally(() => getCurrentWindow().setIcon(icon))
    if (shell.warnings.length > 0) {
      console.warn('同步 Ferry 快捷方式图标失败', shell.warnings)
      return
    }
    appliedTheme = dark
  }).catch(error => {
    console.warn('同步 Ferry 主题图标失败', error)
  })
  return nativeUpdate
}
