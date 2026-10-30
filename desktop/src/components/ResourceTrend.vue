<script setup lang="ts">
import { computed } from 'vue'
import { trendSegments, type ResourcePoint } from '../resourceTrend'
import { percentText } from '../serverinfo'
const props = defineProps<{ title: string; points: ResourcePoint[]; interval: number; accent?: string }>()
const segments = computed(() => trendSegments(props.points, Math.max(props.interval, 1) * 120_000))
const current = computed(() => props.points.at(-1)?.value)
const valid = computed(() => props.points.filter((p) => p.value != null).length)
const when = (at?: number) => at ? new Date(at).toLocaleTimeString('zh-CN', { hour12: false }) : '—'
</script>
<template>
  <section class="resource-trend" :style="{ '--trend-accent': accent || 'var(--el-color-primary)' }">
    <div class="trend-title"><span>{{ title }}</span><strong>{{ percentText(current) }}</strong></div>
    <div class="chart">
      <svg viewBox="0 0 300 72" preserveAspectRatio="none" role="img" :aria-label="`${title}：本次会话的真实采样趋势`">
        <path d="M0 8H300 M0 36H300 M0 64H300" class="grid-lines" />
        <polyline v-for="(segment, i) in segments" :key="i" :points="segment" />
      </svg>
      <span v-if="!segments.length" class="awaiting">{{ valid ? '等待下一次采样生成趋势' : '暂无可用采样' }}</span>
    </div>
    <div class="trend-caption"><span>{{ when(points[0]?.at) }} — {{ when(points.at(-1)?.at) }}</span><span>{{ valid }} 次有效采样 · 本次会话</span></div>
  </section>
</template>
<style scoped>
.resource-trend { padding: 18px 20px; border: 1px solid var(--ferry-stroke); border-radius: var(--ferry-radius); background: var(--ferry-surface); box-shadow: var(--ferry-shadow); min-width: 0; }
.trend-title { display: flex; align-items: baseline; justify-content: space-between; font-size: 13px; }
.trend-title strong { font-size: 25px; letter-spacing: -1px; color: var(--trend-accent); font-variant-numeric: tabular-nums; }
.chart { height: 82px; position: relative; margin-top: 12px; }
svg { width: 100%; height: 100%; overflow: visible; }
.grid-lines { stroke: var(--el-border-color-lighter); stroke-width: 1; fill: none; vector-effect: non-scaling-stroke; }
polyline { stroke: var(--trend-accent); stroke-width: 2; fill: none; stroke-linecap: round; stroke-linejoin: round; vector-effect: non-scaling-stroke; }
.awaiting { position: absolute; inset: 0; display: grid; place-items: center; color: var(--el-text-color-placeholder); font-size: 12px; }
.trend-caption { display: flex; justify-content: space-between; flex-wrap: wrap; gap: 6px; color: var(--el-text-color-secondary); font-size: 11px; margin-top: 8px; font-variant-numeric: tabular-nums; }
</style>
