export interface FileSelectionModifiers {
  toggle?: boolean
  range?: boolean
}

/** 按当前显示顺序选择文件；普通点击保持单选，Ctrl 切换，Shift 从锚点扩展。 */
export function selectFileNames(order: readonly string[], selected: readonly string[], anchor: string | undefined, name: string, modifiers: FileSelectionModifiers = {}) {
  const index = order.indexOf(name)
  const available = new Set(order)
  const previous = new Set(selected.filter((item) => available.has(item)))
  if (index < 0) return { names: [...previous], anchor }
  if (modifiers.range) {
    const start = anchor ? order.indexOf(anchor) : -1
    const from = start < 0 ? index : start
    const names = modifiers.toggle ? previous : new Set<string>()
    for (const item of order.slice(Math.min(from, index), Math.max(from, index) + 1)) names.add(item)
    return { names: order.filter((item) => names.has(item)), anchor: start < 0 ? name : anchor }
  }
  if (modifiers.toggle) {
    if (previous.has(name)) previous.delete(name)
    else previous.add(name)
    return { names: order.filter((item) => previous.has(item)), anchor: name }
  }
  return { names: [name], anchor: name }
}
