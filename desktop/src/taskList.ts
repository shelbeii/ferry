import { computed, ref, watch } from 'vue'
import type { Task } from './state'

export const taskBatchSize = 20

/** 概要作为搜索索引保留；列表只按批次创建条目，任务输出仍由选中时读取。 */
export function useTaskList(tasks: readonly Task[]) {
  const query = ref('')
  const statusFilter = ref('all')
  const limit = ref(taskBatchSize)
  const filteredTasks = computed(() => {
    const text = query.value.trim().toLowerCase()
    return tasks.filter((t) =>
      (statusFilter.value === 'all' || (statusFilter.value === 'finished' ? t.status !== 'running' : t.status === statusFilter.value)) &&
      `${t.title} ${t.target ?? ''} ${t.error ?? ''}`.toLowerCase().includes(text),
    )
  })
  const visibleTasks = computed(() => filteredTasks.value.slice(0, limit.value))
  const hasMore = computed(() => visibleTasks.value.length < filteredTasks.value.length)
  function reset() { limit.value = taskBatchSize }
  function loadMore() {
    if (hasMore.value) limit.value = Math.min(limit.value + taskBatchSize, filteredTasks.value.length)
  }
  watch([query, statusFilter], reset, { flush: 'sync' })
  return { query, statusFilter, filteredTasks, visibleTasks, hasMore, reset, loadMore }
}
