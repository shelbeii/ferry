import { getCurrentWindow } from '@tauri-apps/api/window'
import { invoke } from '@tauri-apps/api/core'
import { isTauri } from './platform'
import { isDarkTheme, isResolvedTheme, type ResolvedTheme } from './theme'

function normalizeTheme(theme: ResolvedTheme | boolean): ResolvedTheme {
  if (typeof theme === 'boolean') return theme ? 'dark' : 'light'
  return isResolvedTheme(theme) ? theme : 'light'
}

export function appIconPath(theme: ResolvedTheme | boolean): string {
  return `/branding/ferry-${normalizeTheme(theme)}.png`
}

/** 仅更新页面 favicon（预览与正式切换共用，不触碰 Windows 快捷方式与任务栏） */
export function syncFavicon(theme: ResolvedTheme | boolean): void {
  const favicon = document.getElementById?.('ferry-favicon') as HTMLLinkElement | null | undefined
  if (favicon) favicon.href = appIconPath(theme)
}

export interface SyncAppIconOptions {
  /** 系统深浅色事件防抖（毫秒），避免短时间内连发多次导致反复写 .lnk 与 Explorer 通知乱序 */
  debounceMs?: number
}

const iconBytes = new Map<string, Promise<ArrayBuffer>>()
let revision = 0
let appliedTheme: ResolvedTheme | undefined
/** 最近一次要求的主题（正在做或已经做完）；做失败了清掉，下次同样的主题重新做 */
let requestedTheme: ResolvedTheme | undefined
let nativeUpdate = Promise.resolve()
let latestUpdate = Promise.resolve()
/** 启动时主窗口刚显示出来的时刻：若紧接着收到与缓存不同的正式设置，先等满 AFTER_SHOW_MS 再改快捷方式 */
let windowShownAt: number | undefined

interface ShellReport {
  updated: number
  warnings: string[]
  /** 属于临时文件占用（Explorer / 索引器 / OneDrive 锁住）导致的失败数 */
  busy?: number
  generation: number
  refresh: boolean
  /** 窗口还没显示、有这么多个快捷方式要改：这次什么都没改，等窗口显示出来再同步一次（见 changeAfterShow） */
  pending?: number
}

/** 个别快捷方式没改成（被占用）时，隔这么久再试（毫秒）；只重试快捷方式，不重复刷新任务栏 */
const SHORTCUT_RETRY_MS = [1000, 3000]
/** 窗口显示出来后隔这么久再改快捷方式（毫秒）：等任务栏把按钮建好。实测显示后 0.7 秒改，8 次全对 */
const AFTER_SHOW_MS = 700
/** 启动推迟后最多等待多少轮可见态，避免窗口瞬时隐藏导致静默丢弃并锁死 requestedTheme */
const MAX_PENDING_ROUNDS = 3

const delay = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms))

/** 主窗口刚由 main.ts 显示时调用，避免紧随其后的正式设置主题撞上任务栏按钮初始化期 */
export function notifyWindowShown(): void {
  windowShownAt = Date.now()
}

/** 启动期间主题可能被正式设置或系统事件替换，等待最后一次更新。 */
export async function waitForAppIcon(): Promise<void> {
  let pending: Promise<void>
  do {
    pending = latestUpdate
    await pending
  } while (pending !== latestUpdate)
}

/** 同步窗口、桌面和任务栏图标；原生更新串行，避免旧请求覆盖新主题。 */
export function syncAppIcon(input: ResolvedTheme | boolean, options: SyncAppIconOptions = {}): Promise<void> {
  const theme = normalizeTheme(input)
  syncFavicon(theme)
  if (!isTauri) return Promise.resolve()
  // 同一个主题正在做或已经做完：不重新来一遍（保存设置、切换项目、启动时读到正式设置都会再要求一次同样的主题，
  // 重来会重新扫快捷方式，并把等着刷新任务栏的那 2.5 秒从头算）
  if (requestedTheme === theme) return latestUpdate

  const debounceMs = options.debounceMs ?? 0
  requestedTheme = theme
  const current = ++revision
  // 如果取消了一个尚在防抖等待期的新主题、且当前请求的主题本来就已经完整生效，无需再做任何原生写入。
  if (appliedTheme === theme) {
    latestUpdate = Promise.resolve()
    return latestUpdate
  }
  latestUpdate = updateAppIcon(theme, current, 0, debounceMs)
  return latestUpdate
}

/** 改快捷方式和窗口属性；withIcon 时接着换窗口图标。主题已经换成别的、或这个主题已经做完时返回 undefined */
function prepareIcon(theme: ResolvedTheme, current: number, withIcon: boolean): Promise<ShellReport | undefined> {
  const update = nativeUpdate.then(async () => {
    if (current !== revision || appliedTheme === theme) return
    let icon: ArrayBuffer | undefined
    if (withIcon) {
      const path = appIconPath(theme)
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
    // 快捷方式还没改好就记成旧的，之后任务栏按钮一直是旧图标（见 shellicon.rs 的 sync）。
    // sync_shell_icon 抛异常时绝不能在 finally 里单方面换窗口图标，避免桌面和任务栏分裂。
    const report = await invoke<ShellReport>('sync_shell_icon', { theme, dark: isDarkTheme(theme) })
    // 快捷方式留到窗口显示后再改的，窗口图标也留到那时一起换（和平时切主题一样的顺序）
    if (icon && current === revision && !report.pending) await getCurrentWindow().setIcon(icon)
    return report
  })
  // 只串行化实际写入，等待任务栏或重试期间允许新主题继续更新。
  nativeUpdate = update.then(() => undefined, () => undefined)
  return update
}

function hasRetryableWarnings(shell: ShellReport): boolean {
  if (shell.busy !== undefined) return shell.busy > 0
  return shell.warnings.length > 0
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

/** 窗口图标和快捷方式改好之后的收尾：刷新任务栏（refresh 为 true 时），临时占用的快捷方式再试几次，最后记下状态 */
async function settle(theme: ResolvedTheme, current: number, first: ShellReport, refresh: boolean): Promise<void> {
  let shell = first
  if (refresh) await refreshTaskbar(shell, current)
  // 个别快捷方式因临时占用没改成：只把快捷方式再试几次，不把整个过程重来。
  // 属于只读/无权限（busy === 0）的直接跳过无效重试，减少不必要的扫描与延迟。
  for (const wait of SHORTCUT_RETRY_MS) {
    if (current !== revision || !hasRetryableWarnings(shell)) break
    await delay(wait)
    const again = await prepareIcon(theme, current, false)
    if (!again || current !== revision) return
    // 这次才改成的快捷方式，任务栏那边还是旧图标：补一次刷新；一个也没改成就不刷新
    if (again.updated > 0) await refreshTaskbar(again, current)
    shell = again
  }
  if (current !== revision) return
  if (shell.warnings.length > 0) {
    console.warn('部分 Ferry 快捷方式的图标没改成', shell.warnings)
  }
  if ((shell.busy ?? 0) > 0) {
    // 仍存在临时文件占用失败：不把该主题标为已完成，允许后续同主题或重启自愈。
    requestedTheme = undefined
    return
  }
  // 没有临时占用（全部成功，或仅剩只读/无权限等永久性警告）：这个主题算做完，不再反复试。
  appliedTheme = theme
}

/** 启动时的收尾（没改成的快捷方式再试几次），不拦着窗口显示 */
async function settleStartup(theme: ResolvedTheme, current: number, shell: ShellReport): Promise<void> {
  try {
    await settle(theme, current, shell, false)
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

/**
 * 启动时（窗口还没显示）发现快捷方式要改：上次关掉后系统换了深浅色、重装后快捷方式的图标被重置时会这样。
 * 这时先不改（sync_shell_icon 只检查），等窗口显示出来，再走平时切主题的那一套：改快捷方式、换窗口图标、
 * 等 2.5 秒刷新任务栏。任务栏在窗口出来两三秒后闪一下，换成对的图标。
 */
async function changeAfterShow(theme: ResolvedTheme, current: number, round = 0): Promise<void> {
  try {
    if (!await waitVisible(current)) {
      if (current === revision) requestedTheme = undefined
      return
    }
    await delay(AFTER_SHOW_MS)
    if (current === revision) await updateAppIcon(theme, current, round + 1, 0)
  } catch (error) {
    if (current !== revision) return
    console.warn('同步 Ferry 主题图标失败，后续切换将重新尝试', error)
    requestedTheme = undefined
  }
}

async function updateAppIcon(theme: ResolvedTheme, current: number, pendingRound = 0, debounceMs = 0): Promise<void> {
  if (debounceMs > 0) {
    await delay(debounceMs)
    if (current !== revision) return
  }
  if (windowShownAt !== undefined && pendingRound === 0) {
    const remaining = AFTER_SHOW_MS - (Date.now() - windowShownAt)
    if (remaining > 0) {
      await delay(remaining)
      if (current !== revision) return
    }
  }
  for (let attempt = 0; attempt < 3 && current === revision; attempt++) {
    try {
      const shell = await prepareIcon(theme, current, true)
      if (!shell || current !== revision) return
      if (shell.pending) {
        // 启动时快捷方式要改：留到窗口显示出来再改，这里先返回，让 main.ts 显示窗口
        if (pendingRound < MAX_PENDING_ROUNDS) {
          void changeAfterShow(theme, current, pendingRound)
        } else if (current === revision) {
          requestedTheme = undefined
        }
        return
      }
      if (shell.refresh) {
        await settle(theme, current, shell, true)
        return
      }
      // 窗口还没显示（启动）、快捷方式本来就对：窗口图标已经换好，main.ts 等这里返回就显示窗口，剩下的在后面做
      void settleStartup(theme, current, shell)
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
