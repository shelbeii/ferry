// ferry.yaml 编辑器（CodeMirror 6）：YAML 高亮、折叠、搜索、按结构补全字段与取值、悬停说明、实时校验。
import { Compartment, EditorState, type Extension } from '@codemirror/state'
import {
  EditorView,
  crosshairCursor,
  drawSelection,
  dropCursor,
  highlightActiveLine,
  highlightActiveLineGutter,
  highlightSpecialChars,
  hoverTooltip,
  keymap,
  tooltips,
  lineNumbers,
  rectangularSelection,
} from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { bracketMatching, foldGutter, foldKeymap, indentOnInput, indentUnit, syntaxHighlighting, defaultHighlightStyle } from '@codemirror/language'
import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap, type CompletionContext, type CompletionResult } from '@codemirror/autocomplete'
import { highlightSelectionMatches, searchKeymap } from '@codemirror/search'
import { ferrySearch } from './searchPanel'
import { diagnosticCount, lintGutter, linter, lintKeymap, type Diagnostic } from '@codemirror/lint'
import { yaml } from '@codemirror/lang-yaml'
import { oneDark } from '@codemirror/theme-one-dark'
import { isMap, isScalar, isSeq, parseDocument, type Document } from 'yaml'
import { builtinVars, describe, lookup, schema, type SchemaNode } from './schema'
import { HOVER_DELAY } from '../ui'

// ---------------------------------------------------------------- 由缩进推断当前位置的字段路径

interface LineInfo {
  indent: number // 行首空格数
  isItem: boolean // 以 "- " 开头
  key?: string
  keyIndent: number // 键所在列
  hasValue: boolean // 键后面同一行有值（不是嵌套对象的开头）
}

function parseLine(text: string): LineInfo | null {
  const indent = text.length - text.trimStart().length
  let rest = text.slice(indent)
  if (!rest || rest.startsWith('#')) return null
  let isItem = false
  let keyIndent = indent
  if (rest === '-' || rest.startsWith('- ')) {
    isItem = true
    const after = rest.slice(1)
    const sp = after.length - after.trimStart().length
    rest = after.trimStart()
    keyIndent = indent + 1 + sp
  }
  const m = /^("[^"]*"|'[^']*'|[^\s:#][^:#]*?)\s*:(\s|$)/.exec(rest)
  if (!m) return { indent, isItem, keyIndent, hasValue: true }
  const key = m[1].replace(/^["']|["']$/g, '')
  const value = rest.slice(m[0].length).trim()
  const hasValue = value !== '' && !value.startsWith('&') && !value.startsWith('#')
  return { indent, isItem, key, keyIndent, hasValue }
}

/** 第 lineIdx 行（0 起）键所在层级的路径（不含该行自己的键）。keyIndent：该行键的列。 */
function pathAt(lines: string[], lineIdx: number, keyIndent: number, currentIsItem: boolean): string[] {
  const path: string[] = []
  let want = keyIndent
  let seqParent = false
  if (currentIsItem) {
    path.unshift('[]')
    want = lines[lineIdx].length - lines[lineIdx].trimStart().length
    seqParent = true
  }
  for (let i = lineIdx - 1; i >= 0; i--) {
    const li = parseLine(lines[i])
    if (!li) continue
    if (li.isItem && !seqParent) {
      if (li.indent < want && want <= li.keyIndent) {
        // 我们是这个列表元素里的字段
        path.unshift('[]')
        want = li.indent
        seqParent = true
      }
      continue
    }
    if (!li.key) continue
    if (seqParent) {
      if (!li.isItem && li.keyIndent <= want && !li.hasValue) {
        path.unshift(li.key)
        want = li.keyIndent
        seqParent = false
      }
      continue
    }
    if (li.keyIndent < want && !li.hasValue) {
      path.unshift(li.key)
      want = li.keyIndent
      if (li.isItem) {
        // 键写在 "- " 后面：它本身属于一个列表元素
        path.unshift('[]')
        want = li.indent
        seqParent = true
      }
    }
  }
  return path
}

// ---------------------------------------------------------------- 补全

function nodeInfo(n: SchemaNode): string {
  return n.type ?? (n.fields ? '对象' : n.map ? '映射' : n.item ? '列表' : '')
}

/** 文档里定义过的变量名（项目、各目标、各应用的 vars）。 */
function definedVars(text: string): string[] {
  const names = new Set<string>()
  try {
    const doc = parseDocument(text)
    const collect = (m: unknown) => {
      if (isMap(m)) for (const p of m.items) if (isScalar(p.key)) names.add(String(p.key.value))
    }
    collect(doc.getIn(['vars']))
    const targets = doc.getIn(['targets'])
    if (isMap(targets)) for (const p of targets.items) if (isMap(p.value)) collect(p.value.get('vars'))
    const apps = doc.getIn(['apps'])
    if (isSeq(apps)) for (const a of apps.items) if (isMap(a)) collect(a.get('vars'))
  } catch {
    /* 解析失败时只给内置变量 */
  }
  return [...names]
}

function ferryCompletions(ctx: CompletionContext): CompletionResult | null {
  const line = ctx.state.doc.lineAt(ctx.pos)
  const before = line.text.slice(0, ctx.pos - line.from)

  // ${变量}
  const vm = /\$\{([\w:.-]*)$/.exec(before)
  if (vm) {
    // 接受时补上右括号（后面已经有 } 就不补）
    const close = line.text.slice(ctx.pos - line.from).startsWith('}') ? '' : '}'
    const user = definedVars(ctx.state.doc.toString()).map((v) => ({ label: v, type: 'variable', detail: '变量', apply: v + close }))
    const builtins = Object.entries(builtinVars).map(([label, info]) => ({ label, type: 'constant', detail: '内置', info, apply: label + close }))
    return {
      from: ctx.pos - vm[1].length,
      options: [
        ...user,
        ...builtins,
        { label: 'env:', type: 'keyword', detail: '本机环境变量', info: '${env:NAME}：本机环境变量（未设置时报错）' },
        { label: 'each:', type: 'keyword', detail: '仅构建命令', info: '${each:变量}：同一条构建的各应用的值，逗号连接' },
      ],
      validFor: /^[\w:.-]*$/,
    }
  }

  const lines = ctx.state.doc.toString().split('\n')
  const idx = line.number - 1

  // 取值：key: <光标>
  const km = /^(\s*)(-\s+)?([^\s:#][^:#]*?)\s*:\s+([\w.-]*)$/.exec(before)
  if (km) {
    const keyIndent = km[1].length + (km[2]?.length ?? 0)
    const path = pathAt(lines, idx, keyIndent, !!km[2])
    const node = lookup([...path, km[3]])
    if (!node?.values) return null
    return { from: ctx.pos - km[4].length, options: node.values.map((v) => ({ label: v, type: 'enum' })), validFor: /^[\w.-]*$/ }
  }

  // 字段名
  const wm = /^(\s*)(-\s+)?([A-Za-z_][\w-]*)?$/.exec(before)
  if (!wm || (!wm[3] && !ctx.explicit)) return null
  const keyIndent = wm[1].length + (wm[2]?.length ?? 0)
  const path = pathAt(lines, idx, keyIndent, !!wm[2])
  const node = lookup(path)
  const fields = node?.fields
  if (!fields) return null
  return {
    from: ctx.pos - (wm[3]?.length ?? 0),
    options: Object.entries(fields).map(([label, n]) => ({
      label,
      type: 'property',
      detail: n.label ? `${n.label} · ${nodeInfo(n)}` : nodeInfo(n),
      info: describe(n),
      apply: label + ': ',
    })),
    validFor: /^[\w-]*$/,
  }
}

// ---------------------------------------------------------------- 悬停说明

const ferryHover = hoverTooltip((view, pos) => {
  const line = view.state.doc.lineAt(pos)
  const li = parseLine(line.text)
  if (!li?.key) return null
  const start = line.from + li.keyIndent
  const end = start + li.key.length
  if (pos < start || pos > end) return null
  const lines = view.state.doc.toString().split('\n')
  const path = pathAt(lines, line.number - 1, li.keyIndent, li.isItem)
  const node = lookup([...path, li.key])
  if (!node) return null
  return {
    pos: start,
    end,
    above: true,
    create() {
      const dom = document.createElement('div')
      dom.className = 'cm-ferry-doc'
      const title = document.createElement('div')
      title.className = 'cm-ferry-doc-title'
      title.textContent = [...path.filter((p) => p !== '[]'), li.key].join('.') + (nodeInfo(node) ? `（${nodeInfo(node)}）` : '')
      const body = document.createElement('div')
      body.textContent = describe(node)
      dom.append(title, body)
      return { dom }
    },
  }
}, { hoverTime: HOVER_DELAY })

// ---------------------------------------------------------------- 校验

export type Validate = (text: string) => Promise<string | null>

/** 把 ferry 的错误信息映射到文档位置。 */
function mapErrors(error: string, text: string): Diagnostic[] {
  const lines = error.split('\n').map((l) => l.trim()).filter(Boolean)
  // 第一行通常是「<路径>:」或「<路径>: yaml: …」
  if (lines.length && /ferry\.ya?ml:?$/i.test(lines[0])) lines.shift()
  let doc: Document | undefined
  try {
    doc = parseDocument(text)
  } catch {
    doc = undefined
  }
  const lineStarts = [0]
  for (let i = 0; i < text.length; i++) if (text[i] === '\n') lineStarts.push(i + 1)
  const lineRange = (n: number) => {
    const from = lineStarts[Math.max(0, Math.min(n - 1, lineStarts.length - 1))]
    const nextStart = lineStarts[n] ?? text.length + 1
    return { from, to: Math.max(from, nextStart - 1) }
  }
  const out: Diagnostic[] = []
  for (const raw of lines) {
    const msg = raw.replace(/^.*?ferry\.ya?ml:\s*/i, '').replace(/^yaml:\s*(unmarshal errors:)?\s*/, '')
    if (!msg) continue
    const lm = /line (\d+)|第 (\d+) 行/.exec(msg)
    if (lm) {
      out.push({ ...lineRange(Number(lm[1] ?? lm[2])), severity: 'error', message: msg })
      continue
    }
    const pm = /^([^\s：]+)：/.exec(msg) ?? /^(project|version|targets|apps|builds|vars)\b/.exec(msg)
    const range = pm && doc ? rangeOf(doc, pm[1]) : undefined
    out.push({ ...(range ?? { from: 0, to: Math.min(text.length, lineRange(1).to) }), severity: 'error', message: msg })
  }
  return out
}

/** 「targets.prod-a.vars」「apps.System」「apps[3]」「builds.maven」这类路径在文档中的位置。 */
function rangeOf(doc: Document, path: string): { from: number; to: number } | undefined {
  const segs: (string | number)[] = []
  for (const part of path.split('.')) {
    const m = /^([^[]+)\[(\d+)\]$/.exec(part)
    if (m) segs.push(m[1], Number(m[2]))
    else segs.push(part)
  }
  let node: unknown = doc.contents
  let range: [number, number, number?] | undefined
  for (let i = 0; i < segs.length; i++) {
    const seg = segs[i]
    if (isMap(node)) {
      const pair = node.items.find((p) => isScalar(p.key) && String(p.key.value) === String(seg))
      if (!pair) break
      range = (pair.key as { range?: [number, number, number] }).range
      node = pair.value
    } else if (isSeq(node)) {
      const item =
        typeof seg === 'number'
          ? node.items[seg]
          : node.items.find((it) => isMap(it) && String(it.get('name')).toLowerCase() === String(seg).toLowerCase())
      if (!item) break
      range = (item as { range?: [number, number, number] }).range
      node = item
    } else break
  }
  if (!range) return undefined
  return { from: range[0], to: Math.max(range[0] + 1, Math.min(range[1], range[0] + 200)) }
}

function ferryLinter(validate: Validate) {
  return linter(
    async (view) => {
      const text = view.state.doc.toString()
      const doc = parseDocument(text)
      if (doc.errors.length) {
        return doc.errors.map((e) => ({
          from: e.pos[0],
          to: Math.max(e.pos[0] + 1, e.pos[1]),
          severity: 'error' as const,
          message: e.message.split('\n')[0],
        }))
      }
      const err = await validate(text)
      return err ? mapErrors(err, text) : []
    },
    { delay: 700 },
  )
}

// ---------------------------------------------------------------- 定位

export interface Location {
  from: number
  to: number
  /** 路径存在：选中的是它的值（或对象的键）；不存在：位置是最近一个存在的上级的键 */
  found: boolean
}

/** 字段路径（如 ['targets', 'prod-b', 'root']）在文本中的位置。 */
export function locate(text: string, path: string[]): Location | null {
  let doc: Document
  try {
    doc = parseDocument(text)
  } catch {
    return null
  }
  let node: unknown = doc.contents
  let last: { from: number; to: number } | null = null
  for (const seg of path) {
    if (!isMap(node)) return last ? { ...last, found: false } : null
    const pair = node.items.find((p) => isScalar(p.key) && String(p.key.value) === seg)
    if (!pair) return last ? { ...last, found: false } : null
    const kr = (pair.key as { range?: [number, number, number] }).range
    if (kr) last = { from: kr[0], to: kr[1] }
    node = pair.value
  }
  const vr = isScalar(node) ? (node as { range?: [number, number, number] }).range : undefined
  if (vr) return { from: vr[0], to: vr[1], found: true }
  return last ? { ...last, found: true } : null
}

/** 选中并滚动到位置中间。 */
export function reveal(view: EditorView, loc: Location) {
  const from = Math.min(loc.from, view.state.doc.length)
  const to = Math.min(loc.found ? loc.to : loc.from, view.state.doc.length)
  view.dispatch({ selection: { anchor: from, head: to }, effects: EditorView.scrollIntoView(from, { y: 'center' }) })
  view.focus()
}

// ---------------------------------------------------------------- 组装

export interface EditorOptions {
  parent: HTMLElement
  doc: string
  dark: boolean
  validate: Validate
  onChange?: (text: string) => void
  onCursor?: (line: number, col: number) => void
  onDiagnostics?: (count: number) => void
  onSave?: () => void
}

export interface FerryEditor {
  view: EditorView
  setDark(dark: boolean): void
  setDoc(text: string): void
}

const lightTheme = EditorView.theme({
  '.cm-gutters': { backgroundColor: 'var(--el-fill-color-lighter)', borderRight: '1px solid var(--el-border-color-lighter)' },
})

const commonTheme = EditorView.theme({
  '&': { height: '100%', fontSize: '13px' },
  '.cm-scroller': { fontFamily: "'Cascadia Mono', Consolas, 'Courier New', monospace", lineHeight: '1.55' },
  '.cm-ferry-doc': { padding: '6px 10px', maxWidth: '460px', lineHeight: '1.5', fontSize: '12.5px', whiteSpace: 'pre-line' },
  '.cm-ferry-doc-title': { fontWeight: '600', marginBottom: '2px' },
  '.cm-tooltip.cm-tooltip-autocomplete > ul': { fontFamily: "'Cascadia Mono', Consolas, monospace" },
})

export function createEditor(o: EditorOptions): FerryEditor {
  const themeSlot = new Compartment()
  const themeOf = (dark: boolean): Extension => (dark ? oneDark : [lightTheme, syntaxHighlighting(defaultHighlightStyle, { fallback: true })])
  const view = new EditorView({
    parent: o.parent,
    state: EditorState.create({
      doc: o.doc,
      extensions: [
        lineNumbers(),
        highlightActiveLineGutter(),
        highlightSpecialChars(),
        history(),
        foldGutter(),
        drawSelection(),
        dropCursor(),
        EditorState.allowMultipleSelections.of(true),
        indentOnInput(),
        indentUnit.of('  '),
        EditorState.tabSize.of(2),
        bracketMatching(),
        closeBrackets(),
        rectangularSelection(),
        crosshairCursor(),
        highlightActiveLine(),
        highlightSelectionMatches(),
        ferrySearch(),
        yaml(),
        // 提示框挂在 body 上，不会被对话框的边缘裁掉
        tooltips({ parent: document.body }),
        autocompletion({ override: [ferryCompletions], icons: true }),
        ferryHover,
        lintGutter(),
        ferryLinter(o.validate),
        commonTheme,
        themeSlot.of(themeOf(o.dark)),
        keymap.of([
          { key: 'Mod-s', preventDefault: true, run: () => (o.onSave?.(), true) },
          ...closeBracketsKeymap,
          ...defaultKeymap,
          ...searchKeymap,
          ...historyKeymap,
          ...foldKeymap,
          ...completionKeymap,
          ...lintKeymap,
          indentWithTab,
        ]),
        EditorView.updateListener.of((u) => {
          if (u.docChanged) o.onChange?.(u.state.doc.toString())
          if (u.selectionSet || u.docChanged) {
            const head = u.state.selection.main.head
            const line = u.state.doc.lineAt(head)
            o.onCursor?.(line.number, head - line.from + 1)
          }
          o.onDiagnostics?.(diagnosticCount(u.state))
        }),
      ],
    }),
  })
  return {
    view,
    setDark(dark) {
      view.dispatch({ effects: themeSlot.reconfigure(themeOf(dark)) })
    },
    setDoc(text) {
      // 只替换不同的那一段：表单改一处时 YAML 的滚动位置、撤销记录都正常
      const cur = view.state.doc.toString()
      if (cur === text) return
      let s = 0
      while (s < cur.length && s < text.length && cur[s] === text[s]) s++
      let e = 0
      while (e < cur.length - s && e < text.length - s && cur[cur.length - 1 - e] === text[text.length - 1 - e]) e++
      view.dispatch({ changes: { from: s, to: cur.length - e, insert: text.slice(s, text.length - e) } })
    },
  }
}

// 供测试
export const __test = { parseLine, pathAt, mapErrors, schema, locate }
