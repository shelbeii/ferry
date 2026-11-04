import { describe, expect, it } from 'vitest'
import { effectScope, ref } from 'vue'
import { useInteractionScope } from './interactionScope'

describe('等待确认时的操作上下文', () => {
  it('切换连接再切回，旧确认也不能对新页面执行操作', async () => {
    const scope = effectScope()
    const target = ref('162')
    const capture = scope.run(() => useInteractionScope(() => target.value))!
    const stillHere = capture()
    let confirm!: () => void
    const confirmation = new Promise<void>((resolve) => { confirm = resolve })
    const sent: string[] = []
    const operation = (async () => { await confirmation; if (stillHere()) sent.push(target.value) })()
    target.value = '41'
    target.value = '162'
    confirm()
    await operation
    expect(sent).toEqual([])
    expect(capture()()).toBe(true)
    scope.stop()
  })

  it('关闭弹窗或销毁页面后忽略迟到的读取和确认结果', () => {
    const scope = effectScope()
    const visible = ref(true)
    const capture = scope.run(() => useInteractionScope(() => visible.value))!
    const opened = capture()
    visible.value = false
    expect(opened()).toBe(false)
    visible.value = true
    const reopened = capture()
    expect(reopened()).toBe(true)
    scope.stop()
    expect(reopened()).toBe(false)
  })
})
