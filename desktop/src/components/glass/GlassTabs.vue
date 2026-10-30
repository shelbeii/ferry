<script setup lang="ts" generic="T extends string">
// 玻璃导航（对应 iOS 26 的标签栏）。静止时选中项下面是一块浅色底；按下、拖动或切换时，
// 选中块「浮起」成一滴透明的液滴：比导航条更高、放大下面的文字，移动越快拉得越长，
// 到位后落回成底色块。松手吸附到最近的一项。方向键、Home、End 也能切换。
// 放大的文字不是把背后截图放大（会糊），而是液滴里另有一排真正的文字，按放大倍数重新绘制；
// 液滴底下的原文字同时隐去，不会叠出两层。
// 这排文字不能放在液滴里面：液滴有 backdrop-filter，是单独合成的一层，缩放每帧都在变时浏览器不重新绘制它，
// 而是把开始时画好的拉伸着用，移动中文字就糊了（停下才重画清楚）。所以文字放在液滴上面另一层，按液滴的轮廓裁出来。
import { nextTick, onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, watch, type Component } from 'vue'
import GlassPane from './GlassPane.vue'
import { createSprings } from '../../glass/optics'
import { glass, reducedMotion } from '../../glass/runtime'

const props = withDefaults(defineProps<{ options: { value: T; label: string; icon?: Component }[]; label?: string }>(), { label: '切换' })
const model = defineModel<T>({ required: true })
const track = ref<InstanceType<typeof GlassPane>>()
const drop = ref<InstanceType<typeof GlassPane>>()
const pill = ref<HTMLElement>()
const lens = ref<HTMLElement>()
const view = ref<HTMLElement>()
const trackEl = () => track.value?.el as HTMLElement | undefined
// 浮起、拖动只在液态玻璃打开、系统没要求减少动画时有；关掉时选中块只是滑过去
const lifts = () => glass.enabled && !reducedMotion()

let slots: { value: T; left: number; width: number; top: number; height: number; button: HTMLElement }[] = []
let base = 0
let dropTop = 0
let dropHeight = 0
let trackWidth = 0
let trackHeight = 0
let masked = false
let landing = false
let press: { id: number; x: number; value: T; dragging: boolean } | undefined

// 浮起要快：一按下就有反馈。落下要慢一些（约 0.3 秒）、略带回弹，像水滴落回去；到位后先在上面停一下再落
const RISE = { stiffness: 520, damping: 32, precision: .002 }
const LAND = { stiffness: 110, damping: 14, precision: .002 }
const HOLD = 90
let holdTimer: number | undefined
function rise() {
  clearTimeout(holdTimer)
  springs.tune(2, RISE)
  springs.to(2, 1)
}
function land(delay: number) {
  clearTimeout(holdTimer)
  holdTimer = window.setTimeout(() => {
    if (press) return
    springs.tune(2, LAND)
    springs.to(2, 0)
  }, delay)
}

// x、宽度、浮起程度。滑动用接近临界的阻尼：顺滑、到位时只有一点点过冲
const springs = createSprings([
  { stiffness: 230, damping: 26, precision: .1 },
  { stiffness: 230, damping: 26, precision: .1 },
  RISE,
], render)

function render([x, width, lift]: number[], [velocity]: number[]) {
  const droplet = drop.value?.el as HTMLElement | undefined
  if (!droplet || !pill.value || !base) return
  const shift = x + width / 2 - base / 2
  const stretch = Math.min(.16, Math.abs(velocity) / 2600) * Math.min(1, lift * 1.5)
  const grow = 1 + .12 * lift
  droplet.style.transform = `translateX(${shift.toFixed(2)}px) scale(${(width / base * grow * (1 + stretch)).toFixed(4)}, ${((1 + .34 * lift) * (1 - stretch * .45)).toFixed(4)})`
  // 落下时缩小和变透明一起进行，不是缩到最后一下子消失
  droplet.style.opacity = Math.max(0, Math.min(1, lift * 1.6)).toFixed(3)
  droplet.style.visibility = lift < .004 ? 'hidden' : 'visible'
  if (view.value) { view.value.style.opacity = droplet.style.opacity; view.value.style.visibility = droplet.style.visibility }
  pill.value.style.transform = `translateX(${shift.toFixed(2)}px) scaleX(${(width / base).toFixed(4)})`
  pill.value.style.opacity = (1 - Math.min(1, lift * 1.6)).toFixed(3)
  magnify(shift, width / base * grow * (1 + stretch), (1 + .34 * lift) * (1 - stretch * .45), lift)
  // 移动中保持浮起，快到位时停一下再落下
  if (landing && !press && Math.abs(x - springs.target(0)) < 3) {
    landing = false
    land(HOLD)
  }
}
const paint = () => render([0, 1, 2].map(springs.value), [springs.velocity(0)])

// 液滴里那排文字：按放大倍数 m 绕液滴中心放大，和背后那一排对齐；外面一层按液滴现在的轮廓（拉伸后的胶囊形）裁出来
function magnify(shift: number, sx: number, sy: number, lift: number) {
  const visible = lift >= .004
  if (lens.value && view.value && visible && dropHeight) {
    const m = 1 + .18 * lift
    const cx = shift + base / 2, cy = dropTop + dropHeight / 2
    lens.value.style.transform = `translate(${((1 - m) * cx).toFixed(2)}px, ${((1 - m) * cy).toFixed(2)}px) scale(${m.toFixed(4)})`
    const hw = base * sx / 2, hh = dropHeight * sy / 2
    const px = (n: number) => `${n.toFixed(2)}px`
    view.value.style.clipPath = `inset(${px(cy - hh)} ${px(trackWidth - cx - hw)} ${px(trackHeight - cy - hh)} ${px(cx - hw)} round ${px(Math.min(hw, dropHeight / 2 * sx))} / ${px(hh)})`
  }
  // 液滴底下的原文字隐去（边缘 6px 渐变）：只改遮罩，不用 CSS 变量
  if (!visible) {
    if (masked) for (const s of slots) s.button.style.removeProperty('mask-image')
    masked = false
    return
  }
  const half = base * sx / 2
  const left = shift + base / 2 - half, right = shift + base / 2 + half
  const keep = `rgb(0 0 0 / ${(1 - Math.min(1, lift * 1.6)).toFixed(3)})`
  for (const s of slots) {
    if (right <= s.left || left >= s.left + s.width) { s.button.style.removeProperty('mask-image'); continue }
    const a = left - s.left, b = right - s.left
    s.button.style.maskImage = `linear-gradient(to right, #000 ${a.toFixed(1)}px, ${keep} ${(a + 6).toFixed(1)}px, ${keep} ${(b - 6).toFixed(1)}px, #000 ${b.toFixed(1)}px)`
  }
  masked = true
}

function measure() {
  const buttons = trackEl()?.querySelectorAll<HTMLElement>(':scope > .cg-tab') ?? []
  slots = Array.from(buttons, (button, i) => ({ value: props.options[i].value, left: button.offsetLeft, width: button.offsetWidth, top: button.offsetTop, height: button.offsetHeight, button }))
  const droplet = drop.value?.el as HTMLElement | undefined
  if (droplet) { dropTop = droplet.offsetTop; dropHeight = droplet.offsetHeight }
  if (view.value) { trackWidth = view.value.offsetWidth; trackHeight = view.value.offsetHeight }
  // 液滴里那排文字摆到和背后完全相同的位置（尺寸变化时才改）
  const copies = lens.value?.children ?? []
  slots.forEach((s, i) => {
    const copy = copies[i] as HTMLElement | undefined
    if (!copy) return
    copy.style.left = `${s.left}px`; copy.style.top = `${s.top}px`
    copy.style.width = `${s.width}px`; copy.style.height = `${s.height}px`
  })
}
function setBase(width: number) {
  if (width === base) return
  base = width
  for (const element of [drop.value?.el as HTMLElement | undefined, pill.value]) if (element) element.style.width = `${width}px`
  paint()
}
function nearest(center: number) {
  return slots.reduce((a, b) => Math.abs(a.left + a.width / 2 - center) <= Math.abs(b.left + b.width / 2 - center) ? a : b)
}
function go(value: T, animate = true) {
  measure()
  const slot = slots.find((s) => s.value === value)
  if (!slot) return
  setBase(slot.width)
  if (!animate || reducedMotion() || !springs.value(1)) {
    springs.jump(0, slot.left); springs.jump(1, slot.width); springs.jump(2, 0)
    return
  }
  springs.to(0, slot.left)
  springs.to(1, slot.width)
  if (lifts() && Math.abs(springs.value(0) - slot.left) > 2) { landing = true; rise() }
  else if (!press) land(0)
}
function choose(value: T) {
  if (model.value !== value) model.value = value
  else go(value)
}

function down(event: PointerEvent) {
  const button = (event.target as Element).closest<HTMLElement>('.cg-tab')
  if (event.button !== 0 || press || !button) return
  measure()
  press = { id: event.pointerId, x: event.clientX, value: button.dataset.value as T, dragging: false }
  trackEl()?.setPointerCapture(event.pointerId)
  if (lifts()) rise()
}
function move(event: PointerEvent) {
  if (!press || event.pointerId !== press.id || !lifts()) return
  if (!press.dragging && Math.abs(event.clientX - press.x) < 4) return
  press.dragging = true
  const x = event.clientX - trackEl()!.getBoundingClientRect().left
  const slot = nearest(x)
  const last = slots[slots.length - 1]
  springs.to(1, slot.width)
  springs.to(0, Math.max(slots[0].left - 8, Math.min(x - slot.width / 2, last.left + last.width - slot.width + 8)))
}
function up(event: PointerEvent) {
  if (!press || event.pointerId !== press.id) return
  // 用手指的位置（弹簧目标）而不是液滴当前的位置，快速甩动也落在松手处
  const value = press.dragging ? nearest(springs.target(0) + springs.target(1) / 2).value : press.value
  if (trackEl()?.hasPointerCapture(event.pointerId)) trackEl()!.releasePointerCapture(event.pointerId)
  press = undefined
  choose(value)
}
function keydown(event: KeyboardEvent) {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const index = Math.max(0, props.options.findIndex((o) => o.value === model.value))
  const count = props.options.length
  const next = event.key === 'Home' ? 0 : event.key === 'End' ? count - 1 : (index + (event.key === 'ArrowRight' ? 1 : -1) + count) % count
  choose(props.options[next].value)
  nextTick(() => trackEl()?.querySelectorAll<HTMLElement>(':scope > .cg-tab')[next]?.focus())
}

let observer: ResizeObserver | undefined
onMounted(() => {
  observer = new ResizeObserver(() => go(model.value, false))
  observer.observe(trackEl()!)
  go(model.value, false)
})
function settle() {
  press = undefined
  landing = false
  clearTimeout(holdTimer)
  go(model.value, false)
}
onActivated(settle)
onDeactivated(settle)
onBeforeUnmount(() => { observer?.disconnect(); clearTimeout(holdTimer); springs.stop() })
watch(() => glass.enabled, settle)
watch(model, (value) => go(value))
</script>

<template>
  <GlassPane ref="track" variant="regular" radius="pill" class="cg-tabs" role="radiogroup" :aria-label="label"
    @pointerdown="down" @pointermove="move" @pointerup="up" @pointercancel="up" @keydown="keydown">
    <span ref="pill" class="cg-tabs-pill" aria-hidden="true" />
    <button v-for="option in options" :key="option.value" type="button" class="cg-tab" role="radio" :data-value="option.value"
      :aria-checked="model === option.value" :tabindex="model === option.value ? 0 : -1" @click="choose(option.value)">
      <el-icon v-if="option.icon"><component :is="option.icon" /></el-icon>{{ option.label }}
    </button>
    <GlassPane ref="drop" variant="clear" radius="pill" :dispersion="false" class="cg-drop cg-tabs-drop" aria-hidden="true" />
    <span ref="view" class="cg-tabs-view" aria-hidden="true">
      <span ref="lens" class="cg-tabs-lens">
        <span v-for="option in options" :key="option.value" class="cg-tab" :aria-checked="model === option.value">
          <el-icon v-if="option.icon"><component :is="option.icon" /></el-icon>{{ option.label }}
        </span>
      </span>
    </span>
  </GlassPane>
</template>

<style>
.cg-tabs { display: inline-flex; align-items: center; height: 50px; padding: 0 5px; box-sizing: border-box; touch-action: pan-y; user-select: none; }
.cg-tab {
  display: inline-flex; align-items: center; gap: 6px; height: 40px; padding: 0 17px;
  border: 0; border-radius: 20px; background: none; font: inherit; font-size: 13px; font-weight: 500;
  color: var(--el-text-color-regular); cursor: pointer; white-space: nowrap; -webkit-tap-highlight-color: transparent;
  transition: color .2s;
}
.cg-tab > .el-icon { font-size: 16px; }
.cg-tab[aria-checked="true"] { color: var(--el-color-primary); font-weight: 600; }
.cg-tab:focus-visible { outline: 2px solid var(--el-color-primary); outline-offset: -2px; }
.cg-tabs-pill, .cg.cg-tabs-drop { position: absolute; left: 0; top: 5px; height: 40px; border-radius: 20px; pointer-events: none; }
.cg-tabs-pill { background: rgb(118 118 128 / .16); }
/* 关掉液态玻璃：和原来的分段切换一样，选中项是主色底白字 */
.cg-plain > .cg-tabs-pill { background: var(--el-color-primary); box-shadow: 0 2px 6px color-mix(in srgb, var(--el-color-primary) 24%, transparent); }
.cg-plain > .cg-tab[aria-checked="true"] { color: #fff; }
html.dark .cg-plain > .cg-tab[aria-checked="true"] { color: var(--el-bg-color-page); }
html.dark .cg-tabs-pill { background: rgb(255 255 255 / .12); }
.cg.cg-tabs-drop { visibility: hidden; overflow: hidden; box-shadow: 0 6px 18px -6px rgb(16 24 40 / .3); }
/* 液滴里放大的那排文字：叠在液滴上面、铺满导航条，由 magnify() 设裁剪轮廓和放大 */
.cg-tabs-view { position: absolute; inset: 0; visibility: hidden; pointer-events: none; }
.cg-tabs-lens { position: absolute; left: 0; top: 0; transform-origin: 0 0; pointer-events: none; }
.cg-tabs-lens > .cg-tab { position: absolute; box-sizing: border-box; cursor: default; transition: none; }
</style>
