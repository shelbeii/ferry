<script setup lang="ts">
// 实验页：项目标题、五个按钮、概况卡片、五项导航、浮动卡片，浮在一层会滚动、色彩丰富的内容上面，
// 用来调材质参数、看折射和动效。
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Monitor, ChatDotRound, FolderOpened, EditPen, Refresh, Grid, Box, Connection, User, DataLine } from '@element-plus/icons-vue'
import GlassPane from '../../components/glass/GlassPane.vue'
import GlassButton from '../../components/glass/GlassButton.vue'
import GlassTabs from '../../components/glass/GlassTabs.vue'
import GlassSwitch from '../../components/glass/GlassSwitch.vue'
import { glass, retone } from '../../glass/runtime'

const dark = ref(false)
const scene = ref<'app' | 'vivid' | 'grid'>('app')
const moving = ref(true)
const refraction = ref(100)
const frost = ref(100)
const tab = ref('apps')
const autoRefresh = ref(true)
const notify = ref(false)
const message = ref('')
const backdrop = ref<HTMLElement>()
const tabs = [
  { value: 'apps', label: '应用', icon: Grid },
  { value: 'software', label: '软件', icon: Box },
  { value: 'ports', label: '端口', icon: Connection },
  { value: 'users', label: '登录用户', icon: User },
  { value: 'server', label: '服务器信息', icon: DataLine },
]
const apps = [
  { name: 'gateway', kind: 'Go', hue: 250, status: '运行中', mem: '182 MB' },
  { name: 'shop-api', kind: 'Java', hue: 18, status: '运行中', mem: '644 MB' },
  { name: 'console', kind: '静态站点', hue: 165, status: '运行中', mem: '—' },
  { name: 'worker', kind: 'Python', hue: 45, status: '已停止', mem: '0 MB' },
  { name: 'billing', kind: '.NET', hue: 300, status: '运行中', mem: '312 MB' },
  { name: 'notifier', kind: 'Node', hue: 120, status: '运行中', mem: '96 MB' },
]
const logs = Array.from({ length: 22 }, (_, i) => ({
  time: `10:${String(12 + Math.floor(i / 2)).padStart(2, '0')}:${String((i * 7) % 60).padStart(2, '0')}`,
  level: ['INFO', 'INFO', 'WARN', 'INFO', 'DEBUG', 'ERROR'][i % 6],
  text: ['worker ready · pool=8', 'GET /api/tunnels 200 12ms', 'slow query 820ms on deployments', 'upload chunk 14/32 · 3.1 MB/s', 'health check passed', 'connection reset by peer'][i % 6],
}))
const spark = (seed: number) => Array.from({ length: 14 }, (_, i) => `${i * 10},${(22 - 14 * Math.abs(Math.sin(seed * 1.7 + i * .8)) - 4 * Math.sin(i * 2.1 + seed)).toFixed(1)}`).join(' ')

let message_timer: number | undefined
function say(text: string) {
  message.value = text
  clearTimeout(message_timer)
  message_timer = window.setTimeout(() => { message.value = '' }, 1800)
}

watch(dark, (value) => document.documentElement.classList.toggle('dark', value))
watch([dark, scene], () => nextTick(retone))
watch(refraction, (value) => { glass.refraction = value / 100 }, { immediate: true })
watch(frost, (value) => { glass.frost = value / 100 }, { immediate: true })

// 背景缓慢滚动，看玻璃后面的内容实时变化；窗口隐藏时停下
let frame: number | undefined
let offset = 0
function scroll() {
  frame = undefined
  const element = backdrop.value
  if (!moving.value || document.hidden || !element) return
  offset += .5
  if (offset >= element.scrollHeight - element.clientHeight) offset = 0
  element.scrollTop = offset
  frame = requestAnimationFrame(scroll)
}
function restart() {
  if (frame !== undefined) cancelAnimationFrame(frame)
  frame = undefined
  offset = backdrop.value?.scrollTop ?? 0
  if (moving.value) frame = requestAnimationFrame(scroll)
}
watch([moving, scene], restart)
// 玻璃上滚动滚轮也滚动背景
function wheel(event: WheelEvent) {
  if (!backdrop.value) return
  backdrop.value.scrollTop += event.deltaY
  offset = backdrop.value.scrollTop
}
onMounted(() => { document.addEventListener('visibilitychange', restart); restart() })
onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', restart)
  if (frame !== undefined) cancelAnimationFrame(frame)
  document.documentElement.classList.remove('dark')
})
</script>

<template>
  <main class="cgl">
    <header class="cgl-head">
      <div>
        <p class="cgl-eyebrow">FERRY / MATERIAL STUDY · CLAUDE</p>
        <h1>液态玻璃 · Claude 版</h1>
        <p class="cgl-sub">按物理折射算的玻璃边缘、跟着指针走的镜面高光、按住会变形的按钮、浮起的液滴导航</p>
      </div>
    </header>

    <section class="cgl-controls" aria-label="效果设置">
      <label>主题
        <el-radio-group v-model="dark" size="small"><el-radio-button :value="false">浅色</el-radio-button><el-radio-button :value="true">深色</el-radio-button></el-radio-group>
      </label>
      <label>背景
        <el-radio-group v-model="scene" size="small"><el-radio-button value="app">应用页面</el-radio-button><el-radio-button value="vivid">彩色</el-radio-button><el-radio-button value="grid">校准网格</el-radio-button></el-radio-group>
      </label>
      <label class="cgl-slider">折射 <el-slider v-model="refraction" :min="0" :max="200" :show-tooltip="false" /> <output>{{ refraction }}%</output></label>
      <label class="cgl-slider">磨砂 <el-slider v-model="frost" :min="0" :max="300" :show-tooltip="false" /> <output>{{ frost }}%</output></label>
      <label>色散 <el-switch v-model="glass.dispersion" size="small" /></label>
      <label>背景滚动 <el-switch v-model="moving" size="small" /></label>
    </section>

    <section class="cgl-stage" :class="`scene-${scene}`">
      <div ref="backdrop" class="cgl-backdrop">
        <template v-if="scene === 'app'">
          <div class="bg-hero">
            <span class="blob b1" /><span class="blob b2" /><span class="blob b3" />
            <div class="bg-hero-text"><small>PROJECT</small><strong>shop</strong><span>production · 3 台服务器 · 11 个应用</span></div>
          </div>
          <div class="bg-apps">
            <article v-for="(app, i) in apps" :key="app.name" class="bg-app" :style="{ '--h': app.hue }">
              <div class="bg-app-top"><b>{{ app.name }}</b><span>{{ app.kind }}</span></div>
              <svg viewBox="0 0 130 30" preserveAspectRatio="none"><polyline :points="spark(i)" /></svg>
              <div class="bg-app-foot"><i :class="{ off: app.status === '已停止' }">{{ app.status }}</i><span>{{ app.mem }}</span></div>
            </article>
          </div>
          <div class="bg-tiles"><span v-for="n in 8" :key="n" :style="{ '--h': n * 41 }" /></div>
          <pre class="bg-log"><template v-for="line in logs" :key="line.time + line.text"><span class="t">{{ line.time }}</span> <span :class="line.level">{{ line.level.padEnd(5) }}</span> {{ line.text }}
</template></pre>
          <div class="bg-rows">
            <div v-for="n in 18" :key="n" class="bg-row" :style="{ '--h': n * 53 }"><span class="av">{{ 'ABCDEFGHJKLMNPQRST'[n - 1] }}</span><b>deploy #{{ 1200 - n }}</b><span>{{ ['成功', '成功', '回滚', '成功', '失败'][n % 5] }}</span><span class="mono">{{ (n * 2654435761 % 0xffffff).toString(16).padStart(6, '0') }}</span></div>
          </div>
        </template>
        <div v-else-if="scene === 'vivid'" class="bg-vivid">
          <span class="orb o1" /><span class="orb o2" /><span class="orb o3" /><span class="orb o4" />
          <p v-for="n in 6" :key="n">Liquid Glass · 液态玻璃 · Ferry</p>
        </div>
        <div v-else class="bg-grid"><p v-for="n in 40" :key="n"><b>{{ String(n).padStart(2, '0') }}</b> Ferry · server resources / applications / deployment logs</p></div>
      </div>

      <div class="cgl-overlay" @wheel.passive="wheel">
        <div class="cgl-top">
          <GlassPane radius="pill" class="cgl-title"><span class="cgl-dot" /><b>shop</b><span class="cgl-mono">root@demo-server:22</span></GlassPane>
          <div class="cgl-actions">
            <GlassButton :icon="Monitor" @click="say('打开终端')">终端</GlassButton>
            <GlassButton :icon="ChatDotRound" @click="say('打开 AI 助手')">AI 助手</GlassButton>
            <GlassButton :icon="FolderOpened" @click="say('打开文件')">文件</GlassButton>
            <GlassButton :icon="EditPen" @click="say('编辑配置')">编辑配置</GlassButton>
            <GlassButton :icon="Refresh" aria-label="刷新" @click="say('已刷新')" />
          </div>
        </div>

        <GlassPane class="cgl-overview" :radius="28">
          <div><span>运行中</span><strong>4 <small>/ 11</small></strong><em>全部服务正常</em></div>
          <div><span>内存使用</span><strong>38.6<small>%</small></strong><em>6.2 GB / 16 GB</em></div>
          <div><span>最近部署</span><strong>10:42</strong><em>System · production</em></div>
        </GlassPane>

        <GlassPane class="cgl-card" :radius="28">
          <p class="cgl-eyebrow">LIVE BACKDROP</p>
          <h3>按住拖动试试</h3>
          <p>按住下方导航的选中项左右拖，它会浮起成一滴液体、放大下面的字；按钮按住也能拉动。</p>
          <div class="cgl-row"><span>自动刷新</span><GlassSwitch v-model="autoRefresh" aria-label="自动刷新" /></div>
          <div class="cgl-row"><span>部署通知</span><GlassSwitch v-model="notify" aria-label="部署通知" /></div>
        </GlassPane>

        <Transition name="cgl-toast">
          <GlassPane v-if="message" radius="pill" class="cgl-toast" role="status"><span>{{ message }}</span></GlassPane>
        </Transition>

        <div class="cgl-dock"><GlassTabs v-model="tab" :options="tabs" label="切换服务器功能" /></div>
      </div>
    </section>

    <section class="cgl-notes">
      <h2>这一版怎么做的</h2>
      <ul>
        <li><b>折射</b>：把玻璃边缘当成真实的凸面（超椭圆轮廓、折射率 1.5），按斯涅尔定律算出每个像素看到哪里的背景，生成位移图交给 <code>backdrop-filter</code>。平顶部分不偏折，所以中间清楚、边缘一圈把背景弯过来。</li>
        <li><b>色散</b>：红绿蓝三个通道折射量差 ±4%，边缘有很淡的彩边。</li>
        <li><b>高光</b>：外沿一圈镜面反光，朝向指针的一侧最亮、对侧有较弱的反光，指针靠近时更亮；内沿有厚度感的亮边。</li>
        <li><b>材质分两种</b>：面板、导航条用 regular（轻磨砂 + 提饱和），按钮、液滴用 clear（几乎透明，主要靠折射）。文字不经过滤镜，始终清晰。</li>
        <li><b>动效</b>：全部是弹簧，改方向时保留速度。按钮按下会胀大并从按下处亮起，拖动时被拉长；导航和开关按下后选中块浮起成放大镜一样的液滴，移动越快拉得越长，到位后落回。</li>
      </ul>
    </section>
  </main>
</template>

<style>
body:has(.cgl) { overflow: auto; background: var(--el-bg-color-page); }
.cgl { max-width: 1180px; margin: auto; padding: 34px 32px 56px; color: var(--el-text-color-primary); }
.cgl-head { display: flex; justify-content: space-between; align-items: flex-end; gap: 20px; margin-bottom: 22px; }
.cgl-eyebrow { margin: 0 0 8px; font-size: 11px; font-weight: 600; letter-spacing: 2px; color: var(--el-color-primary); }
.cgl-head h1 { margin: 0 0 8px; font-size: 28px; letter-spacing: -.6px; }
.cgl-sub { margin: 0; color: var(--el-text-color-secondary); font-size: 13px; }
.cgl-controls { display: flex; flex-wrap: wrap; align-items: center; gap: 14px 24px; margin-bottom: 18px; font-size: 13px; color: var(--el-text-color-regular); }
.cgl-controls label { display: inline-flex; align-items: center; gap: 10px; }
.cgl-slider .el-slider { width: 130px; }
.cgl-slider output { width: 38px; font-variant-numeric: tabular-nums; color: var(--el-text-color-secondary); }

.cgl-stage { position: relative; height: 640px; border-radius: 30px; overflow: hidden; background: #eef1f7; }
html.dark .cgl-stage { background: #0d121c; }
.cgl-backdrop { position: absolute; inset: 0; overflow: auto; scrollbar-width: none; }
.cgl-overlay { position: absolute; inset: 0; pointer-events: none; }
.cgl-overlay > *, .cgl-top > * { pointer-events: auto; }
.cgl-top { position: absolute; top: 18px; left: 20px; right: 20px; display: flex; justify-content: space-between; align-items: center; gap: 12px; pointer-events: none; }
.cgl-title { display: inline-flex; align-items: center; gap: 10px; height: 44px; padding: 0 18px; font-size: 15px; }
.cgl-dot { width: 8px; height: 8px; border-radius: 50%; background: #30d158; box-shadow: 0 0 0 3px rgb(48 209 88 / .25); }
.cgl-mono { font: 12px Consolas, monospace; color: var(--el-text-color-secondary); }
.cgl-actions { display: flex; gap: 8px; }
.cgl-overview { position: absolute; top: 86px; left: 20px; width: min(600px, calc(100% - 380px)); display: grid; grid-template-columns: repeat(3, 1fr); padding: 22px 26px; box-sizing: border-box; }
.cgl-overview > div { display: flex; flex-direction: column; gap: 6px; }
.cgl-overview span, .cgl-overview em { font-size: 12px; font-style: normal; color: var(--el-text-color-secondary); }
.cgl-overview strong { font-size: 30px; font-weight: 600; letter-spacing: -.5px; font-variant-numeric: tabular-nums; }
.cgl-overview small { font-size: 14px; font-weight: 500; color: var(--el-text-color-secondary); }
.cgl-card { position: absolute; top: 86px; right: 20px; width: 320px; padding: 22px 24px 14px; box-sizing: border-box; }
.cgl-card h3 { margin: 0 0 8px; font-size: 17px; }
.cgl-card > p:not(.cgl-eyebrow) { margin: 0 0 10px; font-size: 13px; line-height: 1.7; color: var(--el-text-color-regular); }
.cgl-row { display: flex; justify-content: space-between; align-items: center; padding: 10px 0; font-size: 14px; border-top: 1px solid rgb(127 127 140 / .18); }
.cgl-dock { position: absolute; left: 0; right: 0; bottom: 22px; display: flex; justify-content: center; pointer-events: none; }
.cgl-dock > * { pointer-events: auto; }
.cgl-toast { position: absolute; left: 50%; bottom: 92px; transform: translateX(-50%); padding: 10px 18px; font-size: 13px; white-space: nowrap; }
.cgl-toast-enter-active, .cgl-toast-leave-active { transition: transform .45s cubic-bezier(.3, 1.5, .5, 1), opacity .25s; }
.cgl-toast-enter-from, .cgl-toast-leave-to { opacity: 0; transform: translateX(-50%) translateY(14px) scale(.85); }

/* 背景：应用页面 */
.bg-hero { position: relative; height: 330px; overflow: hidden; background: linear-gradient(120deg, #4f46e5, #0ea5e9 45%, #14b8a6 70%, #f59e0b); }
.blob { position: absolute; border-radius: 50%; filter: blur(30px); opacity: .85; }
.b1 { width: 360px; height: 360px; left: -60px; top: -120px; background: #ec4899; }
.b2 { width: 300px; height: 300px; right: 120px; top: 60px; background: #facc15; }
.b3 { width: 260px; height: 260px; left: 40%; bottom: -150px; background: #22d3ee; }
.bg-hero-text { position: absolute; left: 40px; bottom: 34px; color: #fff; display: flex; flex-direction: column; gap: 4px; text-shadow: 0 2px 20px rgb(0 0 0 / .25); }
.bg-hero-text small { font-size: 12px; letter-spacing: 3px; opacity: .8; }
.bg-hero-text strong { font-size: 64px; letter-spacing: -2px; line-height: 1; }
.bg-apps { display: grid; grid-template-columns: repeat(3, 1fr); gap: 16px; padding: 24px 24px 8px; }
.bg-app { border-radius: 18px; padding: 16px; color: #fff; background: linear-gradient(145deg, hsl(var(--h) 80% 58%), hsl(calc(var(--h) + 40) 75% 45%)); }
.bg-app-top, .bg-app-foot { display: flex; justify-content: space-between; font-size: 13px; }
.bg-app-top b { font-size: 16px; }
.bg-app svg { width: 100%; height: 44px; margin: 10px 0; }
.bg-app polyline { fill: none; stroke: #fff; stroke-width: 2; vector-effect: non-scaling-stroke; }
.bg-app i { font-style: normal; padding: 2px 8px; border-radius: 10px; background: rgb(255 255 255 / .25); }
.bg-app i.off { background: rgb(0 0 0 / .25); }
.bg-tiles { display: grid; grid-template-columns: repeat(8, 1fr); gap: 10px; padding: 16px 24px; }
.bg-tiles span { aspect-ratio: 1; border-radius: 16px; background: radial-gradient(circle at 30% 25%, hsl(var(--h) 95% 75%), transparent 60%), conic-gradient(from calc(var(--h) * 1deg), hsl(var(--h) 85% 55%), hsl(calc(var(--h) + 90) 85% 55%), hsl(var(--h) 85% 55%)); }
.bg-log { margin: 16px 24px; padding: 18px 20px; border-radius: 16px; background: #0f172a; color: #cbd5e1; font: 13px/1.75 Consolas, monospace; }
.bg-log .t { color: #64748b; } .bg-log .INFO { color: #34d399; } .bg-log .WARN { color: #fbbf24; } .bg-log .ERROR { color: #f87171; } .bg-log .DEBUG { color: #818cf8; }
.bg-rows { padding: 8px 24px 120px; }
.bg-row { display: grid; grid-template-columns: 36px 1fr 80px 90px; align-items: center; gap: 14px; padding: 12px 0; border-bottom: 1px solid rgb(127 127 140 / .2); font-size: 14px; }
.bg-row .av { display: grid; place-items: center; width: 34px; height: 34px; border-radius: 50%; color: #fff; font-weight: 700; background: hsl(var(--h) 70% 55%); }
.bg-row .mono { font: 12px Consolas, monospace; color: var(--el-text-color-secondary); }
html.dark .bg-row { color: #e2e8f0; }

/* 背景：彩色 */
.bg-vivid { position: relative; height: 1400px; overflow: hidden; background: #1e1b4b; }
html:not(.dark) .bg-vivid { background: #fdf2f8; }
.orb { position: absolute; border-radius: 50%; filter: blur(8px); animation: cgl-float 14s ease-in-out infinite alternate; }
.o1 { width: 420px; height: 420px; left: 4%; top: 40px; background: radial-gradient(circle at 35% 35%, #f472b6, #db2777); }
.o2 { width: 360px; height: 360px; right: 6%; top: 120px; background: radial-gradient(circle at 35% 35%, #67e8f9, #0284c7); animation-duration: 11s; animation-direction: alternate-reverse; }
.o3 { width: 300px; height: 300px; left: 38%; top: 330px; background: radial-gradient(circle at 35% 35%, #fde047, #f97316); animation-duration: 17s; }
.o4 { width: 260px; height: 260px; left: 20%; top: 700px; background: radial-gradient(circle at 35% 35%, #a7f3d0, #10b981); animation-duration: 13s; }
@keyframes cgl-float { from { transform: translate(0, 0) scale(1); } to { transform: translate(140px, 80px) scale(1.15); } }
.bg-vivid p { position: relative; margin: 0; padding: 26px 40px; font-size: 54px; font-weight: 800; letter-spacing: -1.5px; color: rgb(255 255 255 / .9); white-space: nowrap; }
html:not(.dark) .bg-vivid p { color: rgb(30 27 75 / .85); }

/* 背景：校准网格 */
.bg-grid { min-height: 1600px; background: repeating-linear-gradient(0deg, transparent 0 19px, rgb(79 70 229 / .35) 19px 20px), repeating-linear-gradient(90deg, transparent 0 19px, rgb(79 70 229 / .35) 19px 20px); }
.bg-grid p { margin: 0; padding: 11px 24px; font: 15px Consolas, monospace; color: #334155; }
.bg-grid b { color: #7c3aed; margin-right: 18px; }
html.dark .bg-grid p { color: #cbd5e1; }

.cgl-notes { margin-top: 28px; font-size: 13px; line-height: 1.8; color: var(--el-text-color-regular); }
.cgl-notes h2 { font-size: 16px; color: var(--el-text-color-primary); }
.cgl-notes ul { padding-left: 20px; }
.cgl-notes code { font-size: 12px; }

@media (max-width: 900px) {
  .cgl { padding: 22px 16px; }
  .cgl-actions .cg-button-label { display: none; }
  .cgl-actions .cg-button { width: 36px; padding: 0; }
  .cgl-overview { width: calc(100% - 40px); }
  .cgl-card { top: 220px; left: 20px; right: auto; width: calc(100% - 40px); }
  .bg-apps { grid-template-columns: repeat(2, 1fr); }
}
</style>
