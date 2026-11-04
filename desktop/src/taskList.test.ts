import { describe, expect, it } from 'vitest'
import { reactive } from 'vue'
import { useTaskList } from './taskList'
import type { Task } from './state'

function records(count: number) {
  return reactive(Array.from({ length: count }, (_, i): Task => ({
    id: i + 1, hid: `record-${i}`, title: `部署 app-${i}`, status: i % 2 ? 'failed' : 'ok',
    target: 'prod', error: i % 2 ? '启动失败' : undefined, startedAt: count - i, lastAt: count - i, lines: [],
  })))
}

describe('任务列表增量加载', () => {
  it('首次显示 20 条，逐批加载至总数，没有重复或越界', () => {
    const list = useTaskList(records(53))
    expect(list.visibleTasks.value).toHaveLength(20)
    expect(list.filteredTasks.value).toHaveLength(53)
    list.loadMore()
    expect(list.visibleTasks.value).toHaveLength(40)
    list.loadMore()
    expect(list.visibleTasks.value.map((t) => t.id)).toEqual(Array.from({ length: 53 }, (_, i) => i + 1))
    expect(list.hasMore.value).toBe(false)
    list.loadMore()
    expect(list.visibleTasks.value).toHaveLength(53)
  })

  it('搜索未加载的任务，筛选按全部记录计数，并回到第一批', () => {
    const list = useTaskList(records(100))
    list.loadMore()
    list.query.value = ' APP-99 '
    expect(list.visibleTasks.value.map((t) => t.id)).toEqual([100])
    expect(list.filteredTasks.value).toHaveLength(1)
    list.query.value = ''
    list.statusFilter.value = 'failed'
    expect(list.visibleTasks.value).toHaveLength(20)
    expect(list.filteredTasks.value).toHaveLength(50)
    list.loadMore()
    expect(list.visibleTasks.value).toHaveLength(40)
    list.statusFilter.value = 'finished'
    expect(list.visibleTasks.value).toHaveLength(20)
    expect(list.filteredTasks.value).toHaveLength(100)
    list.query.value = '不存在'
    expect(list.visibleTasks.value).toHaveLength(0)
    expect(list.hasMore.value).toBe(false)
  })

  it('新任务、状态变化和清理同步更新计数，保留进行中任务', () => {
    const tasks = records(45)
    const list = useTaskList(tasks)
    tasks.unshift({ id: 100, hid: 'running-task', title: '安装 Nginx', target: 'test', status: 'running', startedAt: 100, lastAt: 100, lines: [] })
    expect(list.visibleTasks.value[0].id).toBe(100)
    expect(list.filteredTasks.value).toHaveLength(46)
    list.statusFilter.value = 'running'
    expect(list.visibleTasks.value.map((t) => t.id)).toEqual([100])
    list.statusFilter.value = 'finished'
    tasks[0].status = 'ok'
    expect(list.filteredTasks.value).toHaveLength(46)
    tasks[0].status = 'running'
    tasks.splice(1)
    expect(list.filteredTasks.value).toHaveLength(0)
    list.statusFilter.value = 'all'
    expect(list.visibleTasks.value.map((t) => t.id)).toEqual([100])
    expect(list.hasMore.value).toBe(false)
  })
})
