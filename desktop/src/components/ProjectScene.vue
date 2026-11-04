<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useId } from 'vue'

const props = defineProps<{
  identity: string
  name: string
  mode: 'project' | 'connection' | 'welcome'
  traits?: { types: string[]; servers: number }
  summary?: { up: number; total: number }
  connected?: boolean
}>()

const id = useId()
const hidden = ref(document.hidden)
const visibility = () => { hidden.value = document.hidden }
onMounted(() => document.addEventListener('visibilitychange', visibility))
onBeforeUnmount(() => document.removeEventListener('visibilitychange', visibility))

// 同一项目始终使用相同的配色与布局；切换时由各自的配置与已读取状态驱动。
const seed = computed(() => {
  let value = 0
  for (const ch of props.identity || 'ferry') value = (Math.imul(value, 31) + ch.charCodeAt(0)) >>> 0
  return value
})
const hue = computed(() => 195 + seed.value % 66)
const flowing = computed(() => props.mode === 'connection' ? props.connected : !!props.summary?.up)
const families: Record<string, { label: string; mark: string; color: string }> = {
  java: { label: 'Java', mark: 'J', color: '#d38a39' },
  static: { label: 'Web', mark: '</>', color: '#6581d8' },
  binary: { label: '原生', mark: '{ }', color: '#4e9cab' },
  python: { label: 'Python', mark: 'Py', color: '#a9943f' },
  node: { label: 'Node', mark: 'N', color: '#4c9c77' },
  dotnet: { label: '.NET', mark: '.n', color: '#9b79c4' },
}
const types = computed(() => Object.keys(families).filter(type => props.traits?.types.includes(type)))
const nodes = computed(() => {
  const list = types.value.length ? types.value : ['source', 'package', 'ship']
  return list.map((type, i) => ({
    type,
    x: 17 + i * (list.length > 1 ? 93 / (list.length - 1) : 0),
    y: 15 + ((seed.value >>> (i * 3)) % 3) * 5,
    mark: families[type]?.mark ?? ['<>', '□', '↗'][i],
    color: families[type]?.color,
  }))
})
const servers = computed(() => props.mode === 'connection' ? 1 : props.traits?.servers ?? 0)
const ports = computed(() => Array.from({ length: Math.min(4, servers.value) }, (_, i) => ({ x: 150 + (i % 2) * 24, y: 22 + Math.floor(i / 2) * 27 })))
const caption = computed(() => {
  if (props.mode === 'welcome') return '从代码，驶向每一次上线'
  if (props.mode === 'connection') return props.connected ? '终端已连接' : '服务器连接'
  const stack = types.value.map(type => families[type].label).join(' · ')
  return stack || '项目航线'
})
const description = computed(() => {
  if (props.mode === 'welcome') return 'Ferry：连接代码、应用与服务器'
  if (props.mode === 'connection') return `${props.name}：${caption.value}`
  const configured = props.traits ? `${props.traits.types.length} 个应用，${servers.value} 台服务器` : '尚未读取项目配置'
  const status = props.summary ? `；已读取的 ${props.summary.total} 个应用实例中 ${props.summary.up} 个运行中` : ''
  return `${props.name}：${caption.value}；${configured}${status}`
})
</script>

<template>
  <figure class="project-scene" :class="{ flowing, paused: hidden }" :style="{ '--scene-hue': hue }" :title="description">
    <svg class="scene" viewBox="0 0 188 84" role="img" :aria-label="description">
      <defs>
        <linearGradient :id="`${id}-wash`" x1="0" y1="0" x2="1" y2="1"><stop stop-color="currentColor" stop-opacity=".12" /><stop offset="1" stop-color="currentColor" stop-opacity="0" /></linearGradient>
        <linearGradient :id="`${id}-hull`" x1="0" x2="1"><stop stop-color="currentColor" /><stop offset="1" stop-color="currentColor" stop-opacity=".65" /></linearGradient>
      </defs>
      <path d="M4 58C-6 21 30 1 66 7s55 26 90 14 47 41 13 49S17 94 4 58Z" :fill="`url(#${id}-wash)`" />
      <path class="horizon" d="M9 64H180M118 14h11m-5.5-5.5v11M37 47h5m-2.5-2.5v5" />
      <g v-for="node in nodes" :key="node.type" :style="{ color: node.color }">
        <path class="route" :d="`M${node.x} ${node.y + 9} Q${node.x} 45 83 54`" />
        <circle class="app-node" :cx="node.x" :cy="node.y" r="9" />
        <text :x="node.x" :y="node.y + 3" text-anchor="middle">{{ node.mark }}</text>
      </g>
      <path v-for="(port, i) in ports" :key="i" class="route server-route" :style="{ animationDelay: `${i * -.7}s` }" :d="`M99 55Q${port.x - 10} 65 ${port.x} ${port.y}`" />
      <g v-for="(port, i) in ports" :key="`server-${i}`" class="server" :transform="`translate(${port.x},${port.y})`">
        <rect x="-7" y="-9" width="14" height="18" rx="3" />
        <path d="M-3-3H3M-3 2H1" />
        <circle cx="3" cy="5" r="1.4" class="server-light" />
      </g>
      <g class="boat">
        <path class="mast" d="M84 33v19m0-18 15 10H84" />
        <path class="cabin" d="M73 47h22v9H73zM76 43h9v5h-9z" />
        <path :fill="`url(#${id}-hull)`" d="m66 53 7 10h23l12-10Z" />
        <path class="boat-line" d="M77 51h3m4 0h3m4 0h1" />
      </g>
      <g class="water"><path d="M9 72q8-4 16 0t16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0" /><path d="M-7 79q8-3 16 0t16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0 16 0" /></g>
    </svg>
    <figcaption><span class="stack">{{ caption }}</span><span v-if="mode === 'project' && traits" class="server-count">{{ servers }} 台服务器</span><span v-else-if="mode === 'connection'" class="connection-dot" :class="{ connected }" /></figcaption>
  </figure>
</template>

<style scoped>
.project-scene { --ink: hsl(var(--scene-hue) 55% 54%); margin: 0; padding: 5px 4px 15px; color: var(--ink); }
.scene { display: block; width: 100%; height: 84px; overflow: hidden; }
.horizon { stroke: currentColor; stroke-width: 1; opacity: .17; fill: none; }
.route { fill: none; stroke: currentColor; stroke-width: .9; stroke-dasharray: 2 4; opacity: .22; }
.app-node { fill: var(--ferry-surface); stroke: currentColor; stroke-opacity: .4; stroke-width: .9; }
text { fill: currentColor; font: 600 7.5px 'Cascadia Mono', Consolas, monospace; }
.server rect { fill: var(--ferry-surface); stroke: currentColor; stroke-opacity: .5; stroke-width: 1; }
.server path { fill: none; stroke: currentColor; stroke-width: 1; stroke-linecap: round; opacity: .6; }
.server-light { fill: var(--el-text-color-placeholder); }
.flowing .server-light { fill: currentColor; }
.mast { stroke: currentColor; stroke-width: 1.3; stroke-linejoin: round; fill: currentColor; fill-opacity: .18; }
.cabin { fill: var(--ferry-surface); stroke: currentColor; stroke-width: .9; }
.boat-line { fill: none; stroke: currentColor; stroke-width: 1.2; stroke-linecap: round; }
.water { fill: none; stroke: currentColor; stroke-width: 1; stroke-linecap: round; opacity: .45; animation: tide 9s linear infinite; }
.water path + path { opacity: .4; }
.flowing .boat { animation: sail 4s ease-in-out infinite; transform-origin: 87px 56px; }
.flowing .server-route { animation: stream 3s linear infinite; opacity: .55; }
.paused * { animation-play-state: paused !important; }
figcaption { display: flex; align-items: center; gap: 6px; min-height: 16px; padding: 4px 3px 0; color: var(--el-text-color-secondary); font-size: 10px; line-height: 16px; }
.stack { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.server-count { flex-shrink: 0; margin-left: auto; font-variant-numeric: tabular-nums; opacity: .8; }
.connection-dot { width: 5px; height: 5px; border-radius: 50%; margin-left: auto; background: var(--el-text-color-placeholder); }
.connection-dot.connected { background: var(--el-color-success); }
:global(html.dark .project-scene) { --ink: hsl(var(--scene-hue) 65% 72%); }
@keyframes tide { to { transform: translateX(-32px); } }
@keyframes stream { to { stroke-dashoffset: -18; } }
@keyframes sail { 0%, 100% { transform: translateY(0) rotate(-1deg); } 50% { transform: translateY(-2px) rotate(1deg); } }
@media (prefers-reduced-motion: reduce) { .project-scene * { animation: none !important; } }
</style>
