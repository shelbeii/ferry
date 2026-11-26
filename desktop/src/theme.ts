// Ferry 桌面版主题目录与配色元数据。
// 每套主题都建立在浅色（light）或深色（dark）二元基座之上：
// Element Plus 基础变量、Tauri 原生标题栏与液态玻璃明暗自适应按基座工作，
// 主题自身的专属配色由 html[data-theme="<id>"] 叠加覆写。

export type ResolvedTheme = 'light' | 'dark' | 'warm-paper' | 'warm-dusk' | 'dimmed-sage' | 'obsidian'
export type ThemeSetting = 'system' | ResolvedTheme

export interface TerminalPalette {
  background: string
  foreground: string
  cursor: string
  selectionBackground: string
  statusBg: string
  statusBorder: string
  statusText: string
}

export interface ThemeDefinition {
  id: ResolvedTheme
  label: string
  summary: string
  dark: boolean
  /** 主题选择器里展示的缩略色块（背景、卡片、主色） */
  swatches: {
    page: string
    surface: string
    primary: string
  }
  /** 弹窗、下拉框粒子特效的调色盘 */
  particles: {
    colors: string[]
    maxAlpha: number
  }
  /** 内置 SSH 终端（xterm.js）与底部状态栏配色 */
  terminal: TerminalPalette
}

export const THEMES: readonly ThemeDefinition[] = [
  {
    id: 'light',
    label: '浅色',
    summary: '冷雾白底 · 靛蓝',
    dark: false,
    swatches: { page: '#f2f5fa', surface: '#ffffff', primary: '#4f46e5' },
    particles: { colors: ['#4f46e5', '#3f38b7', '#4a72d4', '#7b62e6'], maxAlpha: 0.78 },
    terminal: {
      background: '#1e1e1e',
      foreground: '#d4d4d4',
      cursor: '#d4d4d4',
      selectionBackground: '#264f78',
      statusBg: '#252526',
      statusBorder: '#333333',
      statusText: '#a0a0a0',
    },
  },
  {
    id: 'dark',
    label: '深色',
    summary: '午夜深蓝 · 靛蓝',
    dark: true,
    swatches: { page: '#0d1421', surface: '#141c2b', primary: '#9297ff' },
    particles: { colors: ['#e8efff', '#c9dafc', '#a0cfff', '#d6d0ff'], maxAlpha: 0.5 },
    terminal: {
      background: '#121822',
      foreground: '#d9e2f2',
      cursor: '#9297ff',
      selectionBackground: '#273a5e',
      statusBg: '#161e2c',
      statusBorder: '#27344a',
      statusText: '#8e9eb7',
    },
  },
  {
    id: 'warm-paper',
    label: '暖纸护眼',
    summary: '羊皮暖白 · 陶土琥珀',
    dark: false,
    swatches: { page: '#f4efe4', surface: '#fcfaf4', primary: '#b85d19' },
    particles: { colors: ['#b85d19', '#9c4c12', '#c97b32', '#8c5327'], maxAlpha: 0.74 },
    terminal: {
      background: '#1f1a16',
      foreground: '#eae0d2',
      cursor: '#e89b4e',
      selectionBackground: '#523b26',
      statusBg: '#27211c',
      statusBorder: '#3a3028',
      statusText: '#b5a695',
    },
  },
  {
    id: 'warm-dusk',
    label: '夜航琥珀',
    summary: '暖炭暗夜 · 航标金',
    dark: true,
    swatches: { page: '#13110f', surface: '#1c1815', primary: '#e59546' },
    particles: { colors: ['#f7e4cb', '#f0c48b', '#e59546', '#d8b490'], maxAlpha: 0.5 },
    terminal: {
      background: '#110e0c',
      foreground: '#ece3d5',
      cursor: '#f0a152',
      selectionBackground: '#4b3421',
      statusBg: '#191512',
      statusBorder: '#302922',
      statusText: '#9c8f80',
    },
  },
  {
    id: 'dimmed-sage',
    label: '雾岚森绿',
    summary: '中暗青灰 · 鼠尾草绿',
    dark: true,
    swatches: { page: '#1b2326', surface: '#232d31', primary: '#5ec4a4' },
    particles: { colors: ['#dcf2ea', '#a8dfd0', '#5ec4a4', '#94c7bd'], maxAlpha: 0.48 },
    terminal: {
      background: '#161d20',
      foreground: '#dce7e2',
      cursor: '#68cfa9',
      selectionBackground: '#294842',
      statusBg: '#1d262a',
      statusBorder: '#334349',
      statusText: '#8da49d',
    },
  },
  {
    id: 'obsidian',
    label: '极夜黑',
    summary: '纯黑高对比 · 冰川青',
    dark: true,
    swatches: { page: '#050507', surface: '#0d0e12', primary: '#38bdf8' },
    particles: { colors: ['#eef8ff', '#bae6fd', '#38bdf8', '#cbd5e1'], maxAlpha: 0.54 },
    terminal: {
      background: '#040507',
      foreground: '#e5ecf4',
      cursor: '#38bdf8',
      selectionBackground: '#18374d',
      statusBg: '#0c0d11',
      statusBorder: '#222631',
      statusText: '#8b96a9',
    },
  },
] as const

const THEME_MAP = new Map<ResolvedTheme, ThemeDefinition>(THEMES.map((t) => [t.id, t]))

export function isResolvedTheme(value: unknown): value is ResolvedTheme {
  return typeof value === 'string' && THEME_MAP.has(value as ResolvedTheme)
}

export function isThemeSetting(value: unknown): value is ThemeSetting {
  return value === 'system' || isResolvedTheme(value)
}

export function themeDef(theme: ResolvedTheme): ThemeDefinition {
  return THEME_MAP.get(theme) ?? THEMES[0]
}

export function isDarkTheme(theme: ResolvedTheme): boolean {
  return themeDef(theme).dark
}

export function resolveTheme(setting: string | null | undefined, systemDark: boolean): ResolvedTheme {
  if (isResolvedTheme(setting)) return setting
  return systemDark ? 'dark' : 'light'
}

export function currentResolvedTheme(): ResolvedTheme {
  if (typeof document === 'undefined') return 'light'
  const root = document.documentElement
  const attr = root.dataset?.theme ?? root.getAttribute?.('data-theme')
  if (isResolvedTheme(attr)) return attr
  return root.classList?.contains('dark') ? 'dark' : 'light'
}
