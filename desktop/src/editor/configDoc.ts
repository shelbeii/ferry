// 表单编辑 ferry.yaml：在 yaml 的 Document 上增删改，再写回文字。
// 写回时没改到的行保持原样（yaml 库重新输出时会改动注释前的空格、{} 里的空格等），
// 做法：原文重新输出一遍（R0）和改后的输出（R1）逐行对比，相同的行用原文。
import { Document, isAlias, isCollection, isMap, isScalar, isSeq, parseDocument, YAMLMap, YAMLSeq, type Node } from 'yaml'

export type Path = (string | number)[]

const OUT = { lineWidth: 0, minContentWidth: 0, flowCollectionPadding: false } as const

export function parse(text: string): Document {
  return parseDocument(text, { keepSourceTokens: false })
}

/** 改过的文档转回文字：没改到的行保持 src 原样。 */
export function serialize(src: string, doc: Document): string {
  const r1 = doc.toString(OUT)
  const orig = src.split('\n')
  const r0 = parse(src).toString(OUT).split('\n')
  if (r0.length !== orig.length) return r1
  const out: string[] = []
  for (const op of lineDiff(r0, r1.split('\n'))) {
    if (op.t === '=') out.push(orig[op.a])
    else if (op.t === '+') out.push(op.text)
  }
  return out.join('\n')
}

// ---------------------------------------------------------------- 读

/** 路径上的节点（不解开别名）；没有时 undefined。 */
export function nodeAt(doc: Document, path: Path): unknown {
  return path.length ? doc.getIn(path, true) : doc.contents
}

/** 路径上（含）有没有 YAML 别名、合并键（<<）：这样的地方表单不改。 */
export function hasAlias(doc: Document, path: Path): boolean {
  let n: unknown = doc.contents
  for (let i = 0; ; i++) {
    if (isAlias(n)) return true
    if (isMap(n) && n.items.some((p) => isScalar(p.key) && p.key.value === '<<')) return true
    if (i >= path.length || !isCollection(n)) return false
    n = n.get(path[i], true)
  }
}

/** 标量的值；不是标量时 undefined。 */
export function scalarAt(doc: Document, path: Path): string | number | boolean | null | undefined {
  const n = nodeAt(doc, path)
  return isScalar(n) ? (n.value as string | number | boolean | null) : undefined
}

/** 列表的元素（文字）；只写了一个值时当作一项。 */
export function listAt(doc: Document, path: Path): string[] | undefined {
  const n = nodeAt(doc, path)
  if (isSeq(n)) return n.items.map((x) => (isScalar(x) ? String(x.value ?? '') : String(x)))
  if (isScalar(n) && n.value != null && n.value !== '') return [String(n.value)]
  return undefined
}

/** 映射的键值（文字）。 */
export function mapAt(doc: Document, path: Path): [string, string][] | undefined {
  const n = nodeAt(doc, path)
  if (!isMap(n)) return undefined
  return n.items.map((p) => [keyOf(p.key), isScalar(p.value) ? String(p.value.value ?? '') : String(p.value ?? '')])
}

/** 映射的键名。 */
export function keysAt(doc: Document, path: Path): string[] {
  const n = nodeAt(doc, path)
  return isMap(n) ? n.items.map((p) => keyOf(p.key)) : []
}

/** 写了没有（值为 null 的也算写了）。 */
export function has(doc: Document, path: Path): boolean {
  if (!path.length) return true
  const parent = nodeAt(doc, path.slice(0, -1))
  return isCollection(parent) && parent.has(path[path.length - 1])
}

// ---------------------------------------------------------------- 改

/** 沿路径把空的 {} []（流式）改成块格式：往里加东西后按块格式写。 */
function unflow(doc: Document, path: Path) {
  let n: unknown = doc.contents
  for (let i = 0; i <= path.length; i++) {
    if ((isMap(n) || isSeq(n)) && n.flow && n.items.length === 0) n.flow = false
    if (i === path.length || !isCollection(n)) return
    n = n.get(path[i], true)
  }
}

/** 设置一个值（文字、数字、是 / 否）；原来是标量时只改值，保留引号风格和注释。 */
export function setScalar(doc: Document, path: Path, value: string | number | boolean) {
  const n = nodeAt(doc, path)
  if (isScalar(n)) {
    // 原来是数字、新值也能原样写成数字时保持数字（不加引号）
    if (typeof value === 'string' && typeof n.value === 'number' && /^-?\d+(\.\d+)?$/.test(value) && String(Number(value)) === value) value = Number(value)
    n.value = value
    return
  }
  unflow(doc, path.slice(0, -1))
  doc.setIn(path, value)
}

/** 新建列表、映射：短的写成一行。 */
function newCollection(doc: Document, value: unknown): Node {
  const n = doc.createNode(value) as Node
  if (isSeq(n) || isMap(n)) {
    const flat = JSON.stringify(value) ?? ''
    n.flow = isSeq(n) && n.items.length > 0 && flat.length <= 60 && n.items.every((x) => isScalar(x))
  }
  return n
}

/** 设置列表；空列表 = 删除。single：原来只写了一个值、现在也只有一项时保持一个值。 */
export function setList(doc: Document, path: Path, values: (string | number)[], keep = 0, single = false) {
  if (!values.length) return remove(doc, path, keep)
  const n = nodeAt(doc, path)
  if (single && values.length === 1 && isScalar(n)) return setScalar(doc, path, values[0])
  if (isSeq(n)) {
    // 逐项改：保留每项的注释、引号风格
    n.items = values.map((v, i) => {
      const old = n.items[i]
      if (isScalar(old)) {
        old.value = v
        return old
      }
      return doc.createNode(v)
    })
    return
  }
  unflow(doc, path.slice(0, -1))
  doc.setIn(path, newCollection(doc, values))
}

/** 设置映射（vars、env）：保留已有键的位置，删掉没有了的，新的加在后面；空 = 删除。 */
export function setMap(doc: Document, path: Path, entries: [string, string][], keep = 0) {
  if (!entries.length) return remove(doc, path, keep)
  const n = nodeAt(doc, path)
  if (!isMap(n)) {
    unflow(doc, path.slice(0, -1))
    doc.setIn(path, newCollection(doc, Object.fromEntries(entries)))
    return
  }
  const want = new Map(entries)
  n.items = n.items.filter((p) => want.has(keyOf(p.key)))
  for (const [k, v] of entries) {
    const p = n.items.find((x) => keyOf(x.key) === k)
    if (p && isScalar(p.value)) p.value.value = v
    else if (p) p.value = doc.createNode(v)
    else n.set(doc.createNode(k), v)
  }
  // 按 entries 的顺序排（改名后的键留在原位置）
  const order = entries.map(([k]) => k)
  n.items.sort((a, b) => order.indexOf(keyOf(a.key)) - order.indexOf(keyOf(b.key)))
}

const keyOf = (k: unknown) => (isScalar(k) ? String(k.value) : String(k))

/** 设置任意值（新建对象、列表项用）。 */
export function setValue(doc: Document, path: Path, value: unknown) {
  unflow(doc, path.slice(0, -1))
  doc.setIn(path, isCollection(value) ? value : newCollection(doc, value))
}

/** 删除；之后上一级成了空对象就一并删掉（不删到 keep 层以上）。 */
export function remove(doc: Document, path: Path, keep = 0) {
  if (!has(doc, path)) return
  doc.deleteIn(path)
  for (let p = path.slice(0, -1); p.length > keep; p = p.slice(0, -1)) {
    const n = nodeAt(doc, p)
    if (!isCollection(n) || n.items.length) break
    doc.deleteIn(p)
  }
}

/** 映射里改键名，位置不变。 */
export function renameKey(doc: Document, mapPath: Path, from: string, to: string) {
  const n = nodeAt(doc, mapPath)
  if (!isMap(n)) return
  const p = n.items.find((x) => keyOf(x.key) === from)
  if (!p) return
  if (isScalar(p.key)) p.key.value = to
  else p.key = doc.createNode(to)
}

/** 列表末尾加一项；列表不存在时新建（块格式）。 */
export function append(doc: Document, listPath: Path, value: unknown): number {
  const n = nodeAt(doc, listPath)
  const item = doc.createNode(value)
  if (isSeq(n)) {
    if (n.flow && !isScalar(item)) n.flow = false
    n.items.push(item)
    return n.items.length - 1
  }
  unflow(doc, listPath.slice(0, -1))
  const seq = new YAMLSeq()
  seq.items.push(item)
  doc.setIn(listPath, seq)
  return 0
}

/** 映射里加一项（targets、builds）。 */
export function addEntry(doc: Document, mapPath: Path, key: string, value: unknown) {
  unflow(doc, mapPath)
  const n = nodeAt(doc, mapPath)
  if (isMap(n)) n.set(key, doc.createNode(value))
  else {
    const m = new YAMLMap()
    m.set(key, doc.createNode(value))
    doc.setIn(mapPath, m)
  }
}

/** 列表里移动一项。 */
export function move(doc: Document, listPath: Path, from: number, to: number) {
  const n = nodeAt(doc, listPath)
  if (!isSeq(n) || to < 0 || to >= n.items.length) return
  const [x] = n.items.splice(from, 1)
  n.items.splice(to, 0, x)
}

/** 只写了一个值的对象（hooks 里的一条命令、附带文件）改成对象写法：{short: 值}。 */
export function expandShort(doc: Document, path: Path, short: string) {
  const n = nodeAt(doc, path)
  if (!isScalar(n)) return
  const m = new YAMLMap()
  m.set(short, n)
  doc.setIn(path, m)
}

// ---------------------------------------------------------------- 逐行对比

export type LineOp = { t: '='; a: number; b: number; text: string } | { t: '-'; a: number; text: string } | { t: '+'; b: number; text: string }

/** 逐行对比（最长公共子序列；先去掉相同的开头和结尾）。 */
export function lineDiff(a: string[], b: string[]): LineOp[] {
  let s = 0
  while (s < a.length && s < b.length && a[s] === b[s]) s++
  let ea = a.length
  let eb = b.length
  while (ea > s && eb > s && a[ea - 1] === b[eb - 1]) {
    ea--
    eb--
  }
  const n = ea - s
  const m = eb - s
  const ops: LineOp[] = []
  for (let i = 0; i < s; i++) ops.push({ t: '=', a: i, b: i, text: a[i] })
  if (n * m > 4_000_000) {
    // 改动太大：整段算删除再新增
    for (let i = s; i < ea; i++) ops.push({ t: '-', a: i, text: a[i] })
    for (let j = s; j < eb; j++) ops.push({ t: '+', b: j, text: b[j] })
  } else {
    const w = m + 1
    const len = new Uint32Array((n + 1) * w)
    for (let i = n - 1; i >= 0; i--)
      for (let j = m - 1; j >= 0; j--)
        len[i * w + j] = a[s + i] === b[s + j] ? len[(i + 1) * w + j + 1] + 1 : Math.max(len[(i + 1) * w + j], len[i * w + j + 1])
    let i = 0
    let j = 0
    while (i < n || j < m) {
      if (i < n && j < m && a[s + i] === b[s + j]) {
        ops.push({ t: '=', a: s + i, b: s + j, text: a[s + i] })
        i++
        j++
      } else if (j < m && (i === n || len[i * w + j + 1] > len[(i + 1) * w + j])) {
        ops.push({ t: '+', b: s + j, text: b[s + j] })
        j++
      } else {
        ops.push({ t: '-', a: s + i, text: a[s + i] })
        i++
      }
    }
  }
  for (let k = 0; k < a.length - ea; k++) ops.push({ t: '=', a: ea + k, b: eb + k, text: a[ea + k] })
  return ops
}

export interface Hunk {
  /** 每行：' ' 相同、'-' 删除、'+' 新增；no 是行号（删除的行是原文件的，其他是新文件的） */
  lines: { t: ' ' | '-' | '+'; no: number; text: string }[]
}

/** 改动分段显示，每段前后带 context 行没改的。 */
export function diffHunks(before: string, after: string, context = 3): Hunk[] {
  const ops = lineDiff(before.split('\n'), after.split('\n'))
  const changed = ops.map((o) => o.t !== '=')
  const show = ops.map((_, i) => {
    for (let k = Math.max(0, i - context); k <= Math.min(ops.length - 1, i + context); k++) if (changed[k]) return true
    return false
  })
  const hunks: Hunk[] = []
  let cur: Hunk | null = null
  ops.forEach((o, i) => {
    if (!show[i]) {
      cur = null
      return
    }
    if (!cur) hunks.push((cur = { lines: [] }))
    if (o.t === '=') cur.lines.push({ t: ' ', no: o.b + 1, text: o.text })
    else if (o.t === '-') cur.lines.push({ t: '-', no: o.a + 1, text: o.text })
    else cur.lines.push({ t: '+', no: o.b + 1, text: o.text })
  })
  return hunks
}
