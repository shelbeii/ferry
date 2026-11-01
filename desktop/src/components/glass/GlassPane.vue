<script setup lang="ts">
// 玻璃材质本身。分四层：光学层（对背后的真实内容做磨砂 + 折射 + 提饱和）、染色层、
// 边缘高光层、内容。内容是普通 DOM，不经过任何滤镜，文字始终清晰。
// 设置里关掉「液态玻璃效果」时（glass.enabled 为 false）变成普通的实心底，不做任何计算和监听。
import { computed, inject, onActivated, onBeforeUnmount, onDeactivated, onMounted, ref, useId, watch } from 'vue'
import { cachedLensImage, glass, lensImage, trackLight, trackTone, viewport, type LensImage } from '../../glass/runtime'
import { glassMotionKey } from '../../glass/context'
import type { LensSpec } from '../../glass/optics'

const props = withDefaults(defineProps<{
  as?: string
  /** regular：面板、导航，带磨砂；clear：按钮、液滴，几乎透明 */
  variant?: 'regular' | 'clear'
  radius?: number | 'pill'
  /** 边缘弧面宽度，默认按短边算 */
  bezel?: number
  magnify?: number
  /** 根据背后内容的明暗换深浅；液滴这类附在别的玻璃上的不需要 */
  adaptive?: boolean
  /** 边缘色散：要多算两遍折射。不填时只有 clear（按钮这类小块）有；regular 的大面板（工具栏条、标签栏）没有，
   *  列表在它们下面滚动时每帧都要重算，色散让滚动的开销多一半，而磨砂后彩边也看不太出来。液滴上关掉：文字边上出彩边会显得糊 */
  dispersion?: boolean
  /** 边缘高光跟着指针转；被别的东西盖住的（开关的滑块静止时盖着白色的面）不需要 */
  lit?: boolean
}>(), { as: 'div', variant: 'regular', radius: 20, magnify: 0, adaptive: true, dispersion: undefined, lit: true })

const root = ref<HTMLElement>()
const beam = ref<HTMLElement>()
const id = `cg-${useId().replace(/[^\w-]/g, '')}`
const size = ref({ width: 0, height: 0 })
const image = ref<LensImage | null>(null)
const moving = inject(glassMotionKey, ref(false))
const paused = computed(() => moving.value || viewport.resizing)

const radius = computed(() => {
  const { width, height } = size.value
  return props.radius === 'pill' ? Math.min(width, height) / 2 : Math.min(props.radius, width / 2, height / 2)
})
// 动画期间只暂停新贴图的计算，已有滤镜始终保留，避免每次打开都从磨砂切回折射。
const scale = computed(() => (glass.enabled ? image.value?.scale ?? 0 : 0) * glass.refraction)
const dispersed = computed(() => glass.dispersion && (props.dispersion ?? props.variant === 'clear'))
const style = computed(() => ({
  '--cg-r': props.radius === 'pill' && !size.value.height ? '999px' : `${radius.value}px`,
  '--cg-frost-k': String(glass.frost),
  '--cg-lens': image.value && scale.value > 0 ? `url(#${id})` : undefined,
}))

function lensSpec(): LensSpec | null {
  const { width, height } = size.value
  if (!glass.enabled || width < 2 || height < 2) return null
  const bezel = props.bezel ?? Math.max(6, Math.min(26, Math.min(width, height) * .32))
  return { width, height, radius: radius.value, bezel, edge: bezel * .85, magnify: props.magnify }
}
function refresh() {
  const spec = lensSpec()
  image.value = spec && lensImage(spec)
}
// 生成位移图要逐像素计算，不放在抽屉滑出、拖动窗口的那几帧里：生成过的直接用；没生成过的等浏览器空下来
// （最多等 0.3 秒）再算，这期间先只有磨砂、或拉伸着用旧图。尺寸还在变时等它停下来
let pending: number | undefined
let idle: number | undefined
function cancelRefresh() {
  clearTimeout(pending)
  if (idle !== undefined) cancelIdleCallback(idle)
  pending = idle = undefined
}
function queueRefresh() {
  cancelRefresh()
  const spec = lensSpec()
  const hit = spec && cachedLensImage(spec)
  if (!spec || hit) { image.value = hit ?? null; return }
  if (paused.value) return
  pending = window.setTimeout(() => {
    pending = undefined
    idle = requestIdleCallback(() => { idle = undefined; refresh() }, { timeout: 300 })
  }, image.value ? 150 : 0)
}

let observer: ResizeObserver | undefined
let untrack: (() => void) | undefined
function start() {
  const element = root.value
  if (!element || untrack || !glass.enabled) return
  const unlight = props.lit ? trackLight(element) : () => {}
  const untone = props.adaptive ? trackTone(element) : () => {}
  untrack = () => { unlight(); untone() }
}
function stop() {
  untrack?.()
  untrack = undefined
}
onMounted(() => {
  const element = root.value!
  observer = new ResizeObserver(() => {
    // 用布局尺寸而不是 getBoundingClientRect：按压、液滴放大用的是 transform，不该触发重算
    const width = element.offsetWidth, height = element.offsetHeight
    // 抽屉用 display:none 隐藏时保留最后一次有效尺寸，滤镜和圆角下次打开可以直接复用。
    if (width < 2 || height < 2) return
    if (width !== size.value.width || height !== size.value.height) size.value = { width, height }
  })
  observer.observe(element)
  start()
})
// 项目页切走后组件还在（KeepAlive），不再跟着指针和滚动更新
onActivated(start)
onDeactivated(stop)
onBeforeUnmount(() => {
  observer?.disconnect()
  cancelRefresh()
  stop()
})
watch(size, ({ width, height }) => {
  // 高光的锥形渐变要盖住整块玻璃：边长取对角线（尺寸变化时才改，不是每帧）
  const side = `${Math.ceil(Math.hypot(width, height)) + 2}px`
  if (beam.value && beam.value.style.width !== side) beam.value.style.width = beam.value.style.height = side
  // 优先复用已有贴图，未缓存的尺寸等滑动抽屉或拖动窗口结束再计算。
  queueRefresh()
})
watch(paused, (value) => {
  if (value) cancelRefresh()
  else queueRefresh()
})
watch(() => [props.radius, props.bezel, props.magnify], queueRefresh)
watch(() => glass.enabled, (on) => {
  queueRefresh()
  if (on) start()
  else { stop(); delete root.value?.dataset.tone }
})

defineExpose({ el: root })
</script>

<template>
  <component :is="as" ref="root" class="cg" :class="[`cg-${variant}`, { 'cg-plain': !glass.enabled }]" :style="style">
    <svg v-if="image && scale > 0" class="cg-defs" aria-hidden="true" focusable="false" width="0" height="0">
      <filter :id="id" x="0" y="0" width="1" height="1" color-interpolation-filters="sRGB">
        <feImage :href="image.url" x="0" y="0" :width="size.width" :height="size.height" preserveAspectRatio="none" result="map" />
        <template v-if="dispersed">
          <!-- 色散：三个通道折射量差 ±4%，边缘出现很淡的彩边 -->
          <feDisplacementMap in="SourceGraphic" in2="map" :scale="scale * .96" xChannelSelector="R" yChannelSelector="G" result="shiftR" />
          <feColorMatrix in="shiftR" type="matrix" values="1 0 0 0 0  0 0 0 0 0  0 0 0 0 0  0 0 0 1 0" result="red" />
          <feDisplacementMap in="SourceGraphic" in2="map" :scale="scale" xChannelSelector="R" yChannelSelector="G" result="shiftG" />
          <feColorMatrix in="shiftG" type="matrix" values="0 0 0 0 0  0 1 0 0 0  0 0 0 0 0  0 0 0 1 0" result="green" />
          <feDisplacementMap in="SourceGraphic" in2="map" :scale="scale * 1.04" xChannelSelector="R" yChannelSelector="G" result="shiftB" />
          <feColorMatrix in="shiftB" type="matrix" values="0 0 0 0 0  0 0 0 0 0  0 0 1 0 0  0 0 0 1 0" result="blue" />
          <feBlend in="red" in2="green" mode="screen" result="redGreen" />
          <feBlend in="redGreen" in2="blue" mode="screen" />
        </template>
        <feDisplacementMap v-else in="SourceGraphic" in2="map" :scale="scale" xChannelSelector="R" yChannelSelector="G" />
      </filter>
    </svg>
    <span class="cg-layer cg-optic" aria-hidden="true" />
    <span class="cg-layer cg-tint" aria-hidden="true" />
    <span class="cg-layer cg-rim" aria-hidden="true"><span class="cg-sheen"><span ref="beam" class="cg-beam" /></span></span>
    <slot />
  </component>
</template>

<style>
/* 注意：不要用 @property 注册变量、不要给 CSS 变量做过渡、不要每帧用 JS 改 CSS 变量——
   WebView2 131 在拖动窗口（大量重算样式）时会在 CSS 变量的处理里崩溃（STATUS_ACCESS_VIOLATION，
   崩在 StyleCascade::Apply / CSSVariableParser）。跟着指针、按压这类每帧变化的只改 transform、opacity */
.cg {
  --cg-blur: calc(var(--cg-frost) * var(--cg-frost-k, 1) * var(--cg-blur-k, 1));
  /* 高光强度的上限；离指针远近由 .cg-sheen 的 opacity 调 */
  --cg-rim-hi: 1;
  --cg-rim-back: .45;
  --cg-rim-lo: .16;
  position: relative;
  /* 自成一层：材质层放在 z-index -1，任何内容（包括直接写的文字）都画在它上面 */
  isolation: isolate;
  border-radius: var(--cg-r);
  color: var(--el-text-color-primary);
  /* 最后一道是极细的外沿：浅色主题里白玻璃压在浅色底上，靠它和阴影把轮廓勾出来 */
  box-shadow: 0 10px 30px -12px rgb(16 24 40 / .28), 0 2px 6px -2px rgb(16 24 40 / .1), 0 0 0 .5px rgb(16 24 40 / .07);
}
/* 饱和度提得比实际颜色高一些：透过玻璃看到的背景颜色更鲜，像真的液体（苹果原生也这样） */
.cg-regular { --cg-frost: 7px; --cg-sat: 1.6; --cg-bright: 1.05; --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.5 * var(--cg-k, 1))), rgb(255 255 255 / calc(.32 * var(--cg-k, 1)))); }
.cg-clear { --cg-frost: .5px; --cg-sat: 1.5; --cg-bright: 1.08; --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.14 * var(--cg-k, 1))), rgb(255 255 255 / calc(.05 * var(--cg-k, 1)))); }
/* 浮起的液滴：完全不磨砂、几乎不染色，只靠折射和放大，像一颗水珠 */
.cg.cg-drop { --cg-frost: 0px; --cg-sat: 1.15; --cg-bright: 1.02; --cg-fill: radial-gradient(120% 90% at 50% 0%, rgb(255 255 255 / calc(.1 * var(--cg-k, 1))), transparent 60%); --cg-rim-hi: .95; --cg-rim-back: .55; --cg-rim-lo: .18; }
.cg.cg-drop > .cg-rim > .cg-sheen { opacity: 1 !important; }
/* 深色主题：边缘高光更亮、玻璃本身更通透（一层很淡的白光 + 一层薄薄的深色），背后的光晕颜色能透出来 */
html.dark .cg { --cg-rim-hi: .82; --cg-rim-back: .34; --cg-rim-lo: .08; box-shadow: 0 12px 34px -12px rgb(0 0 0 / .6), 0 2px 6px -2px rgb(0 0 0 / .35), 0 0 0 .5px rgb(255 255 255 / .06); }
html.dark .cg-regular { --cg-bright: .94; --cg-sat: 1.5; --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.1 * var(--cg-k, 1))), rgb(255 255 255 / calc(.04 * var(--cg-k, 1)))), linear-gradient(rgb(16 22 36 / calc(.36 * var(--cg-k, 1))), rgb(16 22 36 / calc(.3 * var(--cg-k, 1)))); }
html.dark .cg-clear { --cg-bright: 1.06; --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.1 * var(--cg-k, 1))), rgb(255 255 255 / calc(.03 * var(--cg-k, 1)))); }

/* 自适应：浅色主题里压在深色内容上，或深色主题里压在浅色内容上，玻璃和文字一起反过来 */
html:not(.dark) .cg[data-tone="dark"] {
  --el-text-color-primary: #fff; --el-text-color-regular: rgb(255 255 255 / .88); --el-text-color-secondary: rgb(255 255 255 / .68);
  --el-color-primary: #c7c4ff; --cg-rim-hi: .76; --cg-rim-back: .26; --cg-rim-lo: .09;
}
html:not(.dark) .cg-regular[data-tone="dark"] { --cg-bright: .82; --cg-fill: linear-gradient(180deg, rgb(30 32 44 / calc(.34 * var(--cg-k, 1))), rgb(20 22 30 / calc(.26 * var(--cg-k, 1)))); }
html:not(.dark) .cg-clear[data-tone="dark"] { --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.08 * var(--cg-k, 1))), rgb(255 255 255 / calc(.02 * var(--cg-k, 1)))); }
html.dark .cg[data-tone="light"] {
  --el-text-color-primary: #111827; --el-text-color-regular: rgb(17 24 39 / .85); --el-text-color-secondary: rgb(17 24 39 / .6);
  --el-color-primary: #4f46e5;
}
html.dark .cg-regular[data-tone="light"] { --cg-bright: 1.05; --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.42 * var(--cg-k, 1))), rgb(255 255 255 / calc(.26 * var(--cg-k, 1)))); }
html.dark .cg-clear[data-tone="light"] { --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.16 * var(--cg-k, 1))), rgb(255 255 255 / calc(.06 * var(--cg-k, 1)))); }

.cg-defs { position: absolute; width: 0; height: 0; pointer-events: none; }
.cg-layer { position: absolute; inset: 0; z-index: -1; border-radius: inherit; pointer-events: none; }
.cg-optic {
  -webkit-backdrop-filter: blur(var(--cg-blur)) saturate(var(--cg-sat)) brightness(var(--cg-bright));
  backdrop-filter: blur(var(--cg-blur)) var(--cg-lens,) saturate(var(--cg-sat)) brightness(var(--cg-bright));
}
/* 工具栏下面垫的玻璃条（项目页的工具栏、软件页和端口页贴在顶上的工具栏都用）：列表从它下面滑过，
   滚过去的行被磨砂、折射，只透出颜色，不和按钮、输入框的字混在一起；底色淡，按钮少时也不像一条空白条。
   放在工具栏里第一个、铺满工具栏，工具栏要自成一层（position + z-index） */
.cg.cg-strip {
  position: absolute;
  inset: 0;
  z-index: -1;
  --cg-frost: 16px;
  --cg-fill: linear-gradient(180deg, rgb(255 255 255 / calc(.34 * var(--cg-k, 1))), rgb(255 255 255 / calc(.22 * var(--cg-k, 1))));
  box-shadow: none;
}
html.dark .cg.cg-strip { --cg-fill: linear-gradient(180deg, rgb(24 30 44 / calc(.5 * var(--cg-k, 1))), rgb(20 26 38 / calc(.4 * var(--cg-k, 1)))); box-shadow: none; }
/* 拖动窗口改大小期间不做折射、只留磨砂（见 runtime.ts 的 viewport） */
html.cg-resizing .cg-optic {
  -webkit-backdrop-filter: blur(var(--cg-blur)) saturate(var(--cg-sat)) brightness(var(--cg-bright));
  backdrop-filter: blur(var(--cg-blur)) saturate(var(--cg-sat)) brightness(var(--cg-bright));
}
.cg-tint { background: var(--cg-fill); }
/* 玻璃的厚度感：内沿一圈亮边，左上更亮；底部一点暗影 */
.cg-rim {
  box-shadow:
    inset 1.5px 2px 1px -1.5px rgb(255 255 255 / .75),
    inset -1px -1.5px 1px -1px rgb(255 255 255 / .35),
    inset 0 0 10px 1px rgb(255 255 255 / .1),
    inset 0 -8px 16px -12px rgb(16 24 40 / .12);
}
html.dark .cg-rim {
  box-shadow:
    inset 1.5px 2px 1px -1.5px rgb(255 255 255 / .35),
    inset -1px -1.5px 1px -1px rgb(255 255 255 / .14),
    inset 0 0 12px 1px rgb(255 255 255 / .04),
    inset 0 -8px 16px -12px rgb(0 0 0 / .4);
}
/* 外沿的镜面高光：朝向光源（指针）的一侧最亮，对侧有一道较弱的反光。
   .cg-sheen 是 1px 的圆角环（遮罩出来），里面的 .cg-beam 是一块以玻璃中心为圆心的锥形渐变，
   转动它（transform）改高光方向，改 .cg-sheen 的 opacity 表示指针远近 */
.cg-sheen {
  position: absolute;
  inset: 0;
  padding: 1px;
  border-radius: inherit;
  overflow: hidden;
  opacity: .62;
  transition: opacity .4s ease-out;
  -webkit-mask: linear-gradient(#000 0 0) content-box, linear-gradient(#000 0 0);
  -webkit-mask-composite: xor;
  mask: linear-gradient(#000 0 0) content-box exclude, linear-gradient(#000 0 0);
}
.cg-beam {
  position: absolute;
  left: 50%;
  top: 50%;
  width: 200px;
  height: 200px;
  translate: -50% -50%;
  transform: rotate(-45deg);
  transition: transform .4s cubic-bezier(.2, .8, .2, 1);
  background: conic-gradient(
    rgb(255 255 255 / var(--cg-rim-hi)) 0deg,
    rgb(255 255 255 / var(--cg-rim-lo)) 55deg,
    rgb(255 255 255 / var(--cg-rim-lo)) 125deg,
    rgb(255 255 255 / var(--cg-rim-back)) 180deg,
    rgb(255 255 255 / var(--cg-rim-lo)) 235deg,
    rgb(255 255 255 / var(--cg-rim-lo)) 305deg,
    rgb(255 255 255 / var(--cg-rim-hi)) 360deg);
}
/* 内容叠放顺序：子元素按出现先后排，后面的（如液滴）盖住前面的 */
:where(.cg) > :where(:not(.cg-layer, .cg-defs)) { position: relative; }

/* 关掉液态玻璃：普通的实心底和细边框 */
.cg.cg-plain { box-shadow: var(--ferry-shadow, none); }
.cg-plain > .cg-optic, .cg-plain > .cg-rim { display: none; }
.cg-plain > .cg-tint { background: var(--ferry-surface, var(--el-bg-color)); box-shadow: inset 0 0 0 1px var(--ferry-stroke, var(--el-border-color)); }

@media (prefers-reduced-transparency: reduce), (forced-colors: active) {
  .cg-optic, html.cg-resizing .cg-optic { backdrop-filter: none; -webkit-backdrop-filter: none; }
  .cg-tint { background: var(--el-bg-color); }
}
</style>
