import { describe, expect, it } from 'vitest'
import {
  actionTone,
  classifyCodeLine,
  extractStructuredData,
  formatCountdown,
  formatOutcomeMessage,
  type ConfirmCardData,
} from './confirm'

describe('MCP App 确认卡片辅助逻辑', () => {
  it('按操作类型与危险标记区分色调', () => {
    expect(actionTone('deploy')).toBe('warning')
    expect(actionTone('deploy', '危险原因')).toBe('danger')
    expect(actionTone('rollback')).toBe('danger')
    expect(actionTone('db-exec')).toBe('danger')
    expect(actionTone('backup-run')).toBe('primary')
  })

  it('正确计算倒计时与过期状态', () => {
    const base = Date.parse('2026-11-25T10:00:00Z')
    expect(formatCountdown('2026-11-25T10:02:05Z', base)).toEqual({ text: '2:05 后过期', expired: false })
    expect(formatCountdown('2026-11-25T09:59:59Z', base)).toEqual({ text: '已过期，请重新发起', expired: true })
  })

  it('识别差异高亮行（增、删、区块头）', () => {
    expect(classifyCodeLine('+ new line')).toBe('add')
    expect(classifyCodeLine('- old line')).toBe('del')
    expect(classifyCodeLine('+++ b/file')).toBe('')
    expect(classifyCodeLine('--- a/file')).toBe('')
    expect(classifyCodeLine('@@ -1,2 +1,3 @@')).toBe('hunk')
    expect(classifyCodeLine('  unchanged')).toBe('')
  })

  it('优先读取 structuredContent，并兼容 JSON 与纯文本 content', () => {
    expect(extractStructuredData({ structuredContent: { kind: 'ferry-confirm-card', opId: '1' } })).toEqual({
      kind: 'ferry-confirm-card',
      opId: '1',
    })
    expect(extractStructuredData({ content: [{ type: 'text', text: '{"state":"完成"}' }] })).toEqual({ state: '完成' })
    expect(extractStructuredData({ content: [{ type: 'text', text: '普通错误文本' }] })).toEqual({
      kind: 'text',
      text: '普通错误文本',
    })
    expect(extractStructuredData(null)).toBeNull()
  })

  it('按执行结果生成发回给模型的自动续接消息', () => {
    const card: ConfirmCardData = {
      kind: 'ferry-confirm-card',
      approvalId: 'app-1',
      appToken: 'tok',
      opId: 'op-123',
      title: '重启 File（tunnel）',
      action: 'restart',
      actionLabel: '重启',
      desktopOnline: true,
      expiresAt: '2026-11-25T10:05:00Z',
    }

    const doneMsg = formatOutcomeMessage(card, { state: 'done', result: { status: 'ok' } })
    expect(doneMsg).toContain('同意执行「重启 File（tunnel）」')
    expect(doneMsg).toContain('op-123')
    expect(doneMsg).toContain('"status":"ok"')

    const rejMsg = formatOutcomeMessage(card, { state: 'rejected', message: '已拒绝，未执行任何改动' })
    expect(rejMsg).toContain('拒绝执行「重启 File（tunnel）」')
    expect(rejMsg).toContain('不要换方式重试')

    const rejWithReason = formatOutcomeMessage(card, { state: 'rejected', message: '已拒绝：等我改完配置再重启' })
    expect(rejWithReason).toContain('等我改完配置再重启')

    const errMsg = formatOutcomeMessage(card, {
      state: 'error',
      error: 'systemd 启动超时',
      log: ['line1', 'line2'],
    })
    expect(errMsg).toContain('执行失败：systemd 启动超时')
    expect(errMsg).toContain('line1\nline2')
  })
})
