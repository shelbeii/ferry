// 「数据库」查询窗口、DDL 显示用的 SQL 编辑器（CodeMirror）。
import { Compartment, EditorState, type Extension } from '@codemirror/state'
import {
  EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, highlightSpecialChars, drawSelection, tooltips,
} from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { bracketMatching, indentOnInput, syntaxHighlighting, defaultHighlightStyle } from '@codemirror/language'
import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap } from '@codemirror/autocomplete'
import { highlightSelectionMatches, searchKeymap } from '@codemirror/search'
import { sql, MySQL, PostgreSQL, type SQLNamespace } from '@codemirror/lang-sql'
import { oneDark } from '@codemirror/theme-one-dark'
import { ferrySearch } from './searchPanel'

export interface SqlEditorOptions {
  parent: HTMLElement
  doc: string
  kind: 'mysql' | 'postgres'
  dark: boolean
  readOnly?: boolean
  /** 补全用的表、列：{ 表: [列…] } 或 { 模式: { 表: [列…] } } */
  schema?: SQLNamespace
  defaultTable?: string
  onChange?: (text: string) => void
  /** Ctrl+Enter：执行（选中了就执行选中的，否则执行光标所在的那一条） */
  onRun?: (text: string, all: boolean) => void
}

export interface SqlEditor {
  view: EditorView
  setDark(dark: boolean): void
  setSchema(schema: SQLNamespace | undefined): void
  setDoc(text: string): void
  /** 选中的文字；没选中时为空 */
  selection(): string
  /** 光标所在的那条语句（按分号和空行分） */
  current(): string
  focus(): void
}

const lightTheme = EditorView.theme({
  '.cm-gutters': { backgroundColor: 'var(--el-fill-color-lighter)', borderRight: '1px solid var(--el-border-color-lighter)' },
})

const commonTheme = EditorView.theme({
  '&': { height: '100%', fontSize: '13px' },
  '&.cm-focused': { outline: 'none' },
  '.cm-scroller': { fontFamily: "'Cascadia Mono', Consolas, 'Courier New', monospace", lineHeight: '1.55' },
  '.cm-tooltip.cm-tooltip-autocomplete > ul': { fontFamily: "'Cascadia Mono', Consolas, monospace" },
})

/** 光标所在的语句：从光标往前、往后找到分号（不细分引号，够用） */
export function statementAt(text: string, pos: number): string {
  let start = text.lastIndexOf(';', pos - 1) + 1
  // 光标正好在一条语句的分号后面：算前一条
  if (text.slice(start, pos).trim() === '' && start > 0) {
    const prev = text.lastIndexOf(';', start - 2) + 1
    if (text.slice(prev, start - 1).trim()) {
      return text.slice(prev, start).trim()
    }
  }
  let end = text.indexOf(';', pos)
  if (end < 0) end = text.length
  else end += 1
  return text.slice(start, end).trim()
}

export function createSqlEditor(o: SqlEditorOptions): SqlEditor {
  const themeSlot = new Compartment()
  const langSlot = new Compartment()
  const themeOf = (dark: boolean): Extension => (dark ? oneDark : [lightTheme, syntaxHighlighting(defaultHighlightStyle, { fallback: true })])
  const langOf = (schema?: SQLNamespace) =>
    sql({ dialect: o.kind === 'mysql' ? MySQL : PostgreSQL, schema, defaultTable: o.defaultTable, upperCaseKeywords: true })
  let view: EditorView
  const run = (all: boolean) => {
    const sel = view.state.selection.main
    const text = sel.empty ? (all ? view.state.doc.toString() : statementAt(view.state.doc.toString(), sel.head)) : view.state.sliceDoc(sel.from, sel.to)
    o.onRun?.(text, all && sel.empty)
    return true
  }
  view = new EditorView({
    parent: o.parent,
    state: EditorState.create({
      doc: o.doc,
      extensions: [
        lineNumbers(),
        highlightActiveLineGutter(),
        highlightSpecialChars(),
        history(),
        drawSelection(),
        EditorState.allowMultipleSelections.of(true),
        indentOnInput(),
        bracketMatching(),
        closeBrackets(),
        highlightActiveLine(),
        highlightSelectionMatches(),
        ferrySearch(),
        langSlot.of(langOf(o.schema)),
        tooltips({ parent: document.body }),
        autocompletion({ icons: true }),
        commonTheme,
        themeSlot.of(themeOf(o.dark)),
        EditorState.readOnly.of(!!o.readOnly),
        EditorView.editable.of(!o.readOnly),
        keymap.of([
          { key: 'Mod-Enter', preventDefault: true, run: () => run(false) },
          { key: 'Shift-Mod-Enter', preventDefault: true, run: () => run(true) },
          { key: 'F9', preventDefault: true, run: () => run(true) },
          ...closeBracketsKeymap,
          ...defaultKeymap,
          ...searchKeymap,
          ...historyKeymap,
          ...completionKeymap,
          indentWithTab,
        ]),
        EditorView.updateListener.of((u) => {
          if (u.docChanged) o.onChange?.(u.state.doc.toString())
        }),
      ],
    }),
  })
  return {
    view,
    setDark(dark) {
      view.dispatch({ effects: themeSlot.reconfigure(themeOf(dark)) })
    },
    setSchema(schema) {
      view.dispatch({ effects: langSlot.reconfigure(langOf(schema)) })
    },
    setDoc(text) {
      if (view.state.doc.toString() !== text) view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text } })
    },
    selection() {
      const sel = view.state.selection.main
      return sel.empty ? '' : view.state.sliceDoc(sel.from, sel.to)
    },
    current() {
      return statementAt(view.state.doc.toString(), view.state.selection.main.head)
    },
    focus() {
      view.focus()
    },
  }
}
