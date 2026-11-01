// 液态玻璃的分段切换：所有 el-radio-group 里的 el-radio-button（「表单 / YAML」「全部配置 / 只看写了的」……）。
// 液态玻璃打开时是一条浅色凹槽（样式见 style.css），选中项下面是一颗玻璃液滴：切换时液滴浮起、变清透、放大一点，
// 滑到新的选项（移动越快拉得越长），到位后停一下再落回成白色的选中块。按住时也会浮起。
// 和下拉框的液滴（menus.ts）一样不改组件：鼠标第一次移到分段上、或用键盘聚焦时才放进液滴，之前由选中项自己画出同样的选中块。
// 只改 transform、opacity（不改 CSS 变量，见 GlassPane 的样式说明）。液态玻璃关掉时把液滴都拿掉。
import { watch } from 'vue'
import { createSprings } from './optics'
import { glass, reducedMotion } from './runtime'

const BUTTON = '.el-radio-button'

interface Segments {
  group: HTMLElement
  drop: HTMLElement
  fill: HTMLElement
  springs: ReturnType<typeof createSprings>
  base: number
  shown: boolean
  landing: boolean
  pressed: boolean
  hold?: number
  observers: [MutationObserver, ResizeObserver]
}
const all = new Map<HTMLElement, Segments>()

// 浮起要快，落下慢一些、略带回弹；到位后先在上面停一下再落（和 GlassTabs 一样）
const RISE = { stiffness: 520, damping: 32, precision: .002 }
const LAND = { stiffness: 110, damping: 14, precision: .002 }
const HOLD = 90
const INSET = 2

function render(s: Segments, [x, width, lift]: number[], [velocity]: number[]) {
  if (!s.base) return
  const shift = x + width / 2 - s.base / 2
  const stretch = Math.min(.14, Math.abs(velocity) / 2800) * Math.min(1, lift * 1.5)
  const grow = 1 + .1 * lift
  s.drop.style.transform = `translateX(${shift.toFixed(2)}px) scale(${(width / s.base * grow * (1 + stretch)).toFixed(4)}, ${((1 + .22 * lift) * (1 - stretch * .45)).toFixed(4)})`
  // 浮起时白色的底淡去，只剩玻璃的亮边：像一滴清水
  s.fill.style.opacity = (1 - .8 * Math.max(0, Math.min(1, lift))).toFixed(3)
  if (s.landing && !s.pressed && Math.abs(x - s.springs.target(0)) < 2) {
    s.landing = false
    land(s, HOLD)
  }
}

function rise(s: Segments) {
  clearTimeout(s.hold)
  if (reducedMotion()) return
  s.springs.tune(2, RISE)
  s.springs.to(2, 1)
}
function land(s: Segments, delay: number) {
  clearTimeout(s.hold)
  s.hold = window.setTimeout(() => {
    if (s.pressed) return
    s.springs.tune(2, LAND)
    s.springs.to(2, 0)
  }, delay)
}

function segmentsOf(group: HTMLElement): Segments {
  let s = all.get(group)
  if (s) return s
  const drop = document.createElement('span')
  drop.className = 'cg-seg-drop'
  drop.setAttribute('aria-hidden', 'true')
  const fill = document.createElement('span')
  fill.className = 'cg-seg-fill'
  drop.append(fill)
  // 放在最前面，选项文字画在它上面；Vue 只管自己的节点，前面多一个不影响它更新
  group.prepend(drop)
  const segments: Segments = {
    group, drop, fill, base: 0, shown: false, landing: false, pressed: false,
    springs: createSprings([
      { stiffness: 230, damping: 26, precision: .1 },
      { stiffness: 230, damping: 26, precision: .1 },
      RISE,
    ], (values, velocities) => render(segments, values, velocities)),
    // 选中项变了（点击、键盘或程序改了值）就滑过去；大小变了直接到位。
    // 液滴自己的 class 变化不算：classList 即使没变也会产生一条变化记录，自己改自己会无限循环、整个页面卡死
    observers: [
      new MutationObserver((records) => { if (records.some((r) => r.target !== drop && r.target !== fill)) place(segments, true) }),
      new ResizeObserver(() => place(segments, false)),
    ],
  }
  segments.observers[0].observe(group, { subtree: true, attributes: true, attributeFilter: ['class'], childList: true })
  segments.observers[1].observe(group)
  s = segments
  all.set(group, s)
  return s
}

function place(s: Segments, animate: boolean) {
  if (!s.group.isConnected) { remove(s); return }
  const button = s.group.querySelector<HTMLElement>(`:scope > ${BUTTON}.is-active`)
  if (!button || !button.offsetWidth) {
    if (s.shown) { s.shown = false; s.drop.classList.remove('is-on') }
    return
  }
  // 选中块在按钮里四边各缩进 2px（凹槽和按钮保持原来的大小）
  const left = button.offsetLeft + INSET
  const width = button.offsetWidth - INSET * 2
  const top = `${button.offsetTop + INSET}px`
  const height = `${button.offsetHeight - INSET * 2}px`
  const wrapped = s.drop.style.top !== top
  if (wrapped) s.drop.style.top = top
  if (s.drop.style.height !== height) s.drop.style.height = height
  if (width !== s.base) { s.base = width; s.drop.style.width = `${width}px` }
  const { springs } = s
  // 第一次出现、换了一行、减少动画时直接到位，否则滑过去
  if (!animate || !s.shown || wrapped || reducedMotion()) {
    springs.jump(0, left); springs.jump(1, width)
    if (!s.pressed) springs.jump(2, 0)
  } else if (springs.target(0) !== left || springs.target(1) !== width) {
    clearTimeout(s.hold)
    springs.to(0, left)
    springs.to(1, width)
    if (Math.abs(springs.value(0) - left) > 2) { s.landing = true; rise(s) }
  }
  if (!s.shown) { s.shown = true; s.drop.classList.add('is-on') }
}

function remove(s: Segments) {
  clearTimeout(s.hold)
  s.springs.stop()
  s.observers.forEach((o) => o.disconnect())
  s.drop.remove()
  all.delete(s.group)
}

// 只管由 el-radio-button 组成的分段（普通的圆点单选 el-radio 不变）
function groupOf(target: EventTarget | null): HTMLElement | undefined {
  const button = (target as Element | null)?.closest?.<HTMLElement>(BUTTON)
  const group = button?.parentElement
  return group?.classList.contains('el-radio-group') ? group : undefined
}
function onEnter(event: Event) {
  if (!glass.enabled) return
  const group = groupOf(event.target)
  if (group && !all.has(group)) place(segmentsOf(group), false)
}
function onDown(event: PointerEvent) {
  if (!glass.enabled || event.button !== 0) return
  const button = (event.target as Element | null)?.closest?.<HTMLElement>(BUTTON)
  const group = groupOf(event.target)
  if (!button || !group || button.querySelector('input:disabled')) return
  const s = segmentsOf(group)
  if (!s.shown) place(s, false)
  s.pressed = true
  rise(s)
}
// 松手：点的是另一项时，place 会接着让液滴滑过去再落下；点的是已选中的一项，就在这里落下
function onUp() {
  for (const s of all.values()) {
    if (!s.pressed) continue
    s.pressed = false
    if (!s.landing) land(s, HOLD)
  }
}

let installed = false
export function installGlassSegments() {
  if (installed || typeof document === 'undefined') return
  installed = true
  document.addEventListener('pointerover', onEnter, { capture: true, passive: true })
  document.addEventListener('focusin', onEnter, { capture: true, passive: true })
  document.addEventListener('pointerdown', onDown, { capture: true, passive: true })
  document.addEventListener('pointerup', onUp, { capture: true, passive: true })
  document.addEventListener('pointercancel', onUp, { capture: true, passive: true })
  watch(() => glass.enabled, (on) => { if (!on) for (const s of [...all.values()]) remove(s) })
}
