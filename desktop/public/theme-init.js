// 界面画出来之前，按上次的主题切好深色 / 浅色。
// 主题在 settings.json 里，要等 ferry 读完设置才知道；那时界面已经按浅色画过一次了，深色主题下会先闪一下浅色。
// 上次的主题由 state.ts 的 applyTheme 记在 localStorage 的 ferry.theme（第一次打开时没有：跟随系统）。
;(function () {
  var theme = null
  try {
    theme = localStorage.getItem('ferry.theme')
  } catch (e) {}
  var dark = theme === 'dark' || (theme !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches)
  if (dark) document.documentElement.classList.add('dark')
})()
