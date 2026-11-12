import { getCurrentWindow } from '@tauri-apps/api/window'
import { invoke } from '@tauri-apps/api/core'
import { isTauri } from './platform'

export function appIconPath(dark: boolean): string {
  return `/branding/ferry-${dark ? 'dark' : 'light'}.png`
}

const iconBytes = new Map<string, Promise<ArrayBuffer>>()
let revision = 0
let appliedTheme: boolean | undefined
/** 最近一次要求的主题（正在做或已经做完）；做失败了清掉，下次同样的主题重新做 */
let requestedTheme: boolean | undefined
let nativeUpdate = Promise.resolve()
let latestUpdate = Promise.resolve()

interface ShellReport {
  updated: number
  warnings: string[]
  generation: number
  refresh: boolean
  /** 窗口还没显示、有这么多个快捷方式要改：这次什么都没改，等窗口显示出来再同步一次（见 changeAfterShow） */
  pending?: number
}

/** 个别快捷方式没改成（被占用、只读）时，隔这么久再试（毫秒）；只重试快捷方式，不重复刷新任务栏 */
const SHORTCUT_RETRY_MS = [1000, 3000]

const delay = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms))

/** 启动期间主题可能被正式设置或系统事件替换，等待最后一次更新。 */
export async function waitForAppIcon(): Promise<void> {
  let pending: Promise<void>
  do {
    pending = latestUpdate
    await pending
  } while (pending !== latestUpdate)
}

/** 同步窗口、桌面和任务栏图标，包含「跟随系统」和设置预览；原生更新串行，避免旧请求覆盖新主题。 */
export function syncAppIcon(dark: boolean): Promise<void> {
  const path = appIconPath(dark)
  const favicon = document.getElementById?.('ferry-favicon') as HTMLLinkElement | null | undefined
  if (favicon) favicon.href = path
  if (!isTauri) return Promise.resolve()
  // 同一个主题正在做或已经做完：不重新来一遍（保存设置、切换项目、启动时读到正式设置都会再要求一次同样的主题，
  // 重来会重新扫快捷方式，并把等着刷新任务栏的那 2.5 秒从头算）
  if (requestedTheme === dark) return latestUpdate

  requestedTheme = dark
  const current = ++revision
  latestUpdate = updateAppIcon(dark, current)
  return latestUpdate
}

/** 改快捷方式和窗口属性；withIcon 时接着换窗口图标。主题已经换成别的、或这个主题已经做完时返回 undefined */
function prepareIcon(dark: boolean, current: number, withIcon: boolean): Promise<ShellReport | undefined> {
  const update = nativeUpdate.then(async () => {
    if (current !== revision || appliedTheme === dark) return
    let icon: ArrayBuffer | undefined
    if (withIcon) {
      const path = appIconPath(dark)
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
      icon = await bytes
      if (current !== revision) return
    }
    // 从这里开始可能已经改动一部分原生状态，旧的成功记录不再可靠。
    appliedTheme = undefined
    // 先改快捷方式、再换窗口图标：任务栏看到窗口图标变了会重新取这一组（合并的按钮）的图标并记下来，
    // 快捷方式还没改好就记成旧的，之后任务栏按钮一直是旧图标（见 shellicon.rs 的 sync）
    let report: ShellReport | undefined
    try {
      report = await invoke<ShellReport>('sync_shell_icon', { dark })
      return report
    } finally {
      // 快捷方式留到窗口显示后再改的，窗口图标也留到那时一起换（和平时切主题一样的顺序）
      if (icon && current === revision && !report?.pending) await getCurrentWindow().setIcon(icon)
    }
  })
  // 只串行化实际写入，等待任务栏或重试期间允许新主题继续更新。
  nativeUpdate = update.then(() => undefined, () => undefined)
  return update
}

/** 让任务栏重新取一次图标。窗口显示着时先给 Explorer 读取快捷方式的时间；启动时窗口还隐藏着，直接完成 */
async function refreshTaskbar(shell: ShellReport, current: number): Promise<void> {
  if (shell.refresh) await delay(2500)
  while (current === revision) {
    const finished = await invoke<boolean>('refresh_shell_icon', { generation: shell.generation })
    if (finished) break
    // 模态框、系统菜单或拖动期间保留原窗口，交互结束后再刷新。
    await delay(500)
  }
}

/** 窗口图标和快捷方式改好之后的收尾：刷新任务栏（refresh 为 true 时），没改成的快捷方式再试几次，最后记下这个主题做完了 */
async function settle(dark: boolean, current: number, first: ShellReport, refresh: boolean): Promise<void> {
  let shell = first
  if (refresh) await refreshTaskbar(shell, current)
  // 个别快捷方式没改成：只把快捷方式再试几次，不把整个过程重来。
  // 以前当成失败整个重来，每来一次任务栏就多刷新一次（窗口多隐藏、显示一次），一共三次
  for (const wait of SHORTCUT_RETRY_MS) {
    if (current !== revision || shell.warnings.length === 0) break
    await delay(wait)
    const again = await prepareIcon(dark, current, false)
    if (!again || current !== revision) return
    // 这次才改成的快捷方式，任务栏那边还是旧图标：补一次刷新；一个也没改成就不刷新
    if (again.updated > 0) await refreshTaskbar(again, current)
    shell = again
  }
  if (current !== revision) return
  if (shell.warnings.length > 0) {
    // 一直改不了的（只读、没有权限）：这个主题算做完，不再反复试；下次切主题或重新打开 Ferry 时再改
    console.warn('部分 Ferry 快捷方式的图标没改成', shell.warnings)
  }
  appliedTheme = dark
}

/** 启动时的收尾（没改成的快捷方式再试几次），不拦着窗口显示 */
async function settleStartup(dark: boolean, current: number, shell: ShellReport): Promise<void> {
  try {
    await settle(dark, current, shell, false)
  } catch (error) {
    if (current !== revision) return
    console.warn('同步 Ferry 主题图标失败，后续切换将重新尝试', error)
    requestedTheme = undefined
  }
}

/** 等窗口显示出来（启动时由 main.ts 显示，Rust 那边 3 秒后也会显示）；主题又换了、一直没显示时返回 false */
async function waitVisible(current: number): Promise<boolean> {
  for (let waited = 0; waited < 15000; waited += 200) {
    if (current !== revision) return false
    if (await getCurrentWindow().isVisible()) return true
    await delay(200)
  }
  return false
}

/** 窗口显示出来后隔这么久再改快捷方式（毫秒）：等任务栏把按钮建好。实测显示后 0.7 秒改，8 次全对 */
const AFTER_SHOW_MS = 500

/**
 * 启动时（窗口还没显示）发现快捷方式要改：上次关掉后系统换了深浅色、重装后快捷方式的图标被重置时会这样。
 * 这时先不改（sync_shell_icon 只检查），等窗口显示出来，再走平时切主题的那一套：改快捷方式、换窗口图标、
 * 等 2.5 秒刷新任务栏。任务栏在窗口出来两三秒后闪一下，换成对的图标。
 *
 * 任务栏只在「按钮已经在任务栏上、这时快捷方式变了」的情况下才可靠地换图标。窗口显示前就改好的话，
 * 任务栏建按钮时用的是它记着的这一组的旧图标，之后一直不变，直到下次切主题（实测每次都这样）。
 * 窗口显示前改好、再想办法补救的，实测都不行：
 * - 晚 1.5 秒再显示窗口：按钮照样是旧图标；
 * - 窗口显示后只刷新任务栏（等 2.5 秒、5 秒、十几秒）：一半以上没换成，刷新执行了但任务栏没重建按钮；
 * - 窗口显示后只再同步一次、不刷新：没换；
 * - 窗口显示后再同步一次、等 2.5 秒刷新：10 次里只有 3 次换成。
 */
async function changeAfterShow(dark: boolean, current: number): Promise<void> {
  try {
    if (!await waitVisible(current)) return
    await delay(AFTER_SHOW_MS)
    if (current === revision) await updateAppIcon(dark, current, true)
  } catch (error) {
    if (current !== revision) return
    console.warn('同步 Ferry 主题图标失败，后续切换将重新尝试', error)
    requestedTheme = undefined
  }
}

async function updateAppIcon(dark: boolean, current: number, shown = false): Promise<void> {
  for (let attempt = 0; attempt < 3 && current === revision; attempt++) {
    try {
      const shell = await prepareIcon(dark, current, true)
      if (!shell || current !== revision) return
      if (shell.pending) {
        // 启动时快捷方式要改：留到窗口显示出来再改，这里先返回，让 main.ts 显示窗口
        if (!shown) void changeAfterShow(dark, current)
        return
      }
      if (shell.refresh) {
        await settle(dark, current, shell, true)
        return
      }
      // 窗口还没显示（启动）、快捷方式本来就对：窗口图标已经换好，main.ts 等这里返回就显示窗口，剩下的在后面做
      void settleStartup(dark, current, shell)
      return
    } catch (error) {
      if (current !== revision) return
      if (attempt === 2) {
        console.warn('同步 Ferry 主题图标失败，后续切换将重新尝试', error)
        requestedTheme = undefined
        return
      }
      await delay(500 * (attempt + 1))
    }
  }
}
