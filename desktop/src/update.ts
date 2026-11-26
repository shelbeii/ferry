// 检查更新：GitHub 上发布了新版本时提示，点「更新」下载这台电脑用的安装包（校验 SHA256）并安装（ferry update）。
// Windows：运行安装程序的更新模式，先优雅关闭 ferry serve 与 Ferry 主程序，装好后自动重新打开；macOS：打开 .dmg，拖进「应用程序」替换。
// 同时在启动时做本机健康检查：核对桌面版与内置 ferry 命令行版本是否一致、检查是否仍有旧版 ferry.exe.old-* 被外部进程（如 ferry mcp）占用。
import { defineComponent, h, reactive } from 'vue'
import { ElNotification } from 'element-plus'
import { appIconPath } from './appIcon'
import { call, ferry, type Running } from './ferry'
import { appInfo, cleanupOldBinaries, exitApp, isTauri, shutdownFerry } from './platform'
import { appSettings, resolvedTheme } from './state'

/** ferry update check 的结果 */
export interface UpdateInfo {
  current: string
  latest: string
  newer: boolean
  page: string
  notes?: string
  published?: string
  asset?: string
  size?: number
  /** installer（Windows 安装包）、dmg（macOS）、replace（Linux）；空 = 这种系统没有安装包，到发布页下载 */
  install: '' | 'installer' | 'dmg' | 'replace'
  downloaded?: string
  /** 除当前进程外，还有多少个外部进程占着 ferry 程序（如编程软件的 ferry mcp 或终端命令） */
  otherProcesses?: number
  /** 清理后仍被旧进程占用的 ferry.exe.old-* 数量 */
  staleBinaries?: number
}

export interface VersionMismatch {
  desktop: string
  cli: string
}

/** 提取正式语义化版本号（去掉前缀 v 及 +commit 等构建元数据）；dev 等非正式版本号返回空串。 */
export function baseVersion(v: string | undefined): string {
  if (!v) return ''
  const s = v.trim().replace(/^v/i, '').split(/[+-]/)[0] ?? ''
  return /^\d+\.\d+\.\d+$/.test(s) ? s : ''
}

/** 比较桌面程序与内置 ferry 命令行的基础版本号；二者都是正式版本且不一致时返回差异。 */
export function detectVersionMismatch(desktop: string | undefined, cli: string | undefined): VersionMismatch | undefined {
  const d = baseVersion(desktop)
  const c = baseVersion(cli)
  if (!d || !c || d === c) return undefined
  return { desktop: d, cli: c }
}

export const updater = reactive({
  info: undefined as UpdateInfo | undefined,
  checking: false,
  /** 上次检查失败的原因 */
  checkError: '',
  checkedAt: 0,
  /** 桌面程序与内置 ferry 命令行版本不一致（上次更新未完整覆盖） */
  mismatch: undefined as VersionMismatch | undefined,
  /** 启动清理后仍被外部进程（如 ferry mcp）占用的旧版 ferry.exe.old-* 数量 */
  staleBinaries: 0,
  /** 更新对话框开着 */
  dialog: false,
  /** download 下载中、install 正在启动安装、opened 已打开 .dmg（macOS） */
  phase: '' as '' | 'download' | 'install' | 'opened',
  done: 0,
  total: 0,
  /** 下载、安装失败的原因 */
  error: '',
  message: '',
})

/** 有新版本可以更新，或组件版本不一致需要重新安装修复 */
export function hasUpdate(): boolean {
  return !!updater.info?.newer || !!updater.mismatch
}

let notified = ''
let notifiedMismatch = ''
let notifiedStale = false

/** 提示左边的图标：跟着当前主题实时切换对应版本的 Ferry 小图标 */
const FerryNoteIcon = defineComponent({
  name: 'FerryNoteIcon',
  render: () => h('span', { class: 'ferry-note-icon' }, [h('img', { src: appIconPath(resolvedTheme.value), alt: '' })]),
})

function notifyMismatch(m: VersionMismatch): void {
  const key = `${m.desktop}:${m.cli}`
  if (notifiedMismatch === key || updater.dialog) return
  notifiedMismatch = key
  const n = ElNotification({
    title: 'Ferry 组件版本不一致',
    message: `桌面程序（${m.desktop}）与内置命令（${m.cli}）版本不一致，可能是上次更新未完成，点这里重新安装修复`,
    icon: FerryNoteIcon,
    duration: 20000,
    position: 'bottom-right',
    onClick: () => {
      n.close()
      openUpdateDialog()
    },
  })
}

function notifyStaleBinaries(count: number): void {
  if (count <= 0 || notifiedStale) return
  notifiedStale = true
  ElNotification({
    title: '旧版 ferry 仍在其他软件中运行',
    message: `检测到 ${count} 个旧版 ferry 进程仍被占用（通常是编程软件启动的 ferry mcp 或终端命令）。重启对应编程软件或重新连接 MCP 后就会切换为新版本。`,
    icon: FerryNoteIcon,
    duration: 18000,
    position: 'bottom-right',
  })
}

/** 打开更新对话框；若因版本不一致打开且尚未拉取发布信息，自动带 --force 查一次。 */
export function openUpdateDialog(): void {
  updater.dialog = true
  if (!updater.info && updater.mismatch && !updater.checking && !updater.phase) {
    void checkUpdate(false, true)
  }
}

/**
 * 纯本机启动健康检查（不联网，不受「自动检查更新」开关限制）：
 * 1. 核对桌面版与内置 ferry 命令行版本是否一致，发现割裂立即提示修复；
 * 2. 清理安装目录残留的 ferry.exe.old-*，若仍有被外部进程锁住的旧版文件，提示用户重启对应 IDE / MCP 客户端。
 */
export async function checkInstallHealth(): Promise<void> {
  if (!isTauri) return
  const [app, cli, locked] = await Promise.all([
    appInfo().catch(() => undefined),
    call<{ version: string }>(['version']).catch(() => undefined),
    cleanupOldBinaries().catch(() => [] as string[]),
  ])
  if (app && cli) {
    const mismatch = detectVersionMismatch(app.version, cli.version)
    updater.mismatch = mismatch
    if (mismatch) notifyMismatch(mismatch)
  }
  updater.staleBinaries = locked.length
  if (locked.length > 0) notifyStaleBinaries(locked.length)
}

/** 查一次。manual：用户点的（失败时抛出）；force：版本不一致时即使命令行已是最新也拉取发布信息。 */
export async function checkUpdate(manual = false, force = false): Promise<UpdateInfo | undefined> {
  if (updater.checking || updater.phase) return updater.info
  updater.checking = true
  try {
    const [desktopVer, firstInfo] = await Promise.all([
      isTauri ? appInfo().then((a) => a.version).catch(() => undefined) : Promise.resolve(undefined),
      call<UpdateInfo>(force || updater.mismatch ? ['update', 'check', '--force'] : ['update', 'check']),
    ])
    let info = firstInfo
    const mismatch = isTauri ? detectVersionMismatch(desktopVer, info.current) : undefined
    updater.mismatch = mismatch
    if (mismatch && !info.newer && !force && !info.notes && !info.size) {
      info = await call<UpdateInfo>(['update', 'check', '--force']).catch(() => info)
    }
    updater.info = info
    updater.checkError = ''
    updater.checkedAt = Date.now()
    if ((info.staleBinaries ?? 0) > updater.staleBinaries) {
      updater.staleBinaries = info.staleBinaries ?? 0
    }
    if (!manual) {
      if (info.newer && notified !== info.latest && !updater.dialog) {
        notified = info.latest
        const n = ElNotification({
          title: 'Ferry 有新版本',
          message: `${info.latest} 已发布（这台电脑上是 ${info.current.replace(/\+.*$/, '')}），点这里看更新了什么`,
          icon: FerryNoteIcon, // 不设 type：设了会用 Element 自带的 ⓘ 图标
          duration: 15000,
          position: 'bottom-right',
          onClick: () => {
            n.close()
            updater.dialog = true
          },
        })
      } else if (mismatch) {
        notifyMismatch(mismatch)
      }
      if ((info.staleBinaries ?? 0) > 0) {
        notifyStaleBinaries(info.staleBinaries!)
      }
    }
    return info
  } catch (e) {
    updater.checkError = (e as Error).message
    if (manual) throw e
    return undefined
  } finally {
    updater.checking = false
  }
}

let started = false

/** 打开 Ferry 后先做本机版本一致性与残留进程检查，过一会儿联网查一次，之后每 6 小时查一次（设置 → 关于 →「自动检查更新」关掉时不联网查）。 */
export function startAutoCheck(): void {
  if (started) return
  started = true
  window.setTimeout(() => {
    void checkInstallHealth()
  }, 2_000)
  const tick = () => {
    if (appSettings.value?.ui.checkUpdate === false) return
    void checkUpdate()
  }
  window.setTimeout(tick, 15_000)
  window.setInterval(tick, 6 * 3600_000)
}

let job: Running<{ message: string }> | undefined

/** 下载并安装新版本（或在组件版本不一致时强制重装修复）。Windows 上先优雅停掉 ferry serve，再立即退出 Ferry 让安装程序干净覆盖。 */
export async function installUpdate(): Promise<void> {
  const info = updater.info
  const repair = !info?.newer && !!updater.mismatch
  if ((!info?.newer && !repair) || !info?.install || updater.phase) return
  Object.assign(updater, { phase: 'download', done: 0, total: info.size ?? 0, error: '', message: '' })
  const args = ['update', 'install']
  if (info.install === 'installer') args.push('--restart')
  if (repair) args.push('--force')
  job = ferry<{ message: string }>(args, (l) => {
    if (l.kind === 'progress') {
      updater.done = l.done ?? 0
      if (l.total) updater.total = l.total
    } else if (l.kind === 'step' && l.msg?.startsWith('安装')) {
      updater.phase = 'install'
    }
  })
  const r = await job.done
  job = undefined
  if (!r.ok) {
    updater.phase = ''
    updater.error = r.error === '已取消' ? '' : r.error ?? '更新失败'
    return
  }
  updater.message = r.data?.message ?? ''
  if (info.install === 'installer') {
    updater.phase = 'install'
    // 先让常驻的 ferry serve 主动退出释放 ferry.exe 文件锁，再立刻退出主程序，避免与 NSIS 覆盖竞争
    await shutdownFerry()
    await exitApp()
  } else {
    updater.phase = 'opened'
  }
}

/** 取消下载 */
export async function cancelUpdate(): Promise<void> {
  await job?.kill()
}
