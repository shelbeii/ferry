import { describe, expect, it } from 'vitest'
import { nextTick, ref } from 'vue'
import { usePager } from './pager'

const list = (n: number) => Array.from({ length: n }, (_, i) => i)

describe('usePager', () => {
  it('一页只取当前页的几条', async () => {
    const items = ref(list(120))
    const p = usePager(items)
    expect(p.paged.value).toEqual(list(50))
    p.page.value = 3
    expect(p.paged.value).toEqual(list(120).slice(100))
    expect(p.total.value).toBe(120)
  })

  it('筛选条件变了回到第一页', async () => {
    const items = ref(list(200))
    const who = ref('')
    const p = usePager(items, [who])
    p.page.value = 3
    who.value = 'a'
    await nextTick()
    expect(p.page.value).toBe(1)
  })

  it('列表变短、当前页超出时停在最后一页', async () => {
    const items = ref(list(200))
    const p = usePager(items)
    p.page.value = 4
    items.value = list(70)
    await nextTick()
    expect(p.page.value).toBe(2)
  })

  it('换每页条数时停在原来第一行所在的那页', async () => {
    const items = ref(list(1000))
    const p = usePager(items)
    p.page.value = 2 // 第 50 条起
    p.pageSize.value = 20
    await nextTick()
    expect(p.page.value).toBe(3)
    expect(p.paged.value[0]).toBe(40)
  })
})
