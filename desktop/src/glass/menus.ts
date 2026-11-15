// 液态玻璃的下拉框：所有下拉选择（el-select）、下拉菜单（el-dropdown）、输入建议（el-autocomplete）的弹出层
// 以及标为 cg-menu-popover 的参数面板都是磨砂玻璃（样式见 style.css），里面的选中高亮是一颗玻璃液滴：
// 鼠标或方向键在选项间移动时，液滴滑过去，移动得快时被拉长一点，到位后回弹。
// 不用一个个改组件：全局监听指针、键盘、滚动，找到所在的弹出层，在弹出层里放一颗液滴、用弹簧移动。
// 液态玻璃关着时用普通底色，悬停与键盘高亮仍按同样的规则清理。
import { createSprings } from './optics'
import { glass, reducedMotion } from './runtime'

const POPPER = '.el-select__popper, .el-dropdown__popper, .el-autocomplete__popper, .cg-menu-popover'
const ITEM = '.el-select-dropdown__item, .el-dropdown-menu__item, .el-autocomplete-suggestion li, .cg-menu-item'
const POINTER = 'data-menu-pointer'
const NAVIGATION_KEYS = ['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'Home', 'End', 'PageUp', 'PageDown']

interface Menu {
  pill: HTMLElement
  springs: ReturnType<typeof createSprings>
  item?: HTMLElement
  shown: boolean
}
const menus = new WeakMap<HTMLElement, Menu>()

function menuOf(popper: HTMLElement): Menu {
  let menu = menus.get(popper)
  if (menu) return menu
  const pill = document.createElement('span')
  pill.className = 'cg-menu-pill'
  pill.setAttribute('aria-hidden', 'true')
  // 放在最前面：后面的内容（选项文字）画在它上面；Vue 只管自己的节点，前面多一个不影响它更新
  popper.prepend(pill)
  const springs = createSprings([{ stiffness: 520, damping: 34, precision: .05 }], ([y], [velocity]) => {
    // 移动快时沿移动方向拉长一点，像液滴被拖着走
    const stretch = 1 + Math.min(.35, Math.abs(velocity) / 2400)
    pill.style.transform = `translateY(${y.toFixed(2)}px) scaleY(${stretch.toFixed(4)})`
  })
  menu = { pill, springs, shown: false }
  menus.set(popper, menu)
  return menu
}

// 选项在弹出层里的位置（弹出层打开时有缩放动画：按实际缩放换回布局尺寸）
function place(popper: HTMLElement, item: HTMLElement) {
  const box = popper.getBoundingClientRect()
  const scale = box.width / (popper.offsetWidth || box.width) || 1
  const rect = item.getBoundingClientRect()
  return { x: (rect.left - box.left) / scale, y: (rect.top - box.top) / scale, width: rect.width / scale, height: rect.height / scale }
}

function moveTo(item: HTMLElement, jump = false) {
  const popper = item.closest<HTMLElement>(POPPER)
  if (!popper || !glass.enabled) return
  const menu = menuOf(popper)
  if (item.classList.contains('is-disabled')) { hide(popper); return }
  const { x, y, width, height } = place(popper, item)
  const { pill, springs } = menu
  pill.style.left = `${x.toFixed(1)}px`
  pill.style.width = `${width.toFixed(1)}px`
  if (pill.style.height !== `${height.toFixed(1)}px`) pill.style.height = `${height.toFixed(1)}px`
  // 第一次出现、刚重新打开、减少动画时直接到位，否则滑过去
  if (jump || !menu.shown || reducedMotion()) springs.jump(0, y)
  else springs.to(0, y)
  menu.item = item
  if (!menu.shown) { menu.shown = true; pill.classList.add('is-on') }
}
function hide(popper: HTMLElement) {
  const menu = menus.get(popper)
  if (!menu?.shown) return
  menu.springs.stop()
  menu.shown = false
  menu.item = undefined
  menu.pill.classList.remove('is-on')
}

function popperFor(target: EventTarget | null): HTMLElement | undefined {
  if (!(target instanceof Element)) return
  // 可输入选择框的焦点留在输入框里，aria-controls 指向弹层内的选项列表。
  return target.closest<HTMLElement>(POPPER)
    ?? document.getElementById(target.getAttribute('aria-controls') ?? '')?.closest<HTMLElement>(POPPER)
    ?? undefined
}

function onPointerOver(event: PointerEvent) {
  if (event.pointerType === 'touch' || !(event.target instanceof Element)) return
  const popper = event.target.closest<HTMLElement>(POPPER)
  if (!popper) return
  // Element Plus 鼠标经过时也会移动焦点或更新内部选项索引，这两种状态都不能当成键盘高亮。
  popper.setAttribute(POINTER, '')
  const item = event.target.closest<HTMLElement>(ITEM)
  if (item && popper.contains(item)) {
    if (menus.get(popper)?.item !== item) moveTo(item)
  } else hide(popper)
}

function onPointerOut(event: PointerEvent) {
  if (!(event.target instanceof Element)) return
  const popper = event.target.closest<HTMLElement>(POPPER)
  if (!popper?.hasAttribute(POINTER) || popper.contains(event.relatedTarget as Node | null)) return
  hide(popper)
}

function onFocusIn(event: FocusEvent) {
  if (!(event.target instanceof Element) || !event.target.matches(':focus-visible')) return
  const item = event.target.closest<HTMLElement>(ITEM)
  if (item && !item.closest(`[${POINTER}]`)) moveTo(item)
}

function onFocusOut(event: FocusEvent) {
  const popper = popperFor(event.target)
  if (!popper || popperFor(event.relatedTarget) === popper || popper.hasAttribute(POINTER)) return
  hide(popper)
}
// 选中后弹出层收起：液滴一起淡出，下次打开从新位置出现
function onClick(event: MouseEvent) {
  const item = (event.target as Element | null)?.closest?.<HTMLElement>(ITEM)
  const popper = item?.closest<HTMLElement>(POPPER)
  if (popper) hide(popper)
}
function onKey(event: KeyboardEvent) {
  const popper = popperFor(event.target)
  if (!popper) return
  if (event.key === 'Escape' || event.key === 'Enter') {
    hide(popper)
    return
  }
  if (event.key === 'Tab') {
    popper.removeAttribute(POINTER)
    hide(popper)
    return
  }
  if (!NAVIGATION_KEYS.includes(event.key)) return
  if (event.target instanceof HTMLInputElement && (event.key === 'ArrowLeft' || event.key === 'ArrowRight')) return
  popper.removeAttribute(POINTER)
  if (!glass.enabled) return
  requestAnimationFrame(() => {
    const active = document.activeElement
    if (popperFor(active) !== popper || popper.hasAttribute(POINTER) || popper.getAttribute('aria-hidden') === 'true') return
    const id = active?.getAttribute('aria-activedescendant')
    const item = id ? document.getElementById(id) : active?.closest<HTMLElement>(ITEM)
    if (item?.matches(ITEM) && popper.contains(item)) moveTo(item)
    else hide(popper)
  })
}

function onInput(event: Event) {
  const popper = popperFor(event.target)
  if (!popper) return
  popper.removeAttribute(POINTER)
  hide(popper)
}
// 选项列表滚动时液滴跟着选项走
function onScroll(event: Event) {
  const target = event.target
  if (!(target instanceof Element)) return
  const popper = target.closest<HTMLElement>(POPPER)
  const menu = popper && menus.get(popper)
  if (menu?.shown && menu.item?.isConnected) moveTo(menu.item, true)
}

let installed = false
export function installGlassMenus() {
  if (installed || typeof document === 'undefined') return
  installed = true
  document.addEventListener('pointerover', onPointerOver, { capture: true, passive: true })
  document.addEventListener('pointerout', onPointerOut, { capture: true, passive: true })
  document.addEventListener('focusin', onFocusIn, { capture: true, passive: true })
  document.addEventListener('focusout', onFocusOut, { capture: true, passive: true })
  document.addEventListener('click', onClick, { capture: true, passive: true })
  document.addEventListener('keydown', onKey, { capture: true, passive: true })
  document.addEventListener('input', onInput, { capture: true, passive: true })
  document.addEventListener('scroll', onScroll, { capture: true, passive: true })
}
