import { describe, expect, it } from 'vitest'
import { reactive } from 'vue'
import { maxTaskLines, pushLine, type Task } from './state'

const newTask = () => reactive<Task>({ id: 1, hid: 'abcdef-1', title: 't', status: 'running', lines: [], startedAt: 0, lastAt: 0 }) as Task

describe('任务输出限量', () => {
  it('超过上限时先删较早的普通输出，步骤和错误留着，开头注明省略了几行', () => {
    const t = newTask()
    pushLine(t, { kind: 'step', msg: '构建' })
    pushLine(t, { kind: 'error', msg: '早先的错误' })
    for (let i = 0; i < maxTaskLines + 1000; i++) pushLine(t, { kind: 'info', msg: `第 ${i} 行` })
    expect(t.received).toBe(maxTaskLines + 1002)
    expect(t.lines.length).toBeLessThanOrEqual(maxTaskLines + 201)
    expect(t.lines[0].msg).toMatch(/^（前面省略了 \d+ 行较早的输出，没有保留）$/)
    expect(t.lines.some((l) => l.msg === '构建')).toBe(true)
    expect(t.lines.some((l) => l.msg === '早先的错误')).toBe(true)
    expect(t.lines[t.lines.length - 1].msg).toBe(`第 ${maxTaskLines + 999} 行`)
    // 省略的行数 + 留下的行数 = 收到的行数（开头那行说明不算）
    const omitted = Number(/省略了 (\d+) 行/.exec(t.lines[0].msg!)![1])
    expect(omitted + t.lines.length - 1).toBe(t.received)
  })

  it('全是要留的行时按先后删', () => {
    const t = newTask()
    for (let i = 0; i < maxTaskLines + 300; i++) pushLine(t, { kind: 'warn', msg: `提示 ${i}` })
    const omitted = Number(/省略了 (\d+) 行/.exec(t.lines[0].msg!)![1])
    expect(omitted + t.lines.length - 1).toBe(maxTaskLines + 300)
    expect(t.lines[1].msg).toBe(`提示 ${omitted}`)
  })
})
