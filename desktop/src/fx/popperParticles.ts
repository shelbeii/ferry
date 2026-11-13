// 下拉框（下拉选择、下拉菜单、输入建议、点击弹出的浮层如「部署参数」）打开、收起时也用对话框那套粒子动画（见 dialogParticles.ts）。
// 这些弹出层都由 Element Plus 生成、用 CSS 过渡（el-zoom-in-top）显示和隐藏，没有全局的过渡设置：
// 全局监听 transitionrun，过渡开始时看弹出层上是 enter 还是 leave 的 class，就知道是打开还是收起，不用一个个改组件。
// 打开时弹出层不再缩放弹出（style.css 里 html.fx-particles 的规则），改由粒子收拢、弹出层跟着淡入；收起时淡出、化成粒子散开。
import { CLOSE_MS, OPEN_MS, particlesIn, particlesOut, slow } from './dialogParticles'

const POPPER = '.el-select__popper, .el-dropdown__popper, .el-autocomplete__popper, .el-popover.el-popper'
// 下拉框小，按对话框的密度（每 50 平方像素一颗）只有几百颗、显得稀：密一倍多，最少 900 颗；
// 下拉框要跟手，打开时收拢用对话框六成的时间（约 0.18 秒），收起和对话框一样
const AMOUNT = { min: 900, density: 20, pace: .6 }

const phase = new WeakMap<Element, 'enter' | 'leave'>()
const running = new WeakMap<Element, Animation>()

function phaseOf(el: Element): 'enter' | 'leave' | '' {
  for (const c of el.classList) {
    if (c.endsWith('-enter-active')) return 'enter'
    if (c.endsWith('-leave-active')) return 'leave'
  }
  return ''
}

function onTransition(e: Event) {
  const el = e.target
  if (!(el instanceof HTMLElement) || !el.matches(POPPER)) return
  const now = phaseOf(el)
  // 一次过渡有几个属性，每个都会触发一次：同一阶段只处理第一次
  if (!now || phase.get(el) === now) return
  phase.set(el, now)
  running.get(el)?.cancel()
  running.delete(el)
  if (now === 'enter') {
    if (!particlesIn(el, AMOUNT)) return
    const a = el.animate([
      { opacity: 0, scale: '1.04', offset: 0 },
      { opacity: 0, offset: .4 },
      { opacity: 1, scale: '1', offset: 1 },
    ], { duration: OPEN_MS * AMOUNT.pace, easing: 'cubic-bezier(.25,.8,.3,1)', fill: 'backwards' })
    a.playbackRate = 1 / slow()
    running.set(el, a)
  } else {
    if (!particlesOut(el, AMOUNT)) return
    // 保持透明直到 Element Plus 把它隐藏；下次打开时上面会先取消
    const a = el.animate([{ opacity: 1, scale: '1' }, { opacity: 0, scale: '1.02' }], { duration: CLOSE_MS, easing: 'ease-out', fill: 'forwards' })
    a.playbackRate = 1 / slow()
    running.set(el, a)
  }
}

// 设置里关掉粒子特效时 particlesIn / particlesOut 返回 false，弹出层照原样缩放弹出（html 上没有 fx-particles）
export function installPopperParticles() {
  document.addEventListener('transitionrun', onTransition, true)
}
