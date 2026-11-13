// 列表分页（部署历史、审计记录……）：数据一次读回来，界面上一页只生成几十行。
// el-table 一次生成几百上千行要几百毫秒，打开窗口、换筛选条件时整个界面都会卡住。
import { computed, ref, watch, type Ref, type WatchSource } from 'vue'
import type { TableInstance } from 'element-plus'

export const PAGE_SIZES = [20, 50, 100]

/** items：筛选后的整个列表；resetOn：筛选条件，变了回到第一页；table：翻页后表格滚回顶上 */
export function usePager<T>(items: Ref<T[]>, resetOn: WatchSource[] = [], table?: Ref<TableInstance | undefined>, size = 50) {
  const page = ref(1)
  const pageSize = ref(size)
  const total = computed(() => items.value.length)
  // 删掉、筛掉一些后当前页超出了，就停在最后一页
  const pages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))
  watch(pages, (n) => { if (page.value > n) page.value = n })
  if (resetOn.length) watch(resetOn, () => { page.value = 1 })
  // 换每页条数时停在原来第一行所在的那页
  watch(pageSize, (n, old) => { page.value = Math.floor(((page.value - 1) * old) / n) + 1 })
  watch(page, () => table?.value?.setScrollTop(0))
  const paged = computed(() => items.value.slice((page.value - 1) * pageSize.value, page.value * pageSize.value))
  return { page, pageSize, total, paged }
}
