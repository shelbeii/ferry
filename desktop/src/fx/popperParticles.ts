// 下拉框（下拉选择、下拉菜单、输入建议、日期时间选择、右键菜单、点击弹出的浮层如「部署参数」）和确认框（ElMessageBox：启动、停止、删除前的确认……）
// 打开、收起时也用对话框那套粒子动画（见 dialogParticles.ts）。
// 这些都用 CSS 过渡显示和隐藏（下拉框与右键菜单是 el-zoom-in-top，确认框的遮罩是 fade-in-linear），没有全局的过渡设置：
// 全局监听 transitionrun，过渡开始时看上面是 enter 还是 leave 的 class，就知道是打开还是收起，不用一个个改组件。
// 打开时不再缩放弹出、确认框不再下落（style.css 里 html.fx-particles 的规则），改由粒子收拢、跟着淡入；收起时淡出、化成粒子散开。
import { CLOSE_MS, OPEN_MS, particlesIn, particlesOut, slow, type ParticleAmount } from './dialogParticles'

const POPPER = '.el-select__popper, .el-dropdown__popper, .el-autocomplete__popper, .el-picker__popper, .el-popover.el-popper, .db-grid-menu'
const MESSAGE_BOX = '.el-overlay.is-message-box'
// 下拉框小，按对话框的密度（每 50 平方像素一颗）只有几百颗、显得稀：密一倍多，最少 900 颗；
// 下拉框要跟手，打开时收拢用对话框六成的时间（约 0.18 秒），收起和对话框一样
const POPPER_AMOUNT = { min: 900, density: 20, pace: .6 }
// 确认框和对话框一样（按对话框的默认数量和快慢）
const BOX_AMOUNT: ParticleAmount = {}

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
  if (!(el instanceof HTMLElement)) return
  // 过渡在谁身上（下拉框本身、确认框的遮罩），动画和粒子做在谁身上（下拉框、遮罩里的确认框）
  let target: HTMLElement | null, amount: ParticleAmount
  if (el.matches(POPPER)) { target = el; amount = POPPER_AMOUNT }
  else if (el.matches(MESSAGE_BOX)) { target = el.querySelector<HTMLElement>('.el-message-box'); amount = BOX_AMOUNT }
  else return
  if (!target) return
  const now = phaseOf(el)
  // 一次过渡有几个属性，每个都会触发一次：同一阶段只处理第一次
  if (!now || phase.get(el) === now) return
  phase.set(el, now)
  running.get(target)?.cancel()
  running.delete(target)
  const box = target !== el
  if (now === 'enter') {
    // 确认框进场期间 CSS 先把它藏着（不然过渡开始前那一帧会闪一下），这里不管粒子放没放得出来都要把它淡入
    if (!particlesIn(target, amount) && !box) return
    // 只淡入、不缩放（缩放会让里面的字先变大再缩回，动画结束时还会跳一下）。
    // 确认框：一直保持（fill both），进场的 class 去掉前不会被 CSS 的透明盖回去；收起时上面会先取消
    const a = target.animate([
      { opacity: 0, offset: 0 },
      { opacity: 0, offset: .4 },
      { opacity: 1, offset: 1 },
    ], { duration: OPEN_MS * (amount.pace ?? 1), easing: 'cubic-bezier(.25,.8,.3,1)', fill: box ? 'both' : 'backwards' })
    a.playbackRate = 1 / slow()
    running.set(target, a)
  } else {
    if (!particlesOut(target, amount)) return
    // 等 CSS 退场结束再释放透明度，否则一直保留的 opacity: 0 会让下次进场没有过渡可触发。
    const transitions = el.getAnimations().filter((animation) => animation instanceof CSSTransition)
    const a = target.animate([{ opacity: 1 }, { opacity: 0 }], { duration: CLOSE_MS, easing: 'ease-out', fill: 'forwards' })
    a.playbackRate = 1 / slow()
    running.set(target, a)
    void Promise.allSettled([a, ...transitions].map((animation) => animation.finished)).then(() => {
      if (running.get(target) !== a) return
      a.cancel()
      running.delete(target)
    })
  }
}

// 设置里关掉粒子特效时 particlesIn / particlesOut 返回 false、html 上没有 fx-particles：下拉框照原样缩放弹出，确认框照原样下落
export function installPopperParticles() {
  document.addEventListener('transitionrun', onTransition, true)
}
