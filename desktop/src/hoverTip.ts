// 长列表共用一个悬停提示：每行各放一个 el-tooltip 要多出六七个组件，几千行时打开、勾选都会卡。
// 列表上监听 mouseover / focusin，鼠标停在带 data-tip 的元素上 HOVER_DELAY 后由 read 决定显示什么。
import { shallowRef } from 'vue'
import { HOVER_DELAY } from './ui'

export interface HoverTip<T> {
  el: HTMLElement
  placement: 'top' | 'left' | 'right' | 'bottom'
  effect: 'dark' | 'light'
  text?: string
  data?: T
}

export function useHoverTip<T>(read: (el: HTMLElement) => Omit<HoverTip<T>, 'el'> | undefined) {
  const tip = shallowRef<HoverTip<T>>()
  let pending: HTMLElement | undefined
  let timer: ReturnType<typeof setTimeout> | undefined

  function hide() {
    if (timer) clearTimeout(timer)
    timer = undefined
    pending = undefined
    tip.value = undefined
  }

  /** mouseover / focusin：进到另一个带 data-tip 的元素时重新计时，离开时收起。 */
  function over(event: Event) {
    const target = event.target instanceof Element ? event.target.closest<HTMLElement>('[data-tip]') : null
    if (target && (target === pending || target === tip.value?.el)) return
    hide()
    if (!target) return
    pending = target
    timer = setTimeout(() => {
      timer = undefined
      pending = undefined
      if (!target.isConnected) return
      const shown = read(target)
      if (shown) tip.value = { el: target, ...shown }
    }, HOVER_DELAY)
  }

  return { tip, over, hide }
}
