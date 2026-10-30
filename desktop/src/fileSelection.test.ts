import { describe, expect, it } from 'vitest'
import { selectFileNames } from './fileSelection'

const order = ['logs', 'webroot', 'config.ini', 'default.pem', 'MediaServer']

describe('资源管理器式文件选择', () => {
  it('重复单击保持选中，单击其他行替换原来的选择', () => {
    const first = selectFileNames(order, [], undefined, 'config.ini')
    const second = selectFileNames(order, first.names, first.anchor, 'config.ini')
    expect(second.names).toEqual(['config.ini'])
    expect(selectFileNames(order, second.names, second.anchor, 'logs').names).toEqual(['logs'])
  })
  it('Ctrl 单击增减单项，并保留其他选中项', () => {
    const added = selectFileNames(order, ['logs'], 'logs', 'config.ini', { toggle: true })
    expect(added.names).toEqual(['logs', 'config.ini'])
    expect(selectFileNames(order, added.names, added.anchor, 'logs', { toggle: true }).names).toEqual(['config.ini'])
  })
  it('Shift 按显示顺序选中连续范围，往回移动时缩小范围', () => {
    const extended = selectFileNames(order, ['webroot'], 'webroot', 'MediaServer', { range: true })
    expect(extended.names).toEqual(['webroot', 'config.ini', 'default.pem', 'MediaServer'])
    expect(extended.anchor).toBe('webroot')
    expect(selectFileNames(order, extended.names, extended.anchor, 'config.ini', { range: true }).names).toEqual(['webroot', 'config.ini'])
  })
  it('支持反向范围选择，Ctrl+Shift 在原选择上追加范围', () => {
    expect(selectFileNames(order, ['default.pem'], 'default.pem', 'webroot', { range: true }).names).toEqual(['webroot', 'config.ini', 'default.pem'])
    expect(selectFileNames(order, ['logs', 'default.pem'], 'default.pem', 'MediaServer', { toggle: true, range: true }).names).toEqual(['logs', 'default.pem', 'MediaServer'])
  })
  it('目录或筛选变化导致锚点失效时，不意外扩大选择', () => {
    expect(selectFileNames(['config.ini', 'default.pem'], ['logs'], 'logs', 'default.pem', { range: true })).toEqual({ names: ['default.pem'], anchor: 'default.pem' })
    expect(selectFileNames(order, ['config.ini'], 'config.ini', 'missing').names).toEqual(['config.ini'])
  })
})
