// 检查更新：GitHub 上发布了新版本时提示，点「更新」下载这台电脑用的安装包（校验 SHA256）并安装（ferry update）。
// Windows：运行安装程序的更新模式，Ferry 关闭，装好后自动重新打开；macOS：打开 .dmg，拖进「应用程序」替换。
import { h, reactive } from 'vue'
import { ElNotification } from 'element-plus'
import { appIconPath } from './appIcon'
import { call, ferry, type Running } from './ferry'
import { exitApp } from './platform'
import { appSettings } from './state'

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
}

export const updater = reactive({
  info: undefined as UpdateInfo | undefined,
  checking: false,
  /** 上次检查失败的原因 */
  checkError: '',
  checkedAt: 0,
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

/** 有新版本可以更新 */
export function hasUpdate(): boolean {
  return !!updater.info?.newer
}

let notified = ''

/** 提示左边的图标：Ferry 的小图标，跟着主题用浅色 / 深色那张（两张都放着，style.css 按 html.dark 显示一张，提示开着时切主题也跟着换） */
const FerryNoteIcon = () =>
  h('span', { class: 'ferry-note-icon' }, [
    h('img', { class: 'light-only', src: appIconPath(false), alt: '' }),
    h('img', { class: 'dark-only', src: appIconPath(true), alt: '' }),
  ])

/** 查一次。manual：用户点的（失败时抛出）；自动检查发现新版本时弹出提示（每个版本只提示一次）。 */
export async function checkUpdate(manual = false): Promise<UpdateInfo | undefined> {
  if (updater.checking || updater.phase) return updater.info
  updater.checking = true
  try {
    const info = await call<UpdateInfo>(['update', 'check'])
    updater.info = info
    updater.checkError = ''
    updater.checkedAt = Date.now()
    if (!manual && info.newer && notified !== info.latest && !updater.dialog) {
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

/** 打开 Ferry 后过一会儿查一次，之后每 6 小时查一次（设置 → 关于 →「自动检查更新」关掉时不查）。 */
export function startAutoCheck(): void {
  if (started) return
  started = true
  const tick = () => {
    if (appSettings.value?.ui.checkUpdate === false) return
    void checkUpdate()
  }
  window.setTimeout(tick, 15_000)
  window.setInterval(tick, 6 * 3600_000)
}

let job: Running<{ message: string }> | undefined

/** 下载并安装新版本。Windows 上安装程序启动后 Ferry 退出。 */
export async function installUpdate(): Promise<void> {
  const info = updater.info
  if (!info?.newer || !info.install || updater.phase) return
  Object.assign(updater, { phase: 'download', done: 0, total: info.size ?? 0, error: '', message: '' })
  const args = ['update', 'install']
  if (info.install === 'installer') args.push('--restart')
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
    // 让人看清「正在安装」再关；安装程序也会关掉还开着的 Ferry
    await new Promise((ok) => window.setTimeout(ok, 1200))
    await exitApp()
  } else {
    updater.phase = 'opened'
  }
}

/** 取消下载 */
export async function cancelUpdate(): Promise<void> {
  await job?.kill()
}
