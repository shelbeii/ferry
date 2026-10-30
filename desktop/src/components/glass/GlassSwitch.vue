<script setup lang="ts">
// 玻璃开关（对应 iOS 26 的开关）。按下时白色滑块化成一颗透明液滴，透出并放大下面轨道的颜色；
// 可以按住拖动，松手后滑到一侧再变回白色。
// 用法和 el-switch 一样：v-model 或 :model-value + @change（开关状态由外面决定，比如要等服务器开好了才算开）；
// size="small" 小一号；active-text / inactive-text 写在轨道里（滑块的另一侧）；loading 时滑块上转圈、不能点。
// 关掉液态玻璃效果时滑块是普通的白色圆角块，没有浮起和变形。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import GlassPane from './GlassPane.vue'
import { createSprings } from '../../glass/optics'
import { glass, reducedMotion } from '../../glass/runtime'

const props = withDefaults(defineProps<{
  modelValue: boolean
  size?: 'default' | 'small'
  activeText?: string
  inactiveText?: string
  disabled?: boolean
  loading?: boolean
}>(), { size: 'default', activeText: '', inactiveText: '', disabled: false, loading: false })
const emit = defineEmits<{ 'update:modelValue': [value: boolean]; change: [value: boolean] }>()

const root = ref<HTMLElement>()
const thumb = ref<InstanceType<typeof GlassPane>>()
// 滑块能走多远：轨道宽 - 滑块宽 - 两边各 3px（尺寸、文字变了重新量）
let travel = 20
let press: { id: number; x: number; from: number; dragging: boolean } | undefined
let landing = false
const blocked = computed(() => props.disabled || props.loading)
const lifts = () => glass.enabled && !reducedMotion()
// 轨道里的文字取长的那个撑开宽度，开、关时宽度不变
const sizer = computed(() => (props.activeText.length >= props.inactiveText.length ? props.activeText : props.inactiveText))

// 位置（0 关、1 开）、浮起程度
const springs = createSprings([
  { stiffness: 360, damping: 26, precision: .002 },
  { stiffness: 400, damping: 18, precision: .002 },
], ([position, lift], [velocity]) => {
  const element = thumb.value?.el as HTMLElement | undefined
  if (!element || !root.value) return
  const stretch = Math.min(.18, Math.abs(velocity) / 14) * lift
  element.style.transform = `translateX(${(position * travel).toFixed(2)}px) scale(${((1 + .32 * lift) * (1 + stretch)).toFixed(4)}, ${((1 + .5 * lift) * (1 - stretch * .4)).toFixed(4)})`
  // 白色滑块、绿色轨道的深浅都只改 opacity（不用 CSS 变量，见 GlassPane 的样式说明）
  const solid = element.querySelector<HTMLElement>('.cg-switch-solid')
  const fill = root.value.querySelector<HTMLElement>('.cg-switch-fill')
  if (solid) solid.style.opacity = (1 - Math.min(1, lift * 1.5)).toFixed(3)
  if (fill) fill.style.opacity = Math.min(1, Math.max(0, position)).toFixed(3)
  if (landing && !press && Math.abs(position - springs.target(0)) < .04) {
    landing = false
    springs.to(1, 0)
  }
})

function measure() {
  const element = root.value, knob = thumb.value?.el as HTMLElement | undefined
  if (!element || !knob) return
  travel = Math.max(0, element.offsetWidth - knob.offsetWidth - 6)
  springs.jump(0, props.modelValue ? 1 : 0)
}
// 滑到 value 那一侧（外面的值变了、或者松手后外面没接受时滑回去）
function slide(value: boolean, bounce: boolean) {
  if (!lifts()) { springs.jump(0, value ? 1 : 0); springs.jump(1, 0); return }
  springs.to(0, value ? 1 : 0)
  if (bounce) { landing = true; springs.to(1, 1) }
  else springs.to(1, 0)
}
function settle(value: boolean) {
  if (value !== props.modelValue) {
    emit('update:modelValue', value)
    emit('change', value)
  }
  slide(value, true)
  // 外面没改（要等服务器、或者拒绝了）：滑回当前的值；之后外面改了由下面的 watch 滑过去
  nextTick(() => { if (props.modelValue !== value) slide(props.modelValue, false) })
}
watch(() => props.modelValue, (value) => { if (!press) slide(value, false) })

function down(event: PointerEvent) {
  if (event.button !== 0 || press || blocked.value) return
  press = { id: event.pointerId, x: event.clientX, from: springs.value(0), dragging: false }
  root.value?.setPointerCapture(event.pointerId)
  if (lifts()) springs.to(1, 1)
}
function move(event: PointerEvent) {
  if (!press || event.pointerId !== press.id || !lifts()) return
  const dx = event.clientX - press.x
  if (!press.dragging && Math.abs(dx) < 3) return
  press.dragging = true
  springs.to(0, Math.max(-.12, Math.min(1.12, press.from + dx / Math.max(1, travel))))
}
function up(event: PointerEvent) {
  if (!press || event.pointerId !== press.id) return
  const value = press.dragging ? springs.target(0) > .5 : !props.modelValue
  if (root.value?.hasPointerCapture(event.pointerId)) root.value.releasePointerCapture(event.pointerId)
  press = undefined
  settle(value)
}
// 键盘（空格、回车）触发的 click 的 detail 是 0；鼠标点击已经在 pointerup 里处理过
function click(event: MouseEvent) { if (event.detail === 0 && !blocked.value) settle(!props.modelValue) }

let observer: ResizeObserver | undefined
onMounted(() => {
  springs.jump(1, 0)
  measure()
  observer = new ResizeObserver(measure)
  observer.observe(root.value!)
})
watch(() => [props.size, props.activeText, props.inactiveText], () => nextTick(measure))
onBeforeUnmount(() => { observer?.disconnect(); springs.stop() })
</script>

<template>
  <button ref="root" type="button" class="cg-switch" :class="[`cg-switch-${size}`, { 'is-on': modelValue, 'has-text': sizer, 'is-loading': loading }]"
    role="switch" :aria-checked="modelValue" :disabled="disabled" :aria-busy="loading || undefined"
    @pointerdown="down" @pointermove="move" @pointerup="up" @pointercancel="up" @click="click">
    <span class="cg-switch-track" aria-hidden="true" />
    <span class="cg-switch-fill" aria-hidden="true" />
    <template v-if="sizer">
      <span class="cg-switch-sizer" aria-hidden="true">{{ sizer }}</span>
      <span class="cg-switch-text cg-switch-on-text" aria-hidden="true">{{ activeText }}</span>
      <span class="cg-switch-text cg-switch-off-text" aria-hidden="true">{{ inactiveText }}</span>
    </template>
    <GlassPane ref="thumb" variant="clear" radius="pill" :magnify=".24" class="cg-drop cg-switch-thumb" aria-hidden="true">
      <span class="cg-switch-solid" />
      <el-icon v-if="loading" class="cg-switch-spin is-loading"><Loading /></el-icon>
    </GlassPane>
  </button>
</template>

<style>
.cg-switch {
  position: relative; display: inline-flex; align-items: center; flex-shrink: 0; vertical-align: middle;
  min-width: 60px; height: 30px; padding: 0; border: 0; background: none; font: inherit;
  cursor: pointer; touch-action: none; -webkit-tap-highlight-color: transparent;
}
.cg-switch-small { min-width: 44px; height: 22px; }
.cg-switch:focus-visible { outline: 2px solid var(--el-color-primary); outline-offset: 3px; border-radius: 15px; }
.cg-switch:disabled { cursor: not-allowed; opacity: .5; }
.cg-switch.is-loading { cursor: progress; }
.cg-switch-track, .cg-switch-fill { position: absolute; inset: 0; border-radius: 15px; }
.cg-switch-track { background: rgb(120 120 128 / .3); }
.cg-switch-fill { background: #34c759; opacity: 0; }
html.dark .cg-switch-track { background: rgb(120 120 128 / .4); }
html.dark .cg-switch-fill { background: #30d158; }
.cg.cg-switch-thumb { position: absolute; left: 3px; top: 3px; width: 34px; height: 24px; box-shadow: 0 3px 8px -2px rgb(0 0 0 / .25); will-change: transform; }
.cg-switch-small > .cg.cg-switch-thumb { width: 25px; height: 18px; top: 2px; left: 3px; }
.cg-switch-solid { position: absolute; inset: 0; border-radius: inherit; background: #fff; }
.cg-switch-spin { position: absolute; inset: 0; margin: auto; font-size: 13px; color: #8e8e93; }
.cg-switch-small .cg-switch-spin { font-size: 11px; }
/* 轨道里的文字：长的那个撑开宽度（不显示），开时文字在左、关时在右，都在滑块的另一侧 */
.cg-switch-sizer { visibility: hidden; white-space: nowrap; font-size: 12px; padding: 0 10px 0 calc(34px + 3px + 8px); }
.cg-switch-small .cg-switch-sizer { font-size: 11px; padding: 0 8px 0 calc(25px + 3px + 6px); }
.cg-switch-text { position: absolute; top: 0; bottom: 0; display: flex; align-items: center; white-space: nowrap; font-size: 12px; font-weight: 500; transition: opacity .2s; pointer-events: none; }
.cg-switch-small .cg-switch-text { font-size: 11px; }
.cg-switch-on-text { left: 10px; color: #fff; opacity: 0; }
.cg-switch-off-text { right: 10px; color: var(--el-text-color-secondary); }
.cg-switch-small .cg-switch-on-text { left: 8px; }
.cg-switch-small .cg-switch-off-text { right: 8px; }
.cg-switch.is-on .cg-switch-on-text { opacity: 1; }
.cg-switch.is-on .cg-switch-off-text { opacity: 0; }
</style>
