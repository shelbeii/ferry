import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import './style.css'
import App from './App.vue'
import { HOVER_DELAY } from './ui'
import { getCurrentWindow } from '@tauri-apps/api/window'
import { isTauri } from './platform'
import { startTheme } from './state'
import { notifyWindowShown } from './appIcon'
import { preloadLater } from './preload'
import { installGlassMenus } from './glass/menus'
import { installGlassSegments } from './glass/segments'
import { installGlassButtons } from './glass/buttons'
import { installTextSelectionGuard } from './textSelection'
import { dialogTransition } from './fx/dialogParticles'
import { installPopperParticles } from './fx/popperParticles'

// 所有对话框打开、关闭时的粒子动画（见 fx/dialogParticles.ts）；
// 表格的 show-overflow-tooltip 默认用 fixed 定位并挂在 tableWrapper 内，开启液态玻璃时弹窗/抽屉上的 backdrop-filter
// 会新建 fixed 包含块而 Popper.js 不识别 backdrop-filter，导致气泡错位且可能被 overflow: hidden 裁切，统一挂到 body。
const app = createApp(App).use(ElementPlus, {
  locale: zhCn,
  dialog: { transition: dialogTransition },
  table: { tooltipOptions: { appendTo: 'body' } },
})
// 按记着的主题换好标题栏和图标就显示窗口，不等后台读设置（见 state.ts 的 startTheme）
const themeReady = startTheme()
// 所有悬停说明（el-tooltip、表格里过长省略的单元格）都写 :show-after="HOVER_DELAY"
app.config.globalProperties.HOVER_DELAY = HOVER_DELAY
app.mount('#app')
installGlassButtons()
installTextSelectionGuard()
preloadLater()
// 下拉框里的玻璃液滴高亮（弹出层都由 Element Plus 生成，全局监听，见 glass/menus.ts）
installGlassMenus()
// 分段切换（el-radio-button）选中项下面的玻璃液滴（见 glass/segments.ts）
installGlassSegments()
// 下拉框打开、收起时的粒子动画（见 fx/popperParticles.ts）
installPopperParticles()

// 窗口在 Rust 里先隐藏：界面画好、主题切好再显示，免得先看到白底或浅色（Rust 那边 3 秒后也会显示，防止这里出错时一直看不到窗口）
if (isTauri) {
  const w = getCurrentWindow()
  void themeReady.finally(() => w.show().then(notifyWindowShown, () => {}))
}
