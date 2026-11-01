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
import { uiPref } from './state'
import { preloadLater } from './preload'
import { installGlassMenus } from './glass/menus'
import { installGlassSegments } from './glass/segments'
import { installTextSelectionGuard } from './textSelection'

const app = createApp(App).use(ElementPlus, { locale: zhCn })
// 所有悬停说明（el-tooltip、表格里过长省略的单元格）都写 :show-after="HOVER_DELAY"
app.config.globalProperties.HOVER_DELAY = HOVER_DELAY
app.mount('#app')
installTextSelectionGuard()
preloadLater()
// 下拉框里的玻璃液滴高亮（弹出层都由 Element Plus 生成，全局监听，见 glass/menus.ts）
installGlassMenus()
// 分段切换（el-radio-button）选中项下面的玻璃液滴（见 glass/segments.ts）
installGlassSegments()

// 窗口在 Rust 里先隐藏：界面画好、主题切好再显示，免得先看到白底或浅色（Rust 那边 3 秒后也会显示，防止这里出错时一直看不到窗口）
if (isTauri) {
  const w = getCurrentWindow()
  const theme = uiPref('theme')
  ;(theme === 'light' || theme === 'dark' ? w.setTheme(theme) : Promise.resolve())
    .catch(() => {})
    .finally(() => w.show().catch(() => {}))
}
