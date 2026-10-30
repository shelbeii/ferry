// 对话框、抽屉的代码是用到时才加载的（启动快）。代价是第一次打开时要现场加载、解析、执行它们
// （编辑器、设置有几十到上百毫秒），弹出动画会卡一下。所以启动后等界面空闲，挨个把它们先加载好：
// 只加载代码、不显示任何东西；之后真正打开时直接用已经加载好的。
const modules = [
  () => import('./components/SettingsDialog.vue'),
  () => import('./components/AiDrawer.vue'),
  () => import('./components/ConfigEditor.vue'),
  () => import('./components/DeployDialog.vue'),
  () => import('./components/LogsDialog.vue'),
  () => import('./components/ReleasesDialog.vue'),
  () => import('./components/HistoryDialog.vue'),
  () => import('./components/FilesDrawer.vue'),
  () => import('./components/TerminalView.vue'),
  () => import('./components/NewProjectDialog.vue'),
  () => import('./components/ConnectionDialog.vue'),
  () => import('./components/KeygenDialog.vue'),
  () => import('./components/LocalDialog.vue'),
  () => import('./components/TemplateEditor.vue'),
  () => import('./components/CredentialDialog.vue'),
  () => import('./components/BusyDialog.vue'),
]

/** 启动 delay 毫秒后开始，每次空闲加载一个，不和启动、用户操作抢时间 */
export function preloadLater(delay = 1500) {
  let next = 0
  const one = () => {
    if (next >= modules.length) return
    requestIdleCallback(() => { modules[next++]().catch(() => {}).finally(one) }, { timeout: 5000 })
  }
  setTimeout(one, delay)
}
