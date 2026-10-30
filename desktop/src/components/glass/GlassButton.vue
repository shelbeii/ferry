<script setup lang="ts">
// 玻璃按钮。按下时像一滴液体被按住：整体胀大、从按下的位置亮起；按住拖动会被指针拉长、拖偏；
// 松手后弹回，带一点回弹。拖出太远再松手不算点击（浏览器原生规则）。
// 关掉液态玻璃效果时是普通按钮，没有这些动效。
import { onBeforeUnmount, onDeactivated, ref, watch, type Component } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import GlassPane from './GlassPane.vue'
import { createSprings, rubber } from '../../glass/optics'
import { glass, reducedMotion } from '../../glass/runtime'

const props = defineProps<{ icon?: Component; disabled?: boolean; loading?: boolean }>()
const pane = ref<InstanceType<typeof GlassPane>>()
const element = () => pane.value?.el as HTMLElement | undefined
const glow = () => element()?.querySelector<HTMLElement>(':scope > .cg-glow') ?? undefined
// 光从按下的位置亮起：光斑图是按钮的两倍大、中心在图中间，挪背景位置让中心落在按下处（不用 CSS 变量，见 GlassPane 的样式说明）
function aim(px: number, py: number) {
  const g = glow()
  if (g) g.style.backgroundPosition = `${(100 - px).toFixed(1)}% ${(100 - py).toFixed(1)}%`
}
const still = () => reducedMotion() || !glass.enabled

// press、dx、dy：按下程度，被拖动的偏移
const springs = createSprings([
  { stiffness: 520, damping: 22, precision: .002 },
  { stiffness: 420, damping: 24, precision: .05 },
  { stiffness: 420, damping: 24, precision: .05 },
], ([press, dx, dy]) => {
  const el = element()
  if (!el) return
  if (!press && !dx && !dy) {
    el.style.removeProperty('transform')
    glow()?.style.removeProperty('opacity')
    return
  }
  const grow = 1 + press * Math.min(.16, 6 / Math.max(1, el.offsetHeight))
  const pullX = Math.abs(dx) / 8 * .07, pullY = Math.abs(dy) / 8 * .07
  el.style.transform = `translate(${dx.toFixed(2)}px, ${dy.toFixed(2)}px) scale(${(grow * (1 + pullX - pullY * .5)).toFixed(4)}, ${(grow * (1 + pullY - pullX * .5)).toFixed(4)})`
  const g = glow()
  if (g) g.style.opacity = Math.max(0, Math.min(1, press)).toFixed(3)
})

let origin: { x: number; y: number; id: number } | undefined
function down(event: PointerEvent) {
  const el = element()
  if (event.button !== 0 || !el || origin || props.disabled || props.loading || still()) return
  const box = el.getBoundingClientRect()
  aim((event.clientX - box.left) / box.width * 100, (event.clientY - box.top) / box.height * 100)
  origin = { x: event.clientX, y: event.clientY, id: event.pointerId }
  window.addEventListener('pointermove', move)
  window.addEventListener('pointerup', up)
  window.addEventListener('pointercancel', up)
  springs.to(0, 1)
}
function move(event: PointerEvent) {
  if (!origin || event.pointerId !== origin.id) return
  springs.to(1, rubber(event.clientX - origin.x, 8))
  springs.to(2, rubber(event.clientY - origin.y, 8))
}
function up(event?: PointerEvent) {
  if (event && origin && event.pointerId !== origin.id) return
  origin = undefined
  window.removeEventListener('pointermove', move)
  window.removeEventListener('pointerup', up)
  window.removeEventListener('pointercancel', up)
  springs.to(0, 0); springs.to(1, 0); springs.to(2, 0)
}
function reset() {
  up()
  springs.stop()
  for (const i of [0, 1, 2]) springs.jump(i, 0)
}
function key(event: KeyboardEvent, pressed: boolean) {
  if (event.key !== ' ' && event.key !== 'Enter') return
  if (pressed && (props.disabled || props.loading || still())) return
  aim(50, 50)
  springs.to(0, pressed ? 1 : 0)
}
watch(() => [props.disabled, props.loading, glass.enabled], reset)
onDeactivated(reset)
onBeforeUnmount(reset)
</script>

<template>
  <GlassPane ref="pane" as="button" type="button" variant="clear" radius="pill" class="cg-button" :class="{ 'cg-icon-only': !$slots.default }"
    :disabled="disabled || loading" :aria-busy="loading || undefined"
    @pointerdown="down" @keydown="key($event, true)" @keyup="key($event, false)" @blur="springs.to(0, 0)">
    <span class="cg-layer cg-glow" aria-hidden="true" />
    <el-icon v-if="loading" class="is-loading"><Loading /></el-icon>
    <el-icon v-else-if="icon"><component :is="icon" /></el-icon>
    <span v-if="$slots.default" class="cg-button-label"><slot /></span>
  </GlassPane>
</template>

<style>
.cg-button {
  display: inline-flex; align-items: center; justify-content: center; gap: 6px;
  height: 36px; padding: 0 15px; box-sizing: border-box; flex-shrink: 0;
  border: 0; background: none; font: inherit; font-size: 13px; font-weight: 500;
  cursor: pointer; -webkit-tap-highlight-color: transparent; user-select: none;
}
.cg-button.cg-icon-only { width: 36px; padding: 0; }
.cg-button > .el-icon { font-size: 16px; }
.cg-button:focus-visible { outline: 2px solid var(--el-color-primary); outline-offset: 3px; }
.cg-button:disabled { cursor: not-allowed; opacity: .55; }
/* 按下时从按下的位置向外亮起，像光在液体里散开（叠在染色层上，仍在文字下面）。
   光斑图是按钮的两倍大、圆心在图中间；按下程度改 opacity，按下位置改 background-position */
.cg-glow {
  opacity: 0;
  background: radial-gradient(circle at 50% 50%, rgb(255 255 255 / .55) 0%, rgb(255 255 255 / .18) 22%, transparent 40%) 50% 50% / 200% 200% no-repeat;
}
html.dark .cg-glow {
  background-image: radial-gradient(circle at 50% 50%, rgb(255 255 255 / .3) 0%, rgb(255 255 255 / .08) 22%, transparent 40%);
}
.cg-button:hover:not(:disabled) { --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.22 * var(--cg-k, 1))), rgb(255 255 255 / calc(.1 * var(--cg-k, 1)))); }
html.dark .cg-button:hover:not(:disabled) { --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.12 * var(--cg-k, 1))), rgb(255 255 255 / calc(.05 * var(--cg-k, 1)))); }
.cg-button.cg-plain:hover:not(:disabled) { color: var(--el-color-primary); }
.cg-button.cg-plain:hover:not(:disabled) > .cg-tint { box-shadow: inset 0 0 0 1px var(--el-color-primary-light-5); background: var(--el-color-primary-light-9); }
</style>
