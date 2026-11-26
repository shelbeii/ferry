<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useId } from 'vue'
import { getCurrentWindow } from '@tauri-apps/api/window'
import { isTauri } from '../platform'
import { browserOpen } from '../state'
import { dbOpen } from '../db'

const props = defineProps<{
  identity: string
  name: string
  mode: 'project' | 'connection' | 'welcome'
  traits?: { types: string[]; servers: number }
  summary?: { up: number; total: number }
  connected?: boolean
  running?: number
}>()

const emit = defineEmits<{ openTasks: [] }>()

const id = useId()
// 窗口最小化、不在前台、或被全屏抽屉（浏览器/数据库）完全盖住时才暂停动画
const hidden = ref(document.hidden)
const focused = ref(document.hasFocus())
const visibility = () => { hidden.value = document.hidden }
const onFocus = () => { focused.value = true }
const onBlur = () => { focused.value = false }
let unlistenFocus: (() => void) | undefined
let disposed = false

onMounted(() => {
  document.addEventListener('visibilitychange', visibility)
  if (isTauri) {
    const w = getCurrentWindow()
    w.isFocused().then((f) => { focused.value = f }).catch(() => {})
    w.onFocusChanged((e) => { focused.value = e.payload }).then((u) => {
      if (disposed) u()
      else unlistenFocus = u
    }).catch(() => {})
  } else {
    window.addEventListener('focus', onFocus)
    window.addEventListener('blur', onBlur)
  }
})
onBeforeUnmount(() => {
  disposed = true
  document.removeEventListener('visibilitychange', visibility)
  window.removeEventListener('focus', onFocus)
  window.removeEventListener('blur', onBlur)
  unlistenFocus?.()
})

const paused = computed(() => hidden.value || !focused.value || browserOpen.value || dbOpen.value)

// 每个项目根据 identity 生成稳定的专属微调色相（围绕 Ferry 靛蓝～紫罗兰色域）
const seed = computed(() => {
  let value = 0
  for (const ch of props.identity || 'ferry') value = (Math.imul(value, 31) + ch.charCodeAt(0)) >>> 0
  return value
})
const hue = computed(() => 226 + (seed.value % 44)) // 226°–269° 精致靛蓝到紫罗兰

const deploying = computed(() => (props.running ?? 0) > 0)
const flowing = computed(() => {
  if (deploying.value) return true
  if (props.mode === 'welcome') return true
  if (props.mode === 'connection') return !!props.connected
  return !!props.summary?.up
})

type HealthState = 'deploying' | 'healthy' | 'partial' | 'idle'
const health = computed<HealthState>(() => {
  if (deploying.value) return 'deploying'
  if (props.mode === 'connection') return props.connected ? 'healthy' : 'idle'
  if (props.mode === 'welcome') return 'healthy'
  if (!props.summary || props.summary.total === 0) return 'idle'
  if (props.summary.up === props.summary.total) return 'healthy'
  if (props.summary.up > 0) return 'partial'
  return 'idle'
})

const families: Record<string, { label: string; mark: string; color: string }> = {
  java: { label: 'Java', mark: 'JVM', color: '#f59e0b' },
  static: { label: 'Web', mark: 'WEB', color: '#6366f1' },
  binary: { label: '原生', mark: 'BIN', color: '#06b6d4' },
  python: { label: 'Python', mark: 'PY', color: '#eab308' },
  node: { label: 'Node', mark: 'NODE', color: '#10b981' },
  dotnet: { label: '.NET', mark: '.NET', color: '#a855f7' },
}

const types = computed(() => Object.keys(families).filter((type) => props.traits?.types.includes(type)))

interface SourceChip {
  key: string
  mark: string
  color: string
  y: number
  path: string
  delay: string
}

const sourceChips = computed<SourceChip[]>(() => {
  const list =
    props.mode === 'connection'
      ? [{ key: 'ssh', mark: 'SSH', color: '#6366f1' }]
      : types.value.length
        ? types.value.slice(0, 3).map((t) => ({ key: t, mark: families[t].mark, color: families[t].color }))
        : [
            { key: 'src', mark: 'SRC', color: '#6366f1' },
            { key: 'pkg', mark: 'PKG', color: '#06b6d4' },
            { key: 'run', mark: 'RUN', color: '#a855f7' },
          ]
  const ys = list.length === 1 ? [42] : list.length === 2 ? [25, 59] : [17, 42, 67]
  return list.map((item, i) => {
    const y = ys[i]
    // 平滑 S 型贝塞尔光纤：从左侧芯片右端 (46, y) 汇聚到中央棱镜核心左端 (84, 42)
    const path = `M46 ${y} C64 ${y}, 66 42, 84 42`
    return { ...item, y, path, delay: `${(i * -0.85).toFixed(2)}s` }
  })
})

const serverCount = computed(() => {
  if (props.mode === 'connection') return 1
  if (props.mode === 'welcome') return 4
  return Math.max(0, props.traits?.servers ?? 0)
})

interface ServerNode {
  x: number
  y: number
  state: 'on' | 'warn' | 'off'
  delay: string
}

const serverNodes = computed<ServerNode[]>(() => {
  const count = Math.min(9, Math.max(1, serverCount.value || 1))
  const ratio =
    props.mode === 'connection'
      ? props.connected ? 1 : 0
      : props.mode === 'welcome'
        ? 1
        : props.summary && props.summary.total > 0
          ? props.summary.up / props.summary.total
          : 0
  const lit = Math.round(count * ratio)
  const hasPartial = props.mode === 'project' && !!props.summary && props.summary.up > 0 && props.summary.up < props.summary.total

  let coords: [number, number][] = []
  if (count === 1) {
    coords = [[172, 42]]
  } else if (count === 2) {
    coords = [[169, 27], [169, 57]]
  } else if (count <= 4) {
    coords = [
      [158, 27], [180, 27],
      [158, 57], [180, 57],
    ].slice(0, count) as [number, number][]
  } else if (count <= 6) {
    coords = [
      [151, 27], [169, 27], [187, 27],
      [151, 57], [169, 57], [187, 57],
    ].slice(0, count) as [number, number][]
  } else {
    coords = [
      [151, 19], [169, 19], [187, 19],
      [151, 42], [169, 42], [187, 42],
      [151, 65], [169, 65], [187, 65],
    ].slice(0, count) as [number, number][]
  }

  return coords.map(([x, y], i) => {
    let state: ServerNode['state'] = 'off'
    if (i < lit) state = 'on'
    else if (hasPartial && i === lit) state = 'warn'
    return { x, y, state, delay: `${((i * 0.42) % 2.4).toFixed(2)}s` }
  })
})

// 右侧出站光轨：从中央渡口核心右端 (116, 42) 优雅分流到服务器集群入口
const outboundRoutes = computed(() => {
  const nodes = serverNodes.value
  if (nodes.length <= 1) {
    return [{ path: 'M116 42 C134 42, 140 42, 158 42', delay: '0s', active: nodes[0]?.state !== 'off' }]
  }
  const uniqueYs = [...new Set(nodes.map((n) => n.y))].sort((a, b) => a - b)
  const targetX = Math.min(...nodes.map((n) => n.x)) - 7
  return uniqueYs.map((y, i) => {
    const rowHasLit = nodes.some((n) => n.y === y && n.state === 'on')
    return {
      path: `M116 42 C134 42, 134 ${y}, ${targetX} ${y}`,
      delay: `${(i * -0.7 - 0.35).toFixed(2)}s`,
      active: rowHasLit || deploying.value,
    }
  })
})

const caption = computed(() => {
  if (deploying.value) return `正在执行 ${props.running} 个任务`
  if (props.mode === 'welcome') return '从代码，驶向每一次上线'
  if (props.mode === 'connection') return props.connected ? 'SSH 通道已连接' : '服务器连接待命'
  const stack = types.value.map((t) => families[t].label).join(' · ')
  return stack || '项目航线'
})

const rightMetric = computed(() => {
  if (props.mode === 'connection') return props.connected ? '在线' : '未连'
  if (props.mode === 'project' && props.traits) {
    if (props.summary && props.summary.total > 0) return `${props.summary.up}/${props.summary.total} · ${serverCount.value} 台`
    return `${serverCount.value} 台服务器`
  }
  return ''
})

const description = computed(() => {
  if (deploying.value) return `${props.name || 'Ferry'}：正在执行 ${props.running} 个任务，点击打开任务中心`
  if (props.mode === 'welcome') return 'Ferry：连接代码、应用与服务器（点击打开任务中心）'
  if (props.mode === 'connection') return `${props.name}：${caption.value}（点击打开任务中心）`
  const configured = props.traits ? `${props.traits.types.length} 类技术栈，${serverCount.value} 台服务器` : '尚未读取项目配置'
  const status = props.summary ? `；${props.summary.up}/${props.summary.total} 个应用实例运行中` : ''
  return `${props.name}：${caption.value}（${configured}${status}），点击打开任务中心`
})

function onTrigger(e?: Event) {
  ;(e?.currentTarget as HTMLElement | null)?.blur?.()
  emit('openTasks')
}
</script>

<template>
  <figure
    class="project-scene"
    :class="[`health-${health}`, { flowing, deploying, paused }]"
    :style="{ '--scene-hue': hue }"
    :title="description"
    role="button"
    tabindex="0"
    data-glass-motion="local"
    :aria-label="description"
    @click="onTrigger"
    @keydown.enter.prevent="onTrigger"
    @keydown.space.prevent="onTrigger"
  >
    <svg class="scene" viewBox="0 0 200 84" aria-hidden="true">
      <defs>
        <pattern :id="`${id}-dots`" width="10" height="10" patternUnits="userSpaceOnUse">
          <circle cx="1.5" cy="1.5" r="0.65" class="grid-dot" />
        </pattern>
        <radialGradient :id="`${id}-aura`" cx="50%" cy="50%" r="50%">
          <stop offset="0%" class="aura-inner" />
          <stop offset="55%" class="aura-mid" />
          <stop offset="100%" stop-color="transparent" stop-opacity="0" />
        </radialGradient>
        <linearGradient :id="`${id}-prism`" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0%" class="prism-start" />
          <stop offset="100%" class="prism-end" />
        </linearGradient>
        <linearGradient :id="`${id}-beam`" x1="0" y1="0" x2="1" y2="0">
          <stop offset="0%" class="beam-start" />
          <stop offset="100%" class="beam-end" />
        </linearGradient>
      </defs>

      <!-- 精密点阵底纹 + 中央渡口光晕 -->
      <rect x="0" y="0" width="200" height="84" :fill="`url(#${id}-dots)`" />
      <circle cx="100" cy="42" r="38" :fill="`url(#${id}-aura)`" class="core-aura" />

      <!-- 左侧入站光纤轨道 + 光子流 -->
      <g v-for="chip in sourceChips" :key="`in-${chip.key}`">
        <path class="track" :d="chip.path" />
        <template v-if="flowing">
          <path class="photon-glow" :d="chip.path" pathLength="100" :style="{ stroke: chip.color, animationDelay: chip.delay }" />
          <path class="photon-head" :d="chip.path" pathLength="100" :style="{ animationDelay: chip.delay }" />
        </template>
      </g>

      <!-- 右侧出站光纤轨道 + 光子流 -->
      <g v-for="(route, i) in outboundRoutes" :key="`out-${i}`">
        <path class="track" :class="{ 'track-lit': route.active }" :d="route.path" />
        <template v-if="flowing && route.active">
          <path class="photon-glow out-glow" :d="route.path" pathLength="100" :stroke="`url(#${id}-beam)`" :style="{ animationDelay: route.delay }" />
          <path class="photon-head out-head" :d="route.path" pathLength="100" :style="{ animationDelay: route.delay }" />
        </template>
      </g>

      <!-- 左侧技术栈微芯片 -->
      <g v-for="chip in sourceChips" :key="`chip-${chip.key}`" :transform="`translate(8, ${chip.y - 7.5})`">
        <rect width="38" height="15" rx="5" class="chip-bg" />
        <circle cx="7.5" cy="7.5" r="2.2" :fill="chip.color" class="chip-led" />
        <text x="13" y="10.2" class="chip-text">{{ chip.mark }}</text>
        <circle cx="38" cy="7.5" r="1.4" class="pin-dot" />
      </g>

      <!-- 中央 Ferry 棱镜渡口核心 -->
      <g class="core-group" transform="translate(100, 42)">
        <circle v-if="deploying" r="15" class="shockwave" />
        <circle v-if="deploying" r="15" class="shockwave shockwave-2" />
        <circle r="17.5" class="orbit-ring" />
        <circle r="13.5" class="orbit-inner" />
        <polygon points="0,-11.5 11.5,0 0,11.5 -11.5,0" class="prism-body" :stroke="`url(#${id}-prism)`" />
        <!-- 抽象摆渡航标（双尖锋） -->
        <path d="M-4.2 -3.6 L-0.4 0 L-4.2 3.6 M0.6 -3.6 L4.4 0 L0.6 3.6" class="prism-glyph" />
      </g>

      <!-- 右侧服务器节点：单机为精密刀片，多机为节点矩阵 -->
      <g v-if="serverNodes.length === 1" :transform="`translate(${serverNodes[0].x}, ${serverNodes[0].y})`" class="blade" :class="`node-${serverNodes[0].state}`">
        <circle v-if="serverNodes[0].state === 'on'" r="14" class="blade-ping" />
        <rect x="-12" y="-15" width="24" height="30" rx="5.5" class="node-box" />
        <path d="M-6 -6 H6 M-6 -1 H6 M-6 4 H2" class="blade-slot" />
        <circle cx="5.5" cy="9" r="2.2" class="node-led" />
      </g>
      <g v-else>
        <g
          v-for="(node, i) in serverNodes"
          :key="`srv-${i}`"
          :transform="`translate(${node.x}, ${node.y})`"
          class="rack-node"
          :class="`node-${node.state}`"
          :style="{ '--node-delay': node.delay }"
        >
          <rect x="-6.5" y="-6.5" width="13" height="13" rx="3.4" class="node-box" />
          <circle cx="0" cy="0" r="2.1" class="node-led" />
        </g>
      </g>
    </svg>

    <figcaption>
      <span class="status-dot" :class="`dot-${health}`" />
      <span class="stack">{{ caption }}</span>
      <span class="metric">
        <span class="metric-normal">{{ rightMetric }}</span>
        <span class="metric-hover">任务中心 ↗</span>
      </span>
    </figcaption>
  </figure>
</template>

<style scoped>
.project-scene {
  --accent: hsl(var(--scene-hue) 74% 56%);
  --accent-2: hsl(calc(var(--scene-hue) + 34) 78% 60%);
  --live: #10b981;
  --warn: #f59e0b;
  --flow-dur: 2.8s;
  --orbit-dur: 18s;

  position: relative;
  margin: 2px 2px 10px;
  padding: 6px 6px 6px;
  border-radius: 12px;
  cursor: pointer;
  user-select: none;
  outline: none;
  contain: layout style paint;
  background:
    radial-gradient(120% 90% at 50% 0%, hsl(var(--scene-hue) 85% 96% / 0.95), transparent 72%),
    linear-gradient(165deg, rgba(255, 255, 255, 0.92), rgba(242, 244, 253, 0.88));
  border: 1px solid hsl(var(--scene-hue) 55% 60% / 0.18);
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.9),
    0 2px 10px -4px hsl(var(--scene-hue) 60% 45% / 0.14);
  transition: border-color 0.18s ease;
}

.project-scene:hover,
.project-scene:focus-visible {
  border-color: hsl(var(--scene-hue) 72% 58% / 0.42);
}
.project-scene:active {
  border-color: hsl(var(--scene-hue) 78% 56% / 0.58);
}

.project-scene.deploying {
  --flow-dur: 1.15s;
  --orbit-dur: 4.5s;
  border-color: hsl(var(--scene-hue) 82% 60% / 0.52);
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.92),
    0 0 0 1px hsl(var(--scene-hue) 80% 60% / 0.16),
    0 8px 20px -6px hsl(var(--scene-hue) 80% 52% / 0.34);
}

.scene {
  display: block;
  width: 100%;
  height: 82px;
  overflow: hidden;
}

.grid-dot {
  fill: hsl(var(--scene-hue) 40% 45% / 0.14);
}

.aura-inner {
  stop-color: var(--accent);
  stop-opacity: 0.22;
}
.aura-mid {
  stop-color: var(--accent-2);
  stop-opacity: 0.06;
}
.core-aura {
  transition: opacity 0.3s ease;
}
.deploying .core-aura {
  animation: aura-pulse 1.8s ease-in-out infinite;
}

.prism-start { stop-color: var(--accent); }
.prism-end { stop-color: var(--accent-2); }
.beam-start { stop-color: var(--accent); }
.beam-end { stop-color: var(--live); }
.deploying .beam-end { stop-color: var(--accent-2); }

/* 纤维轨道 */
.track {
  fill: none;
  stroke: hsl(var(--scene-hue) 35% 50% / 0.2);
  stroke-width: 1;
  stroke-linecap: round;
  transition: stroke 0.25s ease;
}
.track-lit {
  stroke: hsl(var(--scene-hue) 50% 52% / 0.28);
}
.project-scene:hover .track {
  stroke: hsl(var(--scene-hue) 55% 52% / 0.38);
}

/* 光子流（两层叠绘：宽柔光尾迹 + 极锐白亮头部，零回卷伪影，无滤镜重绘开销） */
.photon-glow {
  fill: none;
  stroke-width: 2.6;
  stroke-linecap: round;
  stroke-dasharray: 16 120;
  stroke-dashoffset: 16;
  opacity: 0.46;
  animation: photon-travel var(--flow-dur) cubic-bezier(0.35, 0.05, 0.25, 1) infinite;
}
.photon-head {
  fill: none;
  stroke: var(--accent);
  stroke-width: 1.45;
  stroke-linecap: round;
  stroke-dasharray: 0 13 3 120;
  stroke-dashoffset: 16;
  opacity: 0.96;
  animation: photon-travel var(--flow-dur) cubic-bezier(0.35, 0.05, 0.25, 1) infinite;
}
.out-head {
  stroke: #ffffff;
}

/* 左侧微芯片 */
.chip-bg {
  fill: rgba(255, 255, 255, 0.88);
  stroke: hsl(var(--scene-hue) 38% 52% / 0.22);
  stroke-width: 0.9;
  transition: stroke 0.18s ease;
}
.project-scene:hover .chip-bg {
  stroke: hsl(var(--scene-hue) 62% 55% / 0.44);
}
.chip-text {
  fill: #334155;
  font: 700 6.5px 'Cascadia Mono', Consolas, monospace;
  letter-spacing: 0.35px;
}
.pin-dot {
  fill: hsl(var(--scene-hue) 55% 56% / 0.45);
}

/* 中央 Ferry 棱镜核心 */
.orbit-ring {
  fill: none;
  stroke: hsl(var(--scene-hue) 65% 56% / 0.34);
  stroke-width: 0.9;
  stroke-dasharray: 4 4 1.5 4;
  transform-box: fill-box;
  transform-origin: center;
  animation: spin var(--orbit-dur) linear infinite;
}
.orbit-inner {
  fill: hsl(var(--scene-hue) 70% 58% / 0.06);
  stroke: hsl(var(--scene-hue) 65% 56% / 0.16);
  stroke-width: 0.75;
}
.prism-body {
  fill: rgba(255, 255, 255, 0.94);
  stroke-width: 1.35;
  stroke-linejoin: round;
  transform-box: fill-box;
  transform-origin: center;
  transition: transform 0.25s ease;
}
.project-scene:hover .prism-body {
  transform: scale(1.06);
}
.prism-glyph {
  fill: none;
  stroke: var(--accent);
  stroke-width: 1.45;
  stroke-linecap: round;
  stroke-linejoin: round;
}

.shockwave {
  fill: none;
  stroke: var(--accent);
  stroke-width: 1.1;
  opacity: 0;
  transform-box: fill-box;
  transform-origin: center;
  animation: shock 1.8s ease-out infinite;
}
.shockwave-2 {
  animation-delay: 0.9s;
}

/* 右侧服务器节点矩阵 */
.node-box {
  fill: rgba(255, 255, 255, 0.88);
  stroke: hsl(var(--scene-hue) 32% 50% / 0.24);
  stroke-width: 0.95;
  transition: stroke 0.25s ease, fill 0.25s ease;
}
.node-on .node-box {
  stroke: rgba(16, 185, 129, 0.45);
}
.node-warn .node-box {
  stroke: rgba(245, 158, 11, 0.48);
}
.blade-slot {
  fill: none;
  stroke: hsl(var(--scene-hue) 30% 48% / 0.36);
  stroke-width: 1.1;
  stroke-linecap: round;
}
.node-led {
  fill: #94a3b8;
  transition: fill 0.25s ease;
}
.node-on .node-led {
  fill: var(--live);
  animation: led-breathe 2.6s ease-in-out infinite;
  animation-delay: var(--node-delay, 0s);
}
.deploying .node-on .node-led {
  fill: var(--accent);
  animation-duration: 1.1s;
}
.node-warn .node-led {
  fill: var(--warn);
}
.blade-ping {
  fill: none;
  stroke: var(--live);
  stroke-width: 0.9;
  opacity: 0;
  transform-box: fill-box;
  transform-origin: center;
  animation: shock 2.8s ease-out infinite;
}

/* 底部紧凑遥测栏 */
figcaption {
  display: flex;
  align-items: center;
  gap: 5px;
  min-height: 16px;
  padding: 4px 2px 0;
  border-top: 1px solid hsl(var(--scene-hue) 35% 55% / 0.1);
  color: var(--el-text-color-secondary);
  font-size: 10px;
  line-height: 15px;
}
.status-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  flex-shrink: 0;
  background: #94a3b8;
}
.dot-healthy {
  background: var(--live);
  box-shadow: 0 0 6px rgba(16, 185, 129, 0.55);
}
.dot-partial {
  background: var(--warn);
  box-shadow: 0 0 6px rgba(245, 158, 11, 0.5);
}
.dot-deploying {
  background: var(--accent);
  box-shadow: 0 0 8px var(--accent);
  animation: led-breathe 0.95s ease-in-out infinite;
}
.stack {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-weight: 500;
}
.metric {
  position: relative;
  flex-shrink: 0;
  margin-left: auto;
  min-width: 50px;
  text-align: right;
  font-variant-numeric: tabular-nums;
}
.metric-normal {
  display: inline-block;
  opacity: 0.85;
  transition: opacity 0.18s ease, transform 0.18s ease;
}
.metric-hover {
  position: absolute;
  right: 0;
  top: 0;
  white-space: nowrap;
  color: var(--accent);
  font-weight: 600;
  opacity: 0;
  transform: translateY(3px);
  transition: opacity 0.18s ease, transform 0.18s ease;
}
.project-scene:hover .metric-normal,
.project-scene:focus-visible .metric-normal {
  opacity: 0;
  transform: translateY(-3px);
}
.project-scene:hover .metric-hover,
.project-scene:focus-visible .metric-hover {
  opacity: 1;
  transform: translateY(0);
}

/* ---------------- 暗黑主题专属深空舰桥调校（完整选择器须包在 :global(...) 内，防止 Vue scoped 截断） ---------------- */
:global(html.dark .project-scene) {
  --accent: hsl(var(--scene-hue) 86% 68%);
  --accent-2: hsl(calc(var(--scene-hue) + 34) 88% 72%);
  background:
    radial-gradient(110% 90% at 50% 0%, hsl(var(--scene-hue) 70% 24% / 0.42), transparent 72%),
    linear-gradient(160deg, rgba(20, 22, 36, 0.92), rgba(12, 13, 21, 0.94));
  border-color: hsl(var(--scene-hue) 68% 68% / 0.22);
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.08),
    0 4px 16px -6px rgba(0, 0, 0, 0.55);
}
:global(html.dark .project-scene:hover),
:global(html.dark .project-scene:focus-visible) {
  border-color: hsl(var(--scene-hue) 82% 72% / 0.48);
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.12),
    0 6px 22px -6px hsl(var(--scene-hue) 80% 55% / 0.35);
}
:global(html.dark .project-scene.deploying) {
  border-color: hsl(var(--scene-hue) 85% 72% / 0.62);
  box-shadow:
    inset 0 1px 0 rgba(255, 255, 255, 0.14),
    0 0 0 1px hsl(var(--scene-hue) 85% 68% / 0.22),
    0 8px 24px -6px hsl(var(--scene-hue) 85% 58% / 0.42);
}
:global(html.dark .project-scene .grid-dot) {
  fill: hsl(var(--scene-hue) 60% 72% / 0.16);
}
:global(html.dark .project-scene .aura-inner) {
  stop-opacity: 0.32;
}
:global(html.dark .project-scene .track) {
  stroke: hsl(var(--scene-hue) 50% 72% / 0.18);
}
:global(html.dark .project-scene .track-lit) {
  stroke: hsl(var(--scene-hue) 65% 74% / 0.3);
}
:global(html.dark .project-scene .photon-glow) {
  opacity: 0.58;
}
:global(html.dark .project-scene .photon-head) {
  stroke: #f8fafc;
}
:global(html.dark .project-scene .chip-bg) {
  fill: rgba(24, 27, 44, 0.9);
  stroke: hsl(var(--scene-hue) 55% 72% / 0.26);
}
:global(html.dark .project-scene .chip-text) {
  fill: #e2e8f0;
}
:global(html.dark .project-scene .prism-body) {
  fill: rgba(22, 25, 42, 0.95);
}
:global(html.dark .project-scene .node-box) {
  fill: rgba(22, 25, 40, 0.9);
  stroke: hsl(var(--scene-hue) 45% 68% / 0.24);
}
:global(html.dark .project-scene .node-on .node-box) {
  stroke: rgba(52, 211, 153, 0.5);
}
:global(html.dark .project-scene .blade-slot) {
  stroke: rgba(148, 163, 184, 0.38);
}
:global(html.dark .project-scene figcaption) {
  border-top-color: rgba(255, 255, 255, 0.07);
}

/* 新增四套主题的侧栏航线插画调色 */
:global(html[data-theme="warm-paper"] .project-scene) {
  --accent: #b85d19;
  --accent-2: #d68a38;
  --live: #1f7a5a;
  background:
    radial-gradient(120% 90% at 50% 0%, rgba(236, 206, 170, 0.42), transparent 72%),
    linear-gradient(165deg, #fbf7ee, #f1e9d8);
  border-color: rgba(168, 128, 82, 0.24);
  box-shadow:
    inset 0 1px 0 rgba(255, 253, 248, 0.9),
    0 2px 10px -4px rgba(96, 68, 36, 0.14);
}
:global(html[data-theme="warm-paper"] .project-scene .grid-dot) { fill: rgba(120, 96, 68, 0.18); }
:global(html[data-theme="warm-paper"] .project-scene .track) { stroke: rgba(125, 98, 66, 0.24); }
:global(html[data-theme="warm-paper"] .project-scene .track-lit) { stroke: rgba(184, 93, 25, 0.34); }
:global(html[data-theme="warm-paper"] .project-scene .chip-bg),
:global(html[data-theme="warm-paper"] .project-scene .node-box),
:global(html[data-theme="warm-paper"] .project-scene .prism-body) { fill: #fffdf8; stroke: rgba(168, 140, 104, 0.32); }
:global(html[data-theme="warm-paper"] .project-scene .chip-text) { fill: #4a3f33; }
:global(html[data-theme="warm-paper"] .project-scene .orbit-ring) { stroke: rgba(184, 93, 25, 0.34); }
:global(html[data-theme="warm-paper"] .project-scene .orbit-inner) { fill: rgba(184, 93, 25, 0.06); stroke: rgba(184, 93, 25, 0.18); }
:global(html[data-theme="warm-paper"] .project-scene figcaption) { border-top-color: rgba(140, 114, 78, 0.14); }

:global(html.dark[data-theme="warm-dusk"] .project-scene) {
  --accent: #e59546;
  --accent-2: #f2be72;
  --live: #5ec293;
  background:
    radial-gradient(110% 90% at 50% 0%, rgba(168, 102, 42, 0.28), transparent 72%),
    linear-gradient(160deg, rgba(33, 27, 22, 0.94), rgba(20, 16, 13, 0.96));
  border-color: rgba(229, 149, 70, 0.22);
}
:global(html.dark[data-theme="warm-dusk"] .project-scene .grid-dot) { fill: rgba(235, 200, 160, 0.15); }
:global(html.dark[data-theme="warm-dusk"] .project-scene .track) { stroke: rgba(235, 200, 160, 0.18); }
:global(html.dark[data-theme="warm-dusk"] .project-scene .track-lit) { stroke: rgba(229, 149, 70, 0.32); }
:global(html.dark[data-theme="warm-dusk"] .project-scene .chip-bg),
:global(html.dark[data-theme="warm-dusk"] .project-scene .node-box),
:global(html.dark[data-theme="warm-dusk"] .project-scene .prism-body) { fill: rgba(36, 29, 24, 0.94); stroke: rgba(235, 200, 160, 0.22); }
:global(html.dark[data-theme="warm-dusk"] .project-scene .chip-text) { fill: #efe8dc; }
:global(html.dark[data-theme="warm-dusk"] .project-scene .orbit-ring) { stroke: rgba(229, 149, 70, 0.36); }
:global(html.dark[data-theme="warm-dusk"] .project-scene .orbit-inner) { fill: rgba(229, 149, 70, 0.08); stroke: rgba(229, 149, 70, 0.18); }

:global(html.dark[data-theme="dimmed-sage"] .project-scene) {
  --accent: #5ec4a4;
  --accent-2: #8cdbc2;
  --live: #6fd19f;
  background:
    radial-gradient(110% 90% at 50% 0%, rgba(62, 134, 114, 0.26), transparent 72%),
    linear-gradient(160deg, rgba(35, 47, 51, 0.94), rgba(24, 33, 36, 0.96));
  border-color: rgba(94, 196, 164, 0.24);
}
:global(html.dark[data-theme="dimmed-sage"] .project-scene .grid-dot) { fill: rgba(180, 228, 212, 0.16); }
:global(html.dark[data-theme="dimmed-sage"] .project-scene .track) { stroke: rgba(180, 228, 212, 0.18); }
:global(html.dark[data-theme="dimmed-sage"] .project-scene .track-lit) { stroke: rgba(94, 196, 164, 0.34); }
:global(html.dark[data-theme="dimmed-sage"] .project-scene .chip-bg),
:global(html.dark[data-theme="dimmed-sage"] .project-scene .node-box),
:global(html.dark[data-theme="dimmed-sage"] .project-scene .prism-body) { fill: rgba(38, 51, 56, 0.94); stroke: rgba(170, 220, 205, 0.24); }
:global(html.dark[data-theme="dimmed-sage"] .project-scene .chip-text) { fill: #e1ebe7; }
:global(html.dark[data-theme="dimmed-sage"] .project-scene .orbit-ring) { stroke: rgba(94, 196, 164, 0.38); }
:global(html.dark[data-theme="dimmed-sage"] .project-scene .orbit-inner) { fill: rgba(94, 196, 164, 0.08); stroke: rgba(94, 196, 164, 0.18); }

:global(html.dark[data-theme="obsidian"] .project-scene) {
  --accent: #38bdf8;
  --accent-2: #7dd3fc;
  --live: #34d399;
  background:
    radial-gradient(110% 90% at 50% 0%, rgba(56, 189, 248, 0.22), transparent 72%),
    linear-gradient(160deg, rgba(16, 18, 25, 0.96), rgba(6, 7, 10, 0.98));
  border-color: rgba(56, 189, 248, 0.24);
}
:global(html.dark[data-theme="obsidian"] .project-scene .grid-dot) { fill: rgba(148, 205, 255, 0.16); }
:global(html.dark[data-theme="obsidian"] .project-scene .track) { stroke: rgba(148, 205, 255, 0.18); }
:global(html.dark[data-theme="obsidian"] .project-scene .track-lit) { stroke: rgba(56, 189, 248, 0.34); }
:global(html.dark[data-theme="obsidian"] .project-scene .chip-bg),
:global(html.dark[data-theme="obsidian"] .project-scene .node-box),
:global(html.dark[data-theme="obsidian"] .project-scene .prism-body) { fill: rgba(18, 20, 28, 0.95); stroke: rgba(148, 205, 255, 0.22); }
:global(html.dark[data-theme="obsidian"] .project-scene .chip-text) { fill: #f1f5f9; }
:global(html.dark[data-theme="obsidian"] .project-scene .orbit-ring) { stroke: rgba(56, 189, 248, 0.38); }
:global(html.dark[data-theme="obsidian"] .project-scene .orbit-inner) { fill: rgba(56, 189, 248, 0.08); stroke: rgba(56, 189, 248, 0.2); }

.paused * {
  animation-play-state: paused !important;
}

@keyframes photon-travel {
  0% { stroke-dashoffset: 16; }
  100% { stroke-dashoffset: -100; }
}
@keyframes spin {
  to { transform: rotate(360deg); }
}
@keyframes shock {
  0% { transform: scale(0.85); opacity: 0.65; }
  100% { transform: scale(1.75); opacity: 0; }
}
@keyframes aura-pulse {
  0%, 100% { opacity: 0.75; transform: scale(0.96); transform-origin: 100px 42px; }
  50% { opacity: 1; transform: scale(1.08); transform-origin: 100px 42px; }
}
@keyframes led-breathe {
  0%, 100% { opacity: 0.65; }
  50% { opacity: 1; }
}

@media (prefers-reduced-motion: reduce) {
  .project-scene * { animation: none !important; }
}
</style>
