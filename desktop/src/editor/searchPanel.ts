// 编辑器的查找替换面板（替换 CodeMirror 自带的英文面板）：中文按钮、区分大小写 / 正则 / 全词开关、匹配计数。
import { EditorState, type Extension } from '@codemirror/state'
import { EditorView, type Panel, type ViewUpdate } from '@codemirror/view'
import {
  SearchQuery,
  closeSearchPanel,
  findNext,
  findPrevious,
  getSearchQuery,
  replaceAll,
  replaceNext,
  search,
  selectMatches,
  setSearchQuery,
} from '@codemirror/search'

/** 最多数到这么多个匹配，再多显示「999+」 */
const COUNT_MAX = 999

function el<K extends keyof HTMLElementTagNameMap>(tag: K, attrs: Record<string, string> = {}, ...children: (Node | string)[]): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag)
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v)
  e.append(...children)
  return e
}

class FerrySearchPanel implements Panel {
  dom: HTMLElement
  top = true
  private find: HTMLInputElement
  private replace: HTMLInputElement
  private toggles: Record<'caseSensitive' | 'regexp' | 'wholeWord', HTMLButtonElement>
  private count: HTMLElement
  private query: SearchQuery

  constructor(private view: EditorView) {
    this.query = getSearchQuery(view.state)

    this.find = el('input', { class: 'fs-input', placeholder: '查找', 'aria-label': '查找', 'main-field': 'true', spellcheck: 'false' })
    this.replace = el('input', { class: 'fs-input', placeholder: '替换为', 'aria-label': '替换为', spellcheck: 'false' })
    const toggle = (key: 'caseSensitive' | 'regexp' | 'wholeWord', label: string, title: string) => {
      const b = el('button', { type: 'button', class: 'fs-toggle', title, 'aria-label': title }, label)
      b.addEventListener('mousedown', (e) => e.preventDefault()) // 不抢输入框的焦点
      b.addEventListener('click', () => {
        this.commit({ [key]: !this.query[key] })
      })
      return b
    }
    this.toggles = {
      caseSensitive: toggle('caseSensitive', 'Aa', '区分大小写'),
      regexp: toggle('regexp', '.*', '正则表达式'),
      wholeWord: toggle('wholeWord', '全词', '全词匹配'),
    }
    this.count = el('span', { class: 'fs-count' })
    const button = (label: string, title: string, run: () => void, primary = false) => {
      const b = el('button', { type: 'button', class: 'fs-btn' + (primary ? ' fs-primary' : ''), title }, label)
      b.addEventListener('click', run)
      return b
    }
    const close = el('button', { type: 'button', class: 'fs-close', title: '关闭（Esc）', 'aria-label': '关闭' }, '×')
    close.addEventListener('click', () => closeSearchPanel(view))

    for (const input of [this.find, this.replace]) {
      input.addEventListener('input', () => this.commit({}))
      input.addEventListener('keydown', (e) => this.keydown(e, input === this.replace))
    }

    this.dom = el(
      'div',
      { class: 'ferry-search', role: 'search' },
      el(
        'div',
        { class: 'fs-row' },
        el('div', { class: 'fs-field' }, this.find, el('span', { class: 'fs-toggles' }, this.toggles.caseSensitive, this.toggles.regexp, this.toggles.wholeWord)),
        this.count,
        button('上一个', '上一个（Shift+Enter）', () => findPrevious(view)),
        button('下一个', '下一个（Enter）', () => findNext(view)),
        button('全部选中', '选中全部匹配（Alt+Enter），可同时编辑', () => selectMatches(view)),
        el('span', { class: 'fs-spacer' }),
        close,
      ),
      el(
        'div',
        { class: 'fs-row' },
        el('div', { class: 'fs-field' }, this.replace),
        el('span', { class: 'fs-count fs-count-blank' }),
        button('替换', '替换当前匹配并跳到下一个（Enter）', () => replaceNext(view)),
        button('全部替换', '替换全部匹配（Ctrl+Alt+Enter）', () => replaceAll(view)),
      ),
    )
    this.show(this.query)
  }

  /** 输入框或开关变了：生成新的查询交给编辑器 */
  private commit(change: Partial<Pick<SearchQuery, 'caseSensitive' | 'regexp' | 'wholeWord'>>) {
    const q = new SearchQuery({
      search: this.find.value,
      replace: this.replace.value,
      caseSensitive: change.caseSensitive ?? this.query.caseSensitive,
      regexp: change.regexp ?? this.query.regexp,
      wholeWord: change.wholeWord ?? this.query.wholeWord,
      literal: this.query.literal,
    })
    if (!q.eq(this.query)) {
      this.query = q
      this.view.dispatch({ effects: setSearchQuery.of(q) })
      this.show(q)
    }
  }

  private keydown(e: KeyboardEvent, inReplace: boolean) {
    if (e.key === 'Escape') {
      e.preventDefault()
      closeSearchPanel(this.view)
      this.view.focus()
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (e.ctrlKey && e.altKey) replaceAll(this.view)
      else if (e.altKey) selectMatches(this.view)
      else if (inReplace && !e.shiftKey) replaceNext(this.view)
      else if (e.shiftKey) findPrevious(this.view)
      else findNext(this.view)
    }
  }

  /** 把查询显示到输入框和开关上（打开面板时用选中的文字、或别处改了查询） */
  private show(q: SearchQuery) {
    if (this.find.value !== q.search) this.find.value = q.search
    if (this.replace.value !== q.replace) this.replace.value = q.replace
    for (const [k, b] of Object.entries(this.toggles)) {
      const on = q[k as keyof typeof this.toggles]
      b.classList.toggle('fs-on', on)
      b.setAttribute('aria-pressed', String(on))
    }
    this.updateCount(this.view.state)
  }

  /** 匹配计数：「第几个 / 共几个」 */
  private updateCount(state: EditorState) {
    const q = this.query
    this.find.classList.remove('fs-bad')
    this.count.classList.remove('fs-none')
    if (!q.search) {
      this.count.textContent = ''
      return
    }
    if (!q.valid) {
      this.count.textContent = '正则有误'
      this.count.classList.add('fs-none')
      this.find.classList.add('fs-bad')
      return
    }
    const sel = state.selection.main
    let total = 0
    let current = 0
    const cursor = q.getCursor(state)
    for (let r = cursor.next(); !r.done; r = cursor.next()) {
      total++
      if (r.value.from === sel.from && r.value.to === sel.to) current = total
      if (total > COUNT_MAX) break
    }
    if (total === 0) {
      this.count.textContent = '无结果'
      this.count.classList.add('fs-none')
      this.find.classList.add('fs-bad')
    } else {
      const all = total > COUNT_MAX ? `${COUNT_MAX}+` : String(total)
      this.count.textContent = current ? `${current} / ${all}` : `共 ${all} 个`
    }
  }

  mount() {
    this.find.select()
  }

  update(u: ViewUpdate) {
    let queryChanged = false
    for (const tr of u.transactions) {
      for (const e of tr.effects) {
        if (e.is(setSearchQuery)) {
          queryChanged = true
          if (!e.value.eq(this.query)) {
            this.query = e.value
            this.show(e.value)
            return
          }
        }
      }
    }
    if (queryChanged || u.docChanged || u.selectionSet) this.updateCount(u.state)
  }
}

const panelTheme = EditorView.theme({
  '.cm-panels.cm-panels-top': { borderBottom: '1px solid var(--el-border-color-lighter)' },
  '.cm-panels': { backgroundColor: 'var(--el-fill-color-light)', color: 'var(--el-text-color-regular)' },
  '.ferry-search': { padding: '8px 10px', display: 'flex', flexDirection: 'column', gap: '6px', fontSize: '13px' },
  '.ferry-search .fs-row': { display: 'flex', alignItems: 'center', gap: '6px' },
  '.ferry-search .fs-field': {
    position: 'relative',
    width: '320px',
    display: 'flex',
    alignItems: 'center',
  },
  '.ferry-search .fs-input': {
    width: '100%',
    height: '28px',
    boxSizing: 'border-box',
    padding: '0 8px',
    border: '1px solid var(--el-border-color)',
    borderRadius: '4px',
    background: 'var(--el-bg-color)',
    color: 'var(--el-text-color-primary)',
    fontFamily: "'Cascadia Mono', Consolas, monospace",
    fontSize: '13px',
    outline: 'none',
    transition: 'border-color .15s',
  },
  '.ferry-search .fs-field .fs-input[main-field]': { paddingRight: '96px' },
  '.ferry-search .fs-input:hover': { borderColor: 'var(--el-border-color-hover)' },
  '.ferry-search .fs-input:focus': { borderColor: 'var(--el-color-primary)' },
  '.ferry-search .fs-input.fs-bad': { borderColor: 'var(--el-color-danger)' },
  '.ferry-search .fs-toggles': { position: 'absolute', right: '3px', display: 'flex', gap: '2px' },
  '.ferry-search .fs-toggle': {
    height: '22px',
    minWidth: '26px',
    padding: '0 5px',
    border: '1px solid transparent',
    borderRadius: '3px',
    background: 'transparent',
    color: 'var(--el-text-color-secondary)',
    fontSize: '12px',
    fontFamily: "'Cascadia Mono', Consolas, monospace",
    cursor: 'pointer',
  },
  '.ferry-search .fs-toggle:hover': { background: 'var(--el-fill-color)', color: 'var(--el-text-color-primary)' },
  '.ferry-search .fs-toggle.fs-on': {
    background: 'var(--el-color-primary-light-9)',
    borderColor: 'var(--el-color-primary-light-5)',
    color: 'var(--el-color-primary)',
  },
  '.ferry-search .fs-count': {
    minWidth: '64px',
    color: 'var(--el-text-color-secondary)',
    fontSize: '12px',
    fontVariantNumeric: 'tabular-nums',
    whiteSpace: 'nowrap',
  },
  '.ferry-search .fs-count.fs-none': { color: 'var(--el-color-danger)' },
  '.ferry-search .fs-btn': {
    height: '28px',
    padding: '0 12px',
    border: '1px solid var(--el-border-color)',
    borderRadius: '4px',
    background: 'var(--el-bg-color)',
    color: 'var(--el-text-color-regular)',
    fontSize: '12px',
    cursor: 'pointer',
    whiteSpace: 'nowrap',
    transition: 'color .15s, border-color .15s, background-color .15s',
  },
  '.ferry-search .fs-btn:hover': {
    color: 'var(--el-color-primary)',
    borderColor: 'var(--el-color-primary-light-7)',
    background: 'var(--el-color-primary-light-9)',
  },
  '.ferry-search .fs-spacer': { flex: '1' },
  '.ferry-search .fs-close': {
    width: '26px',
    height: '26px',
    border: 'none',
    borderRadius: '4px',
    background: 'transparent',
    color: 'var(--el-text-color-secondary)',
    fontSize: '18px',
    lineHeight: '1',
    cursor: 'pointer',
  },
  '.ferry-search .fs-close:hover': { background: 'var(--el-fill-color)', color: 'var(--el-text-color-primary)' },
  // 当前匹配与其他匹配的高亮
  '.cm-searchMatch': { backgroundColor: 'rgba(255, 196, 0, 0.28)', outline: '1px solid rgba(255, 170, 0, 0.55)' },
  '.cm-searchMatch.cm-searchMatch-selected': { backgroundColor: 'rgba(255, 140, 0, 0.55)' },
})

/** 其他内置界面文字的中文（跳转到行等） */
const phrases = EditorState.phrases.of({
  'Go to line': '跳转到行',
  go: '跳转',
  'replaced match on line $': '已替换第 $ 行的匹配',
  'replaced $ matches': '已替换 $ 处',
  'current match': '当前匹配',
  'on line': '在第几行',
  'Folded lines': '已折叠的行',
  'Unfolded lines': '已展开的行',
  'Fold line': '折叠',
  'Unfold line': '展开',
  'folded code': '已折叠的代码',
  unfold: '展开',
})

export function ferrySearch(): Extension {
  return [search({ top: true, createPanel: (view) => new FerrySearchPanel(view) }), panelTheme, phrases]
}
