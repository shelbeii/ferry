// 液态玻璃 Claude 版的独立实验入口。只有本地演示数据，不连接任何服务器。
import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import '../../style.css'
import GlassLab from './GlassLab.vue'

createApp(GlassLab).use(ElementPlus, { locale: zhCn }).mount('#app')
