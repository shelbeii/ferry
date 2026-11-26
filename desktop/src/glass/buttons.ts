import { watch } from 'vue'
import { createSprings, fitButtonAxis, rubber } from './optics'
import { glass, lensImage, reducedMotion } from './runtime'
import './buttons.css'

// 事件委托也覆盖挂在 body 上的 ElMessageBox、el-popconfirm 和按需渲染的按钮。
const buttons = 'button:not(.cg-switch, .cg-tab, .el-collapse-item__header), .el-button, [role="button"], .install-template-card'
const compact = '.is-link, .is-text, [role="button"], .rail-btn, .cg-menu-item'
const svgNS = 'http://www.w3.org/2000/svg'
const filterId = 'ferry-pressed-button-lens'
type Trigger = { pointerId: number; x: number; y: number } | { key: string }

interface Motion {
  button: HTMLElement
  trigger: Trigger | undefined
  clips: { element: HTMLElement; x: boolean; y: boolean }[]
  compact: boolean
  glow: HTMLElement
  effect?: HTMLElement
  idle?: number
  original: { translate: string; scale: string; position: string }
  x: number
  y: number
  scaleX: number
  scaleY: number
}

function enabled(button: HTMLElement) {
  return button.isConnected && !button.closest('[inert]') &&
    !button.matches(':disabled, [aria-disabled="true"], .is-disabled, .is-loading, [role="button"].off')
}

function targetButton(event: Event) {
  if (!(event.target instanceof Element) || event.target.closest('input, textarea, select, [contenteditable="true"]')) return
  const button = event.target.closest<HTMLElement>(buttons)
  // 下拉框和弹出面板按触发按钮的边界定位，按压时保留原来的边界。
  if (button && !button.matches('[data-glass-motion="local"], [aria-haspopup]:not([aria-haspopup="false"])')) return button
}

function clippingParents(button: HTMLElement) {
  const parents: Motion['clips'] = []
  const clips = /^(auto|scroll|hidden|clip)$/
  for (let element = button.parentElement; element; element = element.parentElement) {
    const style = getComputedStyle(element)
    const x = clips.test(style.overflowX), y = clips.test(style.overflowY)
    if (x || y) parents.push({ element, x, y })
  }
  return parents
}

function visibleArea(parents: Motion['clips']) {
  const root = document.documentElement
  const area = { left: 0, top: 0, right: root.clientWidth, bottom: root.clientHeight }
  for (const { element, x, y } of parents) {
    const rect = element.getBoundingClientRect()
    if (x) {
      const scale = rect.width / Math.max(1, element.offsetWidth)
      const left = rect.left + element.clientLeft * scale
      area.left = Math.max(area.left, left)
      area.right = Math.min(area.right, left + element.clientWidth * scale)
    }
    if (y) {
      const scale = rect.height / Math.max(1, element.offsetHeight)
      const top = rect.top + element.clientTop * scale
      area.top = Math.max(area.top, top)
      area.bottom = Math.min(area.bottom, top + element.clientHeight * scale)
    }
  }
  return area
}

/** 普通按钮只给当前按住的那一颗挂折射层，长表格里的其他按钮只画材质。 */
function addLens(button: HTMLElement, effect: HTMLElement) {
  const width = button.offsetWidth, height = button.offsetHeight
  if (width < 2 || height < 2 || !glass.refraction) return
  const radius = Math.min(width / 2, height / 2, parseFloat(getComputedStyle(button).borderTopLeftRadius) || 0)
  const bezel = Math.max(3, Math.min(18, Math.min(width, height) * .3))
  const lens = lensImage({ width, height, radius, bezel, edge: bezel * .85, magnify: 0 })
  if (!lens) return
  const svg = document.createElementNS(svgNS, 'svg')
  svg.setAttribute('width', '0')
  svg.setAttribute('height', '0')
  svg.setAttribute('class', 'cg-defs')
  const filter = document.createElementNS(svgNS, 'filter')
  filter.id = filterId
  filter.setAttribute('x', '0')
  filter.setAttribute('y', '0')
  filter.setAttribute('width', '1')
  filter.setAttribute('height', '1')
  filter.setAttribute('color-interpolation-filters', 'sRGB')
  const image = document.createElementNS(svgNS, 'feImage')
  image.setAttribute('href', lens.url)
  image.setAttribute('width', String(width))
  image.setAttribute('height', String(height))
  image.setAttribute('preserveAspectRatio', 'none')
  image.setAttribute('result', 'map')
  const displacement = document.createElementNS(svgNS, 'feDisplacementMap')
  displacement.setAttribute('in', 'SourceGraphic')
  displacement.setAttribute('in2', 'map')
  displacement.setAttribute('xChannelSelector', 'R')
  displacement.setAttribute('yChannelSelector', 'G')
  displacement.setAttribute('scale', String(lens.scale * glass.refraction))
  filter.append(image, displacement)
  svg.append(filter)
  effect.append(svg)
  effect.style.setProperty('--cg-pressed-lens', `url(#${filterId})`)
}

let active: Motion | undefined
let layoutFrame: number | undefined

function draw([press, dx, dy]: number[]) {
  const motion = active
  if (!motion) return
  const { button, glow } = motion
  if ((!press && !dx && !dy) || !button.isConnected) { reset(); return }
  // 点击后进入加载态的按钮仍然在原位，松手的回弹可以继续跑完。
  if (motion.trigger && !enabled(button)) release()
  // 用上一帧的偏移还原按钮原位，滚动时按当前可见区重新限制拖动。
  const box = button.getBoundingClientRect()
  const area = visibleArea(motion.clips)
  if (!box.width || !box.height || area.right <= area.left || area.bottom <= area.top) { reset(); return }
  const width = box.width / motion.scaleX, height = box.height / motion.scaleY
  const unitX = width / Math.max(1, button.offsetWidth), unitY = height / Math.max(1, button.offsetHeight)
  const x = box.left + box.width / 2 - motion.x * unitX
  const y = box.top + box.height / 2 - motion.y * unitY
  if (x + width / 2 <= area.left || x - width / 2 >= area.right || y + height / 2 <= area.top || y - height / 2 >= area.bottom) {
    reset()
    return
  }
  const grow = 1 + press * Math.min(motion.compact ? .08 : .16, 6 / Math.max(1, button.offsetHeight))
  const pullX = Math.abs(dx) / 8 * .07, pullY = Math.abs(dy) / 8 * .07
  const horizontal = fitButtonAxis(x, width, area.left, area.right, dx * unitX, grow * (1 + pullX - pullY * .5))
  const vertical = fitButtonAxis(y, height, area.top, area.bottom, dy * unitY, grow * (1 + pullY - pullX * .5))
  motion.x = horizontal.offset / unitX
  motion.y = vertical.offset / unitY
  motion.scaleX = horizontal.scale
  motion.scaleY = vertical.scale
  button.style.translate = `${motion.x}px ${motion.y}px`
  button.style.scale = `${motion.scaleX} ${motion.scaleY}`
  glow.style.opacity = Math.max(0, Math.min(1, press)).toFixed(3)
}

const springs = createSprings([
  { stiffness: 520, damping: 22, precision: .002 },
  { stiffness: 420, damping: 24, precision: .05 },
  { stiffness: 420, damping: 24, precision: .05 },
], draw)

function reset() {
  const motion = active
  if (!motion) return
  active = undefined
  springs.stop()
  for (const index of [0, 1, 2]) springs.jump(index, 0)
  if (layoutFrame !== undefined) cancelAnimationFrame(layoutFrame)
  layoutFrame = undefined
  if (motion.idle !== undefined) cancelIdleCallback(motion.idle)
  const { button, glow, effect, original } = motion
  delete button.dataset.glassMoving
  button.style.translate = original.translate
  button.style.scale = original.scale
  button.style.position = original.position
  glow.style.removeProperty('opacity')
  effect?.remove()
}

function refresh() {
  if (!active || springs.running) return
  layoutFrame ??= requestAnimationFrame(() => {
    layoutFrame = undefined
    draw([springs.value(0), springs.value(1), springs.value(2)])
  })
}

function begin(button: HTMLElement, trigger: Trigger) {
  if (active?.trigger || !glass.enabled || reducedMotion() || !enabled(button)) return
  reset()
  const box = button.getBoundingClientRect()
  if (!box.width || !box.height) return
  const original = { translate: button.style.translate, scale: button.style.scale, position: button.style.position }
  const clips = clippingParents(button)
  let glow = button.querySelector<HTMLElement>(':scope > .cg-glow')
  let effect: HTMLElement | undefined
  if (!glow) {
    effect = document.createElement('span')
    effect.className = 'cg-native-button-effect'
    effect.setAttribute('aria-hidden', 'true')
    glow = document.createElement('span')
    glow.className = 'cg-glow'
    effect.append(glow)
    button.append(effect)
  }
  const motion: Motion = { button, trigger, clips, compact: button.matches(compact), glow, effect, original, x: 0, y: 0, scaleX: 1, scaleY: 1 }
  active = motion
  if (getComputedStyle(button).position === 'static') button.style.position = 'relative'
  // Vue 更新加载态时会重写 class，用独立属性保留回弹期间的层级。
  button.dataset.glassMoving = ''
  const px = 'x' in trigger ? (trigger.x - box.left) / box.width * 100 : 50
  const py = 'y' in trigger ? (trigger.y - box.top) / box.height * 100 : 50
  glow.style.backgroundPosition = `${100 - px}% ${100 - py}%`
  if (effect) motion.idle = requestIdleCallback(() => {
    motion.idle = undefined
    if (active === motion && motion.trigger) addLens(button, effect)
  }, { timeout: 250 })
  springs.to(0, 1)
}

function release() {
  if (!active) return
  active.trigger = undefined
  for (const index of [0, 1, 2]) springs.to(index, 0)
  // 按下和松手可能落在同一帧，此时弹簧还没离开原点。
  if (!springs.running) reset()
}

let installed = false
export function installGlassButtons() {
  if (installed || typeof document === 'undefined') return
  installed = true
  const capture = { capture: true, passive: true }
  document.addEventListener('pointerdown', (event) => {
    if (event.button !== 0) return
    const button = targetButton(event)
    if (button) begin(button, { pointerId: event.pointerId, x: event.clientX, y: event.clientY })
  }, capture)
  window.addEventListener('pointermove', (event) => {
    const trigger = active?.trigger
    if (!trigger || !('pointerId' in trigger) || event.pointerId !== trigger.pointerId) return
    const limit = active?.compact ? 4 : 8
    springs.to(1, rubber(event.clientX - trigger.x, limit))
    springs.to(2, rubber(event.clientY - trigger.y, limit))
  }, capture)
  const pointerUp = (event: PointerEvent) => {
    const trigger = active?.trigger
    if (trigger && 'pointerId' in trigger && event.pointerId === trigger.pointerId) release()
  }
  window.addEventListener('pointerup', pointerUp, capture)
  window.addEventListener('pointercancel', pointerUp, capture)
  document.addEventListener('keydown', (event) => {
    if (event.repeat || (event.key !== 'Enter' && event.key !== ' ')) return
    const button = targetButton(event)
    if (button) begin(button, { key: event.key })
  }, capture)
  document.addEventListener('keyup', (event) => {
    const trigger = active?.trigger
    if (trigger && 'key' in trigger && event.key === trigger.key) release()
  }, capture)
  document.addEventListener('focusout', (event) => {
    if (active?.button === event.target && active.trigger && 'key' in active.trigger) release()
  }, capture)
  window.addEventListener('scroll', refresh, capture)
  window.addEventListener('resize', refresh, { passive: true })
  window.addEventListener('blur', reset)
  watch(() => glass.enabled, (on) => { if (!on) reset() })
}
