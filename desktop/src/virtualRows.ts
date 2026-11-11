// 长列表只画看得见的行（加上下各几行余量）：几千个文件也只有几十行在页面上。
// 每行高度事先算好（由数据决定，CSS 里写死同样的高度），按累计高度二分查找可见范围。
import { computed, onBeforeUnmount, onMounted, ref, type Ref } from 'vue'

/** 各行顶部的位置，最后多一项是总高度。 */
export function prefixOffsets(heights: readonly number[]): number[] {
  const out = new Array<number>(heights.length + 1)
  out[0] = 0
  for (let i = 0; i < heights.length; i++) out[i + 1] = out[i] + heights[i]
  return out
}

/** 和 [top, bottom) 有交集的行，前后各多留 overscan 行；返回 [start, end)。 */
export function visibleRange(offsets: readonly number[], top: number, bottom: number, overscan = 6): [number, number] {
  const n = offsets.length - 1
  if (n <= 0 || bottom <= top) return [0, Math.min(n, overscan)]
  // 第一行底部在 top 之下的
  let lo = 0
  let hi = n
  while (lo < hi) {
    const mid = (lo + hi) >> 1
    if (offsets[mid + 1] > top) hi = mid
    else lo = mid + 1
  }
  const first = lo
  // 第一行顶部已到 bottom 的
  hi = n
  while (lo < hi) {
    const mid = (lo + hi) >> 1
    if (offsets[mid] >= bottom) hi = mid
    else lo = mid + 1
  }
  return [Math.max(0, first - overscan), Math.min(n, lo + overscan)]
}

/**
 * scroller：滚动的容器；head：容器里吸顶的表头（行从它下面滚过）；heights：每行高度。
 * 行放在表头之后的一个总高度的容器里，按 offsets 绝对定位。
 */
export function useVirtualRows(scroller: Ref<HTMLElement | undefined>, head: Ref<HTMLElement | undefined>, heights: Ref<readonly number[]>) {
  const offsets = computed(() => prefixOffsets(heights.value))
  const total = computed(() => offsets.value[offsets.value.length - 1])
  const scrollTop = ref(0)
  const viewHeight = ref(0)
  const headHeight = ref(0)

  function sync() {
    const el = scroller.value
    if (!el) return
    scrollTop.value = el.scrollTop
    viewHeight.value = el.clientHeight
    headHeight.value = head.value?.offsetHeight ?? 0
  }

  // 视口里能看到的行：表头盖住的那段也算（玻璃表头下面透出来）
  const range = computed(() => {
    const top = scrollTop.value - headHeight.value
    return visibleRange(offsets.value, top, top + (viewHeight.value || 800))
  })

  /** 把第 i 行滚到表头下面能看全的位置。 */
  function scrollToIndex(i: number) {
    const el = scroller.value
    if (!el || i < 0 || i >= offsets.value.length - 1) return
    sync()
    const top = offsets.value[i]
    const bottom = offsets.value[i + 1]
    if (top < el.scrollTop) el.scrollTop = top
    else if (headHeight.value + bottom - el.scrollTop > el.clientHeight) el.scrollTop = headHeight.value + bottom - el.clientHeight
    sync()
  }

  function scrollToTop() {
    if (scroller.value) scroller.value.scrollTop = 0
    sync()
  }

  let resize: ResizeObserver | undefined
  onMounted(() => {
    resize = new ResizeObserver(sync)
    if (scroller.value) resize.observe(scroller.value)
    if (head.value) resize.observe(head.value)
    sync()
  })
  onBeforeUnmount(() => resize?.disconnect())

  return { offsets, total, range, onScroll: sync, scrollToIndex, scrollToTop }
}
